// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Attempt is one remote worker's claim on a combination; runs in this process's own slots aren't recorded.
type Attempt struct {
	ID      string    `json:"id"`
	Sweep   string    `json:"sweep"`
	Combo   string    `json:"combo"`
	Lease   string    `json:"lease"`
	Worker  string    `json:"worker"`
	Request string    `json:"request,omitempty"`
	State   string    `json:"state"`
	Reason  string    `json:"reason,omitempty"`
	Claimed time.Time `json:"claimed"`
	Hash    string    `json:"hash,omitempty"`
	// Released: counted and its claim let go; nothing more changes it.
	Released bool `json:"released,omitempty"`
}

const (
	AttemptRunning    = "running"
	AttemptInstalling = "installing"
	AttemptInstalled  = "installed"
	AttemptFinished   = "finished" // ended with no result to install
	AttemptHost       = "host_infrastructure"
	AttemptAborted    = "aborted"
	// AttemptOverridden: builder test -ignore-queue took the combination.
	AttemptOverridden = "local_override"
)

var ErrFenced = errors.New("queue: attempt is no longer current")

func (a *Attempt) live() bool {
	return a.State == AttemptRunning || a.State == AttemptInstalling
}

func attemptState(o Outcome) string {
	switch o.Kind {
	case OutcomeHost:
		return AttemptHost
	case OutcomeAborted:
		return AttemptAborted
	}
	return AttemptFinished
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// takeLease gives holder the lease. A live attempt still on it is a dead
// queue's: fenced, so its upload can't overwrite the new holder's result
// once that queue resumes.
func takeLease(d *ledgerData, key, holder string) {
	for _, a := range d.Attempts {
		if a.Lease == key && a.live() && !a.Released {
			a.State = AttemptOverridden
		}
	}
	d.Leases[key] = lease{Holder: holder, At: time.Now().UTC()}
}

// Override fences every live remote attempt at id in resultsDir for a builder test -ignore-queue run.
// The caller holds the lease until it calls Unlease.
func (l *Ledger) Override(resultsDir, id string) error {
	key := leaseKey(resultsDir, id)
	return l.update(func(d *ledgerData) error {
		// Held so the queue can't hand it out again while builder test runs.
		takeLease(d, key, l.holder)
		return nil
	})
}
