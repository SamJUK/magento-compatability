// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package coord

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/result"
)

type Server struct {
	Src         *queue.Local
	Tokens      map[string]string
	Contract    queue.Contract
	Release     string
	Workarounds map[string]bool
	RejectDir   string // rejected uploads are kept here for a look
	Log         *log.Logger

	bg        sync.WaitGroup
	stopping  context.Context
	mu        sync.Mutex
	live      map[string]*liveAttempt
	ready     map[string]bool   // said hello with the sweep's release
	unhealthy map[string]string // worker to reason
	settling  map[string]int    // taken out of live by uploads still being answered
}

type liveAttempt struct {
	job     queue.Job
	expires time.Time
}

func (s *Server) Start(ctx context.Context) error {
	s.live = map[string]*liveAttempt{}
	s.ready = map[string]bool{}
	s.unhealthy = map[string]string{}
	s.settling = map[string]int{}
	s.stopping = ctx
	jobs, err := s.Src.Resume()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		s.live[j.Attempt] = &liveAttempt{job: j, expires: time.Now().Add(RestartGrace)}
	}
	s.bg.Go(func() { s.expire(ctx) })
	return nil
}

// Wait returns once expiry has stopped.
func (s *Server) Wait() { s.bg.Wait() }

// Live is the number of attempts remote workers hold.
func (s *Server) Live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

func (s *Server) expire(ctx context.Context) {
	tick := time.NewTicker(expireEvery)
	defer tick.Stop()
	lastWall, lastMono := time.Now().Round(0), time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		// The monotonic clock stops while a Mac sleeps; the wall clock
		// doesn't. A gap between them means a nap nobody's heartbeats could
		// get through, so nobody's lease runs out for it.
		woke := now.Round(0).Sub(lastWall)-now.Sub(lastMono) > time.Minute
		lastWall, lastMono = now.Round(0), now

		var expired []queue.Job
		s.mu.Lock()
		for id, a := range s.live {
			switch {
			case woke:
				a.expires = now.Add(RestartGrace)
			case now.After(a.expires):
				expired = append(expired, a.job)
				delete(s.live, id)
			}
		}
		s.mu.Unlock()
		for _, j := range expired {
			s.Log.Printf("%s: %s on %s expired", j.Attempt, j.Combination.ID(), j.Worker)
			s.release(j, queue.Outcome{Kind: queue.OutcomeHost, Reason: "expired"})
		}
	}
}

// CheckSocketPath refuses a path too long for macOS's sun_path, less its NUL.
func CheckSocketPath(path string) error {
	if len(path) > 103 {
		return fmt.Errorf("%s is %d bytes, over the 103 a socket path may have; use a shorter XDG_STATE_HOME", path, len(path))
	}
	return nil
}

func (s *Server) Serve(ctx context.Context, path string) error {
	if err := CheckSocketPath(path); err != nil {
		return err
	}
	os.Remove(path) // the caller holds the queue lock, so any socket here is stale
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		// Long enough for an upload to finish installing.
		shut, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		srv.Shutdown(shut)
	}()
	err = srv.Serve(ln)
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as Shutdown starts; handlers may still be running.
	<-shutdown
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hello", s.authed(s.hello))
	mux.HandleFunc("POST /claim", s.authed(s.claim))
	mux.HandleFunc("POST /heartbeat", s.authed(s.heartbeat))
	mux.HandleFunc("PUT /result/{attempt}", s.authed(s.upload))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, worker string)

func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		worker := ""
		for token, name := range s.Tokens {
			if ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
				worker = name
			}
		}
		if worker == "" {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		h(w, r, worker)
	}
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		code := http.StatusBadRequest
		if errors.As(err, new(*http.MaxBytesError)) {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), code)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request, worker string) {
	var req HelloRequest
	if !decode(w, r, 1<<16, &req) {
		return
	}
	var rep HelloReply
	switch {
	case req.Schema != Schema:
		rep.Stale = fmt.Sprintf("schema %d, coordinator speaks %d", req.Schema, Schema)
	case req.Release != s.Release:
		rep.Stale = fmt.Sprintf("release %q, sweep runs %s", req.Release, s.Release)
	}
	s.mu.Lock()
	s.ready[worker] = rep.Stale == ""
	s.mu.Unlock()
	if rep.Stale != "" {
		s.Log.Printf("%s is stale: %s", worker, rep.Stale)
	}
	reply(w, rep)
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request, worker string) {
	var req ClaimRequest
	if !decode(w, r, 1<<16, &req) {
		return
	}
	if req.Request == "" {
		http.Error(w, "no request ID", http.StatusBadRequest)
		return
	}
	until := time.Now().Add(ClaimWait)
	for {
		s.mu.Lock()
		ready, healthy := s.ready[worker], s.unhealthy[worker] == ""
		s.mu.Unlock()
		switch {
		case !ready:
			reply(w, ClaimReply{State: ClaimHello})
			return
		case !healthy || s.stopping.Err() != nil:
			reply(w, ClaimReply{State: ClaimNone})
			return
		}
		j, st, err := s.Src.Claim(r.Context(), worker, req.Request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		switch st {
		case queue.StateJob:
			s.mu.Lock()
			s.live[j.Attempt] = &liveAttempt{job: j, expires: time.Now().Add(LeaseFor)}
			s.mu.Unlock()
			s.Log.Printf("%s: %s to %s (try %d)", j.Attempt, j.Combination.ID(), worker, j.Tries+1)
			reply(w, ClaimReply{State: ClaimJob, Attempt: j.Attempt, Combo: j.Combination, Options: s.Contract})
			return
		case queue.StateDone:
			reply(w, ClaimReply{State: ClaimDone})
			return
		}
		if time.Now().After(until) {
			reply(w, ClaimReply{State: ClaimNone})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.stopping.Done():
		case <-time.After(claimPoll):
		}
	}
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, worker string) {
	var req HeartbeatRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	var rep HeartbeatReply
	var overridden []queue.Job
	s.mu.Lock()
	if s.unhealthy[worker] != req.Unhealthy {
		s.Log.Printf("%s: unhealthy %q", worker, req.Unhealthy)
	}
	s.unhealthy[worker] = req.Unhealthy
	for _, id := range req.Running {
		a := s.live[id]
		if a == nil || a.job.Worker != worker {
			rep.Abort = append(rep.Abort, id)
			continue
		}
		a.expires = time.Now().Add(LeaseFor)
	}
	s.mu.Unlock()

	// builder test -ignore-queue fences an attempt in the ledger.
	for _, id := range req.Running {
		if s.Src.AttemptState(id) != queue.AttemptOverridden {
			continue
		}
		s.mu.Lock()
		if a := s.live[id]; a != nil {
			overridden = append(overridden, a.job)
			delete(s.live, id)
			rep.Abort = append(rep.Abort, id)
		}
		s.mu.Unlock()
	}
	for _, j := range overridden {
		s.Log.Printf("%s: %s taken over by builder test", j.Attempt, j.Combination.ID())
		s.release(j, queue.Outcome{Kind: queue.OutcomeAborted, Requested: true})
	}
	reply(w, rep)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, worker string) {
	var up Upload
	if !decode(w, r, MaxUpload, &up) {
		return
	}
	id := r.PathValue("attempt")
	if up.Attempt != id || up.Schema != Schema {
		http.Error(w, "attempt or schema mismatch", http.StatusBadRequest)
		return
	}

	// Whoever takes an attempt out of live owns its release, so expiry and
	// an upload can't both count it.
	s.mu.Lock()
	a := s.live[id]
	if a != nil && a.job.Worker == worker {
		delete(s.live, id)
		s.settling[id]++
		defer func() {
			s.mu.Lock()
			if s.settling[id]--; s.settling[id] == 0 {
				delete(s.settling, id)
			}
			s.mu.Unlock()
		}()
	}
	settling := s.settling[id] > 0
	s.mu.Unlock()
	if a == nil || a.job.Worker != worker {
		// A re-upload of one already installed, or one fenced off.
		switch st, hash := s.Src.AttemptResult(id, worker); {
		case st == queue.AttemptInstalled && hash == up.SHA256 && hash != "":
			reply(w, UploadReply{Status: "installed"})
		case settling:
			// The first upload is still being answered, and may yet fail.
			http.Error(w, "settling", http.StatusServiceUnavailable)
		default:
			// Including installing with nobody here settling it: a
			// coordinator that died mid-install. Waiting won't help.
			reply(w, UploadReply{Status: "stale"})
		}
		return
	}
	j := a.job

	switch up.Outcome {
	case OutcomeHost, OutcomeAborted:
		kind := queue.OutcomeHost
		if up.Outcome == OutcomeAborted {
			kind = queue.OutcomeAborted
		}
		s.Log.Printf("%s: %s on %s: %s %q", id, j.Combination.ID(), worker, up.Outcome, up.Reason)
		if s.answer(w, j, queue.Outcome{Kind: kind, Reason: up.Reason, Requested: up.Requested}) {
			reply(w, UploadReply{Status: "accepted"})
		}
		return
	case OutcomeResult:
	default:
		s.reject(w, j, up, fmt.Errorf("unknown outcome %q", up.Outcome))
		return
	}

	sum := sha256.Sum256(up.Result)
	if hex.EncodeToString(sum[:]) != up.SHA256 {
		s.reject(w, j, up, errors.New("checksum mismatch"))
		return
	}
	res, err := Validate(up.Result, j.Combination, s.Workarounds)
	if err == nil && s.Contract.Playwright && res.Steps["smoke"].Status == result.StatusPass && res.Steps["playwright"].Status == result.StatusSkip {
		err = errors.New("playwright skipped in a sweep that runs it")
	}
	if err != nil {
		s.reject(w, j, up, err)
		return
	}
	// Only the coordinator's clock is trusted.
	if at, _ := time.Parse(time.RFC3339, res.Timestamp); at.Before(j.Claimed.Truncate(time.Second)) || at.After(time.Now()) {
		res.Timestamp = result.Now()
	}

	switch err := s.Src.Install(j, res, up.SHA256); {
	case errors.Is(err, queue.ErrFenced):
		s.Log.Printf("%s: %s result from %s refused, attempt fenced", id, j.Combination.ID(), worker)
		if s.answer(w, j, queue.Outcome{Kind: queue.OutcomeAborted, Requested: true}) {
			reply(w, UploadReply{Status: "stale"})
		}
	case err != nil:
		// Try again later; the attempt stays live.
		s.mu.Lock()
		a.expires = time.Now().Add(LeaseFor)
		s.live[id] = a
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		s.Log.Printf("%s: %s on %s -> %s", id, j.Combination.ID(), worker, res.OverallStatus)
		if s.answer(w, j, queue.Outcome{Kind: queue.OutcomeResult, Wrote: true}) {
			reply(w, UploadReply{Status: "installed"})
		}
	}
}

// release settles j, putting it back to expire and settle again if the
// ledger can't be saved.
func (s *Server) release(j queue.Job, o queue.Outcome) bool {
	if err := s.Src.Settle(j, o); err != nil {
		s.Log.Printf("%s: saving the release: %v", j.Attempt, err)
		s.mu.Lock()
		s.live[j.Attempt] = &liveAttempt{job: j, expires: time.Now().Add(LeaseFor)}
		s.mu.Unlock()
		return false
	}
	return true
}

func (s *Server) answer(w http.ResponseWriter, j queue.Job, o queue.Outcome) bool {
	if !s.release(j, o) {
		http.Error(w, "couldn't save the ledger", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) reject(w http.ResponseWriter, j queue.Job, up Upload, why error) {
	s.Log.Printf("%s: %s result rejected: %v", j.Attempt, j.Combination.ID(), why)
	if s.RejectDir != "" {
		if err := os.MkdirAll(s.RejectDir, 0o755); err == nil {
			body, _ := json.Marshal(up)
			os.WriteFile(filepath.Join(s.RejectDir, filepath.Base(j.Attempt)+".json"), body, 0o644)
		}
	}
	if s.answer(w, j, queue.Outcome{Kind: queue.OutcomeHost, Reason: "rejected: " + why.Error()}) {
		reply(w, UploadReply{Status: "rejected", Reason: why.Error()})
	}
}
