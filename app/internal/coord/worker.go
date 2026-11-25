// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package coord

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/result"
	"github.com/samjuk/magento-compatability/internal/runner"
)

// hostCodes are failures that describe the worker, not the combination.
var hostCodes = map[string]bool{"docker_name_conflict": true, "docker_cleanup_race": true, "disk_space": true}

type Client struct {
	HTTP  *http.Client
	Token string
}

func NewClient(socket, token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) call(ctx context.Context, method, path string, timeout time.Duration, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://coordinator"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := make([]byte, 512)
		n, _ := resp.Body.Read(msg)
		return &StatusError{Code: resp.StatusCode, Msg: fmt.Sprintf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg[:n]))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

func refused(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return se.Code >= 400 && se.Code < 500
}

// Worker is the queue source on a remote host. A finished attempt waits in its spool until the coordinator has answered for it.
type Worker struct {
	Client  *Client
	Spool   string
	Version string
	Slots   int
	Base    runner.RunConfig // paths; results dir and options are per attempt
	Runner  func(context.Context, matrix.Combination, runner.RunConfig) (bool, error)
	Log     *log.Logger

	mu        sync.Mutex
	ready     bool
	stale     string
	unhealthy string
	running   map[string]*running
	kick      chan struct{}
	bg        sync.WaitGroup
	upOnce    sync.Once
	uploading chan struct{} // Flush and the upload loop take turns
}

type running struct {
	options queue.Contract
	cancel  context.CancelFunc
}

func (w *Worker) Flush(ctx context.Context) { w.uploadSpool(ctx) }

func (w *Worker) Start(ctx context.Context) error {
	w.running = map[string]*running{}
	w.kick = make(chan struct{}, 1)
	if err := os.MkdirAll(w.Spool, 0o700); err != nil {
		return err
	}
	entries, err := w.attempts()
	if err != nil {
		return err
	}
	for _, e := range entries {
		dir := filepath.Join(w.Spool, e.Name())
		// Cut off between the sync and the rename: the upload is whole.
		if data, err := os.ReadFile(filepath.Join(dir, "upload.json.tmp")); err == nil && json.Valid(data) {
			os.Rename(filepath.Join(dir, "upload.json.tmp"), filepath.Join(dir, "upload.json"))
		}
		if _, err := os.Stat(filepath.Join(dir, "upload.json")); errors.Is(err, os.ErrNotExist) {
			// Even with its result written: aborting charges no try, and Run decides what a result counts as.
			w.Log.Printf("%s: left running by the last start, reporting it aborted", e.Name())
			if name, err := os.ReadFile(filepath.Join(dir, "project")); err == nil {
				if err := queue.RemoveStack(ctx, string(name)); err != nil {
					w.Log.Printf("removing leftover stack %s: %v", name, err)
				}
			}
			if err := w.spool(e.Name(), Upload{Schema: Schema, Attempt: e.Name(), Outcome: OutcomeAborted, Reason: "worker restarted"}); err != nil {
				return err
			}
		}
	}
	w.bg.Go(func() { w.heartbeats(ctx) })
	w.bg.Go(func() { w.uploads(ctx) })
	return nil
}

func (w *Worker) Wait() { w.bg.Wait() }

// SetUnhealthy stops claims while reason is set, and tells the coordinator.
func (w *Worker) SetUnhealthy(reason string) {
	w.mu.Lock()
	w.unhealthy = reason
	w.mu.Unlock()
}

func (w *Worker) hello(ctx context.Context) (bool, error) {
	w.mu.Lock()
	ready := w.ready
	w.mu.Unlock()
	if ready {
		return true, nil
	}
	var rep HelloReply
	if err := w.Client.call(ctx, "POST", "/hello", 15*time.Second, HelloRequest{Schema: Schema, Release: w.Version, Slots: w.Slots}, &rep); err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if rep.Stale != w.stale && rep.Stale != "" {
		w.Log.Printf("coordinator says stale: %s", rep.Stale)
	}
	w.stale, w.ready = rep.Stale, rep.Stale == ""
	return w.ready, nil
}

// forget makes the next claim say hello again: after an error the
// coordinator may have restarted.
func (w *Worker) forget() {
	w.mu.Lock()
	w.ready = false
	w.mu.Unlock()
}

func (w *Worker) Next(ctx context.Context) (queue.Job, queue.State, error) {
	w.mu.Lock()
	unhealthy := w.unhealthy
	w.mu.Unlock()
	if unhealthy != "" {
		return queue.Job{}, queue.StateNone, nil
	}
	if ok, _ := w.hello(ctx); !ok {
		return queue.Job{}, queue.StateNone, nil
	}
	// One request ID until a reply arrives, so a reply lost to a tunnel drop
	// doesn't strand a claim.
	req := ClaimRequest{Schema: Schema, Request: newRequestID()}
	for {
		var rep ClaimReply
		err := w.Client.call(ctx, "POST", "/claim", ClaimWait+15*time.Second, req, &rep)
		if err == nil && rep.State == ClaimHello {
			// The coordinator restarted. Same request again, so a claim it
			// made before the restart isn't stranded.
			w.forget()
			ok, herr := w.hello(ctx)
			switch {
			case herr == nil && !ok:
				return queue.Job{}, queue.StateNone, nil // stale
			case herr == nil:
				continue
			}
			err = herr
		}
		if err == nil {
			if rep.State != ClaimJob {
				return queue.Job{}, queue.StateNone, nil
			}
			w.mu.Lock()
			w.running[rep.Attempt] = &running{options: rep.Options}
			w.mu.Unlock()
			return queue.Job{Combination: rep.Combo, Attempt: rep.Attempt}, queue.StateJob, nil
		}
		w.forget()
		if ctx.Err() != nil {
			return queue.Job{}, queue.StateNone, ctx.Err()
		}
		select {
		case <-ctx.Done():
		case <-time.After(retryEvery):
		}
	}
}

// Done is never true: a worker waits for the next sweep.
func (w *Worker) Done() bool { return false }

func (w *Worker) Run(ctx context.Context, _ int, j queue.Job) queue.Outcome {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.mu.Lock()
	r := w.running[j.Attempt]
	r.cancel = cancel
	w.mu.Unlock()

	cfg := w.Base
	cfg.ResultsDir = filepath.Join(w.Spool, j.Attempt, "results")
	cfg.Force = true
	cfg.InstallSampleData = r.options.SampleData
	cfg.MaxLogBytes = r.options.MaxLogBytes
	cfg.Deadline = r.options.Deadline
	if !r.options.Playwright {
		cfg.PlaywrightDir = ""
	}
	project := ""
	cfg.OnProject = func(name string) {
		project = name
		os.WriteFile(filepath.Join(w.Spool, j.Attempt, "project"), []byte(name), 0o600)
	}

	w.Log.Printf("%s: start %s", j.Attempt, j.Combination.ID())
	_, err := w.Runner(ctx, j.Combination, cfg)

	if ctx.Err() != nil {
		// The coordinator or whoever stopped this worker asked for it.
		return queue.Outcome{Kind: queue.OutcomeAborted, Requested: true, Reason: "stopped"}
	}
	if errors.Is(err, runner.ErrCleanupTimeout) {
		go w.clearStack(project)
	}
	path := filepath.Join(cfg.ResultsDir, j.Combination.Product, j.Combination.ID()+".json")
	if _, statErr := os.Stat(path); statErr != nil {
		reason := "no result written"
		if err != nil {
			reason = err.Error()
		}
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: reason}
	}
	if code := hostFailure(path); code != "" {
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: code}
	}
	return queue.Outcome{Kind: queue.OutcomeResult, Wrote: true}
}

func hostFailure(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var r result.TestResult
	if json.Unmarshal(data, &r) != nil {
		return ""
	}
	for _, st := range r.Steps {
		if st.Failure != nil && hostCodes[st.Failure.Code] {
			return st.Failure.Code
		}
	}
	return ""
}

// clearStack keeps the worker off new work until a stack whose teardown
// timed out is gone.
func (w *Worker) clearStack(project string) {
	reason := "teardown timed out for " + project
	w.SetUnhealthy(reason)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := queue.RemoveStack(ctx, project)
		cancel()
		if err == nil {
			break
		}
		w.Log.Printf("removing %s: %v", project, err)
		time.Sleep(30 * time.Second)
	}
	w.mu.Lock()
	if w.unhealthy == reason {
		w.unhealthy = ""
	}
	w.mu.Unlock()
}

// Release spools the attempt's outcome and kicks the uploader. A spool that
// can't be written means a broken disk, so the worker stops taking work.
func (w *Worker) Release(j queue.Job, o queue.Outcome) {
	w.mu.Lock()
	delete(w.running, j.Attempt)
	w.mu.Unlock()

	up := Upload{Schema: Schema, Attempt: j.Attempt, Reason: o.Reason, Requested: o.Requested}
	switch o.Kind {
	case queue.OutcomeHost:
		up.Outcome = OutcomeHost
	case queue.OutcomeAborted:
		up.Outcome = OutcomeAborted
	default:
		up.Outcome = OutcomeResult
		raw, err := os.ReadFile(filepath.Join(w.Spool, j.Attempt, "results", j.Combination.Product, j.Combination.ID()+".json"))
		if err != nil {
			up.Outcome, up.Reason = OutcomeHost, err.Error()
			break
		}
		// Hashed as it travels: encoding/json compacts a RawMessage.
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			up.Outcome, up.Reason = OutcomeHost, "result isn't JSON: "+err.Error()
			break
		}
		sum := sha256.Sum256(compact.Bytes())
		up.Result, up.SHA256 = compact.Bytes(), hex.EncodeToString(sum[:])
	}
	if err := w.spool(j.Attempt, up); err != nil {
		w.Log.Printf("%s: spooling: %v", j.Attempt, err)
		w.SetUnhealthy("spool write failed: " + err.Error())
		up = Upload{Schema: Schema, Attempt: j.Attempt, Outcome: OutcomeHost, Reason: "spool write failed"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var rep UploadReply
		w.Client.call(ctx, "PUT", "/result/"+j.Attempt, time.Minute, up, &rep)
		return
	}
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Worker) spool(attempt string, up Upload) error {
	dir := filepath.Join(w.Spool, attempt)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(up)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "upload.json.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "upload.json"))
}

// attempts is the spool's attempt directories, named by their ID; anything
// else in it is left alone.
func (w *Worker) attempts() ([]os.DirEntry, error) {
	entries, err := os.ReadDir(w.Spool)
	return slices.DeleteFunc(entries, func(e os.DirEntry) bool {
		_, err := hex.DecodeString(e.Name())
		return !e.IsDir() || len(e.Name()) != 24 || err != nil
	}), err
}

func (w *Worker) held() []string {
	w.mu.Lock()
	ids := make([]string, 0, len(w.running))
	for id := range w.running {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	entries, _ := w.attempts()
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(w.Spool, e.Name(), "upload.json")); err == nil {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

func (w *Worker) heartbeats(ctx context.Context) {
	lastErr := ""
	for {
		w.mu.Lock()
		unhealthy := w.unhealthy
		w.mu.Unlock()
		var rep HeartbeatReply
		err := w.Client.call(ctx, "POST", "/heartbeat", 15*time.Second, HeartbeatRequest{Schema: Schema, Running: w.held(), Unhealthy: unhealthy}, &rep)
		if err != nil {
			w.forget()
			if err.Error() != lastErr && ctx.Err() == nil {
				w.Log.Printf("coordinator unreachable: %v", err)
			}
			lastErr = err.Error()
		} else if lastErr != "" {
			w.Log.Print("coordinator reachable again")
			lastErr = ""
		}
		for _, id := range rep.Abort {
			w.mu.Lock()
			if r := w.running[id]; r != nil && r.cancel != nil {
				w.Log.Printf("%s: coordinator asked for an abort", id)
				r.cancel()
			}
			w.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(HeartbeatEvery):
		}
	}
}

func (w *Worker) uploads(ctx context.Context) {
	for {
		w.uploadSpool(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		case <-time.After(retryEvery):
		}
	}
}

func (w *Worker) uploadSpool(ctx context.Context) {
	w.upOnce.Do(func() { w.uploading = make(chan struct{}, 1) })
	select {
	case w.uploading <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-w.uploading }()
	entries, _ := w.attempts()
	for _, e := range entries {
		dir := filepath.Join(w.Spool, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, "upload.json"))
		if err != nil {
			continue
		}
		var up Upload
		if err := json.Unmarshal(data, &up); err != nil {
			w.Log.Printf("%s: unreadable spool entry, dropping it: %v", e.Name(), err)
			os.RemoveAll(dir)
			continue
		}
		var rep UploadReply
		err = w.Client.call(ctx, "PUT", "/result/"+up.Attempt, 5*time.Minute, up, &rep)
		switch {
		case refused(err) && up.Outcome == OutcomeResult:
			// Too big, say. Report the attempt as the host's so it's retried.
			w.Log.Printf("%s: upload refused, reporting a host failure: %v", up.Attempt, err)
			host := Upload{Schema: Schema, Attempt: up.Attempt, Outcome: OutcomeHost, Reason: "upload refused: " + err.Error()}
			if err := w.spool(up.Attempt, host); err != nil {
				w.Log.Printf("%s: spooling: %v", up.Attempt, err)
				os.RemoveAll(dir)
			}
			continue
		case refused(err):
			w.Log.Printf("%s: upload refused, dropping it: %v", up.Attempt, err)
			os.RemoveAll(dir)
			continue
		case errors.As(err, new(*StatusError)):
			// The coordinator answered: this one can't land yet, the rest may.
			w.Log.Printf("%s: upload not taken yet: %v", up.Attempt, err)
			continue
		case err != nil:
			w.forget()
			return
		}
		w.Log.Printf("%s: uploaded %s: %s %s", up.Attempt, up.Outcome, rep.Status, rep.Reason)
		os.RemoveAll(dir)
	}
}

var newRequestID = func() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
