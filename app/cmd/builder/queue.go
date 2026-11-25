// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/samjuk/magento-compatability/internal/coord"
	"github.com/samjuk/magento-compatability/internal/images"
	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/pool"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/runner"
)

func runQueue(args []string) {
	fs := flag.NewFlagSet("queue", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: magento-compatibility-builder queue [flags]

Works through a sweep unattended. Safe to stop with Ctrl+C or SIGTERM and
start again: it carries on where it left off.

Takes the same filters as test, or -ids-file for an exact list. Runs each
combination with no result, retries stack_up failures up to 3 times and
gives any other failure from this sweep one more go.

With -workers, the named workers pull runs from worker.sock in the state
directory as well; -slots 0 leaves all the work to them. Each remote one
gets an SSH tunnel to the socket, the sweep's release and a restart.

A number in the slots file overrides -slots until the file is removed. One
slot is dropped while free memory is under -min-free-mem, and no run starts
while the disk is under -min-free-gb.

Examples:
  builder queue -product magento -version 2.4.8
  builder queue -ids-file deviations.txt -retry-failures
  builder queue -ids-file deviations.txt -status

Flags:`)
		fs.PrintDefaults()
	}

	var (
		sel           = addSelectionFlags(fs, 45*time.Minute)
		idsFile       = fs.String("ids-file", "", "Run only the combination IDs listed in this file, one per line")
		numSlots      = fs.Int("slots", 2, "Concurrent runs")
		minFreeMem    = fs.Int("min-free-mem", 25, "Drop a slot while free memory is under this percentage")
		minFreeGB     = fs.Uint64("min-free-gb", 40, "Start no run while the disk has less than this free")
		diskPath      = fs.String("disk-path", "/", "Filesystem to check for free space (Docker's data root on Linux)")
		force         = fs.Bool("force", false, "Run each combination once even if it already passed")
		retryFailures = fs.Bool("retry-failures", false, "Give failures from earlier sweeps one more go")
		fresh         = fs.Bool("fresh", false, "Forget this sweep's saved attempt counts")
		status        = fs.Bool("status", false, "Print the sweep's progress and exit")
		workerNames   = fs.String("workers", "", "Comma-separated workers from the inventory (builder worker list) to hand runs to")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	combos := sel.load()
	if *idsFile != "" {
		var err error
		if combos, err = keepIDs(combos, *idsFile); err != nil {
			fatalf("%v", err)
		}
	}
	if len(combos) == 0 {
		logWarn("No combinations match the given filters.")
		return
	}

	stateDir := queueStateDir()
	lockPath := filepath.Join(stateDir, "queue.lock")
	if !*status {
		lock, err := takeQueueLock(lockPath)
		if err != nil {
			fatalf("%v", err)
		}
		defer lock.Close()
	}

	var ws *workers
	if *workerNames != "" && !*status {
		ws = loadWorkers(*workerNames, filepath.Join(stateDir, "worker.sock"))
	}
	lg, err := queue.OpenLedger(stateDir)
	if err != nil {
		fatalf("%v", err)
	}
	defer lg.Close()
	contract := sel.contract()
	releases := filepath.Join(stateDir, "releases")
	if contract.Release, err = sweepRelease(lg, sel, combos, *fresh && !*status, *workerNames != "" && !*status, releases); err != nil && !*status {
		fatalf("%v", err)
	}
	if ws != nil && len(ws.remote) > 0 {
		if _, err := os.Stat(filepath.Join(releases, contract.Release) + ".tar.gz"); err != nil {
			fatalf("remote workers need the release tarball: %v; -fresh snapshots the checkout again", err)
		}
	}
	src, err := queue.NewLocal(lg, sel.resultsDir, combos, contract, *fresh && !*status)
	if err != nil && !(*status && errors.Is(err, queue.ErrContractChanged)) {
		fatalf("%v", err)
	}
	src.Force = *force
	src.RetryFailures = *retryFailures
	if *status {
		printQueueStatus(src, lockPath)
		return
	}
	if err := src.Save(); err != nil {
		fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal, a second one kills the process instead of
	// waiting out teardown; the next start removes what it left.
	go func() { <-ctx.Done(); stop() }()
	logger := log.New(os.Stderr, "", log.LstdFlags)

	stacksDir := filepath.Join(stateDir, "stacks")
	removeLeftoverStacks(ctx, logger, stacksDir)

	slotsFile := filepath.Join(stateDir, "slots")
	slots := func() int {
		n := *numSlots
		if data, err := os.ReadFile(slotsFile); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && v >= 0 {
				n = v
			}
		}
		if free, err := queue.MemoryFreePercent(); err == nil && free < *minFreeMem && n > 1 {
			n--
		}
		return n
	}
	maxSlots := *numSlots
	if data, err := os.ReadFile(slotsFile); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			maxSlots = max(maxSlots, v)
		}
	}

	var mem queue.MemorySampler
	mem.Start(ctx, 20*time.Second)

	base := sel.runConfig()
	base.Force = true
	if contract.Release != "" {
		logger.Printf("running release %s", contract.Release)
		dir := filepath.Join(releases, contract.Release)
		base.ComposeDir = filepath.Join(dir, "docker", "compose")
		if maxSlots > 0 {
			// Local slots run what workers run: pinned images, and
			// Playwright in the release's image.
			m, err := images.Load(dir)
			if err != nil {
				fatalf("%v", err)
			}
			logger.Print("readying the release's images for local slots")
			if err := images.Ready(ctx, dir, contract.Release, m); err != nil {
				fatalf("%v", err)
			}
			// The image also sets the base URL, as on workers, even with
			// Playwright off.
			base.PlaywrightImage = m.Playwright
			if base.PlaywrightDir != "" {
				base.PlaywrightDir = filepath.Join(dir, images.PlaywrightTests)
			}
		}
	}

	counts := src.Counts()
	logger.Printf("started: %d combinations, %d to run; attempts in %s; slots override in %s",
		len(combos), counts["eligible"], src.StatePath(), slotsFile)
	if counts["eligible"] == 0 && counts["tried"] > 0 {
		logger.Print("nothing left to run; -fresh forgets this sweep's attempts, so setup failures and missing results run again")
	}

	var stopWorkers func()
	if *workerNames != "" {
		stopWorkers = serveWorkers(ctx, logger, src, contract, releases, ws, stateDir)
	}

	engine := &queue.Engine{
		Source:   src,
		MaxSlots: maxSlots,
		Slots:    slots,
		Wait:     queue.DiskGuard(*diskPath, *minFreeGB),
		Run: func(ctx context.Context, slot int, j queue.Job) queue.Outcome {
			c := j.Combination
			cfg := base
			project := ""
			cfg.OnProject = func(name string) {
				project = name
				if err := os.WriteFile(filepath.Join(stacksDir, name), nil, 0o644); err != nil {
					logger.Printf("recording stack %s: %v", name, err)
				}
			}
			defer func() {
				if project == "" || ctx.Err() != nil {
					return
				}
				// Teardown can fail or time out; keep the record until it's gone.
				if up, err := queue.StackExists(ctx, project); err == nil && !up {
					os.Remove(filepath.Join(stacksDir, project))
				}
			}()
			logger.Printf("slot %d: start %s (try %d)", slot, c.ID(), j.Tries+1)
			started, before := time.Now(), queue.ResultMod(sel.resultsDir, c)
			_, err := runner.Run(ctx, c, cfg)
			if ctx.Err() != nil {
				logger.Printf("slot %d: %s stopped", slot, c.ID())
				return queue.Outcome{Kind: queue.OutcomeAborted, Requested: true}
			}
			msg := fmt.Sprintf("slot %d: %s -> %s in %s", slot, c.ID(), queue.Status(sel.resultsDir, c), time.Since(started).Round(time.Second))
			if peak := mem.Take(project); peak > 0 {
				msg += fmt.Sprintf(", peak memory %.1fGB", float64(peak)/(1<<30))
			}
			if err != nil {
				msg += ", error: " + err.Error()
			}
			logger.Print(msg)
			after := queue.ResultMod(sel.resultsDir, c)
			return runOutcome(!after.IsZero() && !after.Equal(before), err)
		},
		Log: logger,
	}
	engine.Start(ctx)
	if stopWorkers != nil {
		defer func() { stop(); stopWorkers() }()
		for ctx.Err() == nil && !src.Done() {
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}

	if ctx.Err() != nil {
		logger.Print("stopped")
		return
	}
	counts = src.Counts()
	logger.Printf("done: pass=%d fail=%d setup_fail=%d todo=%d",
		counts[queue.StatusPass], counts[queue.StatusFail], counts[queue.StatusSetupFail], counts[queue.StatusTodo])
}

type workers struct {
	pool   *pool.Pool
	tokens map[string]string
	remote map[string]*pool.Worker
}

func loadWorkers(names, sock string) *workers {
	if err := coord.CheckSocketPath(sock); err != nil {
		fatalf("%v", err)
	}
	p, err := pool.Open()
	if err != nil {
		fatalf("%v", err)
	}
	inv, err := p.Load()
	if err != nil {
		fatalf("%v", err)
	}
	ws := &workers{pool: p, tokens: map[string]string{}, remote: map[string]*pool.Worker{}}
	for name := range strings.SplitSeq(names, ",") {
		w := inv.Workers[name]
		if w == nil {
			fatalf("no worker %q in the inventory; builder worker add makes one", name)
		}
		if _, dup := ws.tokens[w.Token]; dup {
			continue
		}
		ws.tokens[w.Token] = name
		if w.Host != "" {
			if err := pool.LocalSocket(sock); err != nil {
				fatalf("%v", err)
			}
			if _, err := p.Lock(name); err != nil {
				fatalf("%v", err)
			}
			ws.remote[name] = w
		}
	}
	return ws
}

func serveWorkers(ctx context.Context, logger *log.Logger, src *queue.Local, contract queue.Contract, releases string, ws *workers, stateDir string) (wait func()) {
	p, tokens, remote := ws.pool, ws.tokens, ws.remote
	releaseDir := filepath.Join(releases, contract.Release)
	workarounds, err := coord.LoadWorkarounds(filepath.Join(releaseDir, "docker", "scripts", "workarounds.json"))
	if err != nil {
		fatalf("%v", err)
	}
	srv := &coord.Server{Src: src, Tokens: tokens, Contract: contract, Release: contract.Release, Workarounds: workarounds,
		RejectDir: filepath.Join(stateDir, "rejected"), Log: logger}
	sock := filepath.Join(stateDir, "worker.sock")
	if err := srv.Start(ctx); err != nil {
		fatalf("%v", err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.Serve(ctx, sock); err != nil {
			fatalf("worker socket: %v", err)
		}
	}()
	logger.Printf("serving %d workers on %s", len(tokens), sock)
	var tunnels sync.WaitGroup
	for name, w := range remote {
		tunnels.Go(func() { p.Tunnel(ctx, name, w, sock, logger) })
		tunnels.Go(func() {
			logger.Printf("%s: deploying release %.12s", name, contract.Release)
			if p.DeployUntilDone(ctx, name, w, contract.Release, releaseDir+".tar.gz", logger) {
				logger.Printf("%s: release %.12s ready", name, contract.Release)
			}
		})
	}

	return func() { <-served; srv.Wait(); tunnels.Wait() }
}

// runOutcome says how a finished run counts: an error with no result is the
// host's fault, so it spends the host budget rather than the combination's.
func runOutcome(wrote bool, err error) queue.Outcome {
	if !wrote && err != nil {
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: err.Error()}
	}
	return queue.Outcome{Kind: queue.OutcomeResult, Wrote: wrote}
}

func keepIDs(combos []matrix.Combination, path string) ([]matrix.Combination, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byID := map[string]matrix.Combination{}
	for _, c := range combos {
		if other, ok := byID[c.ID()]; ok && other != c {
			return nil, fmt.Errorf("%+v and %+v share the ID %s", other, c, c.ID())
		}
		byID[c.ID()] = c
	}
	var kept []matrix.Combination
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id := strings.TrimSpace(sc.Text())
		if id == "" || strings.HasPrefix(id, "#") || seen[id] {
			continue
		}
		c, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%s: %q is not in the matrix (or not matched by the filters)", path, id)
		}
		seen[id] = true
		kept = append(kept, c)
	}
	return kept, sc.Err()
}

func queueStateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatalf("finding home directory: %v", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "magento-compatibility", "queue")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("%v", err)
	}
	return dir
}

// takeQueueLock allows one queue per machine: every sweep shares Docker,
// memory and the caches. The kernel drops the lock if the process dies.
func takeQueueLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid, _ := os.ReadFile(path)
		f.Close()
		return nil, fmt.Errorf("a queue is already running (pid %s)", strings.TrimSpace(string(pid)))
	}
	if err := f.Truncate(0); err == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return f, nil
}

func printQueueStatus(src *queue.Local, lockPath string) {
	running := "not running"
	if f, err := takeQueueLock(lockPath); err != nil {
		running = err.Error()
	} else {
		f.Close()
	}
	n := src.Counts()
	fmt.Printf("%s\npass=%d fail=%d setup_fail=%d todo=%d, %d still to run\n",
		running, n[queue.StatusPass], n[queue.StatusFail], n[queue.StatusSetupFail], n[queue.StatusTodo], n["eligible"])
}

// removeLeftoverStacks tears down stacks a killed queue left behind, only those this queue recorded.
func removeLeftoverStacks(ctx context.Context, logger *log.Logger, stacksDir string) {
	if err := os.MkdirAll(stacksDir, 0o755); err != nil {
		fatalf("%v", err)
	}
	entries, _ := os.ReadDir(stacksDir)
	for _, e := range entries {
		name := e.Name()
		logger.Printf("removing leftover stack %s", name)
		if err := queue.RemoveStack(ctx, name); err != nil {
			logger.Print(err)
			continue
		}
		os.Remove(filepath.Join(stacksDir, name))
	}
}
