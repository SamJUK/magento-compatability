// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/result"
)

func TestClassifyQueueFailures(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
	}{
		{"publish fails", "[FAIL] queue: scheduleBulk returned false, the message was not published", "queue_publish_failed"},
		{"artemis blocks producers on a full disk", "[FAIL] queue: scheduleBulk returned false, the message was not published\n=== queue container log ===\nERROR [org.apache.activemq.artemis.core.server] AMQ229119: Free storage space is at 9.2GB of 206.8GB total.", "disk_space"},
		{"consumer delivers nothing", "[OK] queue: published bulk abc\n[FAIL] queue: bulk status is 0, expected 2", "queue_not_consumed"},
		{"consumer times out", "[FAIL] queue: the consumer exited with 143 after the message was published (15 or 143 means a signal stopped it, probably the 120 second timeout)", "queue_not_consumed"},
		{"applied before a consumer ran", "[ERROR] queue: the update was already applied before any consumer ran", "check_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStepFailure("queue", tc.log)
			if got == nil || got.Code != tc.want {
				t.Fatalf("classifyStepFailure(queue) = %#v, want code %s", got, tc.want)
			}
		})
	}

	if got := classifyStepFailure("queue", "Fatal error: something unrelated"); got != nil {
		t.Fatalf("classifyStepFailure(queue) = %#v, want nil for an unclassified log", got)
	}
}

func TestQueueFailureKeepsItsClassWhenTheBrokerLogIsSlow(t *testing.T) {
	timeout := serviceCheckTimeout
	serviceCheckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { serviceCheckTimeout = timeout })
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if strings.Contains(strings.Join(args, " "), "logs") {
				return exec.CommandContext(ctx, "sh", "-c", "sleep 0.5; echo broker log")
			}
			return exec.CommandContext(ctx, "sh", "-c", "echo '[FAIL] queue: bulk status is 0, expected 2' >&2; exit 1")
		},
	}

	status, log := runServiceCheck(context.Background(), cp, serviceCheck{name: "queue", argv: []string{"php", "x"}, service: "queue"})

	if status != result.StatusFail || !strings.Contains(log, "broker log") {
		t.Fatalf("status %q, log:\n%s", status, log)
	}
	if got := classifyStepFailure("queue", log); got == nil || got.Code != "queue_not_consumed" {
		t.Fatalf("classification = %#v, want queue_not_consumed; log:\n%s", got, log)
	}
}
