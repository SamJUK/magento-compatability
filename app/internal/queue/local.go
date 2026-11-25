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
	ids := make([]string, len(combos))
	byID := map[string]matrix.Combination{}
	for i, c := range combos {
		ids[i] = c.ID()
		if other, seen := byID[ids[i]]; seen && other != c {
			// They'd share a result file and attempt count.
			return nil, fmt.Errorf("queue: %+v and %+v share the ID %s", other, c, ids[i])
		}
		byID[ids[i]] = c
	}
	sort.Strings(ids)
	h := sha256.New()
	fmt.Fprintln(h, canonicalDir(resultsDir))
	for _, id := range ids {
		fmt.Fprintln(h, id)
	}
	key := fmt.Sprintf("%x", h.Sum(nil)[:8])

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

// Next claims the first eligible combination not yet tried, then retries, so
// a retry waits behind fresh work rather than running straight away. One
// leased by another process is left for later.
func (l *Local) Next(context.Context) (Job, State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Parse results before taking the ledger lock: the first pass reads
	// every file and would hold up builder test meanwhile.
	for _, c := range l.Combos {
		l.result(c)
	}
	var j Job
	state := StateDone
	err := l.ledger.update(func(d *ledgerData) error {
		s := l.writeSweep(d)
		l.ledger.dropDeadLeases(d)
		if len(l.claimed) > 0 {
			state = StateNone
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
					state = StateNone
					continue
				}
				if !l.eligible(s, c) {
					continue
				}
				l.claim(d, s, c)
				j, state = Job{Combination: c, Tries: s.state(id).Tries}, StateJob
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return Job{}, StateNone, err
	}
	return j, state, nil
}

func (l *Local) claim(d *ledgerData, s *sweep, c matrix.Combination) {
	id := c.ID()
	l.claimed[id] = true
	d.Leases[leaseKey(l.ResultsDir, id)] = lease{Holder: l.ledger.holder, At: time.Now().UTC()}
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
	l.mu.Lock()
	defer l.mu.Unlock()
	id := j.Combination.ID()
	delete(l.claimed, id)
	err := l.ledger.update(func(d *ledgerData) error {
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
	if err != nil {
		// Losing the count only risks an extra retry after a restart.
		fmt.Fprintf(os.Stderr, "queue: saving attempts: %v\n", err)
	}
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

// and how many have been tried.
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
