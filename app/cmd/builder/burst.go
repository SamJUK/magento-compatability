// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/cloud"
	"github.com/samjuk/magento-compatability/internal/pool"
	"github.com/samjuk/magento-compatability/internal/queue"
)

type cloudConfig struct {
	Hetzner struct {
		Type     string `yaml:"type"`
		Location string `yaml:"location"`
		Image    string `yaml:"image"`
		Slots    int    `yaml:"slots"`
	} `yaml:"hetzner"`
	MaxWorkers    int           `yaml:"max_workers"`
	MaxBurstCost  float64       `yaml:"max_burst_cost"` // euros, gross
	SSHFrom       []string      `yaml:"ssh_from"`
	MaxAge        time.Duration `yaml:"max_age"`
	MinutesPerRun float64       `yaml:"minutes_per_run"`
	SetupMinutes  float64       `yaml:"setup_minutes"`
}

func loadCloudConfig(p *pool.Pool) (cloudConfig, error) {
	var c cloudConfig
	c.Hetzner.Type, c.Hetzner.Location, c.Hetzner.Image, c.Hetzner.Slots = "ccx33", "hel1", "debian-13", 5
	c.MaxWorkers, c.MaxBurstCost, c.MaxAge, c.MinutesPerRun, c.SetupMinutes = 10, 10, 8*time.Hour, 4, 10
	path := filepath.Join(p.Dir, "pool.yml")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("%s is missing; cloud bursts need at least ssh_from, the CIDRs that may reach workers over SSH", path)
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case len(c.SSHFrom) == 0:
		return c, fmt.Errorf("%s: ssh_from is empty", path)
	case c.Hetzner.Slots < 1 || c.Hetzner.Slots > pool.MaxSlots:
		return c, fmt.Errorf("%s: hetzner slots must be 1 to %d", path, pool.MaxSlots)
	case c.MaxWorkers < 1 || c.MaxBurstCost <= 0 || c.MaxAge <= 0 || c.MinutesPerRun <= 0 || c.SetupMinutes < 0:
		return c, fmt.Errorf("%s: max_workers, max_burst_cost, max_age and minutes_per_run must be positive", path)
	}
	return c, nil
}

// parseCloud reads -cloud hetzner:N.
func parseCloud(s string) (int, error) {
	provider, n, ok := strings.Cut(s, ":")
	count, err := strconv.Atoi(n)
	if !ok || provider != "hetzner" || err != nil || count < 1 {
		return 0, fmt.Errorf("-cloud %q: want hetzner:N", s)
	}
	return count, nil
}

// burstPlan sizes a burst: no more workers than the runs can use or the
// cap allows, and its cost if every worker runs to the end, in whole hours.
func burstPlan(c cloudConfig, asked, existing, runs int, hourly float64) (n int, cost float64) {
	n = min(asked, c.MaxWorkers-existing, int(math.Ceil(float64(runs)/float64(c.Hetzner.Slots))))
	if n <= 0 {
		return 0, 0
	}
	minutes := float64(runs)*c.MinutesPerRun/float64(n*c.Hetzner.Slots) + c.SetupMinutes
	return n, float64(n) * math.Ceil(minutes/60) * hourly
}

type provider interface {
	Create(ctx context.Context, s cloud.Spec) (cloud.Server, error)
	Get(ctx context.Context, id int64) (cloud.Server, error)
	Delete(ctx context.Context, id int64) error
	List(ctx context.Context, selector string) ([]cloud.Server, error)
}

type cloudHost interface {
	Script(ctx context.Context, name, script string) ([]byte, error)
	Setup(ctx context.Context, name string, w *pool.Worker, binary string) error
}

// burst creates cloud workers for a sweep and destroys them once it can do without them.
type burst struct {
	pool     *pool.Pool
	host     cloudHost
	prov     provider
	cfg      cloudConfig
	sshKey   int64
	firewall int64
	poolKey  string
	poolID   string
	binary   string
	log      *log.Logger
	// connect runs a ready worker's tunnel and deploy until ctx ends.
	connect func(ctx context.Context, name string, w *pool.Worker)
	// admit lets a worker's token in; drain is coord.Server.Drain.
	admit   func(token, name string)
	drain   func(name string, always bool) bool
	abandon func(name string)

	mu       sync.Mutex
	workers  map[string]*burstWorker
	creating sync.WaitGroup
	running  sync.WaitGroup
}

type burstWorker struct {
	w       *pool.Worker
	stop    context.CancelFunc
	stopped chan struct{}
	ready   bool
}

var (
	provisionWait = 15 * time.Minute
	provisionPoll = 10 * time.Second
	deleteWait    = 2 * time.Minute
)

// start creates n workers in parallel; each joins the sweep once set up.
func (b *burst) start(ctx context.Context, n int) {
	b.workers = map[string]*burstWorker{}
	for range n {
		b.creating.Go(func() {
			if err := b.create(ctx); err != nil {
				b.log.Printf("cloud worker: %v", err)
			}
		})
	}
}

func (b *burst) create(ctx context.Context) error {
	nonce := make([]byte, 6)
	rand.Read(nonce)
	n := hex.EncodeToString(nonce)
	name := "hz-" + n
	token, err := pool.NewToken()
	if err != nil {
		return err
	}
	private, public, err := pool.HostKeyPair(name)
	if err != nil {
		return err
	}
	userData, err := cloud.UserData(cloud.Host{PoolKey: b.poolKey, HostPrivateKey: private, HostPublicKey: public, Dir: pool.DefaultDir})
	if err != nil {
		return err
	}
	w := &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: keyOnly(public), Token: token,
		Slots: b.cfg.Hetzner.Slots, Cloud: &pool.Cloud{Provider: "hetzner", Nonce: n, Created: time.Now().UTC()}}
	// Recorded first, so a server whose create answer is lost is still found.
	if err := b.pool.Update(func(inv *pool.Inventory) error { inv.Workers[name] = w; return nil }); err != nil {
		return err
	}
	spec := cloud.Spec{Name: name, Type: b.cfg.Hetzner.Type, Location: b.cfg.Hetzner.Location, Image: b.cfg.Hetzner.Image,
		UserData: userData, SSHKey: b.sshKey, Firewall: b.firewall,
		Labels: map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID, cloud.Label + "-nonce": n}}
	if ctx.Err() != nil {
		b.forget(name)
		return ctx.Err()
	}
	srv, err := b.prov.Create(ctx, spec)
	switch {
	case srv.ID != 0:
	case cloud.Refused(err):
		b.forget(name)
		return fmt.Errorf("%s: create: %v", name, err)
	case err != nil && srv.ID == 0:
		// It may exist, and take a moment to show. Its actions (the
		// firewall among them) can't be checked, so it isn't used.
		found, lerr := b.findByNonce(context.WithoutCancel(ctx), n)
		if lerr != nil || len(found) != 1 {
			return fmt.Errorf("%s: create: %v; can't tell whether it exists (%d found, %v), so it stays in the inventory for builder worker gc", name, err, len(found), lerr)
		}
		srv = found[0]
	}
	w.Host, w.Cloud.ID = srv.IP, srv.ID
	if rerr := b.pool.Update(func(inv *pool.Inventory) error {
		inv.Workers[name] = w
		return nil
	}); err != nil || rerr != nil {
		b.delete(context.WithoutCancel(ctx), name, w)
		return fmt.Errorf("%s: server %d: %v", name, srv.ID, errors.Join(err, rerr))
	}
	b.log.Printf("%s: server %d at %s; waiting for cloud-init", name, srv.ID, srv.IP)

	bw := &burstWorker{w: w, stopped: make(chan struct{})}
	wctx, stop := context.WithCancel(ctx)
	bw.stop = stop
	b.mu.Lock()
	b.workers[name] = bw
	b.mu.Unlock()
	b.running.Go(func() {
		defer close(bw.stopped)
		if err := b.provision(wctx, name, w); err != nil {
			if wctx.Err() == nil && b.take(name) != nil {
				b.log.Printf("%s: %v; destroying it", name, err)
				b.delete(context.WithoutCancel(ctx), name, w)
			}
			return
		}
		b.admit(token, name)
		b.mu.Lock()
		bw.ready = true
		b.mu.Unlock()
		b.connect(wctx, name, w)
	})
	return nil
}

var findTries = 6

func (b *burst) findByNonce(ctx context.Context, nonce string) (found []cloud.Server, err error) {
	for i := range findTries {
		if i > 0 {
			time.Sleep(provisionPoll / 2)
		}
		if found, err = b.prov.List(ctx, nonceSelector(b.poolID, nonce)); err == nil && len(found) > 0 {
			return found, nil
		}
	}
	return found, err
}

func (b *burst) provision(ctx context.Context, name string, w *pool.Worker) error {
	ctx, cancel := context.WithTimeout(ctx, provisionWait)
	defer cancel()
	marker := w.Dir + "/" + cloud.Provisioned
	for {
		_, err := b.host.Script(ctx, name, "test -f "+marker)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not provisioned after %s: %v", provisionWait, err)
		case <-time.After(provisionPoll):
		}
	}
	b.log.Printf("%s: cloud-init done after %s", name, time.Since(w.Cloud.Created).Round(time.Second))
	if err := b.host.Setup(ctx, name, w, b.binary); err != nil {
		return err
	}
	b.log.Printf("%s: set up after %s", name, time.Since(w.Cloud.Created).Round(time.Second))
	return nil
}

func (b *burst) take(name string) *burstWorker {
	b.mu.Lock()
	defer b.mu.Unlock()
	bw := b.workers[name]
	delete(b.workers, name)
	return bw
}

// destroy stops a worker's tunnel and deletes its server.
func (b *burst) destroy(ctx context.Context, name string) {
	if bw := b.take(name); bw != nil {
		bw.stop()
		<-bw.stopped
		b.delete(ctx, name, bw.w)
	}
}

// delete forgets a worker once its server is confirmed gone; otherwise it stays for gc.
func (b *burst) delete(ctx context.Context, name string, w *pool.Worker) bool {
	var err error
	for until := time.Now().Add(deleteWait); ; {
		if err = b.prov.Delete(ctx, w.Cloud.ID); err == nil {
			_, err = b.prov.Get(ctx, w.Cloud.ID)
			if errors.Is(err, cloud.ErrNotFound) {
				b.log.Printf("%s: destroyed", name)
				return b.forget(name) == nil
			}
		}
		// Locked by a running action, rate limited or a blip: try again.
		if cloud.Refused(err) && !cloud.Locked(err) || ctx.Err() != nil || time.Now().After(until) {
			break
		}
		time.Sleep(provisionPoll / 2)
	}
	if err == nil {
		err = fmt.Errorf("still there after %s", deleteWait)
	}
	b.log.Printf("%s: destroying server %d: %v; builder worker gc retries", name, w.Cloud.ID, err)
	return false
}

func (b *burst) forget(name string) error {
	err := b.pool.Update(func(inv *pool.Inventory) error {
		if w := inv.Workers[name]; w != nil && w.Cloud != nil {
			delete(inv.Workers, name)
		}
		return nil
	})
	if err != nil {
		b.log.Printf("%s: removing it from the inventory: %v", name, err)
	}
	return err
}

// trim destroys the workers the sweep can do without: idle ones once
// nothing is waiting, keeping one until the sweep is done so a late retry
// has somewhere to go, and any past max age once they finish their runs.
func (b *burst) trim(ctx context.Context, waiting int, done bool) {
	b.mu.Lock()
	var ready, old []string
	for name, bw := range b.workers {
		if !bw.ready {
			continue
		}
		if time.Since(bw.w.Cloud.Created) > b.cfg.MaxAge {
			old = append(old, name)
		} else {
			ready = append(ready, name)
		}
	}
	b.mu.Unlock()
	var gone []string
	for _, name := range old {
		if b.drain(name, true) {
			b.log.Printf("%s: past max age %s", name, b.cfg.MaxAge)
			gone = append(gone, name)
		}
	}
	if waiting == 0 {
		accounted := 0
		if !done {
			accounted = 1
		}
		for _, name := range ready {
			if accounted < len(ready) && b.drain(name, false) {
				gone = append(gone, name)
				accounted++
			}
		}
	}
	// A delete cut short would leave a server no one tracks but gc.
	for _, name := range gone {
		b.destroy(context.WithoutCancel(ctx), name)
	}
}

// end destroys every worker the burst still has.
func (b *burst) end(ctx context.Context) {
	b.creating.Wait()
	b.mu.Lock()
	all := b.workers
	b.workers = map[string]*burstWorker{}
	b.mu.Unlock()
	if len(all) > 0 {
		b.log.Printf("destroying %d cloud workers; another Ctrl+C leaves them running for builder worker gc", len(all))
	}
	for _, bw := range all {
		bw.stop()
	}
	b.running.Wait()
	var wg sync.WaitGroup
	for name, bw := range all {
		// Stopped mid-run by the queue, or left for gc to destroy: either
		// way not the host's failure, which a resume would charge it as.
		b.abandon(name)
		wg.Go(func() { b.delete(ctx, name, bw.w) })
	}
	wg.Wait()
	if left, err := gc(ctx, b.pool, b.prov, b.poolID, true, b.log); err != nil || left > 0 {
		b.log.Printf("%d cloud workers left (%v); builder worker gc destroys them", left, err)
	}
}

func keyOnly(public string) string {
	f := strings.Fields(public)
	if len(f) < 2 {
		return public
	}
	return f[0] + " " + f[1]
}

func startBurst(ctx context.Context, logger *log.Logger, f *fleet, src *queue.Local, asked int, yes bool) (end func()) {
	p, err := pool.Open()
	if err != nil {
		fatalf("%v", err)
	}
	cfg, err := loadCloudConfig(p)
	if err != nil {
		fatalf("%v", err)
	}
	hz := newHetzner()
	hourly, goarch, err := hz.ServerType(ctx, cfg.Hetzner.Type, cfg.Hetzner.Location)
	if err != nil {
		fatalf("%v", err)
	}
	poolID, err := p.ID()
	if err != nil {
		fatalf("%v", err)
	}
	if left, err := hz.List(ctx, serverSelector(poolID)); err != nil || len(left) > 0 {
		fatalf("checking for servers an earlier burst left: %d found, %v; builder worker gc destroys them", len(left), err)
	}
	runs := src.Counts()["eligible"]
	n, cost := burstPlan(cfg, asked, 0, runs, hourly)
	if n == 0 {
		logger.Print("no cloud workers needed")
		return func() {}
	}
	logger.Printf("burst: %d %s in %s for %d runs, about €%.2f; €%.2f if every one ran to max_age", n, cfg.Hetzner.Type, cfg.Hetzner.Location, runs, cost,
		float64(n)*math.Ceil(cfg.MaxAge.Hours())*hourly)
	if cost > cfg.MaxBurstCost && !yes {
		fatalf("that is over max_burst_cost (€%.2f); -yes goes ahead", cfg.MaxBurstCost)
	}
	poolKey, err := p.PublicKey()
	if err != nil {
		fatalf("%v", err)
	}
	sshKey, err := hz.SSHKey(ctx, "m2test-pool-"+poolID, poolKey)
	if err != nil {
		fatalf("%v", err)
	}
	firewall, err := hz.Firewall(ctx, "m2test-"+poolID, cfg.SSHFrom)
	if err != nil {
		fatalf("%v", err)
	}
	tmp, err := os.MkdirTemp("", "m2test-burst")
	if err != nil {
		fatalf("%v", err)
	}
	bin := filepath.Join(tmp, "m2test-worker")
	if err := pool.BuildWorker(ctx, filepath.Join(findRepoRoot(), "app"), goarch, bin); err != nil {
		os.RemoveAll(tmp)
		fatalf("%v", err)
	}
	// A sleeping laptop drops every tunnel; macOS only.
	if path, err := exec.LookPath("caffeinate"); err == nil {
		// Not tied to ctx: teardown after Ctrl+C needs the network too.
		exec.Command(path, "-i", "-s", "-w", strconv.Itoa(os.Getpid())).Start()
	}
	b := &burst{pool: p, host: p, prov: hz, cfg: cfg, sshKey: sshKey, firewall: firewall, poolKey: poolKey, binary: bin, poolID: poolID,
		log: logger, connect: f.connect, admit: f.srv.AddWorker, drain: f.srv.Drain, abandon: f.srv.Abandon}
	b.start(ctx, n)
	trimmed := make(chan struct{})
	go func() {
		defer close(trimmed)
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
				b.trim(ctx, src.Counts()["queued"], src.Done())
			}
		}
	}()
	return func() {
		<-trimmed
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		b.end(ctx)
		os.RemoveAll(tmp)
	}
}

// workerGC destroys what dead bursts left, found by inventory and by label.
func workerGC(ctx context.Context, p *pool.Pool, args []string) {
	fs := flag.NewFlagSet("worker gc", flag.ExitOnError)
	yes := fs.Bool("yes", false, "Destroy them; without it, only list them")
	fs.Parse(args)
	lock, err := takeQueueLock(filepath.Join(queueStateDir(), "queue.lock"))
	if err != nil {
		fatalf("%v; a running burst destroys its own workers", err)
	}
	defer lock.Close()
	poolID, err := p.ID()
	if err != nil {
		fatalf("%v", err)
	}
	left, err := gc(ctx, p, newHetzner(), poolID, *yes, log.New(os.Stderr, "", log.LstdFlags))
	if err != nil {
		fatalf("%v", err)
	}
	if left > 0 && *yes {
		fatalf("%d left; run it again", left)
	}
}

func serverSelector(poolID string) string {
	return cloud.Label + "=worker," + cloud.Label + "-pool=" + poolID
}

func nonceSelector(poolID, nonce string) string {
	return serverSelector(poolID) + "," + cloud.Label + "-nonce=" + nonce
}

// unseenFor is how long a create never answered must stay out of the
// listing before gc takes it as never made.
var unseenFor = time.Hour

func gc(ctx context.Context, p *pool.Pool, prov provider, poolID string, yes bool, logger *log.Logger) (left int, err error) {
	inv, err := p.Load()
	if err != nil {
		// The labels still find every server.
		logger.Printf("reading the inventory: %v; going by label only", err)
		inv = &pool.Inventory{}
	}
	servers, err := prov.List(ctx, serverSelector(poolID))
	if err != nil {
		return 0, err
	}
	doomed := map[int64]string{}
	for _, s := range servers {
		doomed[s.ID] = s.Name
	}
	for _, name := range cloudEntries(inv) {
		c := inv.Workers[name].Cloud
		if c.ID == 0 {
			// Its create was never answered. If the server exists, its labels
			// listed it above; one made late may not show yet.
			if found, err := prov.List(ctx, nonceSelector(poolID, c.Nonce)); err != nil || len(found) > 0 || time.Since(c.Created) < unseenFor {
				if err != nil {
					logger.Printf("%s: %v", name, err)
					left++
				} else if len(found) == 0 {
					logger.Printf("%s: create never answered, %s ago; kept until it's been out of sight for %s", name, time.Since(c.Created).Round(time.Second), unseenFor)
					left++
				}
				for _, s := range found {
					doomed[s.ID] = name
				}
				continue
			}
			if !yes {
				logger.Printf("%s: never created, would be forgotten", name)
				left++
			} else if err := p.Update(func(inv *pool.Inventory) error { delete(inv.Workers, name); return nil }); err != nil {
				logger.Printf("%s: %v", name, err)
				left++
			}
			continue
		}
		// The token may be for another project, where this ID is someone
		// else's server.
		srv, err := prov.Get(ctx, c.ID)
		switch {
		case errors.Is(err, cloud.ErrNotFound):
			if yes && p.Update(func(inv *pool.Inventory) error { delete(inv.Workers, name); return nil }) == nil {
				logger.Printf("%s: server %d is already gone", name, c.ID)
				continue
			}
			left++
		case err != nil:
			logger.Printf("%s: %v", name, err)
			left++
		case srv.Labels[cloud.Label] != "worker" || srv.Labels[cloud.Label+"-pool"] != poolID:
			logger.Printf("%s: server %d isn't this pool's (labels %v); left alone", name, c.ID, srv.Labels)
			// Relabelled since List put it in doomed.
			delete(doomed, c.ID)
			left++
		default:
			doomed[c.ID] = name
		}
	}
	b := &burst{pool: p, prov: prov, log: logger}
	for _, id := range slices.Sorted(maps.Keys(doomed)) {
		name := doomed[id]
		if !yes {
			logger.Printf("%s: server %d would be destroyed", name, id)
			left++
			continue
		}
		if !b.delete(ctx, name, &pool.Worker{Cloud: &pool.Cloud{ID: id}}) {
			left++
		}
	}
	if !yes && left > 0 {
		logger.Print("-yes destroys them")
	}
	return left, nil
}

// newHetzner reads the same variables as the hcloud CLI.
func newHetzner() *cloud.Hetzner {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		fatalf("HCLOUD_TOKEN is not set")
	}
	hz := cloud.NewHetzner(token)
	if e := os.Getenv("HCLOUD_ENDPOINT"); e != "" {
		hz.Endpoint = strings.TrimSuffix(e, "/")
	}
	return hz
}
