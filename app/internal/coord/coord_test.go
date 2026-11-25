// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package coord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/result"
	"github.com/samjuk/magento-compatability/internal/runner"
)

func init() {
	ClaimWait, claimPoll, expireEvery, retryEvery = 100*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	HeartbeatEvery = 20 * time.Millisecond
}

var quiet = log.New(io.Discard, "", 0)

func combo(php string) matrix.Combination {
	return matrix.Combination{
		Product: "magento", Version: "2.4.8", PHP: php,
		WebserverType: "nginx", WebserverVersion: "1.28",
		DBType: "mariadb", DBVersion: "11.4",
		SearchType: "opensearch", SearchVersion: "3",
		CacheType: "valkey", CacheVersion: "8",
		QueueType: "rabbitmq", QueueVersion: "4.2",
		Varnish: "none",
	}
}

func goodResult(c matrix.Combination, stackUp string) result.TestResult {
	steps := map[string]result.Step{"stack_up": {Status: stackUp}}
	if stackUp == result.StatusPass {
		steps["install"] = result.Step{Status: result.StatusPass}
		steps["smoke"] = result.Step{Status: result.StatusPass}
		steps["playwright"] = result.Step{Status: result.StatusSkip}
	}
	return result.TestResult{ID: c.ID(), Product: c.Product, Version: c.Version, Services: runner.Services(c),
		Steps: steps, OverallStatus: result.OverallStatus(steps), Timestamp: result.Now()}
}

type rig struct {
	t        *testing.T
	results  string
	state    string
	sock     string
	combos   []matrix.Combination
	src      *queue.Local
	lg       *queue.Ledger
	srv      *Server
	stop     context.CancelFunc
	contract queue.Contract
}

func newRig(t *testing.T, combos []matrix.Combination) *rig {
	t.Helper()
	short, err := os.MkdirTemp("", "co")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	r := &rig{t: t, results: t.TempDir(), state: t.TempDir(), sock: filepath.Join(short, "w.sock"), combos: combos}
	r.start()
	return r
}

func (r *rig) start() {
	t := r.t
	lg, err := queue.OpenLedger(r.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	r.lg = lg
	r.src, err = queue.NewLocal(lg, r.results, r.combos, r.contract, false)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = &Server{Src: r.src, Tokens: map[string]string{"tok-a": "a", "tok-b": "b"}, Release: "rel1", Contract: r.contract,
		RejectDir: filepath.Join(r.state, "rejected"), Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	t.Cleanup(cancel)
	if err := r.srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	srv, sock := r.srv, r.sock
	go func() { srv.Serve(ctx, sock); close(served) }()
	t.Cleanup(func() { cancel(); <-served; srv.Wait() })
	// The socket file appears before it listens, and a restart finds the old one.
	waitFor(t, func() bool {
		c, err := net.Dial("unix", r.sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

func (r *rig) restart() {
	r.down()
	r.start()
}

func (r *rig) down() {
	r.stop()
	r.srv.Wait()
	r.lg.Close()
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) worker(token string, fn func(context.Context, matrix.Combination, runner.RunConfig) (bool, error)) (*Worker, context.CancelFunc) {
	return r.workerIn(token, r.t.TempDir(), fn)
}

func (r *rig) workerIn(token, spool string, fn func(context.Context, matrix.Combination, runner.RunConfig) (bool, error)) (*Worker, context.CancelFunc) {
	w := &Worker{Client: NewClient(r.sock, token), Spool: spool, Version: "rel1", Slots: 2, Runner: fn, Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	bg, stopBG := context.WithCancel(context.Background())
	if err := w.Start(bg); err != nil {
		r.t.Fatal(err)
	}
	e := &queue.Engine{Source: w, MaxSlots: 2, Slots: func() int { return 2 }, Wait: func() string { return "" },
		Run: w.Run, Log: quiet, IdlePoll: 5 * time.Millisecond}
	done := make(chan struct{})
	go func() { e.Start(ctx); close(done) }()
	stop := func() { cancel(); <-done; w.Flush(context.Background()); stopBG(); w.Wait() }
	r.t.Cleanup(stop)
	return w, stop
}

func writes(status func(c matrix.Combination) string) func(context.Context, matrix.Combination, runner.RunConfig) (bool, error) {
	return func(_ context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		return true, result.Write(filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json"), goodResult(c, status(c)))
	}
}

func (r *rig) status(c matrix.Combination) string { return queue.Status(r.results, c) }

func TestEndToEnd_TwoWorkersRunTheSweep(t *testing.T) {
	combos := []matrix.Combination{combo("8.1"), combo("8.2"), combo("8.3"), combo("8.4")}
	r := newRig(t, combos)
	var runs atomic.Int32
	var failedOnce atomic.Bool
	fn := writes(func(c matrix.Combination) string {
		runs.Add(1)
		// 8.2 fails stack_up on its first run and must be retried.
		if c.PHP == "8.2" && failedOnce.CompareAndSwap(false, true) {
			return result.StatusFail
		}
		return result.StatusPass
	})
	_, stopA := r.worker("tok-a", fn)
	_, stopB := r.worker("tok-b", func(ctx context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		return fn(ctx, c, cfg)
	})
	defer stopA()
	defer stopB()

	waitFor(t, func() bool { return r.src.Done() && r.srv.Live() == 0 })
	for _, c := range combos {
		if st := r.status(c); st != queue.StatusPass {
			t.Errorf("%s: %s, want pass", c.PHP, st)
		}
	}
	if runs.Load() != 5 {
		t.Errorf("%d runs, want 5 (4 plus the setup retry)", runs.Load())
	}
}

func TestEndToEnd_HostFailuresSpendTheirOwnBudget(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	var runs atomic.Int32
	_, stop := r.worker("tok-a", func(context.Context, matrix.Combination, runner.RunConfig) (bool, error) {
		runs.Add(1)
		return true, errors.New("docker: cannot connect")
	})
	defer stop()
	waitFor(t, func() bool { return r.src.Done() && r.srv.Live() == 0 })
	if runs.Load() != queue.MaxHostFailures {
		t.Errorf("%d runs, want %d", runs.Load(), queue.MaxHostFailures)
	}
	if r.src.Tries(c) != 0 {
		t.Errorf("host failures used %d of the combination's tries", r.src.Tries(c))
	}
}

func (r *rig) claim(token, request string) ClaimReply {
	r.t.Helper()
	var rep ClaimReply
	c := NewClient(r.sock, token)
	if err := c.call(context.Background(), "POST", "/hello", time.Second, HelloRequest{Schema: Schema, Release: "rel1"}, &HelloReply{}); err != nil {
		r.t.Fatal(err)
	}
	if err := c.call(context.Background(), "POST", "/claim", time.Second, ClaimRequest{Schema: Schema, Request: request}, &rep); err != nil {
		r.t.Fatal(err)
	}
	return rep
}

func (r *rig) upload(token string, up Upload) (UploadReply, error) {
	var rep UploadReply
	err := NewClient(r.sock, token).call(context.Background(), "PUT", "/result/"+up.Attempt, 30*time.Second, up, &rep)
	return rep, err
}

func resultUpload(t *testing.T, attempt string, res result.TestResult) Upload {
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return Upload{Schema: Schema, Attempt: attempt, Outcome: OutcomeResult, Result: raw, SHA256: hex.EncodeToString(sum[:])}
}

func TestClaim_SameRequestSameAttempt(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.1"), combo("8.2")})
	one, two := r.claim("tok-a", "req1"), r.claim("tok-a", "req1")
	if one.State != ClaimJob || one.Attempt != two.Attempt {
		t.Errorf("repeat claim got %+v then %+v", one, two)
	}
	if other := r.claim("tok-a", "req2"); other.Attempt == one.Attempt || other.Combo == one.Combo {
		t.Errorf("a new request got the same claim: %+v", other)
	}
}

func TestUpload_FencingAndOwnership(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	up := resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))

	if rep, err := r.upload("tok-b", up); err != nil || rep.Status != "stale" {
		t.Errorf("another worker's upload: %+v, %v; want stale", rep, err)
	}
	if r.status(c) != queue.StatusTodo {
		t.Fatal("another worker's upload was installed")
	}
	if _, err := r.upload("nope", up); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("unknown token: %v, want 401", err)
	}
	if rep, err := r.upload("tok-a", up); err != nil || rep.Status != "installed" {
		t.Fatalf("owner's upload: %+v, %v", rep, err)
	}
	if r.status(c) != queue.StatusPass {
		t.Error("owner's upload not installed")
	}
	if rep, _ := r.upload("tok-a", up); rep.Status != "installed" {
		t.Errorf("re-upload after a lost reply: %s, want installed", rep.Status)
	}
	if r.src.Tries(c) != 1 {
		t.Errorf("tries = %d, want 1", r.src.Tries(c))
	}
}

func TestUpload_Rejected(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	bad := goodResult(c, result.StatusPass)
	bad.OverallStatus = result.StatusFail
	rep, err := r.upload("tok-a", resultUpload(t, job.Attempt, bad))
	if err != nil || rep.Status != "rejected" {
		t.Fatalf("got %+v, %v; want rejected", rep, err)
	}
	if r.status(c) != queue.StatusTodo {
		t.Error("a rejected result was installed")
	}
	if _, err := os.Stat(filepath.Join(r.state, "rejected", job.Attempt+".json")); err != nil {
		t.Errorf("rejected upload not kept: %v", err)
	}
	if r.src.Tries(c) != 0 {
		t.Error("a rejection used a try")
	}

	// Checksum must match too; and the control: a good one installs.
	job = r.claim("tok-a", "req2")
	up := resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))
	up.SHA256 = strings.Repeat("0", 64)
	if rep, _ := r.upload("tok-a", up); rep.Status != "rejected" {
		t.Errorf("bad checksum: %s, want rejected", rep.Status)
	}
	job = r.claim("tok-a", "req3")
	if rep, _ := r.upload("tok-a", resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))); rep.Status != "installed" {
		t.Errorf("control: good upload %s", rep.Status)
	}
}

func TestUpload_TooBig(t *testing.T) {
	old := MaxUpload
	MaxUpload = 1 << 20 // the race detector makes 64MB crawl
	defer func() { MaxUpload = old }()
	r := newRig(t, []matrix.Combination{combo("8.3")})
	job := r.claim("tok-a", "req1")
	up := Upload{Schema: Schema, Attempt: job.Attempt, Outcome: OutcomeHost, Reason: strings.Repeat("x", int(MaxUpload))}
	if _, err := r.upload("tok-a", up); err == nil || !strings.Contains(err.Error(), "413") {
		t.Errorf("oversized upload: %v, want 413", err)
	}
}

func TestLease_ExpiresWithoutHeartbeats(t *testing.T) {
	old := LeaseFor
	LeaseFor = 50 * time.Millisecond
	defer func() { LeaseFor = old }()
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	waitFor(t, func() bool { return r.srv.Live() == 0 })

	rep, _ := r.upload("tok-a", resultUpload(t, job.Attempt, goodResult(c, result.StatusPass)))
	if rep.Status != "stale" || r.status(c) != queue.StatusTodo {
		t.Errorf("late upload: %s, result %s; want stale and nothing installed", rep.Status, r.status(c))
	}
	if again := r.claim("tok-b", "req2"); again.State != ClaimJob {
		t.Errorf("expired combination not claimable: %+v", again)
	}
}

func TestAbandon_ChargesNoTry(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	for i := range queue.MaxHostFailures {
		job := r.claim("tok-a", fmt.Sprintf("req%d", i))
		if job.State != ClaimJob {
			t.Fatalf("claim %d: %+v; abandoned runs were charged", i, job)
		}
		r.srv.Abandon("b")
		if r.srv.Live() != 1 {
			t.Fatal("control: abandoned another worker's run")
		}
		r.srv.Abandon("a")
		if r.srv.Live() != 0 || r.src.AttemptState(job.Attempt) != queue.AttemptAborted {
			t.Fatalf("%d live, attempt %s; want it released as aborted", r.srv.Live(), r.src.AttemptState(job.Attempt))
		}
	}
	if again := r.claim("tok-a", "last"); again.State != ClaimJob {
		t.Errorf("after %d abandoned runs: %+v, want it still claimable", queue.MaxHostFailures, again)
	}
}

func TestAbandon_CatchesAClaimTheLedgerIsStillAnswering(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.3")})
	claiming, proceed := make(chan struct{}), make(chan struct{})
	afterClaim = func() { close(claiming); <-proceed }
	t.Cleanup(func() { afterClaim = func() {} })
	answered := make(chan ClaimReply, 1)
	go func() { answered <- r.claim("tok-a", "r1") }()
	<-claiming
	afterClaim = func() {}
	r.srv.Drain("a", true)
	r.srv.Abandon("a")
	close(proceed)
	if rep := <-answered; rep.State == ClaimJob || r.srv.Live() != 0 {
		t.Fatalf("a destroyed worker got %+v, %d live", rep, r.srv.Live())
	}
	if again := r.claim("tok-b", "r2"); again.State != ClaimJob {
		t.Errorf("the run wasn't released: %+v", again)
	}
}

func TestLease_HeartbeatsKeepItAlive(t *testing.T) {
	old := LeaseFor
	LeaseFor = 100 * time.Millisecond
	defer func() { LeaseFor = old }()
	r := newRig(t, []matrix.Combination{combo("8.3")})
	job := r.claim("tok-a", "req1")
	cl := NewClient(r.sock, "tok-a")
	for range 10 {
		var rep HeartbeatReply
		if err := cl.call(context.Background(), "POST", "/heartbeat", time.Second, HeartbeatRequest{Schema: Schema, Running: []string{job.Attempt}}, &rep); err != nil {
			t.Fatal(err)
		}
		if len(rep.Abort) != 0 {
			t.Fatalf("heartbeat said abort %v", rep.Abort)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if r.srv.Live() != 1 {
		t.Error("lease expired despite heartbeats")
	}
}

func TestRestart_KeepsRemoteAttempts(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	r.restart()
	if r.srv.Live() != 1 {
		t.Fatalf("%d live after restart, want 1", r.srv.Live())
	}
	if rep, err := r.upload("tok-a", resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))); err != nil || rep.Status != "installed" {
		t.Errorf("upload after restart: %+v, %v", rep, err)
	}
}

func TestOverride_AbortsAndFences(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")

	lg, err := queue.OpenLedger(r.state)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	if err := lg.Override(r.results, c.ID()); err != nil {
		t.Fatal(err)
	}
	var rep HeartbeatReply
	NewClient(r.sock, "tok-a").call(context.Background(), "POST", "/heartbeat", time.Second, HeartbeatRequest{Schema: Schema, Running: []string{job.Attempt}}, &rep)
	if len(rep.Abort) != 1 || rep.Abort[0] != job.Attempt {
		t.Fatalf("abort = %v, want the overridden attempt", rep.Abort)
	}
	if up, _ := r.upload("tok-a", resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))); up.Status != "stale" {
		t.Errorf("overridden upload: %s, want stale", up.Status)
	}
	if r.status(c) != queue.StatusTodo || r.src.Tries(c) != 0 {
		t.Errorf("override: result %s, tries %d; want nothing installed or counted", r.status(c), r.src.Tries(c))
	}
}

func TestWorker_SpoolOutlastsCoordinator(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	release := make(chan struct{})
	_, stop := r.worker("tok-a", func(ctx context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		<-release
		return writes(func(matrix.Combination) string { return result.StatusPass })(ctx, c, cfg)
	})
	defer stop()
	waitFor(t, func() bool { return r.srv.Live() == 1 })
	r.down() // coordinator down while the run finishes
	close(release)
	time.Sleep(100 * time.Millisecond)
	if r.status(c) != queue.StatusTodo {
		t.Fatal("installed with the coordinator down")
	}
	r.start()
	waitFor(t, func() bool { return r.status(c) == queue.StatusPass })
}

func TestWorker_RestartReportsAbandonedRuns(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	spool := t.TempDir()
	os.MkdirAll(filepath.Join(spool, job.Attempt, "results"), 0o700) // a run cut off by a reboot
	os.MkdirAll(filepath.Join(spool, "notes"), 0o700)                // not ours
	os.MkdirAll(filepath.Join(spool, job.Attempt, "results", c.Product), 0o700)
	os.WriteFile(filepath.Join(spool, job.Attempt, "results", c.Product, c.ID()+".json"), []byte("{"), 0o600) // cut off mid-write
	w := &Worker{Client: NewClient(r.sock, "tok-a"), Spool: spool, Version: "rel1", Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Wait()
	defer cancel()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(spool, job.Attempt)); return err != nil })
	if _, err := os.Stat(filepath.Join(spool, "notes")); err != nil {
		t.Errorf("a folder that isn't an attempt was touched: %v", err)
	}
	if r.srv.Live() != 0 {
		t.Error("abandoned attempt still live")
	}
	if r.src.Tries(c) != 0 {
		t.Error("an abandoned run used a try")
	}
	if st := r.src.AttemptState(job.Attempt); st != queue.AttemptAborted {
		t.Errorf("a run cut off mid-write ended %q, not aborted", st)
	}
}

func TestWorker_RestartKeepsRemovingALeftoverStack(t *testing.T) {
	old := clearEvery
	clearEvery = 10 * time.Millisecond
	defer func() { clearEvery = old }()
	bin := t.TempDir()
	os.WriteFile(bin+"/docker", []byte("#!/bin/sh\n[ ! -e "+bin+"/down ]\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r := newRig(t, []matrix.Combination{combo("8.3")})
	job := r.claim("tok-a", "req1")
	spool := t.TempDir()
	os.MkdirAll(filepath.Join(spool, job.Attempt), 0o700)
	os.WriteFile(filepath.Join(spool, job.Attempt, "project"), []byte("m2test-0123456789abcdef-01234567"), 0o600)
	os.WriteFile(bin+"/down", nil, 0o600)
	w := &Worker{Client: NewClient(r.sock, "tok-a"), Spool: spool, Version: "rel1", Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Wait()
	defer cancel()
	unhealthy := func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.unhealthy) > 0 }
	if !unhealthy() {
		t.Fatal("took work with a stack it couldn't remove")
	}
	os.Remove(bin + "/down")
	waitFor(t, func() bool { return !unhealthy() })
}

func TestWorker_AFailedTeardownOutlivesItsUploadAndARestart(t *testing.T) {
	old := clearEvery
	clearEvery = 10 * time.Millisecond
	defer func() { clearEvery = old }()
	bin := t.TempDir()
	os.WriteFile(bin+"/docker", []byte("#!/bin/sh\n[ ! -e "+bin+"/down ]\n"), 0o755)
	os.WriteFile(bin+"/down", nil, 0o600)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	spool := t.TempDir()
	const project = "m2test-0123456789abcdef-01234567"
	first, stop := r.workerIn("tok-a", spool, func(ctx context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		cfg.OnProject(project)
		ran, err := writes(func(matrix.Combination) string { return result.StatusPass })(ctx, c, cfg)
		return ran, errors.Join(err, runner.ErrCleanup)
	})
	waitFor(t, func() bool { return r.status(c) == queue.StatusPass })
	waitFor(t, func() bool { entries, _ := os.ReadDir(spool); return len(entries) == 0 })
	stop()

	w := &Worker{Client: NewClient(r.sock, "tok-a"), Spool: spool, Version: "rel1", Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Wait()
	defer cancel()
	unhealthy := func(w *Worker) bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.unhealthy) > 0 }
	if !unhealthy(w) {
		t.Fatal("the restarted worker forgot the stack it couldn't remove")
	}
	os.Remove(bin + "/down")
	// Both, so neither still retries once clearEvery is put back.
	waitFor(t, func() bool { return !unhealthy(w) && !unhealthy(first) })
	if left, _ := os.ReadDir(spool + "-stacks"); len(left) != 0 {
		t.Errorf("record kept once the stack went: %v", left)
	}
}

func TestHello_StaleWorkerGetsNoWork(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.3")})
	cl := NewClient(r.sock, "tok-a")
	var hello HelloReply
	cl.call(context.Background(), "POST", "/hello", time.Second, HelloRequest{Schema: Schema, Release: "old"}, &hello)
	if hello.Stale == "" {
		t.Error("an old release wasn't called stale")
	}
	var rep ClaimReply
	cl.call(context.Background(), "POST", "/claim", time.Second, ClaimRequest{Schema: Schema, Request: "r"}, &rep)
	if rep.State == ClaimJob {
		t.Errorf("stale worker got %+v", rep)
	}
	if got := r.claim("tok-a", "r2"); got.State != ClaimJob {
		t.Errorf("control: after a good hello, claim got %+v", got)
	}
}

func TestHTTP_MethodsAreFixed(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.3")})
	cl := NewClient(r.sock, "tok-a")
	req, _ := http.NewRequest("GET", "http://coordinator/claim", nil)
	req.Header.Set("Authorization", "Bearer tok-a")
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /claim: %d", resp.StatusCode)
	}
}

func TestRestart_WorkerMustSayHelloAgain(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.1"), combo("8.2")})
	r.claim("tok-a", "req1")
	r.restart()
	var rep ClaimReply
	NewClient(r.sock, "tok-a").call(context.Background(), "POST", "/claim", time.Second, ClaimRequest{Schema: Schema, Request: "req2"}, &rep)
	if rep.State != ClaimHello {
		t.Errorf("claim without hello after a restart: %q, want hello", rep.State)
	}
	if got := r.claim("tok-a", "req3"); got.State != ClaimJob {
		t.Errorf("control: after hello, claim got %+v", got)
	}
}

func TestWorker_RefusedUploadCountsAgainstTheHost(t *testing.T) {
	old := MaxUpload
	MaxUpload = 4 << 10
	defer func() { MaxUpload = old }()
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	var runs atomic.Int32
	spool := t.TempDir()
	_, stop := r.workerIn("tok-a", spool, func(_ context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		runs.Add(1)
		res := goodResult(c, result.StatusPass)
		res.Workarounds = []string{strings.Repeat("x", 8<<10)} // nothing fit can cut
		return true, result.Write(filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json"), res)
	})
	defer stop()
	waitFor(t, func() bool { return r.src.Done() && r.srv.Live() == 0 })
	if runs.Load() != queue.MaxHostFailures || r.src.Tries(c) != 0 {
		t.Errorf("%d runs, %d tries; want %d runs on the host budget", runs.Load(), r.src.Tries(c), queue.MaxHostFailures)
	}
	waitFor(t, func() bool { e, _ := os.ReadDir(spool); return len(e) == 0 })
}

func TestWorker_AnOversizedResultIsCutToFit(t *testing.T) {
	old := MaxUpload
	MaxUpload = 16 << 10
	defer func() { MaxUpload = old }()
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	_, stop := r.workerIn("tok-a", t.TempDir(), func(_ context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		res := goodResult(c, result.StatusPass)
		res.ContainerLogs = map[string]string{"php": strings.Repeat("\x00", 8<<10)} // six bytes each in JSON
		for name, s := range res.Steps {
			s.Log = strings.Repeat("\x01", 8<<10) + "the end" // escaped, more than the cap
			res.Steps[name] = s
		}
		return true, result.Write(filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json"), res)
	})
	defer stop()
	waitFor(t, func() bool { return r.status(c) == queue.StatusPass })
	raw, err := os.ReadFile(filepath.Join(r.results, c.Product, c.ID()+".json"))
	var got result.TestResult
	if err == nil {
		err = json.Unmarshal(raw, &got)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.ContainerLogs != nil {
		t.Error("container logs kept")
	}
	for name, s := range got.Steps {
		if !strings.HasSuffix(s.Log, "the end") || !strings.HasPrefix(s.Log, "[cut") {
			t.Errorf("step %s log %.40q...", name, s.Log)
		}
	}
}

func TestWorker_StoppingIsNotAHostFailure(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	spool := t.TempDir()
	var started atomic.Int32
	block := func(ctx context.Context, _ matrix.Combination, _ runner.RunConfig) (bool, error) {
		started.Add(1)
		<-ctx.Done()
		return false, ctx.Err()
	}
	for i := range int32(queue.MaxHostFailures) {
		_, stop := r.workerIn("tok-a", spool, block)
		waitFor(t, func() bool { return started.Load() == i+1 })
		stop()
		if r.srv.Live() != 0 {
			t.Fatal("the stop didn't report its abort")
		}
	}
	_, stop := r.workerIn("tok-a", spool, writes(func(matrix.Combination) string { return result.StatusPass }))
	defer stop()
	waitFor(t, func() bool { return r.status(c) == queue.StatusPass })
}

func TestWorker_AResultWrittenBeforeAStopIsKept(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	var started atomic.Bool
	write := writes(func(matrix.Combination) string { return result.StatusPass })
	fn := func(ctx context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		ran, err := write(ctx, c, cfg)
		started.Store(true)
		<-ctx.Done() // the stop lands during teardown
		return ran && err == nil, ctx.Err()
	}
	_, stop := r.workerIn("tok-a", t.TempDir(), fn)
	waitFor(t, started.Load)
	stop()
	if st := r.status(c); st != queue.StatusPass {
		t.Errorf("status %s, want the written pass", st)
	}
}

func TestWorker_RecoversUploadCutOffBeforeRename(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	spool := t.TempDir()
	up, _ := json.Marshal(resultUpload(t, job.Attempt, goodResult(c, result.StatusPass)))
	os.MkdirAll(filepath.Join(spool, job.Attempt), 0o700)
	os.WriteFile(filepath.Join(spool, job.Attempt, "upload.json.tmp"), up, 0o600)
	w := &Worker{Client: NewClient(r.sock, "tok-a"), Spool: spool, Version: "rel1", Log: quiet}
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Wait()
	defer cancel()
	waitFor(t, func() bool { return r.status(c) == queue.StatusPass })
}

func TestWorker_LostClaimSurvivesRestart(t *testing.T) {
	old := newRequestID
	newRequestID = func() string { return "req1" }
	defer func() { newRequestID = old }()
	r := newRig(t, []matrix.Combination{combo("8.1"), combo("8.2")})
	lost := r.claim("tok-a", "req1") // the reply that never reached the worker
	r.restart()

	w := &Worker{Client: NewClient(r.sock, "tok-a"), Spool: t.TempDir(), Version: "rel1", Log: quiet, running: map[string]*running{}}
	w.ready = true // it said hello to the coordinator before the restart
	j, st, err := w.Next(context.Background())
	if err != nil || st != queue.StateJob || j.Attempt != lost.Attempt {
		t.Errorf("got %s %v %v, want the lost attempt %s", j.Attempt, st, err, lost.Attempt)
	}
}

type failOnce struct {
	path string
	done atomic.Bool
	next http.RoundTripper
}

func (f *failOnce) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == f.path && f.done.CompareAndSwap(false, true) {
		return nil, errors.New("tunnel down")
	}
	return f.next.RoundTrip(r)
}

func TestWorker_LostClaimSurvivesFailedHello(t *testing.T) {
	old, oldRetry := newRequestID, retryEvery
	newRequestID, retryEvery = func() string { return "req1" }, 10*time.Millisecond
	defer func() { newRequestID, retryEvery = old, oldRetry }()
	r := newRig(t, []matrix.Combination{combo("8.1"), combo("8.2")})
	lost := r.claim("tok-a", "req1")
	r.restart()

	c := NewClient(r.sock, "tok-a")
	f := &failOnce{path: "/hello", next: c.HTTP.Transport}
	c.HTTP.Transport = f
	w := &Worker{Client: c, Spool: t.TempDir(), Version: "rel1", Log: quiet, running: map[string]*running{}}
	w.ready = true
	j, st, err := w.Next(context.Background())
	if !f.done.Load() {
		t.Fatal("hello never failed")
	}
	if err != nil || st != queue.StateJob || j.Attempt != lost.Attempt {
		t.Errorf("got %s %v %v, want the lost attempt %s", j.Attempt, st, err, lost.Attempt)
	}
}

func TestWorker_FlushGivesUpWhenUploadsAreBusy(t *testing.T) {
	w := &Worker{Spool: t.TempDir(), Log: quiet}
	w.upOnce.Do(func() { w.uploading = make(chan struct{}, 1) })
	w.uploading <- struct{}{} // the upload loop is stuck mid-request
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Flush(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Flush ignored its context")
	}
}

func TestWorker_PlaywrightSweepNeedsTheImage(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, nil)
	r.down()
	r.combos, r.contract = []matrix.Combination{c}, queue.Contract{Playwright: true}
	r.start()
	var runs atomic.Int32
	_, stop := r.worker("tok-a", func(_ context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		runs.Add(1)
		return writes(func(matrix.Combination) string { return result.StatusPass })(nil, c, cfg)
	})
	defer stop()
	waitFor(t, func() bool { return r.src.Done() && r.srv.Live() == 0 })
	if runs.Load() != 0 || r.status(c) == queue.StatusPass {
		t.Errorf("%d runs, status %s: ran Playwright with no image", runs.Load(), r.status(c))
	}
}

func TestUpload_AnInstallCutOffBeforeAnotherSweepIsStale(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through 0500")
	}
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	os.Chmod(r.results, 0o500) // the install fails after the ledger says installing
	defer os.Chmod(r.results, 0o700)
	up := resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))
	if _, err := r.upload("tok-a", up); err == nil {
		t.Fatal("control: the install didn't fail")
	}
	os.Chmod(r.results, 0o700)
	r.down()
	r.combos = []matrix.Combination{combo("8.4")}
	r.start()
	if rep, err := r.upload("tok-a", up); err != nil || rep.Status != "stale" {
		t.Errorf("got %+v, %v; want stale", rep, err)
	}
}

func TestUpload_ARetryWhileTheFirstIsSettlingWaits(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	job := r.claim("tok-a", "req1")
	// Rejecting it blocks on this until it's read.
	os.MkdirAll(filepath.Join(r.state, "rejected"), 0o755)
	fifo := filepath.Join(r.state, "rejected", job.Attempt+".json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := goodResult(c, result.StatusPass)
	bad.OverallStatus = result.StatusFail
	up := resultUpload(t, job.Attempt, bad)
	first := make(chan string, 1)
	go func() { rep, _ := r.upload("tok-a", up); first <- rep.Status }()
	waitFor(t, func() bool { return r.srv.Live() == 0 })
	var se *StatusError
	if rep, err := r.upload("tok-a", up); !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Errorf("a retry while the first settles got %+v, %v; want a 503", rep, err)
	}
	f, err := os.Open(fifo)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, f)
	f.Close()
	if st := <-first; st != "rejected" {
		t.Errorf("control: first upload %s", st)
	}
}

func TestUpload_APlaywrightSweepNeedsPlaywrightRun(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, nil)
	r.down()
	r.combos, r.contract = []matrix.Combination{c}, queue.Contract{Playwright: true}
	r.start()
	job := r.claim("tok-a", "req1")
	if rep, _ := r.upload("tok-a", resultUpload(t, job.Attempt, goodResult(c, result.StatusPass))); rep.Status != "rejected" {
		t.Errorf("Playwright skipped: %s, want rejected", rep.Status)
	}
	job = r.claim("tok-a", "req2")
	ran := goodResult(c, result.StatusPass)
	ran.Steps["playwright"] = result.Step{Status: result.StatusPass}
	if rep, _ := r.upload("tok-a", resultUpload(t, job.Attempt, ran)); rep.Status != "installed" {
		t.Errorf("control: Playwright run %s", rep.Status)
	}
}

func TestServe_LongSocketPathSaysSo(t *testing.T) {
	err := (&Server{}).Serve(context.Background(), "/"+strings.Repeat("d", 120)+"/worker.sock")
	if err == nil || !strings.Contains(err.Error(), "XDG_STATE_HOME") {
		t.Errorf("got %v", err)
	}
}

func TestWorker_HealthCausesClearIndependently(t *testing.T) {
	w := &Worker{}
	w.SetUnhealthy("images", "images missing")
	w.SetUnhealthy("stack p", "teardown timed out")
	w.SetUnhealthy("stack p", "")
	if got := w.health(); got != "images missing" {
		t.Errorf("health = %q", got)
	}
	w.SetUnhealthy("images", "")
	if got := w.health(); got != "" {
		t.Errorf("health = %q", got)
	}
}

func TestWorker_ReportsWhyItWaits(t *testing.T) {
	w := &Worker{}
	reason := "under 40GB free on /"
	wait := w.Reporting("disk", func() string { return reason })
	if got := wait(); got != reason || w.health() != reason {
		t.Errorf("wait = %q, health = %q", got, w.health())
	}
	reason = ""
	if got := wait(); got != "" || w.health() != "" {
		t.Errorf("after the disk freed: wait = %q, health = %q", got, w.health())
	}
}

func TestWorker_HungPreflightIsTheHosts(t *testing.T) {
	old := preflightTimeout
	preflightTimeout = 50 * time.Millisecond
	defer func() { preflightTimeout = old }()
	w := &Worker{Spool: t.TempDir(), Log: quiet, running: map[string]*running{"a1": {}}}
	w.Preflight = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	done := make(chan queue.Outcome)
	go func() { done <- w.Run(context.Background(), 0, queue.Job{Attempt: "a1", Combination: combo("8.3")}) }()
	select {
	case o := <-done:
		if o.Kind != queue.OutcomeHost || w.health() == "" {
			t.Errorf("outcome %+v, health %q; want the host's failure", o, w.health())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung preflight held the run")
	}
}

func TestWorker_FailedPreflightIsTheHosts(t *testing.T) {
	combos := []matrix.Combination{combo("8.1"), combo("8.2"), combo("8.3")}
	r := newRig(t, combos)
	var runs atomic.Int32
	w, stop := r.worker("tok-a", func(_ context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		runs.Add(1)
		return writes(func(matrix.Combination) string { return result.StatusPass })(nil, c, cfg)
	})
	defer stop()
	var checks atomic.Int32
	w.mu.Lock()
	w.Preflight = func(context.Context) error { checks.Add(1); return errors.New("images missing") }
	w.mu.Unlock()
	waitFor(t, func() bool { return w.health() != "" && r.srv.Live() == 0 })
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != 0 || checks.Load() > 2 {
		t.Errorf("%d runs, %d preflights; want the worker to stop claiming after the first failures", runs.Load(), checks.Load())
	}
	w.SetUnhealthy("preflight", "")
	w.mu.Lock()
	w.Preflight = nil
	w.mu.Unlock()
	waitFor(t, func() bool { return r.src.Done() })
	for _, c := range combos {
		if r.status(c) != queue.StatusPass {
			t.Errorf("%s: %s", c.ID(), r.status(c))
		}
	}
}

func TestWorker_ClaimArrivingWhileUnhealthyGoesBackUnrun(t *testing.T) {
	ran := false
	w := &Worker{Runner: func(context.Context, matrix.Combination, runner.RunConfig) (bool, error) {
		ran = true
		return true, nil
	},
		Spool: t.TempDir(), Log: quiet, running: map[string]*running{"a1": {}}}
	w.SetUnhealthy("stack p", "teardown failed")
	o := w.Run(context.Background(), 0, queue.Job{Attempt: "a1", Combination: combo("8.3")})
	if ran || o.Kind != queue.OutcomeAborted || !o.Requested {
		t.Errorf("ran %v, outcome %+v", ran, o)
	}
}

func TestWorker_ConcurrentPreflightsChargeOneHostFailure(t *testing.T) {
	w := &Worker{Spool: t.TempDir(), Log: quiet, running: map[string]*running{}}
	w.Preflight = func(context.Context) error { time.Sleep(20 * time.Millisecond); return errors.New("images missing") }
	var host atomic.Int32
	var wg sync.WaitGroup
	for i := range 4 {
		w.running[fmt.Sprintf("a%d", i)] = &running{}
	}
	for i := range 4 {
		id := fmt.Sprintf("a%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if o := w.Run(context.Background(), 0, queue.Job{Attempt: id, Combination: combo("8.3")}); o.Kind == queue.OutcomeHost {
				host.Add(1)
			}
		}()
	}
	wg.Wait()
	if host.Load() != 1 {
		t.Errorf("%d host failures, want 1", host.Load())
	}
}

func TestAddWorker_LetsANewTokenIn(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.3")})
	var rep HelloReply
	if err := NewClient(r.sock, "tok-c").call(context.Background(), "POST", "/hello", time.Second, HelloRequest{Schema: Schema, Release: "rel1"}, &rep); err == nil {
		t.Fatal("an unknown token got in")
	}
	r.srv.AddWorker("tok-c", "c")
	if got := r.claim("tok-c", "r"); got.State != ClaimJob {
		t.Fatalf("added worker got %+v", got)
	}
}

func TestDrain_OnlyAnIdleWorkerUnlessAlways(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.1"), combo("8.2"), combo("8.3")})
	if got := r.claim("tok-a", "r1"); got.State != ClaimJob {
		t.Fatalf("claim got %+v", got)
	}
	if r.srv.Drain("a", false) {
		t.Fatal("a worker holding an attempt drained as idle")
	}
	if got := r.claim("tok-a", "r2"); got.State != ClaimJob {
		t.Fatalf("a busy worker left undrained got %+v", got)
	}
	if !r.srv.Drain("b", false) {
		t.Fatal("an idle worker did not drain")
	}
	if got := r.claim("tok-b", "r3"); got.State == ClaimJob {
		t.Fatalf("a drained worker got %+v", got)
	}
	r.srv.Drain("a", true)
	if got := r.claim("tok-a", "r4"); got.State == ClaimJob {
		t.Fatalf("a worker drained with always got %+v", got)
	}
}

func TestDrain_AWorkerWaitingForWorkIsIdle(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.3")})
	if got := r.claim("tok-b", "r0"); got.State != ClaimJob {
		t.Fatalf("claim got %+v", got)
	}
	// With nothing left to hand out, a's claim long-polls.
	cl := NewClient(r.sock, "tok-a")
	cl.call(context.Background(), "POST", "/hello", time.Second, HelloRequest{Schema: Schema, Release: "rel1"}, &HelloReply{})
	answered := make(chan ClaimReply, 1)
	go func() {
		var rep ClaimReply
		cl.call(context.Background(), "POST", "/claim", time.Minute, ClaimRequest{Schema: Schema, Request: "r1"}, &rep)
		answered <- rep
	}()
	time.Sleep(3 * claimPoll)
	select {
	case rep := <-answered:
		t.Fatalf("control: the claim was answered (%+v) rather than waiting", rep)
	default:
	}
	if r.srv.Drain("b", false) {
		t.Error("b, holding an attempt, drained as idle")
	}
	// Each poll holds it busy for a moment while it looks for work.
	waitFor(t, func() bool { return r.srv.Drain("a", false) })
	if rep := <-answered; rep.State == ClaimJob {
		t.Errorf("the drained worker got %+v", rep)
	}
}

func TestDrain_NotWhileAnUploadIsBeingSettled(t *testing.T) {
	r := newRig(t, []matrix.Combination{combo("8.3")})
	job := r.claim("tok-a", "r0")
	if job.State != ClaimJob {
		t.Fatalf("claim got %+v", job)
	}
	body, feed := io.Pipe()
	req, _ := http.NewRequest("PUT", "http://coordinator/result/"+job.Attempt, body)
	req.Header.Set("Authorization", "Bearer tok-a")
	done := make(chan struct{})
	go func() {
		if resp, err := NewClient(r.sock, "tok-a").HTTP.Do(req); err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	feed.Write([]byte("{"))
	waitFor(t, func() bool {
		r.srv.mu.Lock()
		defer r.srv.mu.Unlock()
		return r.srv.busy["a"] == 1
	})
	// Its attempt may already be out of live; the upload still counts.
	r.srv.mu.Lock()
	delete(r.srv.live, job.Attempt)
	r.srv.mu.Unlock()
	if r.srv.Drain("a", false) {
		t.Error("drained while an upload was in progress")
	}
	feed.Close()
	<-done
	if !r.srv.Drain("a", false) {
		t.Error("control: not idle once the upload was answered")
	}
}

func TestWorker_ResultWithHTMLCharactersIsAccepted(t *testing.T) {
	c := combo("8.3")
	r := newRig(t, []matrix.Combination{c})
	var recorded atomic.Bool
	r.worker("tok-a", func(_ context.Context, c matrix.Combination, cfg runner.RunConfig) (bool, error) {
		cfg.OnProject("m2test-p1")
		_, err := os.Stat(filepath.Join(filepath.Dir(cfg.ResultsDir), "project"))
		recorded.Store(err == nil)
		res := goodResult(c, result.StatusPass)
		res.Steps["install"] = result.Step{Status: result.StatusPass, Log: "a && b > c <x>"}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			return false, err
		}
		path := filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json")
		os.MkdirAll(filepath.Dir(path), 0o700)
		return true, os.WriteFile(path, buf.Bytes(), 0o600)
	})
	waitFor(t, func() bool { return r.src.Done() && r.srv.Live() == 0 })
	if st := r.status(c); st != queue.StatusPass {
		t.Errorf("status %s, want pass", st)
	}
	if !recorded.Load() {
		t.Error("the run's project wasn't recorded in the spool")
	}
}

func TestClaim_AWorkerFailingEveryRunIsBenched(t *testing.T) {
	old := BenchFor
	BenchFor = 300 * time.Millisecond
	defer func() { BenchFor = old }()
	first := combo("8.1")
	r := newRig(t, []matrix.Combination{first, combo("8.2"), combo("8.3"), combo("8.4"), combo("8.5")})
	// A result in between starts the count again.
	for i := range FailingAfter - 1 {
		job := r.claim("tok-a", fmt.Sprint("early", i))
		r.upload("tok-a", Upload{Schema: Schema, Attempt: job.Attempt, Outcome: OutcomeHost, Reason: "flaky"})
	}
	job := r.claim("tok-a", "good")
	if rep, _ := r.upload("tok-a", resultUpload(t, job.Attempt, goodResult(job.Combo, result.StatusPass))); rep.Status != "installed" {
		t.Fatalf("good result: %s", rep.Status)
	}
	for i := range FailingAfter {
		job := r.claim("tok-a", fmt.Sprint("req", i))
		if job.State != ClaimJob {
			t.Fatalf("claim %d: %s", i, job.State)
		}
		if rep, err := r.upload("tok-a", Upload{Schema: Schema, Attempt: job.Attempt, Outcome: OutcomeHost, Reason: "docker: cannot connect"}); err != nil || rep.Status != "accepted" {
			t.Fatalf("host failure %d: %v %v", i, rep, err)
		}
	}
	if rep := r.claim("tok-a", "late"); rep.State != ClaimNone {
		t.Errorf("benched worker got %s", rep.State)
	}
	if rep := r.claim("tok-b", "other"); rep.State != ClaimJob {
		t.Errorf("control: another worker got %s", rep.State)
	}
	// A sole worker benched for good would leave the sweep waiting forever.
	time.Sleep(BenchFor)
	if rep := r.claim("tok-a", "later"); rep.State != ClaimJob {
		t.Errorf("bench lapsed, worker got %s", rep.State)
	}
}

func TestClaim_ExpiriesDontBenchAWorker(t *testing.T) {
	old := LeaseFor
	LeaseFor = 50 * time.Millisecond
	defer func() { LeaseFor = old }()
	r := newRig(t, []matrix.Combination{combo("8.1"), combo("8.2"), combo("8.3"), combo("8.4")})
	for i := range FailingAfter {
		if job := r.claim("tok-a", fmt.Sprint("req", i)); job.State != ClaimJob {
			t.Fatalf("claim %d: %s", i, job.State)
		}
	}
	waitFor(t, func() bool { return r.srv.Live() == 0 })
	if rep := r.claim("tok-a", "after"); rep.State != ClaimJob {
		t.Errorf("after %d expiries the worker got %s", FailingAfter, rep.State)
	}
}

func TestWorker_StacksSitBesideATrailingSlashSpool(t *testing.T) {
	if got := (&Worker{Spool: "/data/spool/"}).stacksDir(); got != "/data/spool-stacks" {
		t.Errorf("stacksDir = %q, want it beside the spool", got)
	}
}

type answer func(*http.Request) int

func (f answer) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f(r), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
}

func TestWorker_AnUnwritableResultDoesntHoldUpTheRest(t *testing.T) {
	a1, a2 := strings.Repeat("a", 24), strings.Repeat("b", 24)
	c := NewClient("/nonexistent", "tok-a")
	c.HTTP.Transport = answer(func(r *http.Request) int {
		if strings.HasSuffix(r.URL.Path, "/"+a1) {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	w := &Worker{Client: c, Spool: t.TempDir(), Log: quiet}
	for _, a := range []string{a1, a2} {
		if err := w.spool(a, Upload{Schema: Schema, Attempt: a, Outcome: OutcomeResult}); err != nil {
			t.Fatal(err)
		}
	}
	w.uploadSpool(context.Background())
	_, kept := os.Stat(filepath.Join(w.Spool, a1))
	_, sent := os.Stat(filepath.Join(w.Spool, a2))
	if kept != nil || !os.IsNotExist(sent) {
		t.Errorf("a1 kept: %v; a2 left: %v", kept, sent)
	}
}
