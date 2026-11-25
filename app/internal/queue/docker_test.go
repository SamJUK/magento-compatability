// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/runner"
)

func TestRemoveStack_PlaywrightContainerGoesFirst(t *testing.T) {
	var calls []string
	command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := strings.Join(args, " ")
		calls = append(calls, c)
		if strings.HasPrefix(c, "ps -aq") && strings.Contains(c, "m2test.role=playwright") {
			return exec.CommandContext(ctx, "echo", "abc123")
		}
		return exec.CommandContext(ctx, "true")
	}
	defer func() { command = runner.Command }()
	if err := RemoveStack(context.Background(), "m2test-0123456789abcdef-01234567"); err != nil {
		t.Fatal(err)
	}
	rm, down := -1, -1
	for i, c := range calls {
		switch {
		case c == "rm --force abc123":
			rm = i
		case strings.HasPrefix(c, "compose -p m2test-0123456789abcdef-01234567 down"):
			down = i
		}
	}
	if rm < 0 || down < rm {
		t.Errorf("rm at %d, down at %d:\n%s", rm, down, strings.Join(calls, "\n"))
	}
}

func TestRemoveStack_StopsAtTheDeadlineDespiteAPlugin(t *testing.T) {
	old := runner.WaitDelay
	runner.WaitDelay = 100 * time.Millisecond
	defer func() { runner.WaitDelay = old }()
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nsleep 10 &\nwait\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := RemoveStack(ctx, "m2test-0123456789abcdef-01234567"); err == nil {
		t.Error("control: a killed docker reported success")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("returned after %v, held by the plugin", d)
	}
}

func TestStackExists_WhileOnlyItsNetworkIsLeft(t *testing.T) {
	command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if strings.HasPrefix(strings.Join(args, " "), "network ls") {
			return exec.CommandContext(ctx, "echo", "net1")
		}
		return exec.CommandContext(ctx, "true")
	}
	defer func() { command = runner.Command }()
	if up, err := StackExists(context.Background(), "m2test-x"); err != nil || !up {
		t.Errorf("exists %v: %v", up, err)
	}
}

func TestRemoveStack_FailsWhileItsNetworkIsHeld(t *testing.T) {
	for _, held := range []bool{false, true} {
		command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if held && strings.HasPrefix(strings.Join(args, " "), "network ls") {
				return exec.CommandContext(ctx, "echo", "net1")
			}
			return exec.CommandContext(ctx, "true")
		}
		if err := RemoveStack(context.Background(), "m2test-x"); (err != nil) != held {
			t.Errorf("network held %v: %v", held, err)
		}
	}
	command = runner.Command
}
