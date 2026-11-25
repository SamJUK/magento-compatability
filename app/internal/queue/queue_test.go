// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"sync/atomic"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

func combo(php string) matrix.Combination {
	return matrix.Combination{
		Product: "magento", Version: "2.4.8", PHP: php,
		WebserverType: "nginx", WebserverVersion: "1.28",
		DBType: "mariadb", DBVersion: "11.4",
		SearchType: "opensearch", SearchVersion: "3",
		CacheType: "valkey", CacheVersion: "8",
		QueueType: "rabbitmq", QueueVersion: "4.2",
		Varnish: "none",
	}
}

var before = time.Now().Add(-24 * time.Hour)

func writeResult(t *testing.T, dir string, c matrix.Combination, stackUp, overall string, at time.Time) {
	t.Helper()
	r := result.TestResult{ID: c.ID(), Product: c.Product, Version: c.Version, OverallStatus: overall,
		Steps:     map[string]result.Step{"stack_up": {Status: stackUp}},
		Timestamp: at.UTC().Format(time.RFC3339)}
	if err := result.Write(filepath.Join(dir, c.Product, c.ID()+".json"), r); err != nil {
		t.Fatal(err)
	}
}

func newLocal(t *testing.T, state, results string, combos []matrix.Combination, fresh bool) *Local {
	t.Helper()
	lg, err := OpenLedger(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	l, err := NewLocal(lg, results, combos, Contract{}, fresh)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func next(t *testing.T, l *Local) (Job, State) {
	t.Helper()
	j, st, err := l.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return j, st
}

var finished = Outcome{Kind: OutcomeResult}

func drain(t *testing.T, l *Local, outcome func(matrix.Combination)) []string {
	t.Helper()
	var order []string
	for {
		j, st := next(t, l)
		if st != StateJob {
			return order
		}
		order = append(order, j.Combination.ID())
		outcome(j.Combination)
		l.Release(j, finished)
	}
}

func TestLocal_RetryRules(t *testing.T) {
	results := t.TempDir()
	passed, setupFail, oldFail, missing := combo("8.1"), combo("8.2"), combo("8.3"), combo("8.4")
	writeResult(t, results, passed, "pass", "pass", before)
	writeResult(t, results, setupFail, "fail", "fail", before)
	writeResult(t, results, oldFail, "pass", "fail", before)

	l := newLocal(t, t.TempDir(), results, []matrix.Combination{passed, setupFail, oldFail, missing}, false)
	// Every run of a missing one fails after stack_up.
	runs := map[string]int{}
	drain(t, l, func(c matrix.Combination) {
		runs[c.ID()]++
		if c == missing {
			writeResult(t, results, c, "pass", "fail", time.Now())
		}
	})

	want := map[string]int{
		setupFail.ID(): MaxTries, // still failing setup: tried up to the cap
		missing.ID():   2,        // ran, failed, one more go
	}
	for id, n := range want {
		if runs[id] != n {
			t.Errorf("%s ran %d times, want %d", id, runs[id], n)
		}
	}
	if runs[passed.ID()] != 0 || runs[oldFail.ID()] != 0 {
		t.Errorf("pass or older failure ran: %v", runs)
	}
}

func TestLocal_RetryFailuresAndForce(t *testing.T) {
	results := t.TempDir()
	passed, oldFail := combo("8.1"), combo("8.3")
	writeResult(t, results, passed, "pass", "pass", before)
	writeResult(t, results, oldFail, "pass", "fail", before)

	// Every rerun fails again, with a fresh timestamp.
	failAgain := func(c matrix.Combination) { writeResult(t, results, c, "pass", "fail", time.Now()) }

	l := newLocal(t, t.TempDir(), results, []matrix.Combination{passed, oldFail}, false)
	l.RetryFailures = true
	if got := drain(t, l, failAgain); len(got) != 1 || got[0] != oldFail.ID() {
		t.Errorf("RetryFailures ran %v, want only the older failure once", got)
	}

	writeResult(t, results, passed, "pass", "pass", before)
	writeResult(t, results, oldFail, "pass", "fail", before)
	l = newLocal(t, t.TempDir(), results, []matrix.Combination{passed, oldFail}, false)
	l.Force = true
	if got := drain(t, l, failAgain); len(got) != 2 {
		t.Errorf("Force ran %v, want each once", got)
	}
}

func TestLocal_RerunsCountOnlyTheirOwnResults(t *testing.T) {
	c := combo("8.3")
	for _, tc := range []struct {
		stackUp, overall string
		force, retry     bool
		status           string
	}{
		{"pass", "pass", true, false, StatusPass},
		{"pass", "fail", false, true, StatusFail},
		{"fail", "fail", false, false, StatusSetupFail}, // retried regardless
	} {
		for _, writes := range []bool{false, true} {
			results := t.TempDir()
			writeResult(t, results, c, tc.stackUp, tc.overall, time.Now().Add(-time.Hour))
			l := newLocal(t, t.TempDir(), results, []matrix.Combination{c}, false)
			l.Force, l.RetryFailures = tc.force, tc.retry
			drain(t, l, func(c matrix.Combination) {
				if writes {
					writeResult(t, results, c, tc.stackUp, tc.overall, time.Now())
				}
			})
			if n := l.Counts(); (n[tc.status] == 1) != writes || (n[StatusTodo] == 1) == writes {
				t.Errorf("%s rerun wrote %v: counts %v", tc.status, writes, n)
			}
		}
	}
}

func TestLocal_AttemptsSurviveRestart(t *testing.T) {
	results, state := t.TempDir(), t.TempDir()
	c := combo("8.3")
	combos := []matrix.Combination{c}

	l := newLocal(t, state, results, combos, false)
	j, _ := next(t, l)
	l.Release(j, finished) // with no result
	l.ledger.Close()

	l = newLocal(t, state, results, combos, false)
	if l.Tries(c) != 1 {
		t.Fatalf("tries after restart = %d, want 1", l.Tries(c))
	}
	if n := len(drain(t, l, func(matrix.Combination) {})); n != MaxTries-1 {
		t.Errorf("ran %d more times, want %d", n, MaxTries-1)
	}

	l.ledger.Close()
	l = newLocal(t, state, results, combos, true)
	if l.Tries(c) != 0 {
		t.Errorf("fresh kept %d tries", l.Tries(c))
	}
	other := newLocal(t, state, results, []matrix.Combination{c, combo("8.4")}, false)
	if other.Tries(c) != 0 {
		t.Error("a different sweep shared attempts")
	}
}

// A run cut short must not use up a forced run or an older failure's one go.
func TestLocal_InterruptedRunsDontCount(t *testing.T) {
	results, state := t.TempDir(), t.TempDir()
	passed, oldFail := combo("8.1"), combo("8.3")
	writeResult(t, results, passed, "pass", "pass", before)
	writeResult(t, results, oldFail, "pass", "fail", before)
	combos := []matrix.Combination{passed, oldFail}

	l := newLocal(t, state, results, combos, false)
	l.Force, l.RetryFailures = true, true
	for range combos {
		j, st := next(t, l)
		if st != StateJob {
			t.Fatal("nothing to claim")
		}
		l.Release(j, Outcome{Kind: OutcomeAborted, Requested: true})
	}
	l.ledger.Close()

	l.ledger.Close()
	l = newLocal(t, state, results, combos, false)
	l.Force, l.RetryFailures = true, true
	if got := drain(t, l, func(matrix.Combination) {}); len(got) != 2 {
		t.Errorf("after an interrupted sweep ran %v, want both once", got)
	}
}

// A crash after the result is written but before it is counted must not turn
// this sweep's failure into an older one and lose its retry.
func TestLocal_CrashAfterWriteKeepsRetry(t *testing.T) {
	results, state := t.TempDir(), t.TempDir()
	c := combo("8.3")
	l := newLocal(t, state, results, []matrix.Combination{c}, false)
	l.initial.Started = time.Now().Add(-time.Hour).Truncate(time.Second)
	next(t, l)
	writeResult(t, results, c, "pass", "fail", time.Now().Add(-30*time.Minute))
	l.ledger.Close() // crash: no Release

	l = newLocal(t, state, results, []matrix.Combination{c}, false)
	if got := drain(t, l, func(matrix.Combination) {}); len(got) == 0 {
		t.Error("this sweep's failure lost its retry after a crash")
	}
}

func TestLocal_FreshIsSaved(t *testing.T) {
	results, state := t.TempDir(), t.TempDir()
	c := combo("8.3")
	l := newLocal(t, state, results, []matrix.Combination{c}, false)
	drain(t, l, func(matrix.Combination) {})
	l.ledger.Close()
	f := newLocal(t, state, results, []matrix.Combination{c}, true)
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	if l := newLocal(t, state, results, []matrix.Combination{c}, false); l.Tries(c) != 0 {
		t.Errorf("-fresh was forgotten: %d tries", l.Tries(c))
	}
}

// State saved before start times were kept must not make every old failure
// look like this sweep's.
func TestLocal_OldStateTreatsResultsAsOlder(t *testing.T) {
	results, state := t.TempDir(), t.TempDir()
	c := combo("8.3")
	writeResult(t, results, c, "pass", "fail", time.Now().Add(-time.Hour))
	l := newLocal(t, state, results, []matrix.Combination{c}, false)
	if err := os.WriteFile(l.legacy, []byte(`{"attempts":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l.ledger.Close()
	l = newLocal(t, state, results, []matrix.Combination{c}, false)
	if got := drain(t, l, func(matrix.Combination) {}); len(got) != 0 {
		t.Errorf("an older failure ran from old state: %v", got)
	}
}

func TestLocal_ConcurrentUse(t *testing.T) {
	results := t.TempDir()
	combos := []matrix.Combination{combo("8.1"), combo("8.2"), combo("8.3"), combo("8.4")}
	l := newLocal(t, t.TempDir(), results, combos, false)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, st, _ := l.Next(context.Background())
				switch st {
				case StateDone:
					return
				case StateNone:
					continue
				}
				_ = l.Tries(j.Combination)
				_ = l.Counts()
				_ = l.Done()
				l.Release(j, finished)
			}
		}()
	}
	wg.Wait()
}

// fakeSource hands out n items; the first one fails once and comes back,
// which only works if the engine keeps polling while it runs.
type fakeSource struct {
	mu      sync.Mutex
	queue   []string
	running int
	retried bool
}

func (f *fakeSource) Next(context.Context) (Job, State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		if f.running > 0 {
			return Job{}, StateNone, nil
		}
		return Job{}, StateDone, nil
	}
	j := Job{Combination: matrix.Combination{Product: f.queue[0]}}
	f.queue = f.queue[1:]
	f.running++
	return j, StateJob, nil
}

func (f *fakeSource) Release(j Job, _ Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running--
	if j.Combination.Product == "a" && !f.retried {
		f.retried = true
		f.queue = append(f.queue, "a")
	}
}

func (f *fakeSource) Done() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue) == 0 && f.running == 0
}

func TestEngine_RunsEverythingIncludingLateRetries(t *testing.T) {
	src := &fakeSource{queue: []string{"a", "b", "c", "d"}}
	var ran, concurrent, peak atomic.Int32
	e := &Engine{
		Source:   src,
		MaxSlots: 4,
		Slots:    func() int { return 2 },
		Wait:     func() string { return "" },
		Run: func(ctx context.Context, slot int, j Job) Outcome {
			n := concurrent.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
			ran.Add(1)
			return finished
		},
		Log:      log.New(io.Discard, "", 0),
		IdlePoll: time.Millisecond,
	}
	done := make(chan struct{})
	go func() { e.Start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("engine never finished")
	}
	if ran.Load() != 5 {
		t.Errorf("ran %d, want 5 (4 plus a retry)", ran.Load())
	}
	if peak.Load() != 2 {
		t.Errorf("peak concurrency %d, want 2", peak.Load())
	}
}

func TestEngine_WaitBlocksRuns(t *testing.T) {
	var ran atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e := &Engine{
		Source:   &fakeSource{queue: []string{"a"}},
		MaxSlots: 1,
		Slots:    func() int { return 1 },
		Wait:     func() string { return "disk full" },
		Run:      func(context.Context, int, Job) Outcome { ran.Add(1); return finished },
		Log:      log.New(io.Discard, "", 0),
		WaitPoll: time.Millisecond,
	}
	e.Start(ctx)
	if ran.Load() != 0 {
		t.Error("ran while Wait said no")
	}

	e.Wait = func() string { return "" }
	e.Start(context.Background())
	if ran.Load() == 0 {
		t.Error("never ran once Wait cleared")
	}
}

// A run that errors without a result must still use up its tries.
func TestEngine_FailedRunsCount(t *testing.T) {
	l := newLocal(t, t.TempDir(), t.TempDir(), []matrix.Combination{combo("8.3")}, false)
	var ran atomic.Int32
	e := &Engine{
		Source:   l,
		MaxSlots: 1,
		Slots:    func() int { return 1 },
		Wait:     func() string { return "" },
		Run:      func(context.Context, int, Job) Outcome { ran.Add(1); return finished },
		Log:      log.New(io.Discard, "", 0),
		IdlePoll: time.Millisecond,
	}
	done := make(chan struct{})
	go func() { e.Start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("engine never finished after %d runs", ran.Load())
	}
	if ran.Load() != MaxTries {
		t.Errorf("ran %d times, want %d", ran.Load(), MaxTries)
	}
}

func TestOwnVolumes(t *testing.T) {
	p := "m2test-0123456789abcdef-01234567"
	got := ownVolumes(p, []string{p + "_db-data", "backup_" + p + "_db-data", p + "x_db-data", "m2test-vendor-cache", p + "_magento"})
	if len(got) != 2 || got[0] != p+"_db-data" || got[1] != p+"_magento" {
		t.Errorf("ownVolumes = %v", got)
	}
}

// A finished sweep must exit even while the disk guard says wait.
func TestEngine_DrainsWhileWaiting(t *testing.T) {
	e := &Engine{
		Source:   &fakeSource{},
		MaxSlots: 2,
		Slots:    func() int { return 2 },
		Wait:     func() string { return "disk full" },
		Run:      func(context.Context, int, Job) Outcome { return finished },
		Log:      log.New(io.Discard, "", 0),
		IdlePoll: time.Millisecond,
		WaitPoll: time.Millisecond,
	}
	done := make(chan struct{})
	go func() { e.Start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine hung waiting with nothing left to run")
	}
}

func TestLocal_RetriesWaitBehindFreshWork(t *testing.T) {
	a, b := combo("8.1"), combo("8.2")
	l := newLocal(t, t.TempDir(), t.TempDir(), []matrix.Combination{a, b}, false)
	got := drain(t, l, func(matrix.Combination) {})
	if len(got) < 2 || got[0] == got[1] {
		t.Errorf("order %v: a retry ran before untried work", got)
	}
}

type countingSource struct {
	fakeSource
	claims atomic.Int32
}

func (s *countingSource) Next(ctx context.Context) (Job, State, error) {
	j, st, err := s.fakeSource.Next(ctx)
	if st == StateJob {
		s.claims.Add(1)
	}
	return j, st, err
}

// A slot held up by the guards claims nothing, so a slot drop while waiting
// is respected once they clear.
func TestEngine_WaitingSlotsHoldNoClaim(t *testing.T) {
	src := &countingSource{fakeSource: fakeSource{queue: []string{"a", "b"}}}
	var slots, concurrent, peak atomic.Int32
	var clear atomic.Bool
	slots.Store(2)
	e := &Engine{
		Source:   src,
		MaxSlots: 2,
		Slots:    func() int { return int(slots.Load()) },
		Wait: func() string {
			if clear.Load() {
				return ""
			}
			return "disk full"
		},
		Run: func(context.Context, int, Job) Outcome {
			n := concurrent.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
			return finished
		},
		Log:      log.New(io.Discard, "", 0),
		IdlePoll: time.Millisecond,
		WaitPoll: time.Millisecond,
	}
	done := make(chan struct{})
	go func() { e.Start(context.Background()); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if n := src.claims.Load(); n != 0 {
		t.Fatalf("%d claims while waiting", n)
	}
	slots.Store(1)
	clear.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("engine never finished")
	}
	if src.claims.Load() != 3 { // a, b and a's retry
		t.Errorf("claims = %d, want 3", src.claims.Load())
	}
	if peak.Load() != 1 {
		t.Errorf("peak concurrency %d after slots dropped to 1", peak.Load())
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"82.67MiB": 86685777, "1.5GiB": 1610612736, "512B": 512, "2GB": 2e9, "": 0, "junk": 0} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
