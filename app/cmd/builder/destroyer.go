// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/samjuk/magento-compatability/internal/cloud"
	"github.com/samjuk/magento-compatability/internal/pool"
)

// destroyer deletes cloud servers and keeps the inventory to match.
type destroyer struct {
	pool  *pool.Pool
	prov  provider
	owner string // AWS account, or Hetzner pool key ID
	log   *log.Logger
}

type deleted int

const (
	// notOurs: the entry is in another account; nothing was tried.
	notOurs deleted = iota
	// stillThere: the delete failed or the server was still listed after deleteWait.
	stillThere
	// gone: the provider confirmed the server is gone.
	gone
)

// ownedBy describes an entry's account: on Hetzner, the ID of the pool
// key in its project, as IDs are unique across projects.
func ownedBy(a string) string {
	if a == "" {
		return "has no account recorded"
	}
	return "is in account " + a
}

// delete asks the provider to delete w's server until it is confirmed gone,
// the provider refuses, or deleteWait passes.
func (d destroyer) delete(ctx context.Context, name string, w *pool.Worker) deleted {
	if w.Cloud.Owner != d.owner {
		d.log.Printf("%s: server %s %s, not %s; not destroying it", name, w.Cloud.ID, ownedBy(w.Cloud.Owner), d.owner)
		return notOurs
	}
	var err error
	for until := time.Now().Add(deleteWait); ; {
		if err = d.prov.Delete(ctx, string(w.Cloud.ID)); err == nil {
			_, err = d.prov.Get(ctx, string(w.Cloud.ID))
			if errors.Is(err, cloud.ErrNotFound) {
				d.log.Printf("%s: destroyed", name)
				return gone
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
	d.log.Printf("%s: destroying server %s: %v; trim and builder worker gc retry", name, w.Cloud.ID, err)
	return stillThere
}

// remove deletes w's server and, once it is gone, its inventory entry. It
// says whether both happened; otherwise the entry stays for gc.
func (d destroyer) remove(ctx context.Context, name string, w *pool.Worker) bool {
	return d.delete(ctx, name, w) == gone && d.unrecord(name) == nil
}

// unrecord removes a burst worker's inventory entry.
func (d destroyer) unrecord(name string) error {
	err := d.pool.Update(func(inv *pool.Inventory) error {
		if w := inv.Workers[name]; w != nil && w.Cloud != nil {
			delete(inv.Workers, name)
		}
		return nil
	})
	if err != nil {
		d.log.Printf("%s: removing it from the inventory: %v", name, err)
	}
	return err
}
