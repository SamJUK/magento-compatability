// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
)

func TestRunOutcome(t *testing.T) {
	boom := errors.New("docker: name in use")
	for _, tc := range []struct {
		wrote bool
		err   error
		want  queue.OutcomeKind
	}{
		{true, nil, queue.OutcomeResult},
		{true, boom, queue.OutcomeResult}, // a result was written; it counts
		{false, boom, queue.OutcomeHost},
		{false, nil, queue.OutcomeResult},
	} {
		if got := runOutcome(tc.wrote, tc.err); got.Kind != tc.want {
			t.Errorf("runOutcome(%v, %v) = %v, want %v", tc.wrote, tc.err, got.Kind, tc.want)
		}
	}
}

func TestKeepIDsRefusesAnIDTwoCombinationsShare(t *testing.T) {
	a := matrix.Combination{Product: "magento", Version: "2.4.8", PHP: "8.3", WebserverType: "nginx", WebserverVersion: "1.26"}
	b := a
	b.WebserverVersion = "1.28"
	if a.ID() != b.ID() {
		t.Skip("IDs now tell them apart")
	}
	path := filepath.Join(t.TempDir(), "ids")
	os.WriteFile(path, []byte(a.ID()+"\n"), 0o600)
	if kept, err := keepIDs([]matrix.Combination{a}, path); err != nil || len(kept) != 1 {
		t.Fatalf("control: %v %v", kept, err)
	}
	if kept, err := keepIDs([]matrix.Combination{a, b}, path); err == nil {
		t.Errorf("kept %v of two sharing an ID", kept)
	}
}

func TestLeftoverStackRecordOutlivesANetworkStillInUse(t *testing.T) {
	for _, held := range []bool{false, true} {
		bin := t.TempDir()
		// Every docker call succeeds; the network is listed while held.
		script := "#!/bin/sh\nexit 0\n"
		if held {
			script = "#!/bin/sh\n[ \"$1 $2\" = \"network ls\" ] && echo net1\nexit 0\n"
		}
		os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755)
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		stacks := t.TempDir()
		os.WriteFile(filepath.Join(stacks, "m2test-x"), nil, 0o644)
		removeLeftoverStacks(context.Background(), log.New(io.Discard, "", 0), stacks)
		if _, err := os.Stat(filepath.Join(stacks, "m2test-x")); (err == nil) != held {
			t.Errorf("network held %v: record kept %v", held, err == nil)
		}
	}
}
