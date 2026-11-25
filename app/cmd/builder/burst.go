// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"cmp"
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/cloud"
	"github.com/samjuk/magento-compatability/internal/pool"
	"github.com/samjuk/magento-compatability/internal/queue"
)

type cloudConfig struct {
	Hetzner struct {
		Type     list   `yaml:"type"`
		Location list   `yaml:"location"`
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

// list is one scalar or a sequence in YAML.
type list []string

func (l *list) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = list{n.Value}
		return nil
	}
	return n.Decode((*[]string)(l))
}

var hzName = regexp.MustCompile(`^[a-z]+[0-9]*$`)

func loadCloudConfig(p *pool.Pool) (cloudConfig, error) {
	var c cloudConfig
	c.Hetzner.Type, c.Hetzner.Location, c.Hetzner.Image, c.Hetzner.Slots = list{"ccx33"}, list{"hel1"}, "debian-13", 5
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
	case len(c.Hetzner.Location) == 0 || slices.ContainsFunc(c.Hetzner.Location, func(l string) bool { return !hzName.MatchString(l) }):
		return c, fmt.Errorf("%s: hetzner location wants a location name such as fsn1, or a list of them", path)
	case len(c.Hetzner.Type) == 0 || slices.ContainsFunc(c.Hetzner.Type, func(t string) bool { return !hzName.MatchString(t) }):
		return c, fmt.Errorf("%s: hetzner type wants a server type such as cx33, or a list of them", path)
	case len(slices.Compact(slices.Sorted(slices.Values(c.Hetzner.Type)))) < len(c.Hetzner.Type) || len(slices.Compact(slices.Sorted(slices.Values(c.Hetzner.Location)))) < len(c.Hetzner.Location):
		return c, fmt.Errorf("%s: hetzner type and location each name something once", path)
	case len(c.SSHFrom) == 0:
		return c, fmt.Errorf("%s: ssh_from is empty", path)
	case slices.ContainsFunc(c.SSHFrom, func(s string) bool { p, err := netip.ParsePrefix(s); return err != nil || p != p.Masked() }):
		return c, fmt.Errorf("%s: ssh_from wants CIDRs such as 198.51.100.4/32", path)
	case !slices.ContainsFunc(c.SSHFrom, func(s string) bool { return netip.MustParsePrefix(s).Addr().Is4() }):
		return c, fmt.Errorf("%s: ssh_from has no IPv4 CIDR; workers only get an IPv4 address", path)
	case slices.ContainsFunc(c.SSHFrom, func(s string) bool { return netip.MustParsePrefix(s).Bits() < 8 }):
		return c, fmt.Errorf("%s: ssh_from opens SSH to a big share of the internet; name the coordinator's address", path)
	case c.Hetzner.Slots < 1 || c.Hetzner.Slots > pool.MaxSlots || c.AWS.Slots < 1 || c.AWS.Slots > pool.MaxSlots:
		return c, fmt.Errorf("%s: slots must be 1 to %d", path, pool.MaxSlots)
	case c.AWS.Region != "" && !pool.ValidRegion(c.AWS.Region):
		return c, fmt.Errorf("%s: aws region %q isn't a region name", path, c.AWS.Region)
	case c.AWS.DiskGB < 30:
		return c, fmt.Errorf("%s: aws disk_gb must be at least 30", path)
	case slices.ContainsFunc([]float64{c.AWS.Hourly, c.MaxBurstCost, c.MinutesPerRun, c.SetupMinutes}, func(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) }):
		return c, fmt.Errorf("%s: a cost or time is not a number", path)
	case c.MaxWorkers < 1 || c.MaxBurstCost <= 0 || c.MaxAge <= 0 || c.MinutesPerRun <= 0 || c.SetupMinutes < 0:
		return c, fmt.Errorf("%s: max_workers, max_burst_cost, max_age and minutes_per_run must be positive", path)
	case c.AWS.Hourly > 100 || c.MaxBurstCost > 100_000 || c.MinutesPerRun > 1440 || c.SetupMinutes > 1440:
		return c, fmt.Errorf("%s: aws hourly over €100, max_burst_cost over €100,000, or minutes_per_run or setup_minutes over a day is surely a typo", path)
	}
	return c, nil
}

func parseCloud(s string) (provider string, count int, err error) {
	provider, n, ok := strings.Cut(s, ":")
	count, err = strconv.Atoi(n)
	if !ok || provider != "hetzner" && provider != "aws" || err != nil || count < 1 {
		return "", 0, fmt.Errorf("-cloud %q: want hetzner:N or aws:N", s)
	}
	return provider, count, nil
}

// burstPlan caps a burst at what the runs can use and max_workers allows, and the server-hours it bills.
func burstPlan(c cloudConfig, slots, asked, runs int) (n int, hours int64) {
	n = min(asked, c.MaxWorkers, int(math.Ceil(float64(runs)/float64(slots))))
	if n <= 0 {
		return 0, 0
	}
	minutes := float64(runs)*c.MinutesPerRun/float64(n*slots) + c.SetupMinutes
	return n, int64(n) * int64(math.Ceil(minutes/60))
}

type provider interface {
	Create(ctx context.Context, s cloud.Spec) (cloud.Server, error)
	Get(ctx context.Context, id string) (cloud.Server, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, selector string) ([]cloud.Server, error)
	DeletePool(ctx context.Context, firewall, key string) error
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
	owner    string // AWS account, or Hetzner pool key ID
	slots    int
	spec     cloud.Spec // what every create asks for, less its own parts
	types    []string   // hetzner: tried in turn while no location has room
	hourly   float64    // euros, the dearest server's price
	// deadline, if set, is how long after its create a server powers
	// itself off; on EC2 that terminates it.
	deadline time.Duration
	poolKey  string
	poolID   string
	binary   string
	log      *log.Logger
	n        int // servers the plan makes
	// budget is max_burst_cost in cents, or no limit once -yes went past it.
	budget int64
	// capped is why the budget tripped: it stays tripped for the sweep.
	capped     string
	connect    func(ctx context.Context, name string, w *pool.Worker)
	admit      func(token, name string)
	drain      func(name string, always bool) bool
	abandon    func(name string)
	sick       func(name string) time.Duration
	undeployed func(name string) time.Duration
	// fail ends the sweep once nothing is left to run it.
	fail     context.CancelCauseFunc
	capacity *capacity

	mu       sync.Mutex
	workers  map[string]*burstWorker
	pending  int // creates not yet returned
	bills    []*bill
	creating sync.WaitGroup
	running  sync.WaitGroup

	// undeleted are servers a delete failed on; trim retries them.
	undeleted map[string]*pool.Worker
}

// bill is a create billed by the started hour until its server's delete
// is confirmed, or it is known to have made none.
type bill struct {
	id            pool.ServerID // once known
	created, gone time.Time
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
	// sickFor is how long after its deploy a worker may go without saying
	// it is healthy before it is destroyed rather than billed for.
	sickFor = 10 * time.Minute
	// deployFor is how long a worker may take to deploy the release.
	deployFor = 90 * time.Minute
)

// capacity is what can still run the sweep.
type capacity struct {
	local bool // local slots, or named workers on this host
	named atomic.Int32
	burst *burst
}

func (c *capacity) left() bool {
	return c.local || c.named.Load() > 0 || c.burst != nil && c.burst.alive()
}

func (b *burst) alive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A create adds its worker before it counts as returned.
	return b.workers == nil || b.pending > 0 || len(b.workers) > 0 // nil: not started yet
}

func (b *burst) start(ctx context.Context, n int) {
	b.mu.Lock()
	b.workers, b.pending = map[string]*burstWorker{}, n
	b.mu.Unlock()
	for range n {
		b.creating.Go(func() {
			defer func() {
				b.mu.Lock()
				b.pending--
				b.mu.Unlock()
			}()
			if err := b.create(ctx); err != nil {
				b.log.Printf("cloud worker: %v", err)
			}
		})
	}
}

func (b *burst) create(ctx context.Context) error {
	raw := make([]byte, 6)
	rand.Read(raw)
	nonce := hex.EncodeToString(raw)
	name := map[string]string{"hetzner": "hz-", "aws": "aws-"}[b.provider] + nonce
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
		Slots: b.slots, Cloud: &pool.Cloud{Provider: b.provider, Region: b.region, Owner: b.owner, Nonce: nonce, Created: created}}
	spec := b.spec
	spec.Name, spec.UserData = name, userData
	spec.Labels = map[string]string{cloud.Label: "worker", cloud.Label + "-pool": b.poolID, cloud.Label + "-nonce": nonce}
	pl, err := b.place(ctx, name, w, spec)
	if err != nil {
		return err
	}
	srv, spec := pl.srv, pl.spec
	if srv.ID == "" {
		if srv, err = b.resolve(ctx, name, nonce, pl.err); err != nil {
			return err
		}
		b.mu.Lock()
		pl.bill.id = pool.ServerID(srv.ID)
		b.mu.Unlock()
	}
	w.Host, w.Cloud.ID = srv.IP, pool.ServerID(srv.ID)
	// One whose actions (the firewall among them) failed or can't be
	// checked isn't used.
	if err := errors.Join(pl.err, b.record(name, w)); err != nil {
		b.delete(context.WithoutCancel(ctx), name, w)
		return fmt.Errorf("%s: server %s: %v", name, srv.ID, err)
	}
	where := ""
	if srv.Where != "" {
		where = " in " + srv.Where
	}
	b.log.Printf("%s: server %s (%s) at %s%s; waiting for cloud-init", name, srv.ID, spec.Type, srv.IP, where)

	bw := &burstWorker{w: w, stopped: make(chan struct{})}
	wctx, stop := context.WithCancel(ctx)
	bw.stop = stop
	b.mu.Lock()
	b.workers[name] = bw
	b.mu.Unlock()
	b.running.Go(func() { b.serve(ctx, wctx, name, bw) })
	return nil
}

// serve provisions a worker, admits it and holds its tunnel until wctx ends,
// destroying it if either gives up first. ctx outlives wctx for the delete.
func (b *burst) serve(ctx, wctx context.Context, name string, bw *burstWorker) {
	defer close(bw.stopped)
	err := b.provision(wctx, name, bw.w)
	// One lock, so trim sees it ready as soon as it can claim.
	b.mu.Lock()
	if err == nil && b.capped != "" {
		err = errors.New(b.capped)
	}
	if err == nil {
		b.admit(bw.w.Token, name)
		bw.ready = true
	}
	b.mu.Unlock()
	if err != nil {
		if wctx.Err() == nil && b.take(name) != nil {
			b.log.Printf("%s: %v; destroying it", name, err)
			b.delete(context.WithoutCancel(ctx), name, bw.w)
		}
		return
	}
	b.connect(wctx, name, bw.w)
	// connect only ends early when the tunnel gave up.
	if wctx.Err() == nil && b.take(name) != nil {
		b.log.Printf("%s: no tunnel; destroying it", name)
		b.delete(context.WithoutCancel(ctx), name, bw.w)
	}
}

type placement struct {
	srv  cloud.Server
	spec cloud.Spec
	err  error // Create's
	bill *bill
}

// place creates the server, trying the next location or type while there
// is no room and max_burst_cost allows another try.
func (b *burst) place(ctx context.Context, name string, w *pool.Worker, spec cloud.Spec) (placement, error) {
	for try := 0; ; try++ {
		if try > 0 {
			if why := b.anotherTry(); why != "" {
				b.forget(name)
				return placement{}, fmt.Errorf("%s: no room, and %s", name, why)
			}
		}
		// Recorded first, so a server whose create answer is lost is still found.
		w.Cloud.Created = time.Now().UTC()
		if err := b.record(name, w); err != nil {
			return placement{}, err
		}
		if ctx.Err() != nil {
			b.forget(name)
			return placement{}, ctx.Err()
		}
		// Billed from the ask: an unanswered create may still have made one.
		bl := &bill{created: w.Cloud.Created}
		b.mu.Lock()
		b.bills = append(b.bills, bl)
		b.mu.Unlock()
		srv, err := b.prov.Create(ctx, spec)
		b.mu.Lock()
		if srv.ID != "" {
			bl.id = pool.ServerID(srv.ID)
		} else if cloud.NoRoom(err) || cloud.Refused(err) {
			b.bills = slices.DeleteFunc(b.bills, func(x *bill) bool { return x == bl })
		}
		b.mu.Unlock()
		if !cloud.NoRoom(err) {
			return placement{srv, spec, err, bl}, nil
		}
		// The next location if one was made and then found full, else the
		// next type in every location.
		next := spec
		if rest := after(spec.Locations, srv.Where); len(rest) > 0 { // Where only once made
			next.Locations = rest
		} else if i := slices.Index(b.types, spec.Type); i >= 0 && i+1 < len(b.types) {
			next.Type, next.Locations = b.types[i+1], b.spec.Locations
		} else {
			return placement{srv, spec, err, bl}, nil
		}
		full := strings.Join(spec.Locations, " or ")
		if srv.ID != "" {
			full = srv.Where + " (server " + srv.ID + ")"
		}
		b.log.Printf("%s: no room for %s in %s; next is %s in %s", name, spec.Type, full, next.Type, strings.Join(next.Locations, " or "))
		spec = next
		if srv.ID == "" {
			continue
		}
		// Made, then found to have no room after all: gone before the next.
		w.Host, w.Cloud.ID = srv.IP, pool.ServerID(srv.ID)
		if rerr := b.record(name, w); rerr != nil {
			b.log.Printf("%s: recording server %s: %v; gc finds it by its nonce", name, srv.ID, rerr)
		}
		if !b.delete(context.WithoutCancel(ctx), name, w) {
			return placement{}, fmt.Errorf("%s: server %s: %v", name, srv.ID, err)
		}
		w.Host, w.Cloud.ID = "", ""
	}
}

// resolve finds the server a create that went unanswered may have made.
func (b *burst) resolve(ctx context.Context, name, nonce string, createErr error) (cloud.Server, error) {
	if cloud.Refused(createErr) {
		b.forget(name)
		return cloud.Server{}, fmt.Errorf("%s: create: %v", name, createErr)
	}
	// It may exist, and take a moment to show.
	found, err := b.findByNonce(context.WithoutCancel(ctx), nonce)
	if err != nil || len(found) != 1 {
		return cloud.Server{}, fmt.Errorf("%s: create: %v; can't tell whether it exists (%d found, %v), so it stays in the inventory for builder worker gc", name, createErr, len(found), err)
	}
	return found[0], nil
}

func (b *burst) record(name string, w *pool.Worker) error {
	return b.pool.Update(func(inv *pool.Inventory) error { inv.Workers[name] = w; return nil })
}

const findTries = 6

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
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ctx.Err()
			}
			// The server goes with its log, so keep the end of it.
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			tail, _ := b.host.Script(tctx, name, "tail -n 40 /var/log/cloud-init-output.log\n")
			cancel()
			if len(tail) > 0 {
				b.log.Printf("%s: end of cloud-init-output.log: %s", name, pool.LastLines(tail, 40))
			}
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

func (b *burst) destroy(ctx context.Context, name string) {
	if bw := b.take(name); bw != nil {
		bw.stop()
		<-bw.stopped
		b.delete(ctx, name, bw.w)
	}
}

// ownedBy describes an entry's account: on Hetzner, the ID of the pool
// key in its project, as IDs are unique across projects.
func ownedBy(a string) string {
	if a == "" {
		return "has no account recorded"
	}
	return "is in account " + a
}

// delete forgets a worker once its server is confirmed gone; otherwise it stays for gc.
func (b *burst) delete(ctx context.Context, name string, w *pool.Worker) bool {
	if w.Cloud.Owner != b.owner {
		b.log.Printf("%s: server %s %s, not %s; not destroying it", name, w.Cloud.ID, ownedBy(w.Cloud.Owner), b.owner)
		return false
	}
	var err error
	for until := time.Now().Add(deleteWait); ; {
		if err = b.prov.Delete(ctx, string(w.Cloud.ID)); err == nil {
			_, err = b.prov.Get(ctx, string(w.Cloud.ID))
			if errors.Is(err, cloud.ErrNotFound) {
				b.log.Printf("%s: destroyed", name)
				b.mu.Lock()
				for _, s := range b.bills {
					if s.id == w.Cloud.ID && s.gone.IsZero() {
						s.gone = time.Now()
					}
				}
				delete(b.undeleted, name)
				b.mu.Unlock()
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
	b.log.Printf("%s: destroying server %s: %v; trim and builder worker gc retry", name, w.Cloud.ID, err)
	b.mu.Lock()
	if b.undeleted == nil {
		b.undeleted = map[string]*pool.Worker{}
	}
	b.undeleted[name] = w
	b.mu.Unlock()
	return false
}

// cents is what that many server-hours cost, rounded half up once, in
// whole numbers: prices have at most four decimals.
func (b *burst) cents(hours int64) int64 {
	micro := int64(math.Round(b.hourly * 1e6))
	if micro > 0 && hours > (math.MaxInt64-5000)/micro {
		return math.MaxInt64 // past any budget
	}
	return (hours*micro + 5000) / 10000
}

func euros(cents int64) string { return fmt.Sprintf("€%d.%02d", cents/100, cents%100) }

// budgetMargin is how close to its next billed hour a worker is retired, when that hour would pass the budget.
const budgetMargin = 5 * time.Minute

// billed is the server-hours billed so far, and those starting within
// budgetMargin. b.mu held.
func (b *burst) billed() (hours, next int64) {
	now := time.Now()
	for _, s := range b.bills {
		end := s.gone
		if end.IsZero() {
			end = now
			if now.Sub(s.created)%time.Hour > time.Hour-budgetMargin {
				next++
			}
		}
		hours += max(1, int64(math.Ceil(end.Sub(s.created).Hours())))
	}
	return hours, next
}

// overBudget says why, if the next billed hours would pass the budget. b.mu held.
func (b *burst) overBudget() string {
	hours, next := b.billed()
	if b.cents(hours+next) <= b.budget {
		return ""
	}
	return fmt.Sprintf("max_burst_cost (%s) reached, about %s billed", euros(b.budget), euros(b.cents(hours)))
}

// unwell says why, if a worker has gone too long without deploying or being healthy.
func (b *burst) unwell(name string) string {
	if d := b.undeployed(name); d > deployFor {
		return fmt.Sprintf("release not deployed after %s", d.Round(time.Second))
	}
	if d := b.sick(name); d > sickFor {
		return fmt.Sprintf("not healthy for %s", d.Round(time.Second))
	}
	return ""
}

// anotherTry says why not, if the sweep is capped or another server-hour
// would pass the budget. Workers asking at once can each pass it by up to
// two hours.
func (b *burst) anotherTry() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if billed, _ := b.billed(); b.capped == "" && b.cents(billed+1) > b.budget {
		return "another try could pass max_burst_cost"
	}
	return b.capped
}

// hetznerPrice is the dearest type and location, as a create may get any of them.
// x86 only: db-mysql.yml pins linux/amd64.
func hetznerPrice(ctx context.Context, hz *cloud.Hetzner, types, locations []string) (hourly float64, goarch string, err error) {
	for _, t := range types {
		p, arch, err := hz.ServerTypeIn(ctx, t, locations)
		if err != nil {
			return 0, "", err
		}
		if err := requireX86("hetzner", t, arch); err != nil {
			return 0, "", err
		}
		hourly = max(hourly, p)
	}
	return hourly, "amd64", nil
}

func requireX86(provider, serverType, arch string) error {
	if arch != "amd64" {
		return fmt.Errorf("%s: %s is %s; workers need x86, as some stacks pin linux/amd64 images", provider, serverType, arch)
	}
	return nil
}

func after(locations []string, l string) []string {
	for i, x := range locations {
		if x == l {
			return locations[i+1:]
		}
	}
	return nil
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

// trim destroys workers the sweep can do without, keeping one idle until the sweep is done for late retries.
// With nothing left to run it, it ends the sweep.
func (b *burst) trim(ctx context.Context, waiting int, finished func() bool) {
	done := finished()
	b.mu.Lock()
	if b.capped == "" {
		b.capped = b.overBudget()
	}
	over := b.capped
	var healthy []string
	retire, unwell := map[string]string{}, map[string]string{}
	overdue := map[string]bool{}
	for name, bw := range b.workers {
		if !bw.ready {
			continue
		}
		// As long as EC2 gives a run before it powers off: a run with no
		// deadline may hang for good, and Hetzner has no power-off.
		overdue[name] = time.Since(bw.w.Cloud.Created) > b.cfg.MaxAge+deadlineGrace
		switch why := b.unwell(name); {
		case over != "":
			retire[name] = over
		case time.Since(bw.w.Cloud.Created) > b.cfg.MaxAge:
			retire[name] = fmt.Sprintf("past max age %s", b.cfg.MaxAge)
		case why != "":
			unwell[name] = why
		default:
			healthy = append(healthy, name)
		}
	}
	b.mu.Unlock()
	var gone []string
	// Fenced while busy; destroyed once idle.
	for name, why := range retire {
		switch {
		case b.drain(name, true):
			b.log.Printf("%s: %s", name, why)
			gone = append(gone, name)
		case overdue[name]:
			b.log.Printf("%s: %s; still busy %s past max age, so stopped mid-run", name, why, deadlineGrace)
			b.abandon(name)
			gone = append(gone, name)
		}
	}
	// Not fenced while busy: it may recover, and can't claim while sick.
	for name, why := range unwell {
		if b.unwell(name) != "" && b.drain(name, false) {
			b.log.Printf("%s: %s", name, why)
			gone = append(gone, name)
		}
	}
	if waiting == 0 {
		keep := 1
		if done {
			keep = 0
		}
		drop := len(healthy) - keep
		for _, name := range healthy {
			if drop > 0 && b.drain(name, false) {
				gone = append(gone, name)
				drop--
			}
		}
	}
	// A delete cut short would leave a server no one tracks but gc. Side by
	// side, so slow deletes don't hold up teardown one after another.
	var wg sync.WaitGroup
	for _, name := range gone {
		wg.Go(func() { b.destroy(context.WithoutCancel(ctx), name) })
	}
	// Still billing: the sweep may run on for hours on local slots.
	b.mu.Lock()
	retry := maps.Clone(b.undeleted)
	b.mu.Unlock()
	for name, w := range retry {
		wg.Go(func() { b.delete(context.WithoutCancel(ctx), name, w) })
	}
	wg.Wait()
	// Asked again: the deletes take a while, and the sweep may have ended.
	if !b.capacity.left() && !finished() {
		b.fail(errors.New("no cloud worker left and nothing else to run on; the queue resumes where it stopped"))
	}
}

func (b *burst) end(ctx context.Context) {
	b.creating.Wait()
	b.mu.Lock()
	all := b.workers
	b.workers = map[string]*burstWorker{}
	b.mu.Unlock()
	if len(all) > 0 {
		b.log.Printf("destroying %d cloud workers; a Ctrl+C now leaves them running for builder worker gc", len(all))
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
	if left, err := gc(ctx, b.pool, b.prov, b.target(), b.poolID, true, b.log); err != nil {
		b.log.Printf("cloud workers may be left (%v); run builder worker gc", err)
	} else if left > 0 {
		b.log.Printf("%d cloud workers left; builder worker gc destroys them, or clears an entry whose create was never answered (after 10 minutes on Hetzner, an hour on AWS)", left)
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
	b.prov, b.region, b.owner, b.slots = a, c.Region, a.Account, c.Slots
	// Past max age the coordinator destroys it; this is for when the
	// coordinator is gone, so it leaves a running job time to finish.
	b.deadline = b.cfg.MaxAge + deadlineGrace
	if goarch, err = a.InstanceType(ctx, c.Type); err != nil {
		return "", err
	}
	if err := requireX86("aws", c.Type, goarch); err != nil {
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

// prepareBurst checks and readies what a burst needs before the sweep
// starts, so a failure leaves nothing running. nil: no servers needed.
func prepareBurst(ctx context.Context, logger *log.Logger, src *queue.Local, provider string, asked int, yes bool) (*burst, error) {
	p, err := pool.Open()
	if err != nil {
		return nil, err
	}
	lock, err := p.BurstLock()
	if err != nil {
		return nil, err
	}
	heldLocks = append(heldLocks, lock)
	cfg, err := loadCloudConfig(p)
	if err != nil {
		return nil, err
	}
	poolID, err := p.ID()
	if err != nil {
		return nil, err
	}
	poolKey, err := p.PublicKey()
	if err != nil {
		return nil, err
	}
	b := &burst{pool: p, host: p, cfg: cfg, provider: provider, poolKey: poolKey, poolID: poolID, log: logger}
	// Checked before anything is made: those cost nothing.
	var hourly float64
	var goarch, where string
	switch provider {
	case "hetzner":
		hz, err := newHetzner()
		if err != nil {
			return nil, err
		}
		b.prov, b.slots, b.types = hz, cfg.Hetzner.Slots, cfg.Hetzner.Type
		where = strings.Join(b.types, " or ") + " in " + strings.Join(cfg.Hetzner.Location, " or ")
		b.spec = cloud.Spec{Type: b.types[0], Locations: cfg.Hetzner.Location, Image: cfg.Hetzner.Image}
		if hourly, goarch, err = hetznerPrice(ctx, hz, b.types, cfg.Hetzner.Location); err != nil {
			return nil, err
		}
	case "aws":
		if cfg.AWS.Region == "" || cfg.AWS.Hourly <= 0 {
			return nil, errors.New("pool.yml: aws needs region and hourly, the price per hour in euros with its public IPv4 address and disk")
		}
		a, err := cloud.NewAWS(ctx, cfg.AWS.Region)
		if err != nil {
			return nil, err
		}
		if goarch, err = b.useAWS(ctx, a); err != nil {
			return nil, err
		}
		hourly, where = cfg.AWS.Hourly, cfg.AWS.Type+" in "+cfg.AWS.Region
	}
	if err := b.plan(ctx, asked, src.Counts()["eligible"], hourly, where, yes); err != nil || b.n == 0 {
		return nil, err
	}
	switch c := b.prov.(type) {
	case *cloud.Hetzner:
		if b.spec.SSHKey, err = c.SSHKey(ctx, "m2test-pool-"+poolID, poolKey); err != nil {
			return nil, err
		}
		b.owner = b.spec.SSHKey
		b.spec.Firewall, err = c.Firewall(ctx, "m2test-"+poolID, cfg.SSHFrom)
	case *cloud.AWS:
		b.spec.Firewall, err = c.Firewall(ctx, "m2test-"+poolID, cfg.SSHFrom)
	}
	if err != nil {
		return nil, err
	}
	if err := b.buildWorker(ctx, goarch); err != nil {
		return nil, err
	}
	keepAwake()
	return b, nil
}

func (b *burst) buildWorker(ctx context.Context, goarch string) error {
	tmp, err := os.MkdirTemp("", "m2test-burst")
	if err != nil {
		return err
	}
	b.binary = filepath.Join(tmp, "m2test-worker")
	if err := pool.BuildWorker(ctx, filepath.Join(findRepoRoot(), "app"), goarch, b.binary); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

// keepAwake stops a sleeping laptop dropping every tunnel; macOS only.
func keepAwake() {
	if path, err := exec.LookPath("caffeinate"); err == nil {
		// Not tied to ctx: teardown after Ctrl+C needs the network too.
		exec.Command(path, "-i", "-s", "-w", strconv.Itoa(os.Getpid())).Start()
	}
}

// plan sizes the burst, and refuses one while an earlier burst's servers
// are left or, without yes, one priced over max_burst_cost.
func (b *burst) plan(ctx context.Context, asked, runs int, hourly float64, where string, yes bool) error {
	if left, err := b.prov.List(ctx, serverSelector(b.poolID)); err != nil || len(left) > 0 {
		return fmt.Errorf("checking for servers an earlier burst left: %d found, %v; builder worker gc destroys them", len(left), err)
	}
	n, hours := burstPlan(b.cfg, b.slots, asked, runs)
	b.hourly = hourly
	cost := b.cents(hours)
	if n == 0 {
		b.log.Print("no cloud workers needed")
		return nil
	}
	life := b.cfg.MaxAge
	if b.deadline > 0 {
		life = b.deadline
	}
	b.log.Printf("burst: %d %s for %d runs, about %s; %s if every one ran as long as it can", n, where, runs, euros(cost),
		euros(b.cents(int64(n)*int64(math.Ceil(life.Hours())))))
	b.budget = int64(math.Round(b.cfg.MaxBurstCost * 100))
	if cost > b.budget {
		if !yes {
			return fmt.Errorf("that is over max_burst_cost (%s); -yes goes ahead", euros(b.budget))
		}
		// Past it by choice: max_age is the limit.
		b.budget = math.MaxInt64
	}
	b.n = n
	return nil
}

const (
	trimEvery = 30 * time.Second
	// endWait bounds the teardown that follows a sweep.
	endWait = 10 * time.Minute
)

// run makes the burst's servers for the fleet, and returns what destroys them.
func (b *burst) run(ctx context.Context, f *fleet, src *queue.Local, fail context.CancelCauseFunc) (end func()) {
	b.connect, b.admit, b.drain, b.abandon, b.sick, b.undeployed, b.fail = f.connect, f.srv.AddWorker, f.srv.Drain, f.srv.Abandon, f.srv.Sick, f.srv.Undeployed, fail
	b.start(ctx, b.n)
	trimmed := make(chan struct{})
	go func() {
		defer close(trimmed)
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(trimEvery):
				b.trim(ctx, src.Counts()["queued"], src.Done)
			}
		}
	}()
	return func() {
		<-trimmed
		ctx, cancel := context.WithTimeout(context.Background(), endWait)
		defer cancel()
		b.end(ctx)
		os.RemoveAll(filepath.Dir(b.binary))
	}
}

// workerGC destroys what dead bursts left, found by inventory and by label.
func workerGC(ctx context.Context, p *pool.Pool, args []string) error {
	fs := flag.NewFlagSet("worker gc", flag.ExitOnError)
	yes := fs.Bool("yes", false, "Destroy them; without it, only list them")
	purge := fs.Bool("purge", false, "With -yes, once no worker is left, also remove the pool's firewall and SSH key, which bursts otherwise reuse")
	fs.Parse(args)
	if *purge && !*yes {
		return errors.New("-purge needs -yes")
	}
	lock, err := p.BurstLock()
	if err != nil {
		return fmt.Errorf("%v; it destroys its own workers", err)
	}
	defer lock.Close()
	poolID, err := p.ID()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, _ := loadCloudConfig(p) // read even if some other setting is invalid
	targets, err := gcTargets(p, os.Getenv("HCLOUD_TOKEN") != "", cfg.AWS.Region, logger)
	if err != nil {
		return err
	}
	left, err := gcAll(ctx, p, targets, openTarget(poolID), poolID, *yes, *purge, logger)
	if err == nil && left > 0 && *yes {
		err = fmt.Errorf("%d left; run it again", left)
	}
	return err
}

// gcAll runs gc on each target; with purge, it removes a target's
// firewall and key once no worker is left there.
func gcAll(ctx context.Context, p *pool.Pool, targets []target, open func(context.Context, target) (provider, target, error), poolID string, yes, purge bool, logger *log.Logger) (left int, err error) {
	failed := 0
	for _, t := range targets {
		prov, t, err := open(ctx, t)
		n := 0
		if err == nil {
			n, err = gc(ctx, p, prov, t, poolID, yes, logger)
		}
		if err == nil && n == 0 && purge {
			if err = purgePool(ctx, prov, poolID); err == nil {
				logger.Printf("%s %s: removed the pool's firewall and key", t.provider, t.region)
			}
		}
		if err != nil {
			logger.Printf("%s %s: %v", t.provider, t.region, err)
			failed++
		}
		left += n
	}
	if failed > 0 {
		return left, fmt.Errorf("%d of %d providers not cleaned up", failed, len(targets))
	}
	return left, nil
}

func openTarget(poolID string) func(context.Context, target) (provider, target, error) {
	return func(ctx context.Context, t target) (provider, target, error) {
		if t.provider == "aws" {
			a, err := cloud.NewAWS(ctx, t.region)
			if err != nil {
				return nil, t, err
			}
			t.owner = a.Account
			return a, t, nil
		}
		h, err := newHetzner()
		if err != nil {
			return nil, t, err
		}
		t.owner, err = h.KeyID(ctx, "m2test-pool-"+poolID)
		return h, t, err
	}
}

var purgeWait = 3 * time.Minute

func purgePool(ctx context.Context, prov provider, poolID string) error {
	for until := time.Now().Add(purgeWait); ; {
		err := prov.DeletePool(ctx, "m2test-"+poolID, "m2test-pool-"+poolID)
		if !cloud.InUse(err) || time.Now().After(until) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(provisionPoll / 2):
		}
	}
}

func serverSelector(poolID string) string {
	return cloud.Label + "=worker," + cloud.Label + "-pool=" + poolID
}

func nonceSelector(poolID, nonce string) string {
	return serverSelector(poolID) + "," + cloud.Label + "-nonce=" + nonce
}

// unseenFor is how long a create never answered stays out of the listing before gc takes it as never made.
// EC2 can be slow to list.
var unseenFor = map[string]time.Duration{"hetzner": 10 * time.Minute, "aws": time.Hour}

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

// target is one provider's servers gc checks.
type target struct{ provider, region, owner string }

func (b *burst) target() target { return target{b.provider, b.region, b.owner} }

// gc destroys this pool's servers on t, and says how many workers are left.
func gc(ctx context.Context, p *pool.Pool, prov provider, t target, poolID string, yes bool, logger *log.Logger) (left int, err error) {
	doomed, kept, err := reconcile(ctx, p, prov, t, poolID, yes, logger)
	if err != nil {
		return 0, err
	}
	if !yes {
		for _, id := range slices.Sorted(maps.Keys(doomed)) {
			logger.Printf("%s: server %s would be destroyed", doomed[id], id)
		}
		if kept+len(doomed) > 0 {
			logger.Print("-yes destroys them")
		}
		return kept + len(doomed), nil
	}
	b := &burst{pool: p, prov: prov, log: logger, provider: t.provider, region: t.region, owner: t.owner}
	failed := 0
	for _, id := range slices.Sorted(maps.Keys(doomed)) {
		if !b.delete(ctx, doomed[id], &pool.Worker{Cloud: &pool.Cloud{ID: pool.ServerID(id), Owner: t.owner}}) {
			failed++
		}
	}
	return kept + failed, nil
}

// reconcile matches the inventory to what t lists. doomed maps the ID of
// each server to destroy to its worker's name; kept counts entries left
// alone. With yes, it forgets entries whose server is gone.
func reconcile(ctx context.Context, p *pool.Pool, prov provider, t target, poolID string, yes bool, logger *log.Logger) (doomed map[string]string, kept int, err error) {
	inv, err := p.Load()
	if err != nil {
		// The labels still find every server.
		logger.Printf("reading the inventory: %v; going by label only", err)
		inv = &pool.Inventory{}
	}
	servers, err := prov.List(ctx, serverSelector(poolID))
	if err != nil {
		return nil, 0, err
	}
	drop := func(name string) error {
		return p.Update(func(inv *pool.Inventory) error { delete(inv.Workers, name); return nil })
	}
	doomed = map[string]string{}
	for _, s := range servers {
		doomed[s.ID] = s.Name // ours are named for their worker
	}
	for _, name := range cloudEntries(inv) {
		c := inv.Workers[name].Cloud
		if c.Provider != t.provider || c.Region != t.region {
			continue
		}
		// Another account's IDs mean nothing here: not found isn't gone.
		// Listed by label, though, it is in this one.
		if _, listed := doomed[string(c.ID)]; (c.Owner != t.owner || t.owner == "") && !listed {
			if t.owner == "" {
				logger.Printf("%s: %s; left alone, this project has no pool key. Remove the entry from workers.json once the server is gone", name, ownedBy(c.Owner))
			} else {
				logger.Printf("%s: %s; left alone. If it is in %s, set its cloud.account to that in workers.json, or remove the entry", name, ownedBy(c.Owner), t.owner)
			}
			kept++
			continue
		}
		unseen := cmp.Or(unseenFor[t.provider], time.Hour)
		if c.ID == "" {
			// Its create was never answered. If the server exists, its labels
			// listed it above; one made late may not show yet.
			if found, err := prov.List(ctx, nonceSelector(poolID, c.Nonce)); err != nil || len(found) > 0 || time.Since(c.Created) < unseen {
				if err != nil {
					logger.Printf("%s: %v", name, err)
					kept++
				} else if len(found) == 0 {
					logger.Printf("%s: create never answered, %s ago; kept until it's been out of sight for %s", name, time.Since(c.Created).Round(time.Second), unseen)
					kept++
				}
				for _, s := range found {
					doomed[s.ID] = name
				}
				continue
			}
			if !yes {
				logger.Printf("%s: never created, would be forgotten", name)
				kept++
			} else if err := drop(name); err != nil {
				logger.Printf("%s: %v", name, err)
				kept++
			}
			continue
		}
		// The token may be for another project, where this ID is someone
		// else's server.
		srv, err := prov.Get(ctx, string(c.ID))
		switch {
		// EC2 may not show an instance yet for a while after its create.
		case errors.Is(err, cloud.ErrNotFound) && errors.As(err, new(*cloud.APIError)) && t.provider == "aws" && time.Since(c.Created) < unseen:
			logger.Printf("%s: instance %s not visible yet, %s after its create; kept", name, c.ID, time.Since(c.Created).Round(time.Second))
			kept++
		case errors.Is(err, cloud.ErrNotFound):
			if yes && drop(name) == nil {
				logger.Printf("%s: server %s is already gone", name, c.ID)
				continue
			}
			kept++
		case err != nil:
			logger.Printf("%s: %v", name, err)
			kept++
		case srv.Labels[cloud.Label] != "worker" || srv.Labels[cloud.Label+"-pool"] != poolID:
			logger.Printf("%s: server %s isn't this pool's (labels %v); left alone", name, c.ID, srv.Labels)
			// Relabelled since List put it in doomed.
			delete(doomed, string(c.ID))
			kept++
		default:
			doomed[string(c.ID)] = name
		}
	}
	return doomed, kept, nil
}

// newHetzner reads the same variables as the hcloud CLI.
func newHetzner() (*cloud.Hetzner, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, errors.New("HCLOUD_TOKEN is not set")
	}
	hz := cloud.NewHetzner(token)
	if e := os.Getenv("HCLOUD_ENDPOINT"); e != "" {
		hz.Endpoint = strings.TrimSuffix(e, "/")
	}
	return hz, nil
}
