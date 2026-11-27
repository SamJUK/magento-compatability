// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/samjuk/magento-compatability/internal/pool"
)

const workerUsage = `Usage: magento-compatibility-builder worker <add|setup|remove|list|gc> [flags] [name]

  add NAME -host H -host-key SHA256:...   pin a worker's host key and give it a token
  add NAME                                a worker on this machine, on worker.sock directly
  setup NAME                              install m2test-worker, its token and its unit;
                                          restarts it, stopping any runs it has going
  remove NAME                             stop its unit (and its runs) and forget it
  list
  gc [-yes [-purge]]                      destroy cloud workers dead bursts left behind
                                          (needs HCLOUD_TOKEN or aws.region in pool.yml;
                                          lists them without -yes); -purge also removes
                                          the pool's firewall and SSH key

The inventory and the pool's SSH key, config and known hosts live in
$XDG_CONFIG_HOME/magento-compatibility. A worker host needs Docker, a user
whose only authorised key is the pool key (printed by add), lingering,
its dir owned by that user, and in sshd StreamLocalBindUnlink yes,
AllowStreamLocalForwarding yes or remote, AllowTcpForwarding remote, PermitListen none, ClientAliveInterval 15 and
ClientAliveCountMax 3.
`

// lockWorker keeps a running sweep's worker from being set up or removed
// under it; held until exit.
func lockWorker(p *pool.Pool, name string) {
	lock, err := p.Lock(name)
	if err != nil {
		fatalf("%v", err)
	}
	heldLocks = append(heldLocks, lock)
}

// heldLocks lives as long as the process: a collected lock file unlocks.
var heldLocks []*os.File

func runWorker(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, workerUsage)
		os.Exit(2)
	}
	p, err := pool.Open()
	if err != nil {
		fatalf("%v", err)
	}
	// Longer than setup or remove may wait out a deploy.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	// Before the inventory is read: gc copes with one that won't load.
	if args[0] == "gc" {
		if err := workerGC(ctx, p, args[1:]); err != nil {
			fatalf("%v", err)
		}
		return
	}
	inv, err := p.Load()
	if err != nil {
		fatalf("%v", err)
	}
	switch args[0] {
	case "add":
		workerAdd(ctx, p, inv, args[1:])
	case "setup":
		name, w := namedWorker(inv, args[1:])
		lockWorker(p, name)
		if !w.Remote() || w.Cloud != nil {
			fatalf("%s runs on this machine or belongs to a burst; nothing to set up", name)
		}
		arch, err := p.Arch(ctx, name)
		if err != nil {
			fatalf("%v", err)
		}
		tmp, err := os.MkdirTemp("", "m2test-worker")
		if err != nil {
			fatalf("%v", err)
		}
		defer os.RemoveAll(tmp)
		bin := filepath.Join(tmp, "m2test-worker")
		if err := pool.BuildWorker(ctx, filepath.Join(findRepoRoot(), "app"), arch, bin); err != nil {
			fatalf("%v", err)
		}
		if err := p.Setup(ctx, name, w, bin); err != nil {
			fatalf("%v", err)
		}
		logOK(fmt.Sprintf("%s set up (linux/%s); it starts at the next sweep with -workers %s", name, arch, name))
	case "remove":
		name, w := namedWorker(inv, args[1:])
		lockWorker(p, name)
		if w.Cloud != nil {
			fatalf("%s belongs to a burst; builder worker gc destroys it", name)
		}
		if w.Remote() {
			if err := p.Stop(ctx, name, w); err != nil {
				logWarn(fmt.Sprintf("stopping %s: %v; forgetting it anyway, so its unit and the pool key stay on the host", name, err))
			}
		}
		if err := p.Update(func(inv *pool.Inventory) error { delete(inv.Workers, name); return nil }); err != nil {
			fatalf("%v", err)
		}
		logOK("removed " + name)
	case "list":
		for _, name := range slices.Sorted(maps.Keys(inv.Workers)) {
			w := inv.Workers[name]
			if !w.Remote() {
				fmt.Printf("%s\tlocal\n", name)
				continue
			}
			if w.Cloud != nil {
				fmt.Printf("%s\t%s server %s at %s\tup %s\tleft by a burst; builder worker gc destroys it\n",
					name, w.Cloud.Provider, w.Cloud.ID, w.Host, time.Since(w.Cloud.Created).Round(time.Minute))
				continue
			}
			fp, _ := pool.Fingerprint(w.HostKey)
			fmt.Printf("%s\t%s@%s:%d\t%s\t%d slots\t%s\n", name, w.User, w.Host, w.Port, w.Dir, w.Slots, fp)
		}
	default:
		fmt.Fprint(os.Stderr, workerUsage)
		os.Exit(2)
	}
}

func workerAdd(ctx context.Context, p *pool.Pool, inv *pool.Inventory, args []string) {
	fs := flag.NewFlagSet("worker add", flag.ExitOnError)
	host := fs.String("host", "", "Address to SSH to; leave out for a worker on this machine")
	hostKey := fs.String("host-key", "", "The host's SSH key fingerprint (SHA256:...), read on the host itself")
	port := fs.Int("port", 22, "SSH port")
	user := fs.String("user", "m2test", "User the worker runs as")
	dir := fs.String("dir", pool.DefaultDir, "Worker directory on the host, owned by the user")
	slots := fs.Int("slots", 2, "Concurrent runs")
	name := parseNamed(fs, args)
	if inv.Workers[name] != nil {
		fatalf("%s is already in the inventory; remove it first", name)
	}
	token, err := pool.NewToken()
	if err != nil {
		fatalf("making a token: %v", err)
	}
	w := &pool.Worker{Token: token}
	if *host != "" {
		if !pool.ValidHost(*host) {
			fatalf("bad host %q", *host)
		}
		if *hostKey == "" {
			fatalf("-host-key is required: run ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the host")
		}
		key, err := pool.PinHostKey(ctx, *host, *port, *hostKey)
		if err != nil {
			fatalf("%v", err)
		}
		*w = pool.Worker{Host: *host, Port: *port, User: *user, Dir: *dir, HostKey: key, Token: w.Token, Slots: *slots}
	}
	var pub string
	if w.Host != "" {
		if pub, err = p.PublicKey(); err != nil {
			fatalf("%v", err)
		}
	}
	if err := p.Update(func(inv *pool.Inventory) error {
		if inv.Workers[name] != nil {
			return fmt.Errorf("%s is already in the inventory; remove it first", name)
		}
		inv.Workers[name] = w
		return nil
	}); err != nil {
		fatalf("%v", err)
	}
	if w.Host == "" {
		fmt.Printf("token for %s: %s\n", name, w.Token)
		return
	}
	logOK(fmt.Sprintf("added %s; authorise this key for %s on it, then run worker setup %s:", name, *user, name))
	fmt.Println(pub)
}

func parseNamed(fs *flag.FlagSet, args []string) string {
	name := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" || fs.NArg() > 1 || (fs.NArg() == 1 && fs.Arg(0) != name) {
		fatalf("give one worker name")
	}
	return name
}

func namedWorker(inv *pool.Inventory, args []string) (string, *pool.Worker) {
	if len(args) != 1 {
		fatalf("give one worker name")
	}
	w := inv.Workers[args[0]]
	if w == nil {
		fatalf("no worker %s; worker list shows them", args[0])
	}
	return args[0], w
}
