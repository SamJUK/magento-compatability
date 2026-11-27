// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

func TestFailedQueueCheckCarriesTheBrokerLog(t *testing.T) {
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if slices.Contains(args, "logs") {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'AMQ229119: Free storage space is at 9.2GB'")
			}
			return exec.CommandContext(ctx, "sh", "-c", "echo '[FAIL] queue: scheduleBulk returned false, the message was not published'; exit 1")
		},
	}
	check := serviceChecks(matrix.Combination{})[0]

	status, log := runServiceCheck(context.Background(), cp, check)

	if status != result.StatusFail {
		t.Fatalf("status = %q, want fail", status)
	}
	if !strings.Contains(log, "AMQ229119") {
		t.Fatalf("log = %q, want the broker log appended", log)
	}
	if got := classifyStepFailure(check.name, log); got == nil || got.Code != "disk_space" {
		t.Fatalf("classification = %#v, want disk_space", got)
	}
}
