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
	"net/netip"
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
	AWS struct {
		Type   string  `yaml:"type"`
		Region string  `yaml:"region"`
		Image  string  `yaml:"image"` // debian-13, or an AMI ID
		Slots  int     `yaml:"slots"`
		Hourly float64 `yaml:"hourly"` // euros, with the public IPv4 address and disk
		DiskGB int32   `yaml:"disk_gb"`
	} `yaml:"aws"`
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
	c.AWS.Type, c.AWS.Image, c.AWS.Slots, c.AWS.DiskGB = "m7a.2xlarge", "debian-13", 5, 160
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
	case slices.ContainsFunc(c.SSHFrom, func(s string) bool { p, err := netip.ParsePrefix(s); return err != nil || p != p.Masked() }):
		return c, fmt.Errorf("%s: ssh_from wants CIDRs such as 198.51.100.4/32", path)
	case c.Hetzner.Slots < 1 || c.Hetzner.Slots > pool.MaxSlots || c.AWS.Slots < 1 || c.AWS.Slots > pool.MaxSlots:
		return c, fmt.Errorf("%s: slots must be 1 to %d", path, pool.MaxSlots)
	case c.AWS.DiskGB < 30:
		return c, fmt.Errorf("%s: aws disk_gb must be at least 30", path)
	case slices.ContainsFunc([]float64{c.AWS.Hourly, c.MaxBurstCost, c.MinutesPerRun, c.SetupMinutes}, func(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) }):
		return c, fmt.Errorf("%s: a cost or time is not a number", path)
	case c.MaxWorkers < 1 || c.MaxBurstCost <= 0 || c.MaxAge <= 0 || c.MinutesPerRun <= 0 || c.SetupMinutes < 0:
		return c, fmt.Errorf("%s: max_workers, max_burst_cost, max_age and minutes_per_run must be positive", path)
	}
	return c, nil
}

// parseCloud reads -cloud hetzner:N or aws:N.
func parseCloud(s string) (provider string, count int, err error) {
	provider, n, ok := strings.Cut(s, ":")
	count, err = strconv.Atoi(n)
	if !ok || provider != "hetzner" && provider != "aws" || err != nil || count < 1 {
		return "", 0, fmt.Errorf("-cloud %q: want hetzner:N or aws:N", s)
	}
	return provider, count, nil
}

// burstPlan sizes a burst: no more workers than the runs can use or the
// cap allows, and its cost if every worker runs to the end, in whole hours.
func burstPlan(c cloudConfig, slots, asked, existing, runs int, hourly float64) (n int, cost float64) {
	n = min(asked, c.MaxWorkers-existing, int(math.Ceil(float64(runs)/float64(slots))))
	if n <= 0 {
		return 0, 0
	}
	minutes := float64(runs)*c.MinutesPerRun/float64(n*slots) + c.SetupMinutes
	return n, float64(n) * math.Ceil(minutes/60) * hourly
}

type provider interface {
	Create(ctx context.Context, s cloud.Spec) (cloud.Server, error)
	Get(ctx context.Context, id string) (cloud.Server, error)
	Delete(ctx context.Context, id string) error
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
	provider string // hetzner or aws
	region   string // aws only
	account  string // aws only
	slots    int
	spec     cloud.Spec // what every create asks for, less its own parts
	// deadline, if set, is how long after its create a server powers
	// itself off; on EC2 that terminates it.
	deadline time.Duration
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
	name := map[string]string{"hetzner": "hz-", "aws": "aws-"}[b.provider] + n
	created := time.Now().UTC()
	var deadline time.Time
	if b.deadline > 0 {
		deadline = created.Add(b.deadline)
	}
	token, err := pool.NewToken()
	if err != nil {
		return err
	}
	private, public, err := pool.HostKeyPair(name)
	if err != nil {
		return err
	}
	userData, err := cloud.UserData(cloud.Host{PoolKey: b.poolKey, HostPrivateKey: private, HostPublicKey: public, Dir: pool.DefaultDir, Deadline: deadline})
	if err != nil {
		return err
	}
	w := &pool.Worker{Port: 22, User: "m2test", Dir: pool.DefaultDir, HostKey: keyOnly(public), Token: token,
		Slots: b.slots, Cloud: &pool.Cloud{Provider: b.provider, Region: b.region, Account: b.account, Nonce: n, Created: created}}
	// Recorded first, so a server whose create answer is lost is still found.
	if err := b.pool.Update(func(inv *pool.Inventory) error { inv.Workers[name] = w; return nil }); err != nil {
		return err
	}
	spec := b.spec
	spec.Name, spec.UserData = name, userData
	spec.Labels = map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID, cloud.Label + "-nonce": n}
	if ctx.Err() != nil {
		b.forget(name)
		return ctx.Err()
	}
	srv, err := b.prov.Create(ctx, spec)
	switch {
	case srv.ID != "":
	case cloud.Refused(err):
		b.forget(name)
		return fmt.Errorf("%s: create: %v", name, err)
	case err != nil && srv.ID == "":
		// It may exist, and take a moment to show. Its actions (the
		// firewall among them) can't be checked, so it isn't used.
		found, lerr := b.findByNonce(context.WithoutCancel(ctx), n)
		if lerr != nil || len(found) != 1 {
			return fmt.Errorf("%s: create: %v; can't tell whether it exists (%d found, %v), so it stays in the inventory for builder worker gc", name, err, len(found), lerr)
		}
		srv = found[0]
	}
	w.Host, w.Cloud.ID = srv.IP, pool.ServerID(srv.ID)
	if rerr := b.pool.Update(func(inv *pool.Inventory) error {
		inv.Workers[name] = w
		return nil
	}); err != nil || rerr != nil {
		b.delete(context.WithoutCancel(ctx), name, w)
		return fmt.Errorf("%s: server %s: %v", name, srv.ID, errors.Join(err, rerr))
	}
	b.log.Printf("%s: server %s at %s; waiting for cloud-init", name, srv.ID, srv.IP)

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

const deadlineGrace = time.Hour

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

func inAccount(a string) string {
	if a == "" {
		return "has no AWS account recorded"
	}
	return "is in AWS account " + a
}

// delete forgets a worker once its server is confirmed gone; otherwise it stays for gc.
func (b *burst) delete(ctx context.Context, name string, w *pool.Worker) bool {
	if w.Cloud.Account != b.account {
		b.log.Printf("%s: server %s %s, not %s; not destroying it", name, w.Cloud.ID, inAccount(w.Cloud.Account), b.account)
		return false
	}
	var err error
	for until := time.Now().Add(deleteWait); ; {
		if err = b.prov.Delete(ctx, string(w.Cloud.ID)); err == nil {
			_, err = b.prov.Get(ctx, string(w.Cloud.ID))
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
	b.log.Printf("%s: destroying server %s: %v; builder worker gc retries", name, w.Cloud.ID, err)
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
	if left, err := gc(ctx, b.pool, b.prov, b.target(), b.poolID, true, b.log); err != nil || left > 0 {
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

func (b *burst) useAWS(ctx context.Context, a *cloud.AWS) (goarch string, err error) {
	c := b.cfg.AWS
	b.prov, b.region, b.account, b.slots = a, c.Region, a.Account, c.Slots
	// Past max age the coordinator destroys it; this is for when the
	// coordinator is gone, so it leaves a running job time to finish.
	b.deadline = b.cfg.MaxAge + deadlineGrace
	if goarch, err = a.InstanceType(ctx, c.Type); err != nil {
		return "", err
	}
	image, root, err := a.Image(ctx, c.Image, goarch)
	if err != nil {
		return "", err
	}
	subnets, err := a.Subnets(ctx, c.Type)
	if err != nil {
		return "", err
	}
	b.spec = cloud.Spec{Type: c.Type, Image: image, RootDevice: root, DiskGB: c.DiskGB, Subnets: subnets}
	return goarch, nil
}

func startBurst(ctx context.Context, logger *log.Logger, f *fleet, src *queue.Local, provider string, asked int, yes bool) (end func()) {
	p, err := pool.Open()
	if err != nil {
		fatalf("%v", err)
	}
	cfg, err := loadCloudConfig(p)
	if err != nil {
		fatalf("%v", err)
	}
	poolID, err := p.ID()
	if err != nil {
		fatalf("%v", err)
	}
	poolKey, err := p.PublicKey()
	if err != nil {
		fatalf("%v", err)
	}
	b := &burst{pool: p, host: p, cfg: cfg, provider: provider, poolKey: poolKey, poolID: poolID,
		log: logger, connect: f.connect, admit: f.srv.AddWorker, drain: f.srv.Drain, abandon: f.srv.Abandon}
	// Checked before anything is made: those cost nothing.
	var hourly float64
	var goarch, where string
	switch provider {
	case "hetzner":
		hz := newHetzner()
		b.prov, b.slots, where = hz, cfg.Hetzner.Slots, cfg.Hetzner.Type+" in "+cfg.Hetzner.Location
		b.spec = cloud.Spec{Type: cfg.Hetzner.Type, Location: cfg.Hetzner.Location, Image: cfg.Hetzner.Image}
		if hourly, goarch, err = hz.ServerType(ctx, cfg.Hetzner.Type, cfg.Hetzner.Location); err != nil {
			fatalf("%v", err)
		}
	case "aws":
		if cfg.AWS.Region == "" || cfg.AWS.Hourly <= 0 {
			fatalf("pool.yml: aws needs region and hourly, the price per hour in euros with its public IPv4 address and disk")
		}
		a, err := cloud.NewAWS(ctx, cfg.AWS.Region)
		if err != nil {
			fatalf("%v", err)
		}
		if goarch, err = b.useAWS(ctx, a); err != nil {
			fatalf("%v", err)
		}
		hourly, where = cfg.AWS.Hourly, cfg.AWS.Type+" in "+cfg.AWS.Region
	}
	if left, err := b.prov.List(ctx, serverSelector(poolID)); err != nil || len(left) > 0 {
		fatalf("checking for servers an earlier burst left: %d found, %v; builder worker gc destroys them", len(left), err)
	}
	runs := src.Counts()["eligible"]
	n, cost := burstPlan(cfg, b.slots, asked, 0, runs, hourly)
	if n == 0 {
		logger.Print("no cloud workers needed")
		return func() {}
	}
	life := cfg.MaxAge
	if b.deadline > 0 {
		life = b.deadline
	}
	logger.Printf("burst: %d %s for %d runs, about €%.2f; €%.2f if every one ran as long as it can", n, where, runs, cost,
		float64(n)*math.Ceil(life.Hours())*hourly)
	if cost > cfg.MaxBurstCost && !yes {
		fatalf("that is over max_burst_cost (€%.2f); -yes goes ahead", cfg.MaxBurstCost)
	}
	switch c := b.prov.(type) {
	case *cloud.Hetzner:
		if b.spec.SSHKey, err = c.SSHKey(ctx, "m2test-pool-"+poolID, poolKey); err != nil {
			fatalf("%v", err)
		}
		b.spec.Firewall, err = c.Firewall(ctx, "m2test-"+poolID, cfg.SSHFrom)
	case *cloud.AWS:
		b.spec.Firewall, err = c.Firewall(ctx, "m2test-"+poolID, cfg.SSHFrom)
	}
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
	b.binary = bin
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
	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, _ := loadCloudConfig(p) // read even if some other setting is invalid
	targets, err := gcTargets(p, os.Getenv("HCLOUD_TOKEN") != "", cfg.AWS.Region, logger)
	if err != nil {
		fatalf("%v", err)
	}
	left, failed := 0, false
	for _, t := range targets {
		var prov provider
		var err error
		if t.provider == "aws" {
			var a *cloud.AWS
			if a, err = cloud.NewAWS(ctx, t.region); err == nil {
				prov, t.account = a, a.Account
			}
		} else {
			prov = newHetzner()
		}
		n := 0
		if err == nil {
			n, err = gc(ctx, p, prov, t, poolID, *yes, logger)
		}
		if err != nil {
			logger.Printf("%s %s: %v", t.provider, t.region, err)
			failed = true
		}
		left += n
	}
	if failed {
		os.Exit(1)
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

// gcTargets is every provider and region the inventory has workers in, and any set up here, so gc also finds servers by label.
func gcTargets(p *pool.Pool, hcloud bool, awsRegion string, logger *log.Logger) ([]target, error) {
	seen := map[target]bool{}
	inv, err := p.Load()
	if err != nil {
		logger.Printf("reading the inventory: %v; going by label too", err)
	}
	if inv != nil {
		for _, w := range inv.Workers {
			if w == nil {
				continue
			}
			if c := w.Cloud; c != nil && (c.Provider == "hetzner" && c.Region == "" || c.Provider == "aws" && pool.ValidRegion(c.Region)) {
				seen[target{provider: c.Provider, region: c.Region}] = true
			}
		}
	}
	if hcloud {
		seen[target{provider: "hetzner"}] = true
	}
	if awsRegion != "" {
		seen[target{provider: "aws", region: awsRegion}] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no provider to check: set HCLOUD_TOKEN, or aws.region in pool.yml")
	}
	return slices.SortedFunc(maps.Keys(seen), func(a, b target) int {
		return strings.Compare(a.provider+" "+a.region, b.provider+" "+b.region)
	}), nil
}

// target is one provider's servers gc checks: a region and account on AWS.
type target struct{ provider, region, account string }

func (b *burst) target() target { return target{b.provider, b.region, b.account} }

func gc(ctx context.Context, p *pool.Pool, prov provider, t target, poolID string, yes bool, logger *log.Logger) (left int, err error) {
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
	doomed := map[string]string{}
	for _, s := range servers {
		doomed[s.ID] = s.Name
	}
	for _, name := range cloudEntries(inv) {
		c := inv.Workers[name].Cloud
		if c.Provider != t.provider || c.Region != t.region {
			continue
		}
		// Another account's IDs mean nothing here: not found isn't gone.
		// Listed by label, though, it is in this one.
		if _, listed := doomed[string(c.ID)]; c.Account != t.account && !listed {
			logger.Printf("%s: %s; left alone. If it is in %s, set its cloud.account to that in workers.json, or remove the entry", name, inAccount(c.Account), t.account)
			left++
			continue
		}
		if c.ID == "" {
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
		srv, err := prov.Get(ctx, string(c.ID))
		switch {
		// EC2 may not show an instance yet for a while after its create.
		case errors.Is(err, cloud.ErrNotFound) && errors.As(err, new(*cloud.APIError)) && t.provider == "aws" && time.Since(c.Created) < unseenFor:
			logger.Printf("%s: instance %s not visible yet, %s after its create; kept", name, c.ID, time.Since(c.Created).Round(time.Second))
			left++
		case errors.Is(err, cloud.ErrNotFound):
			if yes && p.Update(func(inv *pool.Inventory) error { delete(inv.Workers, name); return nil }) == nil {
				logger.Printf("%s: server %s is already gone", name, c.ID)
				continue
			}
			left++
		case err != nil:
			logger.Printf("%s: %v", name, err)
			left++
		case srv.Labels[cloud.Label] != "worker" || srv.Labels[cloud.Label+"-pool"] != poolID:
			logger.Printf("%s: server %s isn't this pool's (labels %v); left alone", name, c.ID, srv.Labels)
			// Relabelled since List put it in doomed.
			delete(doomed, string(c.ID))
			left++
		default:
			doomed[string(c.ID)] = name
		}
	}
	b := &burst{pool: p, prov: prov, log: logger, provider: t.provider, region: t.region, account: t.account}
	for _, id := range slices.Sorted(maps.Keys(doomed)) {
		name := doomed[id]
		if !yes {
			logger.Printf("%s: server %s would be destroyed", name, id)
			left++
			continue
		}
		if !b.delete(ctx, name, &pool.Worker{Cloud: &pool.Cloud{ID: pool.ServerID(id), Account: t.account}}) {
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
