// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Ledger is the queue's shared state on disk: sweeps and the leases that stop two processes running one combination, under an exclusive flock.
type Ledger struct {
	dir    string
	holder string
	lock   *os.File // held for the process's life; marks its leases live
}

type ledgerData struct {
	Sweeps map[string]*sweep `json:"sweeps"`
	Leases map[string]lease  `json:"leases"`
}

type sweep struct {
	Started  time.Time              `json:"started"`
	Contract Contract               `json:"contract"`
	Combos   map[string]*comboState `json:"combos"`
}

// Contract is every option that changes a result. Resuming a sweep with a
// different one would mix results, so it needs -fresh.
type Contract struct {
	Playwright  bool          `json:"playwright"`
	SampleData  bool          `json:"sample_data"`
	MaxLogBytes int64         `json:"max_log_bytes"`
	Deadline    time.Duration `json:"deadline"`
}

type comboState struct {
	Tries        int  `json:"tries,omitempty"`
	Once         bool `json:"once,omitempty"`
	HostFailures int  `json:"host_failures,omitempty"`
}

type lease struct {
	Holder string    `json:"holder"`
	At     time.Time `json:"at"`
}

// OpenLedger opens the ledger in dir and registers this process as a lease holder.
// Its lock file stays locked until exit, which is how a live lease is told from a crashed one's.
func OpenLedger(dir string) (*Ledger, error) {
	holders := filepath.Join(dir, "holders")
	if err := os.MkdirAll(holders, 0o755); err != nil {
		return nil, err
	}
	// Held so no scan removes the new holder between creating and locking it.
	reg, err := lockFile(filepath.Join(dir, "ledger.lock"), syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer reg.Close()
	b := make([]byte, 8)
	rand.Read(b)
	name := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b))
	f, err := os.OpenFile(filepath.Join(holders, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	l := &Ledger{dir: dir, holder: name, lock: f}
	// Exits that skip Close leave their file behind.
	if entries, err := os.ReadDir(holders); err == nil {
		for _, e := range entries {
			if e.Name() != name {
				l.holderAlive(e.Name())
			}
		}
	}
	return l, nil
}

func (l *Ledger) Close() error {
	os.Remove(l.lock.Name())
	return l.lock.Close()
}

func (l *Ledger) Path() string { return filepath.Join(l.dir, "ledger.json") }

func (l *Ledger) update(fn func(*ledgerData) error) error {
	return l.locked(syscall.LOCK_EX, func(d *ledgerData) error {
		if err := fn(d); err != nil {
			return err
		}
		return l.save(d)
	})
}

func (l *Ledger) view(fn func(*ledgerData) error) error {
	return l.locked(syscall.LOCK_SH, fn)
}

func (l *Ledger) locked(how int, fn func(*ledgerData) error) error {
	f, err := lockFile(filepath.Join(l.dir, "ledger.lock"), how)
	if err != nil {
		return err
	}
	defer f.Close()
	d := &ledgerData{}
	data, err := os.ReadFile(l.Path())
	switch {
	case err == nil:
		if err := json.Unmarshal(data, d); err != nil {
			return fmt.Errorf("queue: reading %s: %w", l.Path(), err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if d.Sweeps == nil {
		d.Sweeps = map[string]*sweep{}
	}
	if d.Leases == nil {
		d.Leases = map[string]lease{}
	}
	return fn(d)
}

func lockFile(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (l *Ledger) save(d *ledgerData) error {
	l.dropDeadLeases(d)
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return writeSynced(l.Path(), data)
}

// dropDeadLeases removes leases whose holder has gone; a held flock proves life, so leases need no renewal.
func (l *Ledger) dropDeadLeases(d *ledgerData) {
	alive := map[string]bool{l.holder: true}
	for key, ls := range d.Leases {
		live, seen := alive[ls.Holder]
		if !seen {
			live = l.holderAlive(ls.Holder)
			alive[ls.Holder] = live
		}
		if !live {
			delete(d.Leases, key)
		}
	}
}

func (l *Ledger) holderAlive(name string) bool {
	path := filepath.Join(l.dir, "holders", filepath.Base(name))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		// Only a missing file proves it gone; anything else, play safe.
		return !errors.Is(err, os.ErrNotExist)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	os.Remove(path)
	return false
}

func (l *Ledger) Lease(resultsDir, id string) (bool, error) {
	key := leaseKey(resultsDir, id)
	ok := false
	err := l.update(func(d *ledgerData) error {
		l.dropDeadLeases(d)
		if ls, held := d.Leases[key]; held && ls.Holder != l.holder {
			return nil
		}
		d.Leases[key] = lease{Holder: l.holder, At: time.Now().UTC()}
		ok = true
		return nil
	})
	return ok, err
}

func (l *Ledger) Unlease(resultsDir, id string) error {
	key := leaseKey(resultsDir, id)
	return l.update(func(d *ledgerData) error {
		if d.Leases[key].Holder == l.holder {
			delete(d.Leases, key)
		}
		return nil
	})
}

func leaseKey(resultsDir, id string) string {
	return canonicalDir(resultsDir) + "\x00" + id
}

func canonicalDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// writeSynced replaces path, syncing before the rename so a power cut can't
// leave an empty file that stops the queue starting again.
func writeSynced(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
