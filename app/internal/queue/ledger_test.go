// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

func openLedger(t *testing.T, dir string) *Ledger {
	t.Helper()
	lg, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	return lg
}

// A builder test holding a lease keeps the queue off that combination until
// it exits, and the queue carries on with others meanwhile.
func TestLocal_SkipsLeasedUntilHolderExits(t *testing.T) {
	state, results := t.TempDir(), t.TempDir()
	a, b := combo("8.1"), combo("8.2")
	test := openLedger(t, state)
	if ok, err := test.Lease(results, a.ID()); !ok || err != nil {
		t.Fatalf("lease = %v, %v", ok, err)
	}

	l := newLocal(t, state, results, []matrix.Combination{a, b}, false)
	j, st := next(t, l)
	if st != StateJob || j.Combination != b {
		t.Fatalf("claimed %v (%v), want the unleased one", j.Combination.ID(), st)
	}
	writeResult(t, results, b, "pass", "pass", before)
	l.Release(j, finished)
	if _, st := next(t, l); st != StateNone {
		t.Fatalf("state %v while a is leased elsewhere, want none-now", st)
	}
	if l.Done() {
		t.Error("done while a leased combination is still to run")
	}

	test.Close()
	if j, st := next(t, l); st != StateJob || j.Combination != a {
		t.Errorf("after the holder exited claimed %v (%v), want a", j.Combination.ID(), st)
	}
}

func TestLedger_LeaseIsExclusiveAndFreedOnExit(t *testing.T) {
	state, results := t.TempDir(), t.TempDir()
	one, two := openLedger(t, state), openLedger(t, state)
	if ok, _ := one.Lease(results, "x"); !ok {
		t.Fatal("first lease refused")
	}
	if ok, _ := two.Lease(results, "x"); ok {
		t.Fatal("second holder got a held lease")
	}
	if ok, _ := two.Lease(results, "y"); !ok {
		t.Fatal("an unrelated lease was refused")
	}
	if err := one.Unlease(results, "x"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := two.Lease(results, "x"); !ok {
		t.Fatal("lease not freed by Unlease")
	}
	if ok, _ := one.Lease(results, "y"); ok {
		t.Fatal("first holder took the second's lease")
	}
	two.Close()
	if ok, _ := one.Lease(results, "y"); !ok {
		t.Fatal("lease outlived its holder")
	}
}

func TestLocal_ContractChangeNeedsFresh(t *testing.T) {
	state, results := t.TempDir(), t.TempDir()
	combos := []matrix.Combination{combo("8.3")}
	lg := openLedger(t, state)
	l, err := NewLocal(lg, results, combos, Contract{Playwright: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal(lg, results, combos, Contract{Playwright: true}, false); err != nil {
		t.Errorf("same contract refused: %v", err)
	}
	if _, err := NewLocal(lg, results, combos, Contract{}, false); !errors.Is(err, ErrContractChanged) {
		t.Errorf("changed contract: err = %v, want ErrContractChanged", err)
	}
	if _, err := NewLocal(lg, results, combos, Contract{}, true); err != nil {
		t.Errorf("-fresh with a new contract refused: %v", err)
	}
}

func TestLocal_ImportsPhaseOneState(t *testing.T) {
	state, results := t.TempDir(), t.TempDir()
	c := combo("8.3")
	l := newLocal(t, state, results, []matrix.Combination{c}, false)
	if err := os.WriteFile(l.legacy, []byte(`{"started":"2026-11-01T00:00:00Z","attempts":{"`+c.ID()+`":2}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l.ledger.Close()

	l = newLocal(t, state, results, []matrix.Combination{c}, false)
	if l.Tries(c) != 2 {
		t.Fatalf("imported tries = %d, want 2", l.Tries(c))
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.legacy); !os.IsNotExist(err) {
		t.Error("old state file kept after import")
	}
	l.ledger.Close()
	if l = newLocal(t, state, results, []matrix.Combination{c}, false); l.Tries(c) != 2 {
		t.Errorf("tries after import and restart = %d, want 2", l.Tries(c))
	}
}

// Host failures and unasked-for aborts spend their own budget, not the
// combination's tries; aborts the queue asked for spend neither.
func TestLocal_HostFailureBudget(t *testing.T) {
	c := combo("8.3")
	l := newLocal(t, t.TempDir(), t.TempDir(), []matrix.Combination{c}, false)
	outcomes := []Outcome{
		{Kind: OutcomeAborted, Requested: true},
		{Kind: OutcomeHost, Reason: "disk_space"},
		{Kind: OutcomeAborted},
	}
	for _, o := range outcomes {
		j, st := next(t, l)
		if st != StateJob {
			t.Fatalf("nothing to claim before %+v", o)
		}
		l.Release(j, o)
	}
	if l.Tries(c) != 0 {
		t.Errorf("tries = %d after host failures, want 0", l.Tries(c))
	}
	j, st := next(t, l)
	if st != StateJob {
		t.Fatal("ran out after two host failures")
	}
	l.Release(j, Outcome{Kind: OutcomeHost})
	if _, st := next(t, l); st != StateDone {
		t.Errorf("state %v after %d host failures, want done", st, MaxHostFailures)
	}
}

func TestLocal_DoneWaitsForClaims(t *testing.T) {
	c, results := combo("8.3"), t.TempDir()
	l := newLocal(t, t.TempDir(), results, []matrix.Combination{c}, false)
	l.Force = true
	j, _ := next(t, l)
	writeResult(t, results, c, "pass", "pass", before)
	if l.Done() {
		t.Error("done with a claim running")
	}
	l.Release(j, Outcome{Kind: OutcomeResult, Wrote: true})
	if !l.Done() {
		t.Error("not done once the only forced run finished")
	}
}

func TestLocal_RefusesIDCollisions(t *testing.T) {
	a, b := combo("8.3"), combo("8.3")
	b.WebserverVersion = "1.26" // not in the ID
	lg := openLedger(t, t.TempDir())
	if _, err := NewLocal(lg, t.TempDir(), []matrix.Combination{a, a}, Contract{}, false); err != nil {
		t.Errorf("a repeated combination was refused: %v", err)
	}
	if _, err := NewLocal(lg, t.TempDir(), []matrix.Combination{a, b}, Contract{}, false); err == nil {
		t.Error("two combinations sharing an ID were accepted")
	}
}

// A result written by whoever held the lease must be seen, not the "todo"
// read while it was running.
func TestLocal_RereadsResultAfterOtherHolder(t *testing.T) {
	state, results := t.TempDir(), t.TempDir()
	a := combo("8.1")
	test := openLedger(t, state)
	test.Lease(results, a.ID())

	l := newLocal(t, state, results, []matrix.Combination{a}, false)
	if _, st := next(t, l); st != StateNone {
		t.Fatalf("state %v while leased elsewhere, want none-now", st)
	}
	writeResult(t, results, a, "pass", "pass", time.Now())
	test.Close()
	if j, st := next(t, l); st != StateDone {
		t.Errorf("claimed %s (%v) after the other holder passed it", j.Combination.ID(), st)
	}
}

func TestLocal_ChangedContractStillReadable(t *testing.T) {
	state, results := t.TempDir(), t.TempDir()
	c := combo("8.3")
	lg := openLedger(t, state)
	l, _ := NewLocal(lg, results, []matrix.Combination{c}, Contract{Playwright: true}, false)
	j, _, _ := l.Next(context.Background())
	l.Release(j, finished)

	r, err := NewLocal(lg, results, []matrix.Combination{c}, Contract{}, false)
	if !errors.Is(err, ErrContractChanged) || r == nil {
		t.Fatalf("NewLocal = %v, %v; want a readable Local and ErrContractChanged", r, err)
	}
	if r.Tries(c) != 1 {
		t.Errorf("tries read through a changed contract = %d, want 1", r.Tries(c))
	}
}

func TestResultMod(t *testing.T) {
	results, c := t.TempDir(), combo("8.3")
	if !ResultMod(results, c).IsZero() {
		t.Error("no file, but a time")
	}
	writeResult(t, results, c, "pass", "fail", before)
	first := ResultMod(results, c)
	if first.IsZero() {
		t.Fatal("file written, but no time")
	}
	if !ResultMod(results, c).Equal(first) {
		t.Error("unchanged file reads as rewritten")
	}
	time.Sleep(10 * time.Millisecond)
	writeResult(t, results, c, "pass", "fail", before)
	if ResultMod(results, c).Equal(first) {
		t.Error("rewrite in the same second not seen")
	}
}

// A result written between two claims, with no lease seen, must still be read.
func TestLocal_SeesResultWrittenBetweenClaims(t *testing.T) {
	results := t.TempDir()
	a, b := combo("8.1"), combo("8.2")
	l := newLocal(t, t.TempDir(), results, []matrix.Combination{a, b}, false)
	j, st := next(t, l) // caches "todo" for both
	if st != StateJob || j.Combination != a {
		t.Fatalf("first claim %s (%v), want a", j.Combination.ID(), st)
	}
	writeResult(t, results, b, "pass", "pass", time.Now())
	if j, st := next(t, l); st == StateJob {
		t.Errorf("claimed %s, which builder test already passed", j.Combination.ID())
	}
	l.Release(j, finished)
	if j, st := next(t, l); st != StateJob || j.Combination != a {
		t.Errorf("control: a with no result not retried, got %s (%v)", j.Combination.ID(), st)
	}
}
