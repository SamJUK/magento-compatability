// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package coord lets remote workers pull runs from builder queue: HTTP over a
// Unix socket the coordinator forwards to each worker, with a token per
// worker.
package coord

import (
	"encoding/json"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
)

// Schema changes whenever a request or reply changes shape.
const Schema = 1

// MaxUpload caps a result upload. Container logs are capped per service by
// the runner, so a real result is far smaller.
var MaxUpload int64 = 64 << 20

type HelloRequest struct {
	Schema  int    `json:"schema"`
	Release string `json:"release"`
	Slots   int    `json:"slots"`
}

type HelloReply struct {
	// Stale is set when the worker runs a different release or schema; it
	// gets no work until it is set up again.
	Stale string `json:"stale,omitempty"`
}

type ClaimRequest struct {
	Schema  int    `json:"schema"`
	Request string `json:"request"`
}

type ClaimReply struct {
	State   string             `json:"state"` // job, none, done, hello
	Attempt string             `json:"attempt,omitempty"`
	Combo   matrix.Combination `json:"combo,omitempty"`
	Options queue.Contract     `json:"options,omitempty"`
}

const (
	ClaimJob  = "job"
	ClaimNone = "none"
	ClaimDone = "done"
	// ClaimHello means the coordinator doesn't know the worker (it restarted,
	// say); the worker says hello again.
	ClaimHello = "hello"
)

type HeartbeatRequest struct {
	Schema    int      `json:"schema"`
	Running   []string `json:"running"`
	Unhealthy string   `json:"unhealthy,omitempty"`
}

type HeartbeatReply struct {
	Abort []string `json:"abort,omitempty"`
}

type Upload struct {
	Schema  int    `json:"schema"`
	Attempt string `json:"attempt"`
	Outcome string `json:"outcome"` // result, host_infrastructure, aborted
	Reason  string `json:"reason,omitempty"`
	// Requested marks an abort someone asked for, which costs nothing.
	Requested bool            `json:"requested,omitempty"`
	SHA256    string          `json:"sha256,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

const (
	OutcomeResult  = "result"
	OutcomeHost    = "host_infrastructure"
	OutcomeAborted = "aborted"
)

type UploadReply struct {
	// Status is installed, accepted (counted, nothing to install), rejected
	// or stale. Every one lets the worker drop the upload from its spool.
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Lease timings. Var so tests can shorten them.
var (
	LeaseFor       = 2 * time.Minute
	RestartGrace   = 10 * time.Minute
	HeartbeatEvery = 20 * time.Second
	ClaimWait      = 30 * time.Second
	claimPoll      = time.Second
	expireEvery    = time.Second
	retryEvery     = 5 * time.Second
)
