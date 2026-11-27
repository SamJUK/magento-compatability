// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samjuk/magento-compatability/internal/images"
	rel "github.com/samjuk/magento-compatability/internal/release"
)

func TestBaseConfig_NoPlaywrightImageSkipsPlaywright(t *testing.T) {
	if c := baseConfig("/r", images.Manifest{}); c.PlaywrightDir != "" || c.PlaywrightImage != "" {
		t.Errorf("playwright off, got %+v", c)
	}
	if c := baseConfig("/r", images.Manifest{Playwright: "pw@sha256:x"}); c.PlaywrightDir == "" || c.PlaywrightImage != "pw@sha256:x" {
		t.Errorf("control: playwright on, got %+v", c)
	}
}

func TestPrepare_RefusesATarballThatIsNotTheRelease(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "matrix.yml"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(src, "docker"), 0o755)
	// An empty manifest: nothing to pull or build, so no docker calls.
	hash, err := rel.Snapshot(src, out, map[string][]byte{"images.json": []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(out, hash+".tar.gz")
	releases := t.TempDir()
	if _, err := prepareRelease(context.Background(), tarball, releases, "other"); err == nil {
		t.Error("prepared a release other than the one asked for")
	}
	if entries, _ := os.ReadDir(releases); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	if dir, err := prepareRelease(context.Background(), tarball, t.TempDir(), hash); err != nil || filepath.Base(dir) != hash {
		t.Errorf("control: %q %v", dir, err)
	}
}
