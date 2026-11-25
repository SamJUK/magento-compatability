// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package queue runs combinations through a fixed set of slots, each waiting
// while the host is short of memory or disk, until the source has nothing left.
package queue

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

type State int

const (
	StateJob  State = iota
	StateNone       // nothing now; a release elsewhere may change that
	StateDone       // the sweep has nothing left and nothing running
)

type Job struct {
	Combination matrix.Combination
	Tries       int // finished runs before this one
}

type OutcomeKind int

const (
	// OutcomeResult: the run finished, with or without writing a result.
	OutcomeResult OutcomeKind = iota
	// OutcomeHost: the host failed the run, not the combination.
	OutcomeHost
	// OutcomeAborted: the run was cut short.
	OutcomeAborted
)

type Outcome struct {
	Kind   OutcomeKind
	Wrote  bool
	Reason string
	// Requested marks an abort the queue asked for (Ctrl+C, cancel), which
	// isn't held against the combination.
	Requested bool
}

// Source hands out jobs. Next and Release may block, and are called with no
// engine lock held. Done reports, without claiming, that the sweep has
// nothing left, so a slot held up by the guards can still finish.
type Source interface {
	Next(ctx context.Context) (Job, State, error)
	Release(Job, Outcome)
	Done() bool
}

type Engine struct {
	Source Source
	// MaxSlots is the number of slot goroutines; Slots says how many of them
	// may run at the moment.
	MaxSlots int
	Slots    func() int
	// Wait returns a reason not to start a run now, or "" when it is fine.
	// It is checked before claiming, so a waiting slot holds nothing.
	Wait func() string
	Run  func(ctx context.Context, slot int, j Job) Outcome
	Log  *log.Logger

	IdlePoll, WaitPoll time.Duration
}

func (e *Engine) Start(ctx context.Context) {
	if e.IdlePoll == 0 {
		e.IdlePoll = 5 * time.Second
	}
	if e.WaitPoll == 0 {
		e.WaitPoll = time.Minute
	}
	var wg sync.WaitGroup
	for slot := 1; slot <= e.MaxSlots; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.slot(ctx, slot)
		}()
	}
	wg.Wait()
}

func (e *Engine) slot(ctx context.Context, slot int) {
	logged := ""
	for ctx.Err() == nil {
		if slot > e.Slots() {
			if e.Source.Done() {
				return
			}
			sleep(ctx, e.IdlePoll)
			continue
		}
		if reason := e.Wait(); reason != "" {
			if reason != logged {
				e.Log.Printf("slot %d: waiting, %s", slot, reason)
				logged = reason
			}
			if e.Source.Done() {
				return
			}
			sleep(ctx, e.WaitPoll)
			continue
		}
		logged = ""

		j, state, err := e.Source.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				e.Log.Printf("slot %d: claiming: %v", slot, err)
			}
			sleep(ctx, e.IdlePoll)
			continue
		}
		switch state {
		case StateDone:
			return
		case StateNone:
			sleep(ctx, e.IdlePoll)
			continue
		}
		e.Source.Release(j, e.Run(ctx, slot, j))
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
