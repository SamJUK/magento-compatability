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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samjuk/magento-compatability/internal/atomicfile"
	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/result"
	"github.com/samjuk/magento-compatability/internal/runner"
)

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
	// Preflight, if set, must pass before each run. A failure is the host's
	// and stops claims until a later SetUnhealthy("preflight", "") clears it.
	Preflight func(context.Context) error
	Log       *log.Logger

	mu        sync.Mutex
	ready     bool
	stale     string
	unhealthy map[string]string
	clearing  map[string]bool // stacks a clearStack is already retrying
	running   map[string]*running
	kick      chan struct{}
	bg        sync.WaitGroup
	upOnce    sync.Once
	preflight sync.Mutex    // one check at a time, so one failure stops the rest
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
	handled := w.stacks().RemoveLeftovers(ctx, func(name string, _ error) { w.clearLater(name) })
	for _, e := range entries {
		dir := filepath.Join(w.Spool, e.Name())
		// Cut off between the sync and the rename: the upload is whole.
		if data, err := os.ReadFile(filepath.Join(dir, "upload.json.tmp")); err == nil && json.Valid(data) {
			os.Rename(filepath.Join(dir, "upload.json.tmp"), filepath.Join(dir, "upload.json"))
		}
		if _, err := os.Stat(filepath.Join(dir, "upload.json")); errors.Is(err, os.ErrNotExist) {
			// Even with its result written: aborting charges no try, and Run decides what a result counts as.
			w.Log.Printf("%s: left running by the last start, reporting it aborted", e.Name())
			if name, err := os.ReadFile(filepath.Join(dir, "project")); err == nil && !slices.Contains(handled, string(name)) {
				if err := queue.RemoveStack(ctx, string(name)); err != nil {
					w.Log.Printf("removing leftover stack %s: %v", name, err)
					w.clearLater(string(name))
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

func (w *Worker) SetUnhealthy(cause, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.unhealthy == nil {
		w.unhealthy = map[string]string{}
	}
	if reason == "" {
		delete(w.unhealthy, cause)
	} else {
		w.unhealthy[cause] = reason
	}
}

// Reporting wraps a wait so a held-back worker says why; otherwise a burst worker would bill idle looking healthy.
func (w *Worker) Reporting(cause string, wait func() string) func() string {
	return func() string {
		reason := wait()
		w.SetUnhealthy(cause, reason)
		return reason
	}
}

func (w *Worker) health() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	reasons := make([]string, 0, len(w.unhealthy))
	for _, r := range w.unhealthy {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	return strings.Join(reasons, "; ")
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
	if w.health() != "" {
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

	// A claim already in flight when the worker turned unhealthy goes back
	// unrun, spending no budget.
	if reason := w.health(); reason != "" {
		return queue.Outcome{Kind: queue.OutcomeAborted, Requested: true, Reason: "unhealthy: " + reason}
	}
	if w.Preflight != nil {
		if o, ok := w.runPreflight(ctx); !ok {
			return o
		}
	}
	cfg := w.Base
	cfg.ResultsDir = filepath.Join(w.Spool, j.Attempt, "results")
	cfg.Force = true
	cfg.InstallSampleData = r.options.SampleData
	cfg.MaxLogBytes = r.options.MaxLogBytes
	cfg.Deadline = r.options.Deadline
	if !r.options.Playwright {
		cfg.PlaywrightDir = ""
	} else if cfg.PlaywrightImage == "" {
		// Host npx isn't set up on workers; a run without it isn't a result.
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: "release has no Playwright image"}
	}
	// Here before the run, so a restart finds it if this process dies.
	if err := os.MkdirAll(filepath.Join(w.Spool, j.Attempt), 0o700); err != nil {
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: "spool: " + err.Error()}
	}
	project := ""
	cfg.OnProject = func(name string) {
		project = name
		if err := os.WriteFile(filepath.Join(w.Spool, j.Attempt, "project"), []byte(name), 0o600); err != nil {
			w.Log.Printf("%s: recording project %s: %v", j.Attempt, name, err)
		}
		w.stacks().Record(name)
	}

	w.Log.Printf("%s: start %s", j.Attempt, j.Combination.ID())
	ran, err := w.Runner(ctx, j.Combination, cfg)

	if project != "" {
		if errors.Is(err, runner.ErrCleanup) {
			w.clearLater(project)
		} else {
			w.stacks().ForgetIfGone(ctx, project)
		}
	}
	if ctx.Err() != nil && !ran {
		// The coordinator or whoever stopped this worker asked for it.
		return queue.Outcome{Kind: queue.OutcomeAborted, Requested: true, Reason: "stopped"}
	}
	path := queue.ResultPath(cfg.ResultsDir, j.Combination)
	if _, statErr := os.Stat(path); statErr != nil {
		reason := "no result written"
		if err != nil {
			reason = err.Error()
		}
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: reason}
	}
	if code := queue.HostFailure(path); code != "" {
		return queue.Outcome{Kind: queue.OutcomeHost, Reason: code}
	}
	return queue.Outcome{Kind: queue.OutcomeResult, Wrote: true}
}

var preflightTimeout = 2 * time.Minute

// runPreflight runs one check at a time and marks the worker unhealthy
// before releasing the next, so one failure costs one host failure.
func (w *Worker) runPreflight(ctx context.Context) (queue.Outcome, bool) {
	w.preflight.Lock()
	defer w.preflight.Unlock()
	if reason := w.health(); reason != "" {
		return queue.Outcome{Kind: queue.OutcomeAborted, Requested: true, Reason: "unhealthy: " + reason}, false
	}
	// Before the run's deadline starts: a hung docker must not hold the lease for good.
	pctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	err := w.Preflight(pctx)
	switch {
	case err == nil:
		return queue.Outcome{}, true
	case ctx.Err() != nil:
		return queue.Outcome{Kind: queue.OutcomeAborted, Requested: true, Reason: "stopped"}, false
	}
	// Otherwise every claim fails at once, spending the host budget of
	// combinations healthy workers could run.
	w.SetUnhealthy("preflight", err.Error())
	return queue.Outcome{Kind: queue.OutcomeHost, Reason: err.Error()}, false
}

var clearEvery = 30 * time.Second

// attempts is the spool's attempt directories, named by their ID; anything
// else in it is left alone.
func (w *Worker) attempts() ([]os.DirEntry, error) {
	entries, err := os.ReadDir(w.Spool)
	return slices.DeleteFunc(entries, func(e os.DirEntry) bool {
		_, err := hex.DecodeString(e.Name())
		return !e.IsDir() || len(e.Name()) != 24 || err != nil
	}), err
}

// stacksDir records stacks still to remove: the spool entry naming one is
// gone once its attempt uploads.
func (w *Worker) stacksDir() string { return filepath.Clean(w.Spool) + "-stacks" }

func (w *Worker) stacks() queue.Stacks { return queue.Stacks{Dir: w.stacksDir(), Log: w.Log} }

// clearLater keeps the worker off new work until a stack whose teardown
// failed is gone, across restarts.
func (w *Worker) clearLater(project string) {
	// Unhealthy before the next claim, not when the goroutine runs.
	w.SetUnhealthy("stack "+project, "teardown failed for "+project)
	w.stacks().Record(project)
	w.mu.Lock()
	clearing := w.clearing[project]
	if w.clearing == nil {
		w.clearing = map[string]bool{}
	}
	w.clearing[project] = true
	w.mu.Unlock()
	if !clearing {
		go w.clearStack(project)
	}
}

func (w *Worker) clearStack(project string) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := queue.RemoveStack(ctx, project)
		cancel()
		if err == nil {
			break
		}
		w.Log.Printf("removing %s: %v", project, err)
		time.Sleep(clearEvery)
	}
	os.Remove(filepath.Join(w.stacksDir(), project))
	w.mu.Lock()
	delete(w.clearing, project)
	w.mu.Unlock()
	w.SetUnhealthy("stack "+project, "")
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
		// Hashed as it travels: encoding/json compacts a RawMessage and
		// escapes <, > and &, so this is what every later Marshal sends.
		if cut := fit(raw, int(MaxUpload)*3/4); len(cut) < len(raw) {
			w.Log.Printf("%s: result of %d bytes cut to %d to fit the upload", j.Attempt, len(raw), len(cut))
			raw = cut
		}
		wire, err := json.Marshal(json.RawMessage(raw))
		if err != nil {
			up.Outcome, up.Reason = OutcomeHost, "result isn't JSON: "+err.Error()
			break
		}
		sum := sha256.Sum256(wire)
		up.Result, up.SHA256 = wire, hex.EncodeToString(sum[:])
	}
	if err := w.spool(j.Attempt, up); err != nil {
		w.Log.Printf("%s: spooling: %v", j.Attempt, err)
		w.SetUnhealthy("spool", "spool write failed: "+err.Error())
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

// fit cuts a result that would be refused as too big: container logs go, then each step keeps the end of its log.
func fit(raw []byte, limit int) []byte {
	if len(raw) <= limit {
		return raw
	}
	var r result.TestResult
	if json.Unmarshal(raw, &r) != nil {
		return raw
	}
	r.ContainerLogs = nil
	// Escaping can grow a log several times over, so measure and go again.
	for keep := limit / (2 * (len(r.Steps) + 1)); keep > 0; keep /= 2 {
		for name, s := range r.Steps {
			if len(s.Log) > keep {
				s.Log = "[cut to fit the upload]\n" + strings.ToValidUTF8(s.Log[len(s.Log)-keep:], "")
				r.Steps[name] = s
			}
		}
		b, err := json.Marshal(r)
		if err != nil {
			return raw
		}
		if len(b) <= limit {
			return b
		}
	}
	return raw
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
	if err := os.Rename(tmp, filepath.Join(dir, "upload.json")); err != nil {
		return err
	}
	return atomicfile.SyncDir(dir)
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
		unhealthy := w.health()
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
