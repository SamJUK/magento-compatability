// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/runner"
)

func TestDockerStopsAtTheDeadlineDespiteAPlugin(t *testing.T) {
	old := runner.WaitDelay
	runner.WaitDelay = 100 * time.Millisecond
	defer func() { runner.WaitDelay = old }()
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nsleep 10 &\nwait\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := docker(ctx, "build"); err == nil {
		t.Error("control: a killed docker reported success")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("returned after %v, held by the plugin", d)
	}
}
