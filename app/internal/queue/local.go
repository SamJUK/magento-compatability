// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

const MaxTries = 3

// MaxHostFailures caps runs the host failed, so a combination that keeps
// breaking the host stops.
const MaxHostFailures = 3

var ErrContractChanged = errors.New("this sweep ran with different options; -fresh starts it over")

// Local picks combinations by their result on disk and how often this sweep
// has tried them:
//   - no result, or a stack_up failure: up to MaxTries
//   - any other failure: one more go if this sweep produced it, since load can
//     time a test out; a failure from before the sweep gets one run, and only
//     with RetryFailures or Force
//   - a pass: never, unless Force and not yet tried
//
// A pass or an older failure that gets its one run is marked Once when
// claimed, so a failed rerun's fresh timestamp can't earn it a second.
//
// A run counts once it finishes, so one cut short by Ctrl+C is not held
// against the combination. Whether a failure is from this sweep goes by its
// timestamp, so a crash between writing a result and counting it costs at
// most one extra run. Each claim takes a lease in the ledger, so builder test
// and the queue never run the same combination at once.
type Local struct {
	Combos        []matrix.Combination
	ResultsDir    string
	Force         bool
	RetryFailures bool

	ledger  *Ledger
	key     string
	initial *sweep // used until Save puts the sweep in the ledger
	legacy  string // an older state file to remove once imported

	mu      sync.Mutex
	claimed map[string]bool
	// results caches each result's status, which carries full logs and is
	// slow to parse. Entries are checked against the file's mtime and size.
	results map[string]cachedResult
}

type cachedResult struct {
	mod    time.Time
	size   int64
	status string
	at     time.Time
}

// NewLocal finds this sweep (results directory and combination list) in the ledger.
// A different contract returns ErrContractChanged with a read-only Local unless fresh. Nothing is written until Save.
func NewLocal(lg *Ledger, resultsDir string, combos []matrix.Combination, contract Contract, fresh bool) (*Local, error) {
	key, err := sweepKey(resultsDir, combos)
	if err != nil {
		return nil, err
	}

	l := &Local{
		Combos:     combos,
		ResultsDir: resultsDir,
		ledger:     lg,
		key:        key,
		legacy:     filepath.Join(lg.dir, "sweep-"+key+".json"),
		claimed:    map[string]bool{},
		results:    map[string]cachedResult{},
	}
	newSweep := &sweep{Started: time.Now().UTC().Truncate(time.Second), Contract: contract, Combos: map[string]*comboState{}}
	if fresh {
		l.initial = newSweep
		return l, nil
	}
	var existing *sweep
	if err := lg.view(func(d *ledgerData) error { existing = d.Sweeps[key]; return nil }); err != nil {
		return nil, err
	}
	switch {
	case existing != nil && existing.Contract != contract:
		return l, ErrContractChanged
	case existing != nil:
		return l, nil
	}
	l.initial = newSweep
	imported, err := readLegacy(l.legacy)
	if err != nil {
		return nil, err
	}
	if imported != nil {
		imported.Contract = contract
		l.initial = imported
	}
	return l, nil
}

func sweepKey(resultsDir string, combos []matrix.Combination) (string, error) {
	ids := make([]string, len(combos))
	byID := map[string]matrix.Combination{}
	for i, c := range combos {
		ids[i] = c.ID()
		if other, seen := byID[ids[i]]; seen && other != c {
			// They'd share a result file and attempt count.
			return "", fmt.Errorf("queue: %+v and %+v share the ID %s", other, c, ids[i])
		}
		byID[ids[i]] = c
	}
	sort.Strings(ids)
	h := sha256.New()
	fmt.Fprintln(h, canonicalDir(resultsDir))
	for _, id := range ids {
		fmt.Fprintln(h, id)
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8]), nil
}

func SweepContract(lg *Ledger, resultsDir string, combos []matrix.Combination) (Contract, bool, error) {
	key, err := sweepKey(resultsDir, combos)
	if err != nil {
		return Contract{}, false, err
	}
	var c Contract
	found := false
	err = lg.view(func(d *ledgerData) error {
		if sw := d.Sweeps[key]; sw != nil {
			c, found = sw.Contract, true
		}
		return nil
	})
	return c, found, err
}

func readLegacy(path string) (*sweep, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var old struct {
		Started  time.Time       `json:"started"`
		Attempts map[string]int  `json:"attempts"`
		Once     map[string]bool `json:"once"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return nil, fmt.Errorf("queue: reading %s: %w", path, err)
	}
	s := &sweep{Started: old.Started, Combos: map[string]*comboState{}}
	if s.Started.IsZero() {
		// Kept no start time: every result counts as from before it.
		s.Started = time.Now().UTC().Truncate(time.Second)
	}
	for id, n := range old.Attempts {
		s.state(id).Tries = n
	}
	for id, once := range old.Once {
		s.state(id).Once = once
	}
	return s, nil
}

func (s *sweep) state(id string) *comboState {
	cs := s.Combos[id]
	if cs == nil {
		cs = &comboState{}
		s.Combos[id] = cs
	}
	return cs
}

// Save puts the sweep in the ledger, so its start time and -fresh survive a crash. Only the queue holding the queue lock may call it.
func (l *Local) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.ledger.update(func(d *ledgerData) error {
		l.writeSweep(d)
		return nil
	})
	if err == nil {
		os.Remove(l.legacy)
	}
	return err
}

// readSweep is the sweep for reading: a fresh one not yet saved, else d's.
func (l *Local) readSweep(d *ledgerData) *sweep {
	if l.initial != nil {
		return l.initial
	}
	return l.stored(d)
}

// writeSweep is d's sweep for changing, once a fresh one not yet saved
// has replaced it.
func (l *Local) writeSweep(d *ledgerData) *sweep {
	if l.initial != nil {
		d.Sweeps[l.key] = l.initial
		l.initial = nil
		// Attempts from before -fresh ran under the old contract.
		for _, a := range d.Attempts {
			if a.Sweep == l.key && !a.Released {
				a.State, a.Reason, a.Released = AttemptAborted, "sweep started fresh", true
			}
		}
	}
	return l.stored(d)
}

func (l *Local) stored(d *ledgerData) *sweep {
	s := d.Sweeps[l.key]
	if s == nil {
		// Removed from under us; carry on as a fresh sweep.
		s = &sweep{Started: time.Now().UTC().Truncate(time.Second), Combos: map[string]*comboState{}}
		d.Sweeps[l.key] = s
	}
	if s.Combos == nil {
		s.Combos = map[string]*comboState{}
	}
	return s
}

// StatePath is where the sweep is saved.
func (l *Local) StatePath() string { return l.ledger.Path() }

func (l *Local) Tries(c matrix.Combination) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	l.ledger.view(func(d *ledgerData) error {
		if cs := l.readSweep(d).Combos[c.ID()]; cs != nil {
			n = cs.Tries
		}
		return nil
	})
	return n
}

func (l *Local) Next(ctx context.Context) (Job, State, error) {
	return l.Claim(ctx, "", "")
}

// Claim takes the first eligible combination not yet tried, then retries, so
// a retry waits behind fresh work rather than running straight away. One
// leased by another process is left for later.
//
// A claim for a remote worker is recorded as an attempt. The same request
// again returns the same attempt, so a reply lost on the way isn't a lost
// claim.
func (l *Local) Claim(_ context.Context, worker, request string) (Job, State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Parse results before taking the ledger lock: the first pass reads
	// every file and would hold up builder test meanwhile.
	for _, c := range l.Combos {
		l.result(c)
	}
	var j Job
	state := StateDone
	// Idle workers ask every second; only a claim is worth an fsync.
	err := l.ledger.locked(syscall.LOCK_EX, func(d *ledgerData) error {
		if err := l.claimIn(d, worker, request, &j, &state); err != nil || state != StateJob {
			return err
		}
		return l.ledger.save(d)
	})
	if err != nil {
		return Job{}, StateNone, err
	}
	return j, state, nil
}

func (l *Local) claimIn(d *ledgerData, worker, request string, j *Job, state *State) error {
	s := l.writeSweep(d)
	l.ledger.dropDeadLeases(d)
	if request != "" {
		for _, a := range d.Attempts {
			if a.Worker == worker && a.Request == request && a.Sweep == l.key {
				if a.State != AttemptRunning {
					*state = StateNone
					return nil
				}
				*j, *state = l.jobFor(s, a), StateJob
				return nil
			}
		}
	}
	if len(l.claimed) > 0 {
		*state = StateNone
	}
	for _, retries := range []bool{false, true} {
		for _, c := range l.Combos {
			id := c.ID()
			cs := s.Combos[id]
			tried := cs != nil && cs.Tries > 0
			if tried != retries || l.claimed[id] {
				continue
			}
			if ls, held := d.Leases[leaseKey(l.ResultsDir, id)]; held && ls.Holder != l.ledger.holder {
				*state = StateNone
				continue
			}
			if !l.eligible(s, c) {
				continue
			}
			l.claim(d, s, c)
			*j, *state = Job{Combination: c, Tries: s.state(id).Tries}, StateJob
			if worker != "" {
				a := &Attempt{ID: newID(), Sweep: l.key, Combo: id, Lease: leaseKey(l.ResultsDir, id),
					Worker: worker, Request: request, State: AttemptRunning, Claimed: time.Now().UTC()}
				d.Attempts[a.ID] = a
				j.Attempt, j.Worker, j.Claimed = a.ID, worker, a.Claimed
			}
			return nil
		}
	}
	return nil
}

func (l *Local) jobFor(s *sweep, a *Attempt) Job {
	j := Job{Attempt: a.ID, Worker: a.Worker, Claimed: a.Claimed}
	for _, c := range l.Combos {
		if c.ID() == a.Combo {
			j.Combination = c
		}
	}
	if cs := s.Combos[a.Combo]; cs != nil {
		j.Tries = cs.Tries
	}
	return j
}

func (l *Local) Resume() ([]Job, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var jobs []Job
	err := l.ledger.update(func(d *ledgerData) error {
		s := l.writeSweep(d)
		l.ledger.dropDeadLeases(d)
		for _, a := range d.Attempts {
			if a.Sweep != l.key || a.Released {
				continue
			}
			if ls, held := d.Leases[a.Lease]; held && ls.Holder != l.ledger.holder {
				a.State, a.Reason, a.Released = AttemptAborted, "leased by another process", true
				continue
			}
			l.claimed[a.Combo] = true
			d.Leases[a.Lease] = lease{Holder: l.ledger.holder, At: time.Now().UTC()}
			jobs = append(jobs, l.jobFor(s, a))
		}
		return nil
	})
	return jobs, err
}

func (l *Local) claim(d *ledgerData, s *sweep, c matrix.Combination) {
	id := c.ID()
	l.claimed[id] = true
	takeLease(d, leaseKey(l.ResultsDir, id), l.ledger.holder)
	if r := l.result(c); r.status == StatusPass || (r.status == StatusFail && r.at.Before(s.Started)) {
		s.state(id).Once = true
	}
}

// result re-parses only when the file changed, since builder test can write
// one while the queue runs.
func (l *Local) result(c matrix.Combination) cachedResult {
	var mod time.Time
	var size int64
	if fi, err := os.Stat(resultPath(l.ResultsDir, c)); err == nil {
		mod, size = fi.ModTime(), fi.Size()
	}
	r, ok := l.results[c.ID()]
	if !ok || !r.mod.Equal(mod) || r.size != size {
		r = cachedResult{mod: mod, size: size}
		r.status, r.at = readResult(l.ResultsDir, c)
		l.results[c.ID()] = r
	}
	return r
}

// ResultMod is when c's result file was last written, zero if there is none.
func ResultMod(resultsDir string, c matrix.Combination) time.Time {
	fi, err := os.Stat(resultPath(resultsDir, c))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func resultPath(resultsDir string, c matrix.Combination) string {
	return filepath.Join(resultsDir, c.Product, c.ID()+".json")
}

func (l *Local) Release(j Job, o Outcome) {
	if err := l.Settle(j, o); err != nil {
		// Losing the count only risks an extra retry after a restart.
		fmt.Fprintf(os.Stderr, "queue: saving attempts: %v\n", err)
	}
}

// Settle is Release for a caller that must know the release was saved.
func (l *Local) Settle(j Job, o Outcome) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := j.Combination.ID()
	err := l.ledger.update(func(d *ledgerData) error {
		if a := d.Attempts[j.Attempt]; a != nil {
			switch {
			case a.Released:
				return nil // the claim may be another attempt's by now
			case a.State == AttemptOverridden:
				// builder test took it over; not held against anyone.
				o = Outcome{Kind: OutcomeAborted, Requested: true}
			case a.State == AttemptInstalled:
				// Installed before a crash; the result is on disk.
				o = Outcome{Kind: OutcomeResult, Wrote: true}
			case a.live():
				a.State, a.Reason = attemptState(o), o.Reason
			}
			a.Released = true
		}
		if d.Leases[leaseKey(l.ResultsDir, id)].Holder == l.ledger.holder {
			delete(d.Leases, leaseKey(l.ResultsDir, id))
		}
		cs := l.writeSweep(d).state(id)
		switch {
		case o.Kind == OutcomeResult:
			cs.Tries++
		case o.Kind == OutcomeHost, o.Kind == OutcomeAborted && !o.Requested:
			cs.HostFailures++
		}
		return nil
	})
	if err == nil {
		delete(l.claimed, id)
	}
	return err
}

// Install writes a remote attempt's result if it is still current, marking it installing with the upload's hash first, so a re-upload after a crash finishes the job.
func (l *Local) Install(j Job, r result.TestResult, hash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ledger.update(func(d *ledgerData) error {
		a := d.Attempts[j.Attempt]
		switch {
		case a == nil, a.Sweep != l.key:
			return ErrFenced
		case a.State == AttemptInstalled && a.Hash == hash:
			return nil
		case a.Released, !a.live():
			return ErrFenced
		}
		a.State, a.Hash = AttemptInstalling, hash
		if err := l.ledger.save(d); err != nil {
			return err
		}
		if err := result.Write(resultPath(l.ResultsDir, j.Combination), r); err != nil {
			return err
		}
		a.State = AttemptInstalled
		return nil
	})
}

func (l *Local) AttemptResult(id, worker string) (state, hash string) {
	l.ledger.view(func(d *ledgerData) error {
		if a := d.Attempts[id]; a != nil && a.Worker == worker {
			state, hash = a.State, a.Hash
		}
		return nil
	})
	return state, hash
}

func (l *Local) AttemptState(id string) string {
	st := ""
	l.ledger.view(func(d *ledgerData) error {
		if a := d.Attempts[id]; a != nil {
			st = a.State
		}
		return nil
	})
	return st
}

func (l *Local) Done() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.claimed) > 0 {
		return false
	}
	done := true
	err := l.ledger.view(func(d *ledgerData) error {
		s := l.readSweep(d)
		for _, c := range l.Combos {
			if l.eligible(s, c) {
				done = false
				return nil
			}
		}
		return nil
	})
	return err == nil && done
}

func (l *Local) eligible(s *sweep, c matrix.Combination) bool {
	var cs comboState
	if p := s.Combos[c.ID()]; p != nil {
		cs = *p
	}
	if cs.Tries >= MaxTries || cs.HostFailures >= MaxHostFailures || (cs.Once && cs.Tries >= 1) {
		return false
	}
	if l.Force && cs.Tries == 0 {
		return true
	}
	r := l.result(c)
	switch r.status {
	case StatusTodo, StatusSetupFail:
		return true
	case StatusFail:
		if r.at.Before(s.Started) {
			return l.RetryFailures && cs.Tries == 0
		}
		return cs.Tries <= 1
	}
	return false
}

func (l *Local) Counts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := map[string]int{}
	l.ledger.view(func(d *ledgerData) error {
		s := l.readSweep(d)
		for _, c := range l.Combos {
			r := l.result(c)
			// A forced or retried rerun that left no result of its own confirmed nothing.
			retried := l.Force || r.status == StatusSetupFail || l.RetryFailures && r.status == StatusFail
			if retried && r.at.Before(s.Started) {
				r.status = StatusTodo
			}
			n[r.status]++
			if l.eligible(s, c) {
				n["eligible"]++
				if !l.claimed[c.ID()] {
					n["queued"]++
				}
			}
			if cs := s.Combos[c.ID()]; cs != nil && (cs.Tries > 0 || cs.HostFailures > 0) {
				n["tried"]++
			}
		}
		return nil
	})
	return n
}

const (
	StatusTodo      = "todo"
	StatusPass      = "pass"
	StatusFail      = "fail"       // failed after the stack came up
	StatusSetupFail = "setup_fail" // stack_up failed or is missing
)

func Status(resultsDir string, c matrix.Combination) string {
	s, _ := readResult(resultsDir, c)
	return s
}

// readResult returns the result's status and when it was written. An
// unreadable timestamp reads as zero, so the result counts as older.
func readResult(resultsDir string, c matrix.Combination) (string, time.Time) {
	data, err := os.ReadFile(resultPath(resultsDir, c))
	if err != nil {
		return StatusTodo, time.Time{}
	}
	var r result.TestResult
	if err := json.Unmarshal(data, &r); err != nil {
		return StatusTodo, time.Time{}
	}
	at, _ := time.Parse(time.RFC3339, r.Timestamp)
	switch {
	case r.OverallStatus == result.StatusPass:
		return StatusPass, at
	case r.Steps["stack_up"].Status != result.StatusPass:
		return StatusSetupFail, at
	default:
		return StatusFail, at
	}
}
