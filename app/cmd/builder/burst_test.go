// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/cloud"
	"github.com/samjuk/magento-compatability/internal/pool"
)

type fakeCloud struct {
	mu      sync.Mutex
	next    int64
	servers map[int64]cloud.Server
	// createErr fails Create; made says whether the server exists anyway.
	createErr error
	made      bool
	listErr   error
	hideFor   int // List shows nothing this many times first
	lockedFor int // Delete answers 423 this many times first
	actionErr error
	deleteErr error
	getLabels map[string]string // what Get says the labels are, if set
	onCreate  func(cloud.Spec)
	specs     []cloud.Spec
}

func (f *fakeCloud) Create(_ context.Context, s cloud.Spec) (cloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onCreate != nil {
		f.onCreate(s)
	}
	f.specs = append(f.specs, s)
	if f.createErr != nil && !f.made {
		return cloud.Server{}, f.createErr
	}
	f.next++
	srv := cloud.Server{ID: f.next, Name: s.Name, IP: "203.0.113." + string(rune('0'+f.next)), Labels: s.Labels}
	f.servers[srv.ID] = srv
	if f.actionErr != nil {
		return srv, f.actionErr
	}
	if f.createErr != nil {
		// Made, but the answer lost.
		return cloud.Server{}, f.createErr
	}
	return srv, nil
}

func (f *fakeCloud) Get(_ context.Context, id int64) (cloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok {
		return s, cloud.ErrNotFound
	}
	if f.getLabels != nil {
		s.Labels = f.getLabels
	}
	return s, nil
}

func (f *fakeCloud) Delete(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if f.lockedFor > 0 {
		f.lockedFor--
		return &cloud.APIError{Status: 423, Code: "locked"}
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.servers, id)
	return nil
}

func (f *fakeCloud) List(_ context.Context, selector string) ([]cloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.hideFor > 0 {
		f.hideFor--
		return nil, nil
	}
	var out []cloud.Server
	for _, s := range f.servers {
		match := true
		for _, term := range strings.Split(selector, ",") {
			k, v, _ := strings.Cut(term, "=")
			match = match && s.Labels[k] == v
		}
		if match {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeCloud) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.servers)
}

type fakeHost struct {
	mu       sync.Mutex
	notYet   int // Script fails this many times first
	setupErr error
}

func (h *fakeHost) Script(context.Context, string, string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.notYet > 0 {
		h.notYet--
		return nil, errors.New("connection refused")
	}
	return nil, nil
}

func (h *fakeHost) Setup(context.Context, string, *pool.Worker, string) error { return h.setupErr }

type fakeFleet struct {
	mu        sync.Mutex
	admitted  map[string]bool
	busy      map[string]bool
	drained   map[string]bool
	abandoned []string
}

func testBurst(t *testing.T, prov *fakeCloud, host *fakeHost) (*burst, *fakeFleet) {
	t.Helper()
	provisionPoll, deleteWait = time.Millisecond, 50*time.Millisecond
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ssh"), 0o700)
	if prov.servers == nil {
		prov.servers = map[int64]cloud.Server{}
	}
	ff := &fakeFleet{admitted: map[string]bool{}, busy: map[string]bool{}, drained: map[string]bool{}}
	b := &burst{
		pool: &pool.Pool{Dir: dir}, host: host, prov: prov, log: log.New(io.Discard, "", 0),
		poolKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPool m2test-pool", poolID: "a1b2c3d4e5f6",
		connect: func(ctx context.Context, _ string, _ *pool.Worker) { <-ctx.Done() },
		admit: func(_, name string) {
			ff.mu.Lock()
			defer ff.mu.Unlock()
			ff.admitted[name] = true
		},
		drain: func(name string, always bool) bool {
			ff.mu.Lock()
			defer ff.mu.Unlock()
			if !ff.busy[name] || always {
				ff.drained[name] = true
			}
			return !ff.busy[name]
		},
		abandon: func(name string) {
			ff.mu.Lock()
			defer ff.mu.Unlock()
			ff.abandoned = append(ff.abandoned, name)
		},
	}
	b.cfg.Hetzner.Slots, b.cfg.MaxAge = 5, time.Hour
	return b, ff
}

func inventory(t *testing.T, b *burst) map[string]*pool.Worker {
	t.Helper()
	inv, err := b.pool.Load()
	if err != nil {
		t.Fatal(err)
	}
	return inv.Workers
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for until := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(until) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (b *burst) readyCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, bw := range b.workers {
		if bw.ready {
			n++
		}
	}
	return n
}

func TestBurstRecordsTheWorkerBeforeCreating(t *testing.T) {
	prov := &fakeCloud{}
	b, ff := testBurst(t, prov, &fakeHost{notYet: 3})
	prov.onCreate = func(s cloud.Spec) {
		w := inventory(t, b)[s.Name]
		if w == nil || w.Cloud == nil || w.Cloud.Nonce != s.Labels["m2test-nonce"] || w.Cloud.ID != 0 {
			t.Errorf("%s not recorded with its nonce before create: %+v", s.Name, w)
		}
	}
	b.start(context.Background(), 2)
	b.creating.Wait()
	waitFor(t, "two ready workers", func() bool { return b.readyCount() == 2 })
	inv := inventory(t, b)
	if len(inv) != 2 {
		t.Fatalf("inventory %v", inv)
	}
	for name, w := range inv {
		if w.Cloud.ID == 0 || w.Host == "" || !ff.admitted[name] {
			t.Errorf("%s: %+v admitted %v", name, w, ff.admitted[name])
		}
		if !strings.HasPrefix(w.HostKey, "ssh-ed25519 ") || strings.Count(w.HostKey, " ") != 1 {
			t.Errorf("%s: host key %q", name, w.HostKey)
		}
	}
	spec := prov.specs[0]
	if spec.Labels[cloud.Label] != "worker" {
		t.Fatalf("labels %v", spec.Labels)
	}
	// The real host key went into the user data, so it parses as YAML.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(spec.UserData), &doc); err != nil {
		t.Fatalf("user data: %v", err)
	}
	if pub := doc["ssh_keys"].(map[string]any)["ed25519_public"].(string); !strings.HasPrefix(pub, inv[spec.Name].HostKey) {
		t.Fatalf("pinned key %q, user data has %q", inv[spec.Name].HostKey, pub)
	}
	b.end(context.Background())
	if prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("left %d servers, inventory %v", prov.count(), inventory(t, b))
	}
	if len(ff.abandoned) != 2 {
		t.Errorf("abandoned %v, want both destroyed workers' runs let go", ff.abandoned)
	}
}

func TestBurstDestroysAServerWhoseCreateAnswerWasLost(t *testing.T) {
	for name, prov := range map[string]*fakeCloud{
		"shown at once": {createErr: errors.New("timeout"), made: true},
		"shown late":    {createErr: errors.New("timeout"), made: true, hideFor: 2},
	} {
		b, ff := testBurst(t, prov, &fakeHost{})
		b.start(context.Background(), 1)
		b.creating.Wait()
		if len(prov.specs) != 1 || prov.next != 1 {
			t.Fatalf("%s: control: %d creates, %d made", name, len(prov.specs), prov.next)
		}
		if prov.count() != 0 || len(inventory(t, b)) != 0 || len(ff.admitted) != 0 {
			t.Errorf("%s: servers %d, inventory %v, admitted %v", name, prov.count(), inventory(t, b), ff.admitted)
		}
	}
}

func TestDeleteRetriesWhileLocked(t *testing.T) {
	prov := &fakeCloud{lockedFor: 2}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "the worker", func() bool { return b.readyCount() == 1 })
	b.end(context.Background())
	if prov.count() != 0 || len(inventory(t, b)) != 0 || prov.lockedFor != 0 {
		t.Fatalf("servers %d, inventory %v, locked answers left %d", prov.count(), inventory(t, b), prov.lockedFor)
	}
}

func TestGCKeepsAnUnansweredCreateForAWhile(t *testing.T) {
	b, _ := testBurst(t, &fakeCloud{}, &fakeHost{})
	prov := b.prov.(*fakeCloud)
	tok, _ := pool.NewToken()
	tok2, _ := pool.NewToken()
	_, hk, _ := pool.HostKeyPair("t")
	hk = keyOnly(hk)
	if err := b.pool.Update(func(inv *pool.Inventory) error {
		inv.Workers["hz-recent"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: hk, Token: tok, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Nonce: "aaaaaaaa", Created: time.Now().UTC()}}
		inv.Workers["hz-old"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: hk, Token: tok2, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Nonce: "bbbbbbbb", Created: time.Now().UTC().Add(-2 * unseenFor)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 1 {
		t.Fatalf("%d left, %v", left, err)
	}
	inv := inventory(t, b)
	if inv["hz-recent"] == nil || inv["hz-old"] != nil {
		t.Fatalf("inventory %v", inv)
	}
	prov.mu.Lock()
	prov.servers[90] = cloud.Server{ID: 90, Name: "hz-recent", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID, cloud.Label + "-nonce": "aaaaaaaa"}}
	prov.mu.Unlock()
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("%d left (%v), %d servers, inventory %v", left, err, prov.count(), inventory(t, b))
	}
}

func TestBurstDestroysAServerWhoseActionPollWasRefused(t *testing.T) {
	prov := &fakeCloud{actionErr: fmt.Errorf("server made, but %w", &cloud.APIError{Status: 429, Code: "rate_limit_exceeded"})}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if prov.next != 1 || prov.count() != 0 || len(inventory(t, b)) != 0 || len(ff.admitted) != 0 {
		t.Fatalf("made %d, servers %d, inventory %v, admitted %v", prov.next, prov.count(), inventory(t, b), ff.admitted)
	}
}

func TestGCLeavesAnotherProjectsServer(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	// The token now reaches a project where the inventory's IDs are others'.
	prov.mu.Lock()
	for id, s := range prov.servers {
		s.Labels = map[string]string{"app": "shop"}
		prov.servers[id] = s
	}
	prov.mu.Unlock()
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 1 || prov.count() != 1 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
	// Control: with this pool's labels back, the same gc destroys it.
	prov.mu.Lock()
	for id, s := range prov.servers {
		s.Labels = map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID}
		prov.servers[id] = s
	}
	prov.mu.Unlock()
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 {
		t.Fatalf("control: %d left (%v), %d servers", left, err, prov.count())
	}
}

func TestGCLeavesAServerRelabelledSinceItsList(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	prov.mu.Lock()
	prov.getLabels = map[string]string{"app": "shop"}
	prov.mu.Unlock()
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 1 || prov.count() != 1 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
}

func TestBurstMatchesALostCreateInThisPoolOnly(t *testing.T) {
	prov := &fakeCloud{createErr: errors.New("timeout"), made: true}
	prov.onCreate = func(s cloud.Spec) {
		prov.servers[99] = cloud.Server{ID: 99, Name: "theirs", Labels: map[string]string{
			cloud.Label: "worker", cloud.Label + "-pool": "ffffffffffff", cloud.Label + "-nonce": s.Labels[cloud.Label+"-nonce"]}}
	}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if _, ok := prov.servers[99]; !ok || prov.count() != 1 || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %v, inventory %v", prov.servers, inventory(t, b))
	}
}

func TestBurstKeepsAnAmbiguousCreateNoOneCanSee(t *testing.T) {
	prov := &fakeCloud{createErr: errors.New("timeout")}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if len(inventory(t, b)) != 1 {
		t.Fatalf("a timed-out create was forgotten: %v", inventory(t, b))
	}
}

func TestBurstDestroysAServerWhoseActionsFailed(t *testing.T) {
	prov := &fakeCloud{actionErr: errors.New("firewall not applied")}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if prov.count() != 0 || len(inventory(t, b)) != 0 || len(ff.admitted) != 0 {
		t.Fatalf("servers %d, inventory %v, admitted %v", prov.count(), inventory(t, b), ff.admitted)
	}
	if len(prov.specs) != 1 {
		t.Fatalf("control: %d creates", len(prov.specs))
	}
}

func TestBurstCreatesNothingOnceCancelled(t *testing.T) {
	prov := &fakeCloud{}
	b, _ := testBurst(t, prov, &fakeHost{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.start(ctx, 2)
	b.creating.Wait()
	if len(prov.specs) != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("creates %d, inventory %v", len(prov.specs), inventory(t, b))
	}
	b.start(context.Background(), 1)
	b.creating.Wait()
	if len(prov.specs) != 1 {
		t.Fatalf("control: an uncancelled start made %d", len(prov.specs))
	}
	b.end(context.Background())
}

func TestGCGoesByLabelWithoutAnInventory(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	if err := os.WriteFile(filepath.Join(b.pool.Dir, "workers.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.pool.Load(); err == nil {
		t.Fatal("control: the inventory still loads")
	}
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || prov.count() != 0 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
}

func TestBurstForgetsAWorkerNeverMade(t *testing.T) {
	prov := &fakeCloud{createErr: &cloud.APIError{Status: 403, Code: "resource_limit_exceeded"}}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if len(inventory(t, b)) != 0 {
		t.Fatalf("inventory %v", inventory(t, b))
	}
}

func TestBurstKeepsAnUnknownCreateForGC(t *testing.T) {
	prov := &fakeCloud{createErr: errors.New("timeout"), listErr: errors.New("timeout")}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.end(context.Background())
	inv := inventory(t, b)
	if len(inv) != 1 {
		t.Fatalf("inventory %v", inv)
	}
	for _, w := range inv {
		if w.Cloud.ID != 0 {
			t.Fatalf("%+v", w.Cloud)
		}
	}
}

func TestBurstDestroysAWorkerThatFailsSetup(t *testing.T) {
	prov := &fakeCloud{}
	b, ff := testBurst(t, prov, &fakeHost{setupErr: errors.New("upload failed")})
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "the server to go", func() bool { return prov.count() == 0 && len(inventory(t, b)) == 0 })
	if len(ff.admitted) != 0 {
		t.Fatal("a worker that failed setup was let in")
	}
	b.end(context.Background())
}

func TestBurstKeepsAServerItCouldNotDelete(t *testing.T) {
	prov := &fakeCloud{}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "the worker", func() bool { return b.readyCount() == 1 })
	prov.deleteErr = errors.New("locked")
	b.end(context.Background())
	if prov.count() != 1 || len(inventory(t, b)) != 1 {
		t.Fatalf("servers %d, inventory %v", prov.count(), inventory(t, b))
	}
	if len(ff.abandoned) != 1 {
		t.Errorf("abandoned %v; gc destroys it, so its runs are let go too", ff.abandoned)
	}
}

func readyBurst(t *testing.T, n int) (*burst, *fakeFleet, *fakeCloud) {
	prov := &fakeCloud{}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), n)
	b.creating.Wait()
	waitFor(t, "ready workers", func() bool { return b.readyCount() == n })
	t.Cleanup(func() { b.end(context.Background()) })
	return b, ff, prov
}

func TestTrimLeavesWorkersWhileRunsWait(t *testing.T) {
	b, ff, prov := readyBurst(t, 3)
	b.trim(context.Background(), 4, false)
	if prov.count() != 3 || len(ff.drained) != 0 {
		t.Fatalf("servers %d, drained %v", prov.count(), ff.drained)
	}
}

func TestTrimKeepsOneUntilTheSweepIsDone(t *testing.T) {
	b, _, prov := readyBurst(t, 3)
	b.trim(context.Background(), 0, false)
	if prov.count() != 1 || b.readyCount() != 1 {
		t.Fatalf("servers %d, workers %d", prov.count(), b.readyCount())
	}
	b.trim(context.Background(), 0, true)
	if prov.count() != 0 {
		t.Fatalf("servers %d after the sweep", prov.count())
	}
}

func TestTrimLeavesBusyWorkersAndCountsThemAsKept(t *testing.T) {
	b, ff, prov := readyBurst(t, 3)
	var busy string
	for name := range b.workers {
		busy = name
		break
	}
	ff.busy[busy] = true
	b.trim(context.Background(), 0, false)
	if prov.count() != 1 || b.workers[busy] == nil || ff.drained[busy] {
		t.Fatalf("servers %d; busy %s kept %v, drained %v", prov.count(), busy, b.workers[busy] != nil, ff.drained[busy])
	}
}

func TestTrimRetiresOldWorkersOnceIdle(t *testing.T) {
	b, ff, prov := readyBurst(t, 2)
	var old, busyOld string
	for name, bw := range b.workers {
		bw.w.Cloud.Created = time.Now().Add(-2 * time.Hour)
		if old == "" {
			old = name
		} else {
			busyOld = name
		}
	}
	ff.busy[busyOld] = true
	b.trim(context.Background(), 10, false)
	if prov.count() != 1 || b.workers[old] != nil {
		t.Fatalf("servers %d, old kept %v", prov.count(), b.workers[old] != nil)
	}
	if !ff.drained[busyOld] {
		t.Fatal("a busy worker past max age still gets work")
	}
}

func TestBurstPlan(t *testing.T) {
	var c cloudConfig
	c.Hetzner.Slots, c.MaxWorkers, c.MinutesPerRun, c.SetupMinutes = 5, 10, 4, 10
	for _, tc := range []struct {
		asked, existing, runs, n int
		cost                     float64
	}{
		{asked: 10, runs: 1000, n: 10, cost: 10 * 2},            // 80 + 10 minutes: 2 hours each
		{asked: 20, runs: 1000, n: 10, cost: 10 * 2},            // max_workers
		{asked: 10, existing: 8, runs: 1000, n: 2, cost: 2 * 7}, // 400 + 10 minutes
		{asked: 10, runs: 7, n: 2, cost: 2 * 1},                 // no more than the runs can use
		{asked: 3, runs: 0, n: 0},
	} {
		n, cost := burstPlan(c, tc.asked, tc.existing, tc.runs, 1)
		if n != tc.n || cost != tc.cost {
			t.Errorf("%+v: got %d workers for %v", tc, n, cost)
		}
	}
}

func TestParseCloud(t *testing.T) {
	if n, err := parseCloud("hetzner:4"); n != 4 || err != nil {
		t.Fatalf("got %d %v", n, err)
	}
	for _, bad := range []string{"hetzner", "aws:3", "hetzner:0", "hetzner:x"} {
		if _, err := parseCloud(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadCloudConfig(t *testing.T) {
	p := &pool.Pool{Dir: t.TempDir()}
	if _, err := loadCloudConfig(p); err == nil {
		t.Fatal("no pool.yml accepted")
	}
	write := func(s string) { os.WriteFile(filepath.Join(p.Dir, "pool.yml"), []byte(s), 0o600) }
	write("ssh_from: [198.51.100.4/32]\nmax_age: 3h\n")
	c, err := loadCloudConfig(p)
	if err != nil || c.MaxAge != 3*time.Hour || c.Hetzner.Type != "ccx33" || c.Hetzner.Slots != 5 {
		t.Fatalf("got %+v %v", c, err)
	}
	for _, bad := range []string{"max_age: 3h\n", "ssh_from: [x]\nmax_workers: 0\n", "ssh_from: [x]\nmax_wrokers: 3\n", "ssh_from: [x]\nhetzner: {slots: 100}\n"} {
		write(bad)
		if _, err := loadCloudConfig(p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGC(t *testing.T) {
	prov := &fakeCloud{}
	b, _ := testBurst(t, prov, &fakeHost{})
	// One left by a burst, one whose create answer was lost, one the
	// inventory never heard of, and one the provider never made.
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "the worker", func() bool { return b.readyCount() == 1 })
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	ours := func(more ...string) map[string]string {
		l := map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID}
		for i := 0; i < len(more); i += 2 {
			l[more[i]] = more[i+1]
		}
		return l
	}
	prov.servers[50] = cloud.Server{ID: 50, Name: "hz-lost", IP: "203.0.113.50", Labels: ours(cloud.Label+"-nonce", "abcdef12")}
	prov.servers[60] = cloud.Server{ID: 60, Name: "hz-orphan", Labels: ours()}
	prov.servers[70] = cloud.Server{ID: 70, Name: "someone-elses", Labels: map[string]string{}}
	prov.servers[80] = cloud.Server{ID: 80, Name: "another-pools", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": "ffffffffffff"}}
	key := inventory(t, b)
	var anyKey string
	for _, w := range key {
		anyKey = w.HostKey
	}
	b.pool.Update(func(inv *pool.Inventory) error {
		tok, _ := pool.NewToken()
		inv.Workers["hz-lost"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: anyKey, Token: tok, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Nonce: "abcdef12"}}
		tok2, _ := pool.NewToken()
		inv.Workers["hz-never"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: anyKey, Token: tok2, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Nonce: "99999999"}}
		return nil
	})

	if left, err := gc(context.Background(), b.pool, prov, b.poolID, false, b.log); err != nil || left != 4 || prov.count() != 5 || len(inventory(t, b)) != 3 {
		t.Fatalf("a dry run: %d listed (%v), %d servers, inventory %d", left, err, prov.count(), len(inventory(t, b)))
	}
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 0 {
		t.Fatalf("%d left, %v", left, err)
	}
	if prov.count() != 2 || prov.servers[70].Name != "someone-elses" || prov.servers[80].Name != "another-pools" || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %v, inventory %v", prov.servers, inventory(t, b))
	}
}

func TestTrimDeletesAfterTheSweepIsCancelled(t *testing.T) {
	b, _, prov := readyBurst(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.trim(ctx, 0, true)
	if prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %d, inventory %v", prov.count(), inventory(t, b))
	}
}

func TestEndRetriesWhatADeleteLeft(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	prov.mu.Lock()
	prov.deleteErr = errors.New("locked")
	prov.mu.Unlock()
	b.trim(context.Background(), 0, true)
	if prov.count() != 1 {
		t.Fatalf("control: the first delete went through")
	}
	prov.mu.Lock()
	prov.deleteErr = nil
	prov.mu.Unlock()
	b.end(context.Background())
	if prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %d, inventory %v", prov.count(), inventory(t, b))
	}
}

func TestGCCountsAnEntryItCouldNotForget(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	dir := b.pool.Dir
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if left, err := gc(context.Background(), b.pool, prov, b.poolID, true, b.log); err != nil || left != 1 || prov.count() != 0 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
}
