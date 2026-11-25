// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/cloud"
	"github.com/samjuk/magento-compatability/internal/pool"
)

type fakeCloud struct {
	notFound error // what Get says of an unknown ID, if not ErrNotFound
	mu       sync.Mutex
	next     int64
	servers  map[string]cloud.Server
	// createErr fails Create; made says whether the server exists anyway.
	createErr  error
	made       bool
	listErr    error
	hideFor    int // List shows nothing this many times first
	lockedFor  int // Delete answers 423 this many times first
	actionErr  error
	deleteErr  error
	full       map[string]bool // its create action fails for want of room
	refuse     map[string]bool // type/location: refused outright
	inUseFor   int             // DeletePool says in use this many times first
	purgeErr   error
	purges     int
	slowDelete time.Duration
	getLabels  map[string]string // what Get says the labels are, if set
	onCreate   func(cloud.Spec)
	specs      []cloud.Spec
}

func (f *fakeCloud) Create(_ context.Context, s cloud.Spec) (cloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onCreate != nil {
		f.onCreate(s)
	}
	f.specs = append(f.specs, s)
	for len(s.Locations) > 0 && f.refuse[s.Type+"/"+s.Locations[0]] {
		if s.Locations = s.Locations[1:]; len(s.Locations) == 0 {
			return cloud.Server{}, &cloud.APIError{Status: 412, Code: "resource_unavailable"}
		}
	}
	if f.createErr != nil && !f.made {
		return cloud.Server{}, f.createErr
	}
	f.next++
	srv := cloud.Server{ID: strconv.FormatInt(f.next, 10), Name: s.Name, IP: "203.0.113." + string(rune('0'+f.next)), Labels: s.Labels}
	if len(s.Locations) > 0 {
		srv.Where = s.Locations[0]
	}
	f.servers[srv.ID] = srv
	if f.full[srv.Where] || f.full[s.Type+"/"+srv.Where] {
		return srv, fmt.Errorf("server %s made, but action 1: %w", srv.ID, &cloud.APIError{Code: "resource_unavailable", Message: "resource is currently unavailable"})
	}
	if f.actionErr != nil {
		return srv, f.actionErr
	}
	if f.createErr != nil {
		// Made, but the answer lost.
		return cloud.Server{}, f.createErr
	}
	return srv, nil
}

func (f *fakeCloud) Get(_ context.Context, id string) (cloud.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.servers[id]
	if !ok && f.notFound != nil {
		return s, f.notFound
	}
	if !ok {
		return s, cloud.ErrNotFound
	}
	if f.getLabels != nil {
		s.Labels = f.getLabels
	}
	return s, nil
}

func (f *fakeCloud) Delete(ctx context.Context, id string) error {
	time.Sleep(f.slowDelete)
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if id == "" {
		return errors.New("no server ID")
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

func (f *fakeCloud) DeletePool(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges++
	if f.inUseFor > 0 {
		f.inUseFor--
		return &cloud.APIError{Status: 422, Code: "resource_in_use"}
	}
	return f.purgeErr
}

func (f *fakeCloud) setDeleteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
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
	tails    int
}

func (h *fakeHost) Script(_ context.Context, _ string, script string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.HasPrefix(script, "tail ") {
		h.tails++
	}
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
	sick      map[string]time.Duration
	unseen    map[string]time.Duration // undeployed
	failed    error
}

func testBurst(t *testing.T, prov *fakeCloud, host *fakeHost) (*burst, *fakeFleet) {
	t.Helper()
	provisionPoll, deleteWait = time.Millisecond, 50*time.Millisecond
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ssh"), 0o700)
	if prov.servers == nil {
		prov.servers = map[string]cloud.Server{}
	}
	ff := &fakeFleet{admitted: map[string]bool{}, busy: map[string]bool{}, drained: map[string]bool{}, sick: map[string]time.Duration{}, unseen: map[string]time.Duration{}}
	b := &burst{
		pool: &pool.Pool{Dir: dir}, host: host, prov: prov, provider: "hetzner", owner: "7", slots: 5, log: log.New(io.Discard, "", 0),
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
		sick: func(name string) time.Duration {
			ff.mu.Lock()
			defer ff.mu.Unlock()
			return ff.sick[name]
		},
		undeployed: func(name string) time.Duration {
			ff.mu.Lock()
			defer ff.mu.Unlock()
			return ff.unseen[name]
		},
		fail: func(err error) {
			ff.mu.Lock()
			defer ff.mu.Unlock()
			ff.failed = err
		},
	}
	b.cfg.Hetzner.Slots, b.cfg.MaxAge = 5, time.Hour
	b.budget, b.capacity = math.MaxInt64, &capacity{burst: b}
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

func workerNames(b *burst) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Sorted(maps.Keys(b.workers))
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
		if w == nil || w.Cloud == nil || w.Cloud.Nonce != s.Labels["m2test-nonce"] || w.Cloud.ID != "" {
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
		if w.Cloud.ID == "" || w.Host == "" || !ff.admitted[name] {
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
	if doc["bootcmd"] != nil || strings.Contains(spec.UserData, "m2test-deadline") || !strings.HasPrefix(spec.Name, "hz-") {
		t.Fatalf("hetzner worker %s has a deadline: %v", spec.Name, doc["bootcmd"])
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
			Cloud: &pool.Cloud{Provider: "hetzner", Owner: "7", Nonce: "aaaaaaaa", Created: time.Now().UTC()}}
		inv.Workers["hz-old"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: hk, Token: tok2, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Owner: "7", Nonce: "bbbbbbbb", Created: time.Now().UTC().Add(-20 * time.Minute)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 1 {
		t.Fatalf("%d left, %v", left, err)
	}
	inv := inventory(t, b)
	if inv["hz-recent"] == nil || inv["hz-old"] != nil {
		t.Fatalf("inventory %v", inv)
	}
	prov.mu.Lock()
	prov.servers["90"] = cloud.Server{ID: "90", Name: "hz-recent", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID, cloud.Label + "-nonce": "aaaaaaaa"}}
	prov.mu.Unlock()
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("%d left (%v), %d servers, inventory %v", left, err, prov.count(), inventory(t, b))
	}
}

func TestGCDestroysALateServerFoundOnlyByItsNonce(t *testing.T) {
	b, _ := testBurst(t, &fakeCloud{}, &fakeHost{})
	prov := b.prov.(*fakeCloud)
	tok, _ := pool.NewToken()
	_, hk, _ := pool.HostKeyPair("t")
	if err := b.pool.Update(func(inv *pool.Inventory) error {
		inv.Workers["hz-late"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: keyOnly(hk), Token: tok, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Owner: "7", Nonce: "aaaaaaaa", Created: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prov.servers["90"] = cloud.Server{ID: "90", Name: "hz-late", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID, cloud.Label + "-nonce": "aaaaaaaa"}}
	// The pool listing misses it; the nonce listing after it doesn't.
	prov.hideFor = 1
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 || len(inventory(t, b)) != 0 {
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
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 1 || prov.count() != 1 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
	// Control: with this pool's labels back, the same gc destroys it.
	prov.mu.Lock()
	for id, s := range prov.servers {
		s.Labels = map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID}
		prov.servers[id] = s
	}
	prov.mu.Unlock()
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 {
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
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 1 || prov.count() != 1 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
}

func TestBurstMatchesALostCreateInThisPoolOnly(t *testing.T) {
	prov := &fakeCloud{createErr: errors.New("timeout"), made: true}
	prov.onCreate = func(s cloud.Spec) {
		prov.servers["99"] = cloud.Server{ID: "99", Name: "theirs", Labels: map[string]string{
			cloud.Label: "worker", cloud.Label + "-pool": "ffffffffffff", cloud.Label + "-nonce": s.Labels[cloud.Label+"-nonce"]}}
	}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if _, ok := prov.servers["99"]; !ok || prov.count() != 1 || len(inventory(t, b)) != 0 {
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
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || prov.count() != 0 {
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
		if w.Cloud.ID != "" {
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

func TestBurstDestroysAWorkerWhoseTunnelGaveUp(t *testing.T) {
	prov := &fakeCloud{}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.connect = func(context.Context, string, *pool.Worker) {}
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "the server to go", func() bool { return prov.count() == 0 && len(inventory(t, b)) == 0 })
	if len(ff.admitted) != 1 {
		t.Fatal("control: the worker was never let in")
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
	b.trim(context.Background(), 4, notDone)
	if prov.count() != 3 || len(ff.drained) != 0 {
		t.Fatalf("servers %d, drained %v", prov.count(), ff.drained)
	}
}

func TestTrimKeepsOneUntilTheSweepIsDone(t *testing.T) {
	b, _, prov := readyBurst(t, 3)
	b.trim(context.Background(), 0, notDone)
	if prov.count() != 1 || b.readyCount() != 1 {
		t.Fatalf("servers %d, workers %d", prov.count(), b.readyCount())
	}
	b.trim(context.Background(), 0, isDone)
	if prov.count() != 0 {
		t.Fatalf("servers %d after the sweep", prov.count())
	}
}

func TestTrimDeletesSideBySide(t *testing.T) {
	b, _, prov := readyBurst(t, 4)
	prov.slowDelete = 500 * time.Millisecond
	start := time.Now()
	b.trim(context.Background(), 0, isDone)
	// One after another takes 2s; the margin is for a loaded machine.
	if took := time.Since(start); prov.count() != 0 || took > 1500*time.Millisecond {
		t.Errorf("servers %d left, took %s for four 500ms deletes", prov.count(), took)
	}
}

func TestTrimLeavesBusyWorkersAndCountsThemAsKept(t *testing.T) {
	b, ff, prov := readyBurst(t, 3)
	busy := workerNames(b)[0]
	ff.busy[busy] = true
	b.trim(context.Background(), 0, notDone)
	if prov.count() != 1 || b.workers[busy] == nil || ff.drained[busy] {
		t.Fatalf("servers %d; busy %s kept %v, drained %v", prov.count(), busy, b.workers[busy] != nil, ff.drained[busy])
	}
}

func TestTrimRetiresOldWorkersOnceIdle(t *testing.T) {
	b, ff, prov := readyBurst(t, 2)
	for _, bw := range b.workers {
		// Past max age, not yet past its grace.
		bw.w.Cloud.Created = time.Now().Add(-b.cfg.MaxAge - deadlineGrace/2)
	}
	old, busyOld := workerNames(b)[0], workerNames(b)[1]
	ff.busy[busyOld] = true
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 1 || b.workers[old] != nil {
		t.Fatalf("servers %d, old kept %v", prov.count(), b.workers[old] != nil)
	}
	if !ff.drained[busyOld] {
		t.Fatal("a busy worker past max age still gets work")
	}
}

func TestTrimStopsARunThatOutlivesMaxAge(t *testing.T) {
	b, ff, prov := readyBurst(t, 1)
	var name string
	for n, bw := range b.workers {
		name = n
		bw.w.Cloud.Created = time.Now().Add(-b.cfg.MaxAge - deadlineGrace - time.Minute)
	}
	ff.busy[name] = true
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 0 || !slices.Contains(ff.abandoned, name) {
		t.Fatalf("servers %d, abandoned %v", prov.count(), ff.abandoned)
	}
}

func TestTrimRetriesADeleteThatFailed(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	for _, bw := range b.workers {
		bw.w.Cloud.Created = time.Now().Add(-b.cfg.MaxAge - deadlineGrace/2)
	}
	prov.deleteErr = &cloud.APIError{Status: 403, Code: "forbidden"}
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 1 || len(b.workers) != 0 {
		t.Fatalf("servers %d, workers %d: the delete didn't fail", prov.count(), len(b.workers))
	}
	prov.setDeleteErr(nil)
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 0 {
		t.Fatal("a retired server outlived the provider's recovery")
	}
}

func TestTrimRetriesADeleteFailedOutsideTrim(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	name := workerNames(b)[0]
	// As when its tunnel gives up: off the books, then deleted.
	bw := b.take(name)
	bw.stop()
	<-bw.stopped
	prov.deleteErr = &cloud.APIError{Status: 403, Code: "forbidden"}
	if b.delete(context.Background(), name, bw.w) || prov.count() != 1 {
		t.Fatalf("the delete didn't fail: servers %d", prov.count())
	}
	prov.setDeleteErr(nil)
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 0 || len(b.undeleted) != 0 {
		t.Fatalf("servers %d, undeleted %v", prov.count(), b.undeleted)
	}
}

func TestBurstPlan(t *testing.T) {
	var c cloudConfig
	c.Hetzner.Slots, c.MaxWorkers, c.MinutesPerRun, c.SetupMinutes = 5, 10, 4, 10
	for _, tc := range []struct {
		asked, runs, n int
		cost           int64
	}{
		{asked: 10, runs: 1000, n: 10, cost: 10 * 2}, // 80 + 10 minutes: 2 hours each
		{asked: 20, runs: 1000, n: 10, cost: 10 * 2}, // max_workers
		{asked: 2, runs: 1000, n: 2, cost: 2 * 7},    // 400 + 10 minutes
		{asked: 10, runs: 7, n: 2, cost: 2 * 1},      // no more than the runs can use
		{asked: 3, runs: 0, n: 0},
	} {
		n, cost := burstPlan(c, c.Hetzner.Slots, tc.asked, tc.runs)
		if n != tc.n || cost != tc.cost {
			t.Errorf("%+v: got %d workers for %v", tc, n, cost)
		}
	}
}

func TestParseCloud(t *testing.T) {
	for in, want := range map[string]string{"hetzner:4": "hetzner", "aws:3": "aws"} {
		if p, n, err := parseCloud(in); p != want || n != int(in[len(in)-1]-'0') || err != nil {
			t.Fatalf("%s: got %s %d %v", in, p, n, err)
		}
	}
	for _, bad := range []string{"hetzner", "gcp:3", "hetzner:0", "aws:x"} {
		if _, _, err := parseCloud(bad); err == nil {
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
	if err != nil || c.MaxAge != 3*time.Hour || !slices.Equal(c.Hetzner.Type, list{"ccx33"}) || c.Hetzner.Slots != 5 {
		t.Fatalf("got %+v %v", c, err)
	}
	if c.AWS.Type != "m7a.2xlarge" || c.AWS.DiskGB != 160 || c.AWS.Slots != 5 {
		t.Fatalf("aws defaults %+v", c.AWS)
	}
	ok := "ssh_from: [198.51.100.4/32, 2001:db8::/32]\n"
	for _, bad := range []string{"max_age: 3h\n", "ssh_from: [x]\n", "ssh_from: [198.51.100.4/24]\n", "ssh_from: [0.0.0.0/0]\n", "ssh_from: [128.0.0.0/1]\n", "ssh_from: [\"2001:db8::/32\"]\n", ok + "aws: {region: eu west 2}\n", "ssh_from: [\"::/0\"]\n", ok + "max_workers: 0\n", ok + "max_wrokers: 3\n", ok + "aws: {hourly: 1e12}\n", ok + "setup_minutes: 2.8e20\n", ok + "minutes_per_run: 1441\n",
		ok + "max_burst_cost: .nan\n", ok + "aws: {hourly: .inf}\n", ok + "minutes_per_run: .nan\n", ok + "hetzner: {slots: 100}\n", ok + "aws: {slots: 0}\n", ok + "aws: {disk_gb: 20}\n", ok + "hetzner: {location: []}\n", ok + "hetzner: {location: [fsn1, \"a b\"]}\n", ok + "hetzner: {location: {x: 1}}\n", ok + "hetzner: {type: []}\n", ok + "hetzner: {type: [cx33, \"c x\"]}\n", ok + "hetzner: {type: [cx33, cpx32, cx33]}\n", ok + "hetzner: {location: [fsn1, fsn1]}\n"} {
		write(bad)
		if _, err := loadCloudConfig(p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	write(ok + "aws: {region: eu-west-1, hourly: 0.4, disk_gb: 30}\n")
	if c, err := loadCloudConfig(p); err != nil || c.AWS.Region != "eu-west-1" || c.AWS.DiskGB != 30 {
		t.Fatalf("control: %+v %v", c.AWS, err)
	}
	if !slices.Equal(c.Hetzner.Location, []string{"hel1"}) {
		t.Fatalf("default location %v", c.Hetzner.Location)
	}
	for in, want := range map[string][]string{"fsn1": {"fsn1"}, "[fsn1, nbg1]": {"fsn1", "nbg1"}} {
		write(ok + "hetzner: {location: " + in + "}\n")
		if c, err := loadCloudConfig(p); err != nil || !slices.Equal(c.Hetzner.Location, want) {
			t.Errorf("location %s: %v %v", in, c.Hetzner.Location, err)
		}
		write(ok + "hetzner: {type: " + strings.ReplaceAll(strings.ReplaceAll(in, "fsn1", "cx33"), "nbg1", "cpx32") + "}\n")
		if c, err := loadCloudConfig(p); err != nil || len(c.Hetzner.Type) != len(want) || c.Hetzner.Type[0] != "cx33" {
			t.Errorf("type %s: %v %v", in, c.Hetzner.Type, err)
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
	prov.servers["50"] = cloud.Server{ID: "50", Name: "hz-lost", IP: "203.0.113.50", Labels: ours(cloud.Label+"-nonce", "abcdef12")}
	prov.servers["60"] = cloud.Server{ID: "60", Name: "hz-orphan", Labels: ours()}
	prov.servers["70"] = cloud.Server{ID: "70", Name: "someone-elses", Labels: map[string]string{}}
	prov.servers["80"] = cloud.Server{ID: "80", Name: "another-pools", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": "ffffffffffff"}}
	key := inventory(t, b)
	var anyKey string
	for _, w := range key {
		anyKey = w.HostKey
	}
	b.pool.Update(func(inv *pool.Inventory) error {
		tok, _ := pool.NewToken()
		inv.Workers["hz-lost"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: anyKey, Token: tok, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Owner: "7", Nonce: "abcdef12"}}
		tok2, _ := pool.NewToken()
		inv.Workers["hz-never"] = &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: anyKey, Token: tok2, Slots: 5,
			Cloud: &pool.Cloud{Provider: "hetzner", Owner: "7", Nonce: "99999999"}}
		return nil
	})

	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, false, b.log); err != nil || left != 4 || prov.count() != 5 || len(inventory(t, b)) != 3 {
		t.Fatalf("a dry run: %d listed (%v), %d servers, inventory %d", left, err, prov.count(), len(inventory(t, b)))
	}
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 0 {
		t.Fatalf("%d left, %v", left, err)
	}
	if prov.count() != 2 || prov.servers["70"].Name != "someone-elses" || prov.servers["80"].Name != "another-pools" || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %v, inventory %v", prov.servers, inventory(t, b))
	}
}

func TestTrimDeletesAfterTheSweepIsCancelled(t *testing.T) {
	b, _, prov := readyBurst(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.trim(ctx, 0, isDone)
	if prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %d, inventory %v", prov.count(), inventory(t, b))
	}
}

func TestEndRetriesWhatADeleteLeft(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	prov.mu.Lock()
	prov.deleteErr = errors.New("locked")
	prov.mu.Unlock()
	b.trim(context.Background(), 0, isDone)
	if prov.count() != 1 {
		t.Fatalf("control: the first delete went through")
	}
	prov.setDeleteErr(nil)
	b.end(context.Background())
	if prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("servers %d, inventory %v", prov.count(), inventory(t, b))
	}
}

func TestGCCountsAnEntryItCouldNotForget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through 0555")
	}
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
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 1 || prov.count() != 0 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
}

func TestAWSBurstWorkersPowerOffPastTheirDeadline(t *testing.T) {
	prov := &fakeCloud{}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.provider, b.region, b.owner, b.deadline = "aws", "eu-west-1", "111111111111", 3*time.Hour
	before := time.Now()
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "a ready worker", func() bool { return b.readyCount() == 1 })
	spec := prov.specs[0]
	w := inventory(t, b)[spec.Name]
	if !strings.HasPrefix(spec.Name, "aws-") || w == nil || w.Cloud.Provider != "aws" || w.Cloud.Region != "eu-west-1" || w.Cloud.Owner != "111111111111" {
		t.Fatalf("%s: %+v", spec.Name, w)
	}
	var doc struct{ Bootcmd [][]string }
	if err := yaml.Unmarshal([]byte(spec.UserData), &doc); err != nil || len(doc.Bootcmd) != 1 {
		t.Fatalf("bootcmd %v %v", doc.Bootcmd, err)
	}
	script := doc.Bootcmd[0][2]
	// create takes the deadline just before place records Created, maybe a second earlier.
	at := w.Cloud.Created.Add(3 * time.Hour)
	if early := at.Add(-time.Second); strings.Contains(script, "-lt "+strconv.FormatInt(early.Unix(), 10)+" ]") {
		at = early
	}
	if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("bootcmd doesn't parse: %v %s", err, out)
	}
	if !strings.Contains(script, "-lt "+strconv.FormatInt(at.Unix(), 10)+" ]") || w.Cloud.Created.Before(before.Add(-time.Second)) {
		t.Fatalf("deadline not created + 3h (%s): %s", at, script)
	}
	if !strings.Contains(script, `OnCalendar=`+at.UTC().Format("2006-01-02 15:04:05 UTC")+`\n`) || !strings.Contains(script, "systemctl start --no-block m2test-deadline.timer") || strings.Count(script, `logger -s -t m2test`) != 2 {
		t.Fatalf("timer not armed at boot: %s", script)
	}
	b.end(context.Background())
}

var euWest = target{"aws", "eu-west-1", "111111111111"}

func TestGCLeavesAnotherProvidersWorkers(t *testing.T) {
	b, _, prov := readyBurst(t, 1)
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	if err := b.pool.Update(func(inv *pool.Inventory) error {
		for _, w := range inv.Workers {
			aw := *w
			aw.Token, aw.Host, aw.User = strings.Repeat("a", 32), "198.51.100.9", "other"
			aw.Cloud = &pool.Cloud{Provider: "aws", Region: "eu-west-1", Owner: "111111111111", ID: "i-0123456789abcdef0", Nonce: "abcdef12", Created: time.Now().Add(-30 * time.Minute)} // past Hetzner's wait
			inv.Workers["aws-abcdef12"] = &aw
			break
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Hetzner has never heard of it, which must not read as "gone".
	if left, err := gc(context.Background(), b.pool, prov, target{provider: "hetzner", owner: "7"}, b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 {
		t.Fatalf("%d left (%v), %d servers", left, err, prov.count())
	}
	if inv := inventory(t, b); len(inv) != 1 || inv["aws-abcdef12"] == nil {
		t.Fatalf("inventory %v", inv)
	}
	// EC2 not showing a new instance yet is not it being gone.
	ec2 := &fakeCloud{servers: map[string]cloud.Server{}, notFound: fmt.Errorf("%w: %w", cloud.ErrNotFound, &cloud.APIError{Provider: "aws", Status: 400, Code: "InvalidInstanceID.NotFound"})}
	if left, err := gc(context.Background(), b.pool, ec2, euWest, b.poolID, true, b.log); err != nil || left != 1 || len(inventory(t, b)) != 1 {
		t.Fatalf("new instance: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
	// Nor is it in another region.
	if left, err := gc(context.Background(), b.pool, ec2, target{"aws", "us-east-1", "111111111111"}, b.poolID, true, b.log); err != nil || left != 0 || len(inventory(t, b)) != 1 {
		t.Fatalf("other region: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
	// An hour on, EC2 is not just slow.
	b.pool.Update(func(inv *pool.Inventory) error {
		inv.Workers["aws-abcdef12"].Cloud.Created = time.Now().Add(-2 * time.Hour)
		return nil
	})
	saved := *inventory(t, b)["aws-abcdef12"]
	if left, err := gc(context.Background(), b.pool, ec2, euWest, b.poolID, true, b.log); err != nil || left != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("old instance: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
	saved.Cloud.Created = time.Now()
	b.pool.Update(func(inv *pool.Inventory) error { inv.Workers["aws-abcdef12"] = &saved; return nil })
	// Control: seen terminated, it is gone however new.
	ec2.notFound = nil
	if left, err := gc(context.Background(), b.pool, ec2, euWest, b.poolID, true, b.log); err != nil || left != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("terminated instance: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
}

func TestGCTargetsSurviveABrokenInventory(t *testing.T) {
	p := &pool.Pool{Dir: t.TempDir()}
	os.MkdirAll(filepath.Join(p.Dir, "ssh"), 0o700)
	logger := log.New(io.Discard, "", 0)
	if _, err := gcTargets(p, false, "", logger); err == nil {
		t.Fatal("nothing to check, no error")
	}
	for _, broken := range []string{`{"workers": {`, `{"workers": {"x": null}}`, `{"workers": {"x": {"cloud": {"provider": "aws", "region": "eu-west-1; x"}}}}`,
		`{"workers": {"x": {"cloud": {"provider": "hetzner", "region": "fsn1"}}}}`} {
		os.WriteFile(filepath.Join(p.Dir, "workers.json"), []byte(broken), 0o600)
		got, err := gcTargets(p, true, "eu-west-1", logger)
		if err != nil || !slices.Equal(got, []target{{provider: "aws", region: "eu-west-1"}, {provider: "hetzner"}}) {
			t.Fatalf("broken inventory %s: %v %v", broken, got, err)
		}
	}
	// Control: a readable one adds the regions it has workers in.
	os.WriteFile(filepath.Join(p.Dir, "workers.json"), []byte(`{"workers": {}}`), 0o600)
	if err := p.Update(func(inv *pool.Inventory) error {
		inv.Workers["aws-abcdef12"] = &pool.Worker{Host: "198.51.100.9", Port: 22, User: "m2test", Dir: pool.DefaultDir, Slots: 1,
			Token: strings.Repeat("a", 32), HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHost",
			Cloud: &pool.Cloud{Provider: "aws", Region: "us-east-1", Owner: "111111111111", Nonce: "abcdef12", ID: "i-0123456789abcdef0"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := gcTargets(p, false, "eu-west-1", logger)
	if err != nil || !slices.Equal(got, []target{{provider: "aws", region: "eu-west-1"}, {provider: "aws", region: "us-east-1"}}) {
		t.Fatalf("readable inventory: %v %v", got, err)
	}
}

func TestGCLeavesAnotherAccountsInstances(t *testing.T) {
	prov := &fakeCloud{}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.provider, b.region, b.owner = "aws", "eu-west-1", "111111111111"
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "a ready worker", func() bool { return b.readyCount() == 1 })
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	// Another account sees none of it, and says so long after the create.
	b.pool.Update(func(inv *pool.Inventory) error {
		for _, w := range inv.Workers {
			w.Cloud.Created = time.Now().Add(-2 * unseenFor["aws"])
		}
		return nil
	})
	elsewhere := &fakeCloud{servers: map[string]cloud.Server{}, notFound: fmt.Errorf("%w: %w", cloud.ErrNotFound, &cloud.APIError{Provider: "aws", Status: 400, Code: "InvalidInstanceID.NotFound"})}
	other := target{"aws", "eu-west-1", "222222222222"}
	if left, err := gc(context.Background(), b.pool, elsewhere, other, b.poolID, true, b.log); err != nil || left != 1 || len(inventory(t, b)) != 1 {
		t.Fatalf("other account: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
	for name, w := range inventory(t, b) {
		if (&burst{pool: b.pool, prov: prov, log: b.log, owner: "222222222222"}).delete(context.Background(), name, w) || prov.count() != 1 {
			t.Fatal("destroyed from another account")
		}
	}
	// Made before accounts were recorded: whose it is is unknown.
	b.pool.Update(func(inv *pool.Inventory) error {
		for _, w := range inv.Workers {
			w.Cloud.Owner = ""
		}
		return nil
	})
	if left, err := gc(context.Background(), b.pool, elsewhere, b.target(), b.poolID, true, b.log); err != nil || left != 1 || len(inventory(t, b)) != 1 {
		t.Fatalf("no account: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
	for name, w := range inventory(t, b) {
		if b.delete(context.Background(), name, w) || prov.count() != 1 {
			t.Fatal("destroyed with no account recorded")
		}
	}
	// Listed by its labels here, it is in this account after all.
	if left, err := gc(context.Background(), b.pool, prov, b.target(), b.poolID, true, b.log); err != nil || left != 0 || prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("listed here: %d left (%v), %d servers", left, err, prov.count())
	}
}

// awsSetup answers what a burst asks EC2 before it makes anything.
type awsSetup struct {
	cloud.EC2
	arch types.ArchitectureType
}

func (s awsSetup) DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: []types.InstanceTypeInfo{{ProcessorInfo: &types.ProcessorInfo{SupportedArchitectures: []types.ArchitectureType{s.arch}}}}}, nil
}

func (awsSetup) DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String("ami-0123456789abcdef0"), RootDeviceName: aws.String("/dev/xvda"), CreationDate: aws.String("2026-11-01T00:00:00.000Z")}}}, nil
}

func (awsSetup) DescribeInstanceTypeOfferings(context.Context, *ec2.DescribeInstanceTypeOfferingsInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	return &ec2.DescribeInstanceTypeOfferingsOutput{InstanceTypeOfferings: []types.InstanceTypeOffering{{Location: aws.String("eu-west-1a")}}}, nil
}

func (awsSetup) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{AvailabilityZone: aws.String("eu-west-1a"), SubnetId: aws.String("subnet-a")}}}, nil
}

func TestUseAWSSetsTheDeadlineAndAccount(t *testing.T) {
	b, _ := testBurst(t, &fakeCloud{}, &fakeHost{})
	b.cfg.AWS.Region, b.cfg.AWS.Type, b.cfg.AWS.Slots, b.cfg.AWS.DiskGB, b.cfg.MaxAge = "eu-west-1", "m7i.2xlarge", 4, 60, 5*time.Hour
	a := &cloud.AWS{EC2: awsSetup{arch: "x86_64"}, Account: "111111111111"}
	goarch, err := b.useAWS(context.Background(), a)
	if err != nil || goarch != "amd64" {
		t.Fatalf("got %q, %v", goarch, err)
	}
	if b.deadline != 6*time.Hour || b.owner != "111111111111" || b.region != "eu-west-1" || b.slots != 4 || b.prov != a {
		t.Errorf("burst: deadline %s, account %q, region %q, slots %d", b.deadline, b.owner, b.region, b.slots)
	}
	if s := b.spec; s.Type != "m7i.2xlarge" || s.Image != "ami-0123456789abcdef0" || s.RootDevice != "/dev/xvda" || s.DiskGB != 60 || !slices.Equal(s.Subnets, []string{"subnet-a"}) {
		t.Errorf("spec %+v", s)
	}
}

func TestGCLeavesWhatAnotherProjectCantSee(t *testing.T) {
	prov := &fakeCloud{}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "a ready worker", func() bool { return b.readyCount() == 1 })
	b.mu.Lock()
	for _, bw := range b.workers {
		bw.stop()
	}
	b.workers = nil
	b.mu.Unlock()
	b.running.Wait()
	// Another project, with or without a pool key of its own.
	elsewhere := &fakeCloud{servers: map[string]cloud.Server{}}
	for _, key := range []string{"9", ""} {
		if left, err := gc(context.Background(), b.pool, elsewhere, target{"hetzner", "", key}, b.poolID, true, b.log); err != nil || left != 1 || len(inventory(t, b)) != 1 {
			t.Fatalf("project with key %q: %d left (%v), inventory %v", key, left, err, inventory(t, b))
		}
	}
	// Recorded with no key ID, under a token whose project has no pool key.
	b.pool.Update(func(inv *pool.Inventory) error {
		for _, w := range inv.Workers {
			w.Cloud.Owner = ""
		}
		return nil
	})
	if left, err := gc(context.Background(), b.pool, elsewhere, target{"hetzner", "", ""}, b.poolID, true, b.log); err != nil || left != 1 || len(inventory(t, b)) != 1 {
		t.Fatalf("no key either side: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
	b.pool.Update(func(inv *pool.Inventory) error {
		for _, w := range inv.Workers {
			w.Cloud.Owner = "7"
		}
		return nil
	})
	// Control: the burst's own project, where it's really gone.
	gone := &fakeCloud{servers: map[string]cloud.Server{}}
	if left, err := gc(context.Background(), b.pool, gone, b.target(), b.poolID, true, b.log); err != nil || left != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("own project: %d left (%v), inventory %v", left, err, inventory(t, b))
	}
}

func TestProvisionReadsTheLogOnlyWhenItTimesOut(t *testing.T) {
	host := &fakeHost{notYet: 1 << 30}
	b, _ := testBurst(t, &fakeCloud{}, host)
	old := provisionWait
	defer func() { provisionWait = old }()

	provisionWait = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := b.provision(ctx, "w", &pool.Worker{}); !errors.Is(err, context.Canceled) || host.tails != 0 {
		t.Errorf("cancelled: %v, %d tails; want Canceled and no tail", err, host.tails)
	}

	provisionWait = 20 * time.Millisecond
	if err := b.provision(context.Background(), "w", &pool.Worker{}); err == nil || host.tails != 1 {
		t.Errorf("control: timed out: %v, %d tails", err, host.tails)
	}
}

func (ff *fakeFleet) failure() error {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.failed
}

func TestTrimRetiresAWorkerUnhealthyTooLong(t *testing.T) {
	b, ff, prov := readyBurst(t, 3)
	names := workerNames(b)
	sick, recent := names[0], names[1]
	ff.sick[sick] = sickFor + time.Second
	ff.sick[recent] = sickFor - time.Second
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 2 || b.workers[sick] != nil || b.workers[recent] == nil {
		t.Fatalf("servers %d; sick kept %v, recently sick kept %v", prov.count(), b.workers[sick] != nil, b.workers[recent] != nil)
	}
}

func TestTrimEndsASweepNoWorkerIsLeftFor(t *testing.T) {
	b, ff, prov := readyBurst(t, 1)
	for _, bw := range b.workers {
		bw.w.Cloud.Created = time.Now().Add(-2 * time.Hour)
	}
	b.trim(context.Background(), 1, notDone)
	if prov.count() != 0 || ff.failure() == nil {
		t.Fatalf("servers %d, sweep ended with %v", prov.count(), ff.failure())
	}
}

func TestTrimLeavesTheSweepWhileSomethingCanRunIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		others, done bool
		creating     int
		named        int32
	}{
		{"local slots or named workers", true, false, 0, 0},
		{"a remote worker connected", false, false, 0, 1},
		{"a create still out", false, false, 1, 0},
		{"the sweep done", false, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, ff, _ := readyBurst(t, 1)
			b.capacity.local = tc.others
			b.capacity.named.Store(tc.named)
			b.mu.Lock()
			b.pending += tc.creating
			b.mu.Unlock()
			for _, bw := range b.workers {
				bw.w.Cloud.Created = time.Now().Add(-2 * time.Hour)
			}
			b.trim(context.Background(), 1, func() bool { return tc.done })
			if len(b.workers) != 0 || ff.failure() != nil {
				t.Fatalf("workers %d, sweep ended with %v", len(b.workers), ff.failure())
			}
		})
	}
}

func TestTrimLeavesTheSweepWhileAWorkerIsLeft(t *testing.T) {
	b, ff, _ := readyBurst(t, 2)
	b.trim(context.Background(), 0, notDone)
	if b.readyCount() != 1 || ff.failure() != nil {
		t.Fatalf("workers %d, sweep ended with %v", b.readyCount(), ff.failure())
	}
}

func notDone() bool { return false }
func isDone() bool  { return true }

func TestTrimLeavesABusySickWorkerFreeToRecover(t *testing.T) {
	b, ff, prov := readyBurst(t, 1)
	name := workerNames(b)[0]
	ff.busy[name], ff.sick[name] = true, sickFor+time.Second
	b.trim(context.Background(), 1, notDone)
	if prov.count() != 1 || ff.drained[name] {
		t.Fatalf("servers %d, drained %v", prov.count(), ff.drained[name])
	}
	ff.busy[name] = false
	b.trim(context.Background(), 1, notDone)
	if prov.count() != 0 {
		t.Fatal("control: an idle sick worker was kept")
	}
}

func TestTrimAsksAgainWhetherTheSweepIsDone(t *testing.T) {
	b, ff, prov := readyBurst(t, 1)
	for _, bw := range b.workers {
		bw.w.Cloud.Created = time.Now().Add(-2 * time.Hour)
	}
	asked := 0
	b.trim(context.Background(), 1, func() bool { asked++; return asked > 1 })
	if prov.count() != 0 || ff.failure() != nil {
		t.Fatalf("servers %d, sweep ended with %v", prov.count(), ff.failure())
	}
}

func TestTrimKeepsAWorkerThatRecoveredMeanwhile(t *testing.T) {
	b, _, prov := readyBurst(t, 2)
	asked := 0
	b.sick = func(string) time.Duration {
		asked++
		if asked == 1 {
			return sickFor + time.Second
		}
		return 0
	}
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 2 || asked < 2 {
		t.Fatalf("servers %d after %d asks", prov.count(), asked)
	}
}

func TestPurgeWaitsForTheFirewallToBeLetGo(t *testing.T) {
	old0 := provisionPoll
	provisionPoll = time.Millisecond
	defer func() { provisionPoll = old0 }()
	prov := &fakeCloud{inUseFor: 3}
	if err := purgePool(context.Background(), prov, "p"); err != nil || prov.purges != 4 {
		t.Fatalf("%v after %d tries", err, prov.purges)
	}
	prov = &fakeCloud{purgeErr: errors.New("forbidden")}
	if err := purgePool(context.Background(), prov, "p"); err == nil || prov.purges != 1 {
		t.Fatalf("another error retried: %v after %d tries", err, prov.purges)
	}
	old := purgeWait
	purgeWait = 20 * time.Millisecond
	defer func() { purgeWait = old }()
	prov = &fakeCloud{inUseFor: math.MaxInt}
	if err := purgePool(context.Background(), prov, "p"); !cloud.InUse(err) {
		t.Fatalf("never let go: %v", err)
	}
}

func TestBurstMovesOnWhenTheCreateActionFindsNoRoom(t *testing.T) {
	prov := &fakeCloud{full: map[string]bool{"fsn1": true, "nbg1": true}}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.spec.Locations = []string{"fsn1", "nbg1", "hel1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov.onCreate = func(cloud.Spec) {
		if len(prov.specs) >= 5 { // looping on a full location
			cancel()
		}
	}
	b.start(ctx, 1)
	b.creating.Wait()
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	t.Cleanup(func() { b.end(context.Background()) })
	var got []string
	for _, s := range prov.specs {
		got = append(got, strings.Join(s.Locations, ","))
	}
	if strings.Join(got, " ") != "fsn1,nbg1,hel1 nbg1,hel1 hel1" || prov.count() != 1 || len(inventory(t, b)) != 1 {
		t.Fatalf("creates %v, servers %d, inventory %v", got, prov.count(), inventory(t, b))
	}
	for _, s := range prov.servers {
		if s.Where != "hel1" {
			t.Fatalf("kept %s in %s", s.ID, s.Where)
		}
	}

	// Full everywhere: each made one is gone, and so is the worker.
	prov = &fakeCloud{full: map[string]bool{"fsn1": true, "nbg1": true}}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.spec.Locations = []string{"fsn1", "nbg1"}
	b.start(context.Background(), 1)
	b.creating.Wait()
	if len(prov.specs) != 2 || prov.count() != 0 || len(inventory(t, b)) != 0 || len(ff.admitted) != 0 {
		t.Fatalf("creates %d, servers %d, inventory %v", len(prov.specs), prov.count(), inventory(t, b))
	}

	// One that won't go: kept for gc, nothing more made.
	prov = &fakeCloud{full: map[string]bool{"fsn1": true}, deleteErr: errors.New("locked")}
	b, _ = testBurst(t, prov, &fakeHost{})
	b.spec.Locations = []string{"fsn1", "nbg1"}
	b.start(context.Background(), 1)
	b.creating.Wait()
	inv := inventory(t, b)
	if len(prov.specs) != 1 || prov.count() != 1 || len(inv) != 1 {
		t.Fatalf("creates %d, servers %d, inventory %v", len(prov.specs), prov.count(), inv)
	}
	for _, w := range inv {
		if w.Cloud.ID != "1" {
			t.Fatalf("inventory without the server: %+v", w.Cloud)
		}
	}
}

func TestBurstTriesTheNextTypeWhenNoLocationHasRoom(t *testing.T) {
	// cx33 made then full in fsn1 and refused in nbg1; cpx32 refused in fsn1.
	prov := &fakeCloud{refuse: map[string]bool{"cx33/nbg1": true, "cpx32/fsn1": true}, full: map[string]bool{"cx33/fsn1": true}}
	b, _ := testBurst(t, prov, &fakeHost{})
	b.types = []string{"cx33", "cpx32"}
	b.spec.Type, b.spec.Locations = "cx33", []string{"fsn1", "nbg1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov.onCreate = func(cloud.Spec) {
		if len(prov.specs) >= 5 {
			cancel()
		}
	}
	b.start(ctx, 1)
	b.creating.Wait()
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	t.Cleanup(func() { b.end(context.Background()) })
	var got []string
	for _, s := range prov.specs {
		got = append(got, s.Type+":"+strings.Join(s.Locations, ","))
	}
	if strings.Join(got, " ") != "cx33:fsn1,nbg1 cx33:nbg1 cpx32:fsn1,nbg1" || prov.count() != 1 {
		t.Fatalf("creates %v, servers %d", got, prov.count())
	}

	// Made then full in cx33's last location: gone before cpx32.
	prov = &fakeCloud{full: map[string]bool{"cx33/fsn1": true}}
	b, _ = testBurst(t, prov, &fakeHost{})
	b.types = []string{"cx33", "cpx32"}
	b.spec.Type, b.spec.Locations = "cx33", []string{"fsn1"}
	prov.onCreate = func(s cloud.Spec) {
		if s.Type == "cpx32" && len(prov.servers) != 0 {
			t.Errorf("cpx32 asked for while %d servers stay", len(prov.servers))
		}
	}
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	t.Cleanup(func() { b.end(context.Background()) })
	if len(prov.specs) != 2 || prov.count() != 1 {
		t.Fatalf("creates %d, servers %d", len(prov.specs), prov.count())
	}

	// Every type full everywhere: one create per type, then the worker is gone.
	prov = &fakeCloud{refuse: map[string]bool{"cx33/fsn1": true, "cpx32/fsn1": true}}
	b, _ = testBurst(t, prov, &fakeHost{})
	b.types = []string{"cx33", "cpx32"}
	b.spec.Type, b.spec.Locations = "cx33", []string{"fsn1"}
	b.start(context.Background(), 1)
	b.creating.Wait()
	if len(prov.specs) != 2 || prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("creates %d, servers %d, inventory %v", len(prov.specs), prov.count(), inventory(t, b))
	}
}

func TestHetznerPriceIsTheDearestX86Pair(t *testing.T) {
	types := map[string]string{
		"cx33":  `{"name": "cx33", "architecture": "x86", "prices": [{"location": "fsn1", "price_hourly": {"gross": "0.0153"}}, {"location": "hel1", "price_hourly": {"gross": "0.0153"}}]}`,
		"cpx32": `{"name": "cpx32", "architecture": "x86", "prices": [{"location": "fsn1", "price_hourly": {"gross": "0.0673"}}, {"location": "hel1", "price_hourly": {"gross": "0.0567"}}]}`,
		"cax21": `{"name": "cax21", "architecture": "arm", "prices": [{"location": "fsn1", "price_hourly": {"gross": "0.0100"}}, {"location": "hel1", "price_hourly": {"gross": "0.0100"}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pricing" {
			io.WriteString(w, `{"pricing": {"currency": "EUR", "primary_ips": [{"type": "ipv4", "prices": [{"location": "fsn1", "price_hourly": {"gross": "0.0010"}}, {"location": "hel1", "price_hourly": {"gross": "0.0010"}}]}]}}`)
			return
		}
		io.WriteString(w, `{"server_types": [`+types[r.URL.Query().Get("name")]+`]}`)
	}))
	defer srv.Close()
	hz := &cloud.Hetzner{Endpoint: srv.URL, Client: srv.Client()}
	p, arch, err := hetznerPrice(context.Background(), hz, []string{"cpx32", "cx33"}, []string{"fsn1", "hel1"})
	if err != nil || math.Abs(p-0.0683) > 1e-9 || arch != "amd64" {
		t.Fatalf("got %v %s %v", p, arch, err)
	}
	if _, _, err := hetznerPrice(context.Background(), hz, []string{"cx33", "cax21"}, []string{"fsn1"}); err == nil || !strings.Contains(err.Error(), "cax21 is arm64") {
		t.Fatalf("arm accepted: %v", err)
	}
}

func billedHours(b *burst) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, _ := b.billed()
	return h
}

// age makes every worker and running server that old.
func age(b *burst, d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, bw := range b.workers {
		bw.w.Cloud.Created = time.Now().Add(-d)
	}
	for _, s := range b.bills {
		if s.gone.IsZero() {
			s.created = time.Now().Add(-d)
		}
	}
}

func TestBurstRetriesOnlyWithinMaxBurstCost(t *testing.T) {
	// Each server found full is billed an hour.
	try := func(budget int64) (*burst, *fakeCloud) {
		prov := &fakeCloud{full: map[string]bool{"fsn1": true}}
		b, _ := testBurst(t, prov, &fakeHost{})
		b.spec.Locations, b.hourly, b.budget = []string{"fsn1", "nbg1"}, 1, budget
		b.start(context.Background(), 1)
		b.creating.Wait()
		return b, prov
	}
	b, prov := try(200)
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	t.Cleanup(func() { b.end(context.Background()) })
	if len(prov.specs) != 2 || prov.count() != 1 || billedHours(b) != 2 {
		t.Fatalf("creates %d, servers %d, billed %d", len(prov.specs), prov.count(), billedHours(b))
	}
	// Another try could pass the budget: none, nothing left, still billed.
	b, prov = try(199)
	if len(prov.specs) != 1 || prov.count() != 0 || len(inventory(t, b)) != 0 || billedHours(b) != 1 {
		t.Fatalf("creates %d, servers %d, inventory %v, billed %d", len(prov.specs), prov.count(), inventory(t, b), billedHours(b))
	}
	// Made but not as asked, for another reason: billed too.
	prov = &fakeCloud{actionErr: errors.New("firewall not applied")}
	b, _ = testBurst(t, prov, &fakeHost{})
	b.start(context.Background(), 1)
	b.creating.Wait()
	if prov.count() != 0 || billedHours(b) != 1 {
		t.Fatalf("servers %d, billed %d", prov.count(), billedHours(b))
	}
	// Refused outright costs nothing.
	prov = &fakeCloud{refuse: map[string]bool{"cx33/fsn1": true}}
	b, _ = testBurst(t, prov, &fakeHost{})
	b.types, b.spec.Type, b.spec.Locations, b.hourly, b.budget = []string{"cx33", "cpx32"}, "cx33", []string{"fsn1"}, 1, 100
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	t.Cleanup(func() { b.end(context.Background()) })
	if len(prov.specs) != 2 || billedHours(b) != 1 {
		t.Fatalf("creates %d, billed %d", len(prov.specs), billedHours(b))
	}
}

func TestUseAWSRefusesArm(t *testing.T) {
	b, _ := testBurst(t, &fakeCloud{}, &fakeHost{})
	b.cfg.AWS.Region, b.cfg.AWS.Type, b.cfg.AWS.Slots, b.cfg.AWS.DiskGB = "eu-west-1", "m7g.2xlarge", 4, 60
	if _, err := b.useAWS(context.Background(), &cloud.AWS{EC2: awsSetup{arch: "arm64"}}); err == nil || !strings.Contains(err.Error(), "x86") {
		t.Errorf("arm64 type: %v", err)
	}
}

func TestPlanRefusesALeftoverServerOrTheCost(t *testing.T) {
	plan := func(prov *fakeCloud, yes bool) (*burst, error) {
		b, _ := testBurst(t, prov, &fakeHost{})
		b.cfg.MaxWorkers, b.cfg.MaxBurstCost, b.cfg.MinutesPerRun = 4, 1, 30
		return b, b.plan(context.Background(), 4, 20, 0.5, "cx", yes)
	}
	if b, err := plan(&fakeCloud{}, true); err != nil || b.n != 4 || b.budget != math.MaxInt64 {
		t.Fatalf("control: -yes over the cap: %v, %d servers, budget %v", err, b.n, b.budget)
	}
	if _, err := plan(&fakeCloud{}, false); err == nil || !strings.Contains(err.Error(), "max_burst_cost") {
		t.Errorf("over the cap without -yes: %v", err)
	}
	left := &fakeCloud{servers: map[string]cloud.Server{"9": {ID: "9", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": "a1b2c3d4e5f6"}}}}
	if _, err := plan(left, true); err == nil || !strings.Contains(err.Error(), "earlier burst") {
		t.Errorf("a server left: %v", err)
	}
	b, _ := testBurst(t, &fakeCloud{}, &fakeHost{})
	b.cfg.MaxWorkers, b.cfg.MaxBurstCost, b.cfg.MinutesPerRun = 4, 10, 30
	if err := b.plan(context.Background(), 4, 2, 0.5, "cx", false); err != nil || b.budget != 1000 || b.hourly != 0.5 {
		t.Errorf("under the cap: %v, budget %v", err, b.budget)
	}
}

func TestTrimRetiresEveryWorkerOnceTheBudgetIsSpent(t *testing.T) {
	b, ff, prov := readyBurst(t, 2)
	b.hourly, b.budget = 1, 300
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 2 {
		t.Fatalf("control: within budget, %d servers", prov.count())
	}
	age(b, time.Hour-budgetMargin/2)
	busy := workerNames(b)[0]
	ff.busy[busy] = true
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 1 || b.workers[busy] == nil || !ff.drained[busy] {
		t.Fatalf("over budget: %d servers, busy one kept %v, fenced %v", prov.count(), b.workers[busy] != nil, ff.drained[busy])
	}
	// The survivor alone is back under, but stays retired once idle.
	ff.busy[busy] = false
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 0 {
		t.Fatalf("after its job: %d servers", prov.count())
	}
}

func TestOverBudgetCountsWhatIsStillBilled(t *testing.T) {
	b, _, prov := readyBurst(t, 2)
	b.hourly, b.budget = 1.05, 630
	over := func(d time.Duration) string {
		age(b, d)
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.overBudget()
	}
	if why := over(150 * time.Minute); why != "" {
		t.Fatalf("6 hours at €1.05 against €6.30: %s", why)
	}
	names := slices.Collect(maps.Keys(b.workers))
	b.destroy(context.Background(), names[0])
	prov.deleteErr = errors.New("refused")
	b.destroy(context.Background(), names[1])
	if prov.count() != 1 || len(b.workers) != 0 {
		t.Fatalf("control: servers %d, workers %d", prov.count(), len(b.workers))
	}
	// The deleted one stopped at 3 hours; the other is still billed.
	if over(170*time.Minute) != "" || over(190*time.Minute) == "" || over(290*time.Minute) == "" {
		t.Error("an undeleted server stopped counting as it aged")
	}
	if h := billedHours(b); h != 8 {
		t.Errorf("billed %d hours, want 3 for the deleted one and 5 for the other", h)
	}
}

func TestCentsRoundOnceForTheWholeSum(t *testing.T) {
	b := &burst{hourly: 0.014}
	if c := b.cents(100); c != 140 {
		t.Errorf("100 hours at €0.014 = %d cents, want 140", c)
	}
	if b.hourly = 0.011; b.cents(5) != 6 {
		t.Errorf("5 hours at €0.011 = %d cents, want 6", b.cents(5))
	}
	if b.hourly = 100; b.cents(100_000_000_000) != math.MaxInt64 {
		t.Errorf("an overflowing estimate came to %d cents", b.cents(100_000_000_000))
	}
}

func TestAnUnansweredCreateIsBilledUntilKnown(t *testing.T) {
	for name, tc := range map[string]struct {
		prov  *fakeCloud
		bills int
	}{
		"never found":         {&fakeCloud{createErr: errors.New("timeout"), made: true, hideFor: 1000}, 1},
		"refused outright":    {&fakeCloud{createErr: &cloud.APIError{Status: 403, Code: "forbidden"}}, 0},
		"found and destroyed": {&fakeCloud{createErr: errors.New("timeout"), made: true}, 0},
	} {
		b, _ := testBurst(t, tc.prov, &fakeHost{})
		b.start(context.Background(), 1)
		b.creating.Wait()
		b.mu.Lock()
		live := slices.DeleteFunc(slices.Clone(b.bills), func(s *bill) bool { return !s.gone.IsZero() })
		b.mu.Unlock()
		if len(live) != tc.bills || len(tc.prov.specs) != 1 {
			t.Errorf("%s: %d creates, %d still billed, want %d", name, len(tc.prov.specs), len(live), tc.bills)
		}
	}
}

func TestAWorkerIsAdmittedOnlyWithTheCapChecked(t *testing.T) {
	b, ff := testBurst(t, &fakeCloud{}, &fakeHost{})
	admit := b.admit
	b.admit = func(token, name string) {
		if b.mu.TryLock() {
			b.mu.Unlock()
			t.Error("admitted while trim could trip the cap unseen")
		}
		admit(token, name)
	}
	b.start(context.Background(), 1)
	b.creating.Wait()
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	b.end(context.Background())
	if len(ff.admitted) != 1 {
		t.Errorf("control: admitted %v", ff.admitted)
	}
}

func TestAFallbackIsTimedAndCheckedOnItsOwn(t *testing.T) {
	try := func(capFirst bool) (*burst, *fakeCloud) {
		prov := &fakeCloud{full: map[string]bool{"fsn1": true}}
		b, _ := testBurst(t, prov, &fakeHost{})
		b.spec.Locations = []string{"fsn1", "nbg1"}
		prov.onCreate = func(cloud.Spec) {
			if len(prov.specs) == 0 {
				time.Sleep(20 * time.Millisecond)
				if capFirst {
					b.mu.Lock()
					b.capped = "max_burst_cost reached"
					b.mu.Unlock()
				}
			}
		}
		t.Cleanup(func() { b.end(context.Background()) })
		b.start(context.Background(), 1)
		b.creating.Wait()
		return b, prov
	}
	b, prov := try(false)
	waitFor(t, "ready worker", func() bool { return b.readyCount() == 1 })
	b.mu.Lock()
	first, second := b.bills[0].created, b.bills[1].created
	var w *pool.Worker
	for _, bw := range b.workers {
		w = bw.w
	}
	b.mu.Unlock()
	if len(prov.specs) != 2 || second.Sub(first) < 20*time.Millisecond || !w.Cloud.Created.Equal(second) {
		t.Fatalf("creates %d; the fallback billed from %v after the first, its worker created %v", len(prov.specs), second.Sub(first), w.Cloud.Created.Sub(second))
	}
	// Capped while the full one was made: no other try.
	b, prov = try(true)
	if len(prov.specs) != 1 || prov.count() != 0 || len(inventory(t, b)) != 0 {
		t.Fatalf("capped: creates %d, servers %d, inventory %v", len(prov.specs), prov.count(), inventory(t, b))
	}
}

func TestAWorkerReadyOnceCappedIsDestroyed(t *testing.T) {
	prov := &fakeCloud{}
	b, ff := testBurst(t, prov, &fakeHost{})
	b.capped = "max_burst_cost reached"
	b.start(context.Background(), 1)
	t.Cleanup(func() { b.end(context.Background()) })
	b.creating.Wait()
	waitFor(t, "the server destroyed", func() bool { return prov.count() == 0 })
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.admitted) != 0 || billedHours(b) != 1 {
		t.Fatalf("servers %d, admitted %v, billed %d", prov.count(), ff.admitted, billedHours(b))
	}
}

func TestTrimRetiresAWorkerThatNeverDeploys(t *testing.T) {
	b, ff, prov := readyBurst(t, 2)
	stuck, slow := workerNames(b)[0], workerNames(b)[1]
	ff.unseen[stuck], ff.unseen[slow] = deployFor+time.Second, deployFor-time.Second
	b.trim(context.Background(), 10, notDone)
	if prov.count() != 1 || b.workers[stuck] != nil || b.workers[slow] == nil {
		t.Fatalf("servers %d; stuck kept %v, slow kept %v", prov.count(), b.workers[stuck] != nil, b.workers[slow] != nil)
	}
}

func TestWorkerGCWaitsForARunningBurst(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "")
	p := &pool.Pool{Dir: t.TempDir()}
	if err := workerGC(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("control: no burst: %v", err)
	}
	lock, err := p.BurstLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := workerGC(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "burst is running") {
		t.Errorf("during a burst: %v", err)
	}
}

func TestGCAllPurgesOnlyOnceNothingIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		left   bool
		purges int
	}{{"empty", false, 1}, {"a server left", true, 0}} {
		prov := &fakeCloud{deleteErr: errors.New("busy")}
		b, _ := testBurst(t, prov, &fakeHost{})
		deleteWait = 0
		if tc.left {
			prov.servers["9"] = cloud.Server{ID: "9", Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID}}
		}
		open := func(_ context.Context, t target) (provider, target, error) { return prov, t, nil }
		left, err := gcAll(context.Background(), b.pool, []target{b.target()}, open, b.poolID, true, true, b.log)
		if err != nil || left > 0 != tc.left || prov.purges != tc.purges {
			t.Errorf("%s: %d left, %v, %d purges", tc.name, left, err, prov.purges)
		}
	}
}

func TestABurstIsAliveWhileItStarts(t *testing.T) {
	b, _ := testBurst(t, &fakeCloud{}, &fakeHost{})
	asking, started, asked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(asked)
		for i := 0; ; i++ {
			if !b.alive() {
				t.Error("seen as gone while starting")
				return
			}
			select {
			case <-started:
				return
			default:
			}
			if i == 100 {
				close(asking)
			}
		}
	}()
	<-asking
	b.start(context.Background(), 1)
	b.creating.Wait()
	close(started)
	<-asked
	b.end(context.Background())
}
