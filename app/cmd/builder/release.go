// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/samjuk/magento-compatability/internal/images"
	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/release"
)

var resolve = images.Resolve

// sweepRelease returns the snapshot a sweep runs from: its earlier one unless fresh, else (with workers) a new one with images pinned.
// Without workers or an earlier snapshot it returns "" and runs use the checkout.
func sweepRelease(lg *queue.Ledger, sel *selection, combos []matrix.Combination, fresh, workers bool, dir string) (string, error) {
	prev, ok, err := queue.SweepContract(lg, sel.resultsDir, combos)
	if err != nil {
		return "", err
	}
	if ok && !fresh && prev.Release != "" {
		if got, err := release.Hash(filepath.Join(dir, prev.Release)); err != nil || got != prev.Release {
			return "", fmt.Errorf("this sweep runs release %s, which is gone or changed in %s; -fresh snapshots the checkout again", prev.Release, dir)
		}
		return prev.Release, nil
	}
	if !workers {
		return "", nil
	}
	if ok && !fresh {
		return "", errors.New("this sweep started without -workers, so it runs the checkout; -fresh starts it over on a release")
	}
	if sel.composeDir != filepath.Join(sel.repoRoot, "docker", "compose") {
		return "", errors.New("-compose-dir can't be used with -workers: workers run a snapshot of the checkout's docker/")
	}
	plain, err := release.Snapshot(sel.repoRoot, dir, nil)
	if err != nil {
		return "", err
	}
	plainDir := filepath.Join(dir, plain)
	// Without images.json no worker can run it.
	defer os.Remove(plainDir + ".tar.gz")
	defer os.RemoveAll(plainDir)
	refs, harness, pw, err := images.Plan(plainDir, combos, sel.playwright)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pinned, err := resolve(ctx, refs)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(images.Manifest{Pinned: pinned, Harness: harness, Playwright: pw}, "", "  ")
	if err != nil {
		return "", err
	}
	return release.Snapshot(plainDir, dir, map[string][]byte{images.File: data})
}
