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
		tunnels := connectAll(ctx, remote, connect, cancel, tc.others)
		time.Sleep(20 * time.Millisecond)
		if ended := ctx.Err() != nil; ended != tc.ends {
			t.Errorf("%+v: ended %v (%v)", tc, ended, context.Cause(ctx))
		}
		cancel(nil)
		tunnels.Wait()
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
		removeLeftoverStacks(context.Background(), log.New(io.Discard, "", 0), stacks)
		if _, err := os.Stat(filepath.Join(stacks, "m2test-x")); (err == nil) != held {
			t.Errorf("network held %v: record kept %v", held, err == nil)
		}
	}
}
