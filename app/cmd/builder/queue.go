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
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
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

Works through a sweep unattended. Safe to stop with Ctrl+C, SIGTERM or
SIGHUP and start again: it carries on where it left off.

Takes the same filters as test, or -ids-file for an exact list. Runs each
combination with no result, retries stack_up failures up to 3 times and
gives any other failure from this sweep one more go.

With -workers, the named workers pull runs from worker.sock in the state
directory as well; -slots 0 leaves all the work to them. Each remote one
gets an SSH tunnel to the socket, the sweep's release and a restart.
-cloud hetzner:N or aws:N rents up to N servers as workers for the sweep
and destroys them when it ends (see pool.yml in the README).

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
		cloudFlag     = fs.String("cloud", "", "Create cloud workers for this sweep and destroy them after: hetzner:N or aws:N")
		yes           = fs.Bool("yes", false, "Go ahead with a burst estimated over max_burst_cost")
	)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *numSlots < 0 || *numSlots > pool.MaxSlots {
		fatalf("-slots %d: 0 to %d", *numSlots, pool.MaxSlots)
	}
	// Remote logs ride in the upload; the worker's fit cuts what is still
	// too big.
	if (*workerNames != "" || *cloudFlag != "") && (sel.maxLogBytes < 0 || sel.maxLogBytes > 2<<20) {
		fatalf("-max-log-bytes %d: 0 to %d with remote workers", sel.maxLogBytes, 2<<20)
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
	burstN, burstProvider := 0, ""
	if *cloudFlag != "" && !*status {
		var err error
		if burstProvider, burstN, err = parseCloud(*cloudFlag); err != nil {
			fatalf("%v", err)
		}
	}
	if (*workerNames != "" || burstN > 0) && !*status {
		ws = loadWorkers(*workerNames, filepath.Join(stateDir, "worker.sock"), burstN > 0)
	}
	lg, err := queue.OpenLedger(stateDir)
	if err != nil {
		fatalf("%v", err)
	}
	defer lg.Close()
	contract := sel.contract()
	releases := filepath.Join(stateDir, "releases")
	if contract.Release, err = sweepRelease(lg, sel, combos, *fresh && !*status, ws != nil, releases); err != nil && !*status {
		fatalf("%v", err)
	}
	if ws != nil && (len(ws.remote) > 0 || burstN > 0) {
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

	// With the terminal gone, a log write mustn't kill teardown.
	signal.Ignore(syscall.SIGPIPE)
	signalled, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// A second signal kills the process rather than waiting out teardown; the next start removes its stacks, worker gc its servers.
	go func() { <-signalled.Done(); stop() }()
	ctx, cancel := context.WithCancelCause(signalled)
	defer cancel(nil)
	incomplete := false
	// Deferred first so it runs last, after a burst's teardown.
	defer func() {
		if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fatalf("%v", err)
		}
		// Stopped, or runs left without a result of their own: a caller mustn't publish what's there.
		if signalled.Err() != nil || incomplete {
			os.Exit(1)
		}
	}()
	logger := log.New(os.Stderr, "", log.LstdFlags)

	stacksDir := filepath.Join(stateDir, "stacks")
	removeLeftoverStacks(ctx, logger, stacksDir)

	slotsFile := filepath.Join(stateDir, "slots")
	slots := func() int {
		n := *numSlots
		if data, err := os.ReadFile(slotsFile); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && v >= 0 {
				n = min(v, pool.MaxSlots)
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
			maxSlots = max(maxSlots, min(v, pool.MaxSlots))
		}
	}
	if maxSlots == 0 && ws == nil {
		fatalf("-slots 0 runs nothing here; give it -workers or -cloud")
	}

	var mem queue.MemorySampler
	mem.Start(ctx, 20*time.Second)

	base := sel.runConfig()
	base.Force = true
	// Rechecked before each local claim: a prune mid-sweep would have
	// compose pull by tag.
	var imagesMu sync.Mutex
	readyImages := func(context.Context) error { return nil }
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
			var checked time.Time
			readyImages = func(ctx context.Context) error {
				imagesMu.Lock()
				defer imagesMu.Unlock()
				// Idle slots poll every few seconds; a docker inspect per
				// image each time is a lot.
				if time.Since(checked) < 30*time.Second {
					return nil
				}
				err := images.Ready(ctx, dir, contract.Release, m)
				if err == nil {
					checked = time.Now()
				}
				return err
			}
			if err := readyImages(ctx); err != nil {
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
	if ws != nil {
		c := &capacity{local: maxSlots > 0 || len(ws.tokens) > len(ws.remote)}
		var b *burst
		if burstN > 0 {
			// Before any worker starts, so a refusal has nothing to tear down.
			if b, err = prepareBurst(ctx, logger, src, burstProvider, burstN, *yes); err != nil {
				fatalf("%v", err)
			}
			if b != nil {
				c.burst, b.capacity = b, c
			}
		}
		f := serveWorkers(ctx, cancel, logger, src, contract, releases, ws, stateDir, c)
		stopWorkers = f.wait
		if b != nil {
			endBurst := b.run(ctx, f, src, cancel)
			stopWorkers = func() { endBurst(); f.wait() }
		}
	}

	engine := &queue.Engine{
		Source:   src,
		MaxSlots: maxSlots,
		Slots:    slots,
		Wait:     guards(queue.DiskGuard(*diskPath, *minFreeGB), imagesGuard(ctx, readyImages)),
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
		// cancel, not stop(): a first signal during teardown is still caught;
		// only a second kills.
		defer func() { cancel(nil); stopWorkers() }()
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
	if n := counts["exhausted"]; n > 0 {
		logger.Printf("%d results stand only because their retries ran out", n)
	}
	// Short of every combination when the ledger couldn't be read.
	incomplete = counts[queue.StatusTodo] > 0 ||
		counts[queue.StatusPass]+counts[queue.StatusFail]+counts[queue.StatusSetupFail] != len(combos)
}

type workers struct {
	pool   *pool.Pool
	tokens map[string]string
	remote map[string]*pool.Worker
}

func loadWorkers(names, sock string, burst bool) *workers {
	if err := coord.CheckSocketPath(sock); err != nil {
		fatalf("%v", err)
	}
	p, err := pool.Open()
	if err != nil {
		fatalf("%v", err)
	}
	// Before the inventory is read, local workers too: a worker remove
	// finishing first must not leave this sweep accepting its token.
	locked := map[string]bool{}
	for name := range strings.SplitSeq(names, ",") {
		if names == "" || locked[name] {
			continue
		}
		lock, err := p.Lock(name)
		if err != nil {
			fatalf("%v", err)
		}
		heldLocks = append(heldLocks, lock)
		locked[name] = true
	}
	inv, err := p.Load()
	if err != nil {
		fatalf("%v", err)
	}
	if left := cloudEntries(inv); len(left) > 0 {
		if burst {
			fatalf("cloud workers from an earlier burst are still in the inventory (%s); builder worker gc destroys them", strings.Join(left, ", "))
		}
		logWarn(fmt.Sprintf("cloud workers from an earlier burst are still in the inventory, maybe still billing: %s; builder worker gc destroys them", strings.Join(left, ", ")))
	}
	if p.DockerAuth, err = pool.DockerHubAuth(os.Getenv("DOCKERHUB_USERNAME"), os.Getenv("DOCKERHUB_TOKEN")); err != nil {
		fatalf("%v", err)
	}
	// Nothing this process starts needs it.
	os.Unsetenv("DOCKERHUB_TOKEN")
	ws := &workers{pool: p, tokens: map[string]string{}, remote: map[string]*pool.Worker{}}
	if burst {
		if err := pool.LocalSocket(sock); err != nil {
			fatalf("%v", err)
		}
	}
	for name := range strings.SplitSeq(names, ",") {
		if names == "" {
			break
		}
		w := inv.Workers[name]
		if w == nil {
			fatalf("no worker %q in the inventory; builder worker add makes one", name)
		}
		if w.Cloud != nil {
			fatalf("%s belongs to a burst; -cloud makes new ones", name)
		}
		if _, dup := ws.tokens[w.Token]; dup {
			continue
		}
		ws.tokens[w.Token] = name
		if w.Remote() {
			if err := pool.LocalSocket(sock); err != nil {
				fatalf("%v", err)
			}
			ws.remote[name] = w
		}
	}
	return ws
}

func cloudEntries(inv *pool.Inventory) []string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(inv.Workers)) {
		if inv.Workers[name].Cloud != nil {
			names = append(names, name)
		}
	}
	return names
}

type fleet struct {
	srv     *coord.Server
	connect func(ctx context.Context, name string, w *pool.Worker)
	wait    func()
}

func serveWorkers(ctx context.Context, fail context.CancelCauseFunc, logger *log.Logger, src *queue.Local, contract queue.Contract, releases string, ws *workers, stateDir string, c *capacity) *fleet {
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
		// Not fatal here: exiting now would skip a burst's teardown.
		if err := srv.Serve(ctx, sock); err != nil {
			fail(fmt.Errorf("worker socket: %w", err))
		}
	}()
	logger.Printf("serving %d workers on %s", len(tokens), sock)
	connect := func(ctx context.Context, name string, w *pool.Worker) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var tunnel sync.WaitGroup
		tunnel.Go(func() {
			if err := p.Tunnel(ctx, name, w, sock, logger); err != nil {
				// Without a tunnel it can't claim; stop deploying to it too.
				logger.Printf("%s: giving up on it, it gets no work this sweep: %v", name, err)
				cancel()
			}
		})
		logger.Printf("%s: deploying release %.12s", name, contract.Release)
		started := time.Now()
		switch err := p.DeployUntilDone(ctx, name, w, contract.Release, releaseDir+".tar.gz", logger); {
		case err == nil:
			srv.Deployed(name)
			logger.Printf("%s: release %.12s ready after %s", name, contract.Release, time.Since(started).Round(time.Second))
		case errors.Is(err, pool.ErrUnfit):
			logger.Printf("%s: giving up on it, it gets no work this sweep: %v", name, err)
			cancel()
		case errors.Is(err, pool.ErrPermanent):
			// Every worker would fail the same; a burst's would bill idle.
			fail(fmt.Errorf("%s: %w", name, err))
		}
		tunnel.Wait()
	}
	tunnels := connectAll(ctx, remote, connect, fail, c)
	return &fleet{srv: srv, connect: connect, wait: func() { <-served; srv.Wait(); tunnels.Wait() }}
}

// connectAll connects each named worker; the last to give up ends the sweep when nothing else can run it.
func connectAll(ctx context.Context, remote map[string]*pool.Worker, connect func(context.Context, string, *pool.Worker), fail context.CancelCauseFunc, c *capacity) *sync.WaitGroup {
	var tunnels sync.WaitGroup
	c.named.Store(int32(len(remote)))
	for name, w := range remote {
		tunnels.Go(func() {
			connect(ctx, name, w)
			if c.named.Add(-1) == 0 && !c.left() {
				fail(errors.New("no worker left that can reach this coordinator"))
			}
		})
	}
	return &tunnels
}

// imagesGuard holds a local slot back, before it claims anything, while
// the host's images aren't ready: the host's trouble, not the work's.
func imagesGuard(ctx context.Context, ready func(context.Context) error) func() string {
	return func() string {
		// A stop isn't the images' fault; the engine sees ctx itself.
		if err := ready(ctx); err != nil && ctx.Err() == nil {
			return "images: " + err.Error()
		}
		return ""
	}
}

func guards(gs ...func() string) func() string {
	return func() string {
		for _, g := range gs {
			if reason := g(); reason != "" {
				return reason
			}
		}
		return ""
	}
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
