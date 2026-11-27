// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"context"
	"log"
	"os"
	"path/filepath"
)

// Stacks is a directory with one empty file for each compose project a run
// started. A record stays until its stack is confirmed gone, so a restart
// can remove what a killed process left behind.
type Stacks struct {
	Dir string
	Log *log.Logger
}

func (s Stacks) Record(name string) {
	err := os.MkdirAll(s.Dir, 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(s.Dir, name), nil, 0o600)
	}
	if err != nil {
		s.Log.Printf("recording stack %s: %v", name, err)
	}
}

// ForgetIfGone drops the record once the stack is gone: teardown can fail or
// time out without an error, so it is checked.
func (s Stacks) ForgetIfGone(ctx context.Context, name string) {
	if ctx.Err() != nil {
		return
	}
	if up, err := StackExists(ctx, name); err == nil && !up {
		os.Remove(filepath.Join(s.Dir, name))
	}
}

// RemoveLeftovers tears down every recorded stack and drops its record, and
// returns their names. A stack that won't go keeps its record and is passed
// to failed.
func (s Stacks) RemoveLeftovers(ctx context.Context, failed func(name string, err error)) (names []string) {
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		name := e.Name()
		names = append(names, name)
		s.Log.Printf("removing leftover stack %s", name)
		if err := RemoveStack(ctx, name); err != nil {
			failed(name, err)
			continue
		}
		os.Remove(filepath.Join(s.Dir, name))
	}
	return names
}
