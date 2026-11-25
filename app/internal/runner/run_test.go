// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

type fakeDocker struct {
	mu       sync.Mutex
	calls    []string
	slowDown bool
	diskFull bool // up fails at once for want of disk, and ps -a is slow
}

func (f *fakeDocker) exec(ctx context.Context, name string, args ...string) *exec.Cmd {
	call := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if strings.Contains(call, " exec -T php-fpm ") && !strings.Contains(call, "smoke.sh") {
		return exec.CommandContext(ctx, "sh", "-c", "exec sleep 30")
	}
	if f.diskFull && strings.Contains(call, " up -d ") {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'no space left on device'; exit 1")
	}
	if f.diskFull && strings.Contains(call, " ps -a") {
		return exec.CommandContext(ctx, "sh", "-c", "exec sleep 1")
	}
	if f.slowDown && strings.Contains(call, " down ") {
		return exec.CommandContext(ctx, "sh", "-c", "exec sleep 2")
	}
	return exec.CommandContext(ctx, "true")
}

func (f *fakeDocker) called(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func runWithFakeDocker(t *testing.T, ctx context.Context, cfg RunConfig, fake *fakeDocker) (*fakeDocker, string, bool, error) {
	t.Helper()
	stubExec = fake.exec
	t.Cleanup(func() { stubExec = nil })

	c := matrix.Combination{
		Product: "magento", Version: "2.4.8", PHP: "8.3",
		WebserverType: "nginx", WebserverVersion: "1.28",
		DBType: "mariadb", DBVersion: "11.4",
		SearchType: "opensearch", SearchVersion: "3",
		CacheType: "valkey", CacheVersion: "8",
		QueueType: "rabbitmq", QueueVersion: "4.2",
		Varnish: "none",
	}
	_, file, _, _ := runtime.Caller(0)
	cfg.ComposeDir = filepath.Join(filepath.Dir(file), "..", "..", "..", "docker", "compose")
	cfg.ResultsDir = t.TempDir()
	ran, err := Run(ctx, c, cfg)
	return fake, filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json"), ran, err
}

func TestRun_DeadlineFailsRunningStepAndWritesResult(t *testing.T) {
	var project string
	fake, path, ran, err := runWithFakeDocker(t, context.Background(), RunConfig{
		Deadline:  time.Second,
		OnProject: func(name string) { project = name },
	}, &fakeDocker{})
	if err != nil || !ran {
		t.Fatalf("Run() = %v, %v; want ran with no error", ran, err)
	}
	if !strings.HasPrefix(project, "m2test-") {
		t.Errorf("OnProject got %q", project)
	}

	var r result.TestResult
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("no result written: %v", readErr)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Steps["stack_up"].Status != result.StatusPass {
		t.Errorf("stack_up = %+v, want pass", r.Steps["stack_up"])
	}
	install := r.Steps["install"]
	if install.Status != result.StatusFail || install.Failure == nil || install.Failure.Code != "deadline_exceeded" {
		t.Errorf("install = %+v, want fail with deadline_exceeded", install)
	}
	if _, ok := r.Steps["smoke"]; ok {
		t.Error("smoke ran after the deadline")
	}
	if r.OverallStatus != result.StatusFail {
		t.Errorf("overall = %q, want fail", r.OverallStatus)
	}
	if !fake.called(" down --volumes") {
		t.Error("stack was not torn down")
	}
}

func TestRun_DiagnosticsPastTheDeadlineKeepTheStepsFailure(t *testing.T) {
	_, path, ran, err := runWithFakeDocker(t, context.Background(), RunConfig{Deadline: 500 * time.Millisecond}, &fakeDocker{diskFull: true})
	if err != nil || !ran {
		t.Fatalf("Run() = %v, %v; want ran with no error", ran, err)
	}
	var r result.TestResult
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("no result written: %v", readErr)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if f := r.Steps["stack_up"].Failure; f == nil || f.Code != "disk_space" {
		t.Errorf("stack_up failure = %+v, want disk_space", f)
	}
}

func TestRun_CancelWritesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fake, path, ran, err := runWithFakeDocker(t, ctx, RunConfig{Deadline: time.Hour}, &fakeDocker{})
	if err != nil || ran {
		t.Fatalf("Run() = %v, %v; want not ran, no error", ran, err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cancelled run wrote a result (stat err %v)", statErr)
	}
	if !fake.called(" exec -T php-fpm ") {
		t.Error("install never started, so this proves nothing")
	}
	if !fake.called(" down --volumes") {
		t.Error("stack was not torn down")
	}
}

// A cancel that lands during teardown, after the result is written, must
// still report the run as done.
func TestRun_CancelDuringTeardownKeepsResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, path, ran, err := runWithFakeDocker(t, ctx, RunConfig{Deadline: time.Second}, &fakeDocker{slowDown: true})
	if ctx.Err() == nil {
		t.Fatal("teardown finished before the cancel, so this proves nothing")
	}
	if err != nil || !ran {
		t.Errorf("Run() = %v, %v; want ran", ran, err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("result missing: %v", statErr)
	}
}

// Every failure the classifier or Run can record must be listed in
// FailureCodes with the same category, and nothing else may be listed.
func TestFailureCodes_MatchSource(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "runner.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Failure" {
			return true
		}
		fields := map[string]string{}
		for _, e := range lit.Elts {
			kv := e.(*ast.KeyValueExpr)
			if s, ok := kv.Value.(*ast.BasicLit); ok && s.Kind == token.STRING {
				fields[kv.Key.(*ast.Ident).Name], _ = strconv.Unquote(s.Value)
			}
		}
		if fields["Code"] == "" || fields["Category"] == "" {
			t.Errorf("result.Failure literal without literal Code and Category: %v", fields)
		}
		found[fields["Code"]] = fields["Category"]
		return true
	})
	if len(found) < 30 {
		t.Fatalf("found only %d codes in runner.go; parser check is broken", len(found))
	}
	for code, cat := range found {
		if FailureCodes[code] != cat {
			t.Errorf("FailureCodes[%q] = %q, source says %q", code, FailureCodes[code], cat)
		}
	}
	for code := range FailureCodes {
		if _, ok := found[code]; !ok {
			t.Errorf("FailureCodes lists %q, which nothing records", code)
		}
	}
}
