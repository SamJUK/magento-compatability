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
	resuming := ok && !fresh && prev.Release != ""
	if (resuming || workers) && !sameDir(sel.composeDir, filepath.Join(sel.repoRoot, "docker", "compose")) {
		return "", errors.New("-compose-dir can't be used on a release: it runs the snapshot's docker/")
	}
	if resuming {
		if got, err := release.Hash(filepath.Join(dir, prev.Release)); err != nil || got != prev.Release {
			return "", fmt.Errorf("this sweep runs release %s, which is gone or changed in %s; -fresh snapshots the checkout again", prev.Release, dir)
		}
		if err := coversCombos(filepath.Join(dir, prev.Release), combos); err != nil {
			return "", err
		}
		if m, err := images.Load(filepath.Join(dir, prev.Release)); err != nil {
			return "", err
		} else if sel.buildImages && !m.Build {
			return "", errors.New("this sweep's release pulls its harness images; -fresh makes one that builds them")
		}
		if workers {
			return prev.Release, release.Pack(filepath.Join(dir, prev.Release))
		}
		return prev.Release, nil
	}
	if !workers {
		return "", nil
	}
	if ok && !fresh {
		return "", errors.New("this sweep started without -workers, so it runs the checkout; -fresh starts it over on a release")
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
	if err == nil {
		// An unlocked base fails here, not on every worker.
		_, err = images.Freeze(plainDir, images.Manifest{Harness: harness})
	}
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pinned, err := resolve(ctx, refs)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(images.Manifest{Pinned: pinned, Harness: harness, Playwright: pw, Build: sel.buildImages}, "", "  ")
	if err != nil {
		return "", err
	}
	// In the hash, so a matrix edit or -matrix file that keeps the IDs
	// can't run under the same release.
	cs, err := json.MarshalIndent(combos, "", "  ")
	if err != nil {
		return "", err
	}
	return release.Snapshot(plainDir, dir, map[string][]byte{images.File: data, combosFile: cs})
}

const combosFile = "combos.json"

func sameDir(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)
	return errA == nil && errB == nil && a == b
}

// coversCombos checks a release was made for exactly these combinations:
// matrix.yml can change one without changing its ID.
func coversCombos(dir string, combos []matrix.Combination) error {
	data, err := os.ReadFile(filepath.Join(dir, combosFile))
	if err != nil {
		return fmt.Errorf("this sweep's release predates %s; -fresh snapshots the checkout again: %w", combosFile, err)
	}
	var made []matrix.Combination
	if err := json.Unmarshal(data, &made); err != nil {
		return err
	}
	byID := map[string]matrix.Combination{}
	for _, c := range made {
		byID[c.ID()] = c
	}
	for _, c := range combos {
		if byID[c.ID()] != c {
			return fmt.Errorf("%s isn't what this sweep's release was made for: the matrix changed since it started; -fresh snapshots the checkout again", c.ID())
		}
	}
	return nil
}
