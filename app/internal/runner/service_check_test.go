// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/result"
)

func TestRunServiceCheckStatus(t *testing.T) {
	cases := []struct {
		name string
		exit string
		want string
	}{
		{"exits 0", "0", result.StatusPass},
		{"exits with the skip code", "3", result.StatusSkip},
		{"exits 1", "1", result.StatusFail},
		{"exits 124 after a timeout", "124", result.StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			cp := &Compose{
				projectName: "m2test-abcd",
				files:       []string{"/tmp/base.yml"},
				execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					got = append([]string{name}, args...)
					return exec.CommandContext(ctx, "sh", "-c", "echo output; exit "+tc.exit)
				},
			}

			status, log := runServiceCheck(context.Background(), cp, serviceCheck{name: "queue", argv: []string{"php", "/scripts/tests/check-queue.php"}})

			if status != tc.want {
				t.Fatalf("status = %q, want %q", status, tc.want)
			}
			if !strings.Contains(log, "output") {
				t.Fatalf("log = %q, want the command output", log)
			}
			wantTail := []string{"exec", "-T", "php-fpm", "bash", "/scripts/tests/run-check.sh", "php", "/scripts/tests/check-queue.php"}
			if !reflect.DeepEqual(got[len(got)-len(wantTail):], wantTail) {
				t.Fatalf("command = %v, want it to end with %v", got, wantTail)
			}
		})
	}
}

func TestClassifyCheckError(t *testing.T) {
	got := classifyStepFailure("queue", "[ERROR] queue: consumer exited with 1\n[check] exit 2 after 4s")
	if got == nil || got.Code != "check_error" || got.Category != "harness" || !got.LikelyFlaky {
		t.Fatalf("classifyStepFailure = %#v, want a flaky harness check_error", got)
	}

	if got := classifyStepFailure("queue", "[FAIL] queue: the message was never delivered"); got != nil && got.Code == "check_error" {
		t.Fatalf("classifyStepFailure of a plain failure = %#v, want anything but check_error", got)
	}
}

func stubCompose(script string) *Compose {
	return &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", script)
		},
	}
}

func TestRunServiceCheckClassifiesWhatTheScriptPrints(t *testing.T) {
	cases := []struct {
		name, script, want string
	}{
		{"queue", "echo '[ERROR] queue: the update was applied before a consumer ran' >&2; exit 2", "check_error"},
		{"cron", "echo '[ERROR] cron: cron:run exited with 1' >&2; exit 2", "check_error"},
		{"queue", "echo '[FAIL] queue: uncaught RuntimeException: boom' >&2; exit 1", "check_exception"},
		{"cron", "echo '[FAIL] cron: uncaught fatal error: out of memory' >&2; exit 1", "check_exception"},
		{"cron", "echo '[FAIL] cron: uncaught PDOException: No space left on device' >&2; exit 1", "disk_space"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" "+tc.want, func(t *testing.T) {
			status, log := runServiceCheck(context.Background(), stubCompose(tc.script), serviceCheck{name: tc.name})

			if status != result.StatusFail {
				t.Fatalf("status = %q, want fail", status)
			}
			got := classifyStepFailure(tc.name, log)
			if got == nil || got.Code != tc.want || (tc.want == "check_exception" && got.LikelyFlaky) {
				t.Fatalf("classification = %#v, want %s", got, tc.want)
			}
		})
	}
}

func TestRunServiceCheckRecordsATimeout(t *testing.T) {
	timeout := serviceCheckTimeout
	serviceCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serviceCheckTimeout = timeout })

	status, log := runServiceCheck(context.Background(), stubCompose("echo started; exec sleep 5"), serviceCheck{name: "cron"})

	if status != result.StatusFail {
		t.Fatalf("status = %q, want fail", status)
	}
	if got := classifyStepFailure("cron", log); got == nil || got.Code != "check_error" {
		t.Fatalf("classification = %#v, want check_error; log:\n%s", got, log)
	}
}

func TestRunServiceCheckLeavesACancelledRunAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, log := runServiceCheck(ctx, stubCompose("exec sleep 5"), serviceCheck{name: "cron"})

	if strings.Contains(log, "timed out") {
		t.Fatalf("a cancelled run was recorded as a check timeout:\n%s", log)
	}
}
