// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/pool"
	"github.com/samjuk/magento-compatability/internal/queue"
)

func TestImagesGuardReportsWhileImagesArentReady(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	wait := guards(func() string { return "" }, imagesGuard(context.Background(), func(context.Context) error {
		if broken.Load() {
			return errors.New("daemon gone")
		}
		return nil
	}))
	if reason := wait(); !strings.Contains(reason, "daemon gone") {
		t.Errorf("broken: %q", reason)
	}
	broken.Store(false)
	if reason := wait(); reason != "" {
		t.Errorf("control: %q", reason)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if reason := imagesGuard(ctx, func(ctx context.Context) error { return ctx.Err() })(); reason != "" {
		t.Errorf("stopped: %q", reason)
	}
	if reason := guards(func() string { return "disk" }, func() string { return "" })(); reason != "disk" {
		t.Errorf("first guard: %q", reason)
	}
}

func TestConnectAllEndsTheSweepWhenNoWorkerIsLeft(t *testing.T) {
	remote := map[string]*pool.Worker{"a": {}, "b": {}}
	for _, tc := range []struct {
		others bool
		stays  string // a worker that doesn't give up
		ends   bool
	}{{false, "", true}, {false, "b", false}, {true, "", false}} {
		ctx, cancel := context.WithCancelCause(context.Background())
		connect := func(ctx context.Context, name string, _ *pool.Worker) {
			if name == tc.stays {
				<-ctx.Done()
			}
		}
		tunnels := connectAll(ctx, remote, connect, cancel, &capacity{local: tc.others})
		time.Sleep(20 * time.Millisecond)
		if ended := ctx.Err() != nil; ended != tc.ends {
			t.Errorf("%+v: ended %v (%v)", tc, ended, context.Cause(ctx))
		}
		cancel(nil)
		tunnels.Wait()
	}
}

// A burst's servers can finish what the named workers can't, and the
// named workers what a burst can't; once both are gone, the sweep ends.
func TestConnectAllCountsABurstUntilItIsGone(t *testing.T) {
	b, _, _ := readyBurst(t, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c := &capacity{burst: b}
	b.capacity, b.fail = c, cancel
	connectAll(ctx, map[string]*pool.Worker{"a": {}}, func(context.Context, string, *pool.Worker) {}, cancel, c).Wait()
	if ctx.Err() != nil {
		t.Fatalf("ended with a cloud worker left: %v", context.Cause(ctx))
	}
	for _, bw := range b.workers {
		bw.w.Cloud.Created = time.Now().Add(-2 * time.Hour)
	}
	b.trim(context.Background(), 1, notDone)
	if ctx.Err() == nil {
		t.Fatal("named worker and burst both gone, yet the sweep waits")
	}
}

func TestRunOutcome(t *testing.T) {
	boom := errors.New("docker: name in use")
	for _, tc := range []struct {
		wrote bool
		err   error
		want  queue.OutcomeKind
	}{
		{true, nil, queue.OutcomeResult},
		{true, boom, queue.OutcomeResult}, // a result was written; it counts
		{false, boom, queue.OutcomeHost},
		{false, nil, queue.OutcomeResult},
	} {
		if got := runOutcome(tc.wrote, tc.err); got.Kind != tc.want {
			t.Errorf("runOutcome(%v, %v) = %v, want %v", tc.wrote, tc.err, got.Kind, tc.want)
		}
	}
}

func TestLocalOutcomeClassifiesHostFailuresLikeAWorker(t *testing.T) {
	c := matrix.Combination{Product: "magento", Version: "2.4.8", PHP: "8.3", WebserverType: "nginx", WebserverVersion: "1.26"}
	for _, tc := range []struct {
		code string
		want queue.OutcomeKind
	}{
		{"disk_space", queue.OutcomeHost},
		{"docker_name_conflict", queue.OutcomeHost},
		{"docker_cleanup_race", queue.OutcomeHost},
		{"php_fatal", queue.OutcomeResult}, // the combination's own failure
	} {
		dir := t.TempDir()
		path := queue.ResultPath(dir, c)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"overall_status":"fail","steps":{"stack_up":{"status":"fail","failure":{"code":"` + tc.code + `"}}}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := localOutcome(dir, c, true, nil); got.Kind != tc.want {
			t.Errorf("%s: kind %v, want %v", tc.code, got.Kind, tc.want)
		}
	}
}

type fakeSweep struct {
	done   bool
	counts map[string]int
}

func (f fakeSweep) Done() bool             { return f.done }
func (f fakeSweep) Counts() map[string]int { return f.counts }

func TestSweepIncompleteIgnoresASignalAfterTheLastRun(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	finished := fakeSweep{true, map[string]int{queue.StatusPass: 3}}
	unfinished := fakeSweep{false, map[string]int{queue.StatusPass: 2, queue.StatusTodo: 1}}
	live, cancelled := context.Background(), func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}()
	if sweepIncomplete(cancelled, logger, finished, 3) {
		t.Error("a signal during teardown after a finished sweep made it incomplete")
	}
	if sweepIncomplete(live, logger, finished, 3) {
		t.Error("control: a finished sweep was incomplete")
	}
	if !sweepIncomplete(cancelled, logger, unfinished, 3) {
		t.Error("control: a sweep stopped by a signal with runs left was complete")
	}
	if !sweepIncomplete(live, logger, unfinished, 3) {
		t.Error("control: a sweep with runs left was complete")
	}
}

func TestASweepHoldsItsWorkersLocks(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "m2l")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("DOCKERHUB_USERNAME", "")
	t.Setenv("DOCKERHUB_TOKEN", "")
	p, err := pool.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Update(func(inv *pool.Inventory) error {
		inv.Workers["lab1"] = &pool.Worker{Host: "192.0.2.7", Port: 22, User: "m2test", Dir: pool.DefaultDir, Slots: 1,
			Token: strings.Repeat("a", 32), HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHost"}
		inv.Workers["desk"] = &pool.Worker{Slots: 1, Token: strings.Repeat("b", 32)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loadWorkers("lab1,desk", filepath.Join(dir, "worker.sock"), false)
	runtime.GC()
	runtime.GC()
	for _, name := range []string{"lab1", "desk"} {
		if lock, err := p.Lock(name); err == nil {
			lock.Close()
			t.Errorf("a second sweep, or worker remove, took %s", name)
		}
	}
	for _, l := range heldLocks {
		l.Close()
	}
	heldLocks = nil
	// Control: with the sweep's locks gone, it's free.
	lock, err := p.Lock("lab1")
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	lock.Close()
}

func TestKeepIDsRefusesAnIDTwoCombinationsShare(t *testing.T) {
	a := matrix.Combination{Product: "magento", Version: "2.4.8", PHP: "8.3", WebserverType: "nginx", WebserverVersion: "1.26"}
	b := a
	b.WebserverVersion = "1.28"
	if a.ID() != b.ID() {
		t.Skip("IDs now tell them apart")
	}
	path := filepath.Join(t.TempDir(), "ids")
	os.WriteFile(path, []byte(a.ID()+"\n"), 0o600)
	if kept, err := keepIDs([]matrix.Combination{a}, path); err != nil || len(kept) != 1 {
		t.Fatalf("control: %v %v", kept, err)
	}
	if kept, err := keepIDs([]matrix.Combination{a, b}, path); err == nil {
		t.Errorf("kept %v of two sharing an ID", kept)
	}
}

func TestLeftoverStackRecordOutlivesANetworkStillInUse(t *testing.T) {
	for _, held := range []bool{false, true} {
		bin := t.TempDir()
		// Every docker call succeeds; the network is listed while held.
		script := "#!/bin/sh\nexit 0\n"
		if held {
			script = "#!/bin/sh\n[ \"$1 $2\" = \"network ls\" ] && echo net1\nexit 0\n"
		}
		os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755)
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		stacks := t.TempDir()
		os.WriteFile(filepath.Join(stacks, "m2test-x"), nil, 0o644)
		queue.Stacks{Dir: stacks, Log: log.New(io.Discard, "", 0)}.RemoveLeftovers(context.Background(), func(string, error) {})
		if _, err := os.Stat(filepath.Join(stacks, "m2test-x")); (err == nil) != held {
			t.Errorf("network held %v: record kept %v", held, err == nil)
		}
	}
}
