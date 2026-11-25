// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samjuk/magento-compatability/internal/images"
	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/release"
)

func releaseRig(t *testing.T) (*queue.Ledger, *selection, []matrix.Combination, string) {
	t.Helper()
	calls := 0
	resolve = func(_ context.Context, refs []string) (map[string]string, error) {
		calls++
		out := map[string]string{}
		for _, r := range refs {
			out[r] = fmt.Sprintf("sha256:%064x", calls)
		}
		return out, nil
	}
	t.Cleanup(func() { resolve = images.Resolve })
	repo, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	sel := &selection{repoRoot: repo, composeDir: filepath.Join(repo, "docker", "compose"), resultsDir: t.TempDir(), playwright: true}
	lg, err := queue.OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	c := matrix.Combination{Product: "magento", Version: "2.4.8", PHP: "8.4", WebserverType: "nginx", WebserverVersion: "1.28",
		DBType: "mariadb", DBVersion: "11.4", SearchType: "opensearch", SearchVersion: "2.19.5",
		CacheType: "valkey", CacheVersion: "8", QueueType: "rabbitmq", QueueVersion: "4.1", Varnish: "none"}
	return lg, sel, []matrix.Combination{c}, t.TempDir()
}

func start(t *testing.T, lg *queue.Ledger, sel *selection, combos []matrix.Combination, hash string) {
	t.Helper()
	c := sel.contract()
	c.Release = hash
	src, err := queue.NewLocal(lg, sel.resultsDir, combos, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRelease_WorkersSnapshotAndResumeReusesIt(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	first, err := sweepRelease(lg, sel, combos, false, true, dir)
	if err != nil || first == "" {
		t.Fatalf("first = %q %v", first, err)
	}
	m, err := images.Load(filepath.Join(dir, first))
	if err != nil || m.Pinned["mariadb:11.4"] == "" || m.Playwright == "" {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if h, _ := release.Hash(filepath.Join(dir, first)); h != first {
		t.Errorf("release dir hashes to %s, named %s", h, first)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("want the release and its tarball, got %v", entries)
	}
	start(t, lg, sel, combos, first)

	// Digests moved upstream since; a resume must not notice.
	again, err := sweepRelease(lg, sel, combos, false, true, dir)
	if err != nil || again != first {
		t.Errorf("resume = %q %v, want %s", again, err, first)
	}
	local, err := sweepRelease(lg, sel, combos, false, false, dir)
	if err != nil || local != first {
		t.Errorf("local-only resume = %q %v, want %s", local, err, first)
	}
	fresh, err := sweepRelease(lg, sel, combos, true, true, dir)
	if err != nil || fresh == first {
		t.Errorf("fresh = %q %v, want a new snapshot", fresh, err)
	}
}

func TestSweepRelease_LocalOnlyRunsTheCheckout(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	if h, err := sweepRelease(lg, sel, combos, false, false, dir); h != "" || err != nil {
		t.Errorf("got %q %v", h, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("snapshot taken: %v", entries)
	}
}

func TestSweepRelease_GoneReleaseNeedsFresh(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	h, _ := sweepRelease(lg, sel, combos, false, true, dir)
	start(t, lg, sel, combos, h)
	os.RemoveAll(filepath.Join(dir, h))
	if _, err := sweepRelease(lg, sel, combos, false, true, dir); err == nil {
		t.Error("resumed a release that is gone")
	}
}

func TestSweepRelease_RefusesOtherComposeDir(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	sel.composeDir = t.TempDir()
	if _, err := sweepRelease(lg, sel, combos, false, true, dir); err == nil {
		t.Error("snapshot ignored -compose-dir")
	}
}

func TestSweepRelease_CheckoutSweepCantTakeWorkersMidway(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	start(t, lg, sel, combos, "")
	if _, err := sweepRelease(lg, sel, combos, false, true, dir); err == nil {
		t.Error("a checkout sweep resumed on a release")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("snapshot taken: %v", entries)
	}
	if h, err := sweepRelease(lg, sel, combos, true, true, dir); err != nil || h == "" {
		t.Errorf("-fresh: %q %v", h, err)
	}
}

func TestSweepRelease_FailedResolveLeavesNothing(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	resolve = func(context.Context, []string) (map[string]string, error) { return nil, os.ErrDeadlineExceeded }
	if _, err := sweepRelease(lg, sel, combos, false, true, dir); err == nil {
		t.Fatal("no error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestSweepRelease_ChangedReleaseNeedsFresh(t *testing.T) {
	lg, sel, combos, dir := releaseRig(t)
	h, _ := sweepRelease(lg, sel, combos, false, true, dir)
	start(t, lg, sel, combos, h)
	os.WriteFile(filepath.Join(dir, h, "matrix.yml"), []byte("edited"), 0o644)
	if _, err := sweepRelease(lg, sel, combos, false, true, dir); err == nil {
		t.Error("resumed a release that no longer hashes to its name")
	}
}
