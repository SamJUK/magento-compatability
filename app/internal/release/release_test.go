// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range map[string]string{
		"matrix.yml":              "products: {}\n",
		"docker/compose/base.yml": "services: {}\n",
		"docker/scripts/tests/playwright/a.spec.ts":               "test\n",
		"docker/scripts/tests/playwright/node_modules/x/index.js": "junk\n",
		"docker/scripts/tests/playwright/test-results/r.json":     "junk\n",
		"README.md": "not in a release\n",
	} {
		if err := put(root, rel, []byte(body), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := put(root, "docker/scripts/install.sh", []byte("#!/bin/sh\n"), true); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSnapshot_RoundTrip(t *testing.T) {
	repo, dir := checkout(t), t.TempDir()
	hash, err := Snapshot(repo, dir, map[string][]byte{"images.json": []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Hash(filepath.Join(dir, hash))
	if err != nil || got != hash {
		t.Fatalf("Hash of the snapshot = %s %v, want %s", got, err, hash)
	}
	for _, rel := range []string{"README.md", "docker/scripts/tests/playwright/node_modules", "docker/scripts/tests/playwright/test-results"} {
		if _, err := os.Stat(filepath.Join(dir, hash, rel)); err == nil {
			t.Errorf("%s made it into the release", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, hash, "images.json")); err != nil {
		t.Errorf("extra file missing: %v", err)
	}

	worker := t.TempDir()
	ext, err := Extract(filepath.Join(dir, hash+".tar.gz"), worker)
	if err != nil || ext != hash {
		t.Fatalf("Extract = %s %v, want %s", ext, err, hash)
	}
	info, err := os.Stat(filepath.Join(worker, hash, "docker/scripts/install.sh"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("install.sh lost its exec bit: %v %v", info, err)
	}
}

func TestSnapshot_SameTreeSameTarball(t *testing.T) {
	repo := checkout(t)
	a, b := t.TempDir(), t.TempDir()
	ha, _ := Snapshot(repo, a, nil)
	hb, _ := Snapshot(repo, b, nil)
	ta, _ := os.ReadFile(filepath.Join(a, ha+".tar.gz"))
	tb, _ := os.ReadFile(filepath.Join(b, hb+".tar.gz"))
	if ha != hb || string(ta) != string(tb) {
		t.Error("one tree gave two releases")
	}
	again, err := Snapshot(repo, a, nil)
	if err != nil || again != ha {
		t.Errorf("a second snapshot into the same dir = %s %v", again, err)
	}
}

func TestHash_ChangesWithContentAndExecBit(t *testing.T) {
	repo, dir := checkout(t), t.TempDir()
	base, _ := Snapshot(repo, dir, nil)
	os.WriteFile(filepath.Join(repo, "matrix.yml"), []byte("products: {x: 1}\n"), 0o644)
	edited, _ := Snapshot(repo, dir, nil)
	os.Chmod(filepath.Join(repo, "docker/compose/base.yml"), 0o755)
	chmodded, _ := Snapshot(repo, dir, nil)
	if base == edited || edited == chmodded {
		t.Errorf("hash ignored a change: %s %s %s", base, edited, chmodded)
	}
}

func TestSnapshot_RefusesSymlinks(t *testing.T) {
	repo := checkout(t)
	os.Symlink("/etc/passwd", filepath.Join(repo, "docker/link"))
	if _, err := Snapshot(repo, t.TempDir(), nil); err == nil {
		t.Error("a symlink went into a release")
	}
}

func evilTar(t *testing.T, h tar.Header, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	h.Size = int64(len(body))
	if err := tw.WriteHeader(&h); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func TestExtract_RefusesBadEntries(t *testing.T) {
	cases := map[string]tar.Header{
		"parent":   {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644},
		"absolute": {Name: "/etc/x", Typeflag: tar.TypeReg, Mode: 0o644},
		"unclean":  {Name: "docker/../../x", Typeflag: tar.TypeReg, Mode: 0o644},
		"symlink":  {Name: "docker/l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hardlink": {Name: "docker/h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"},
		"dot":      {Name: ".", Typeflag: tar.TypeReg, Mode: 0o644},
		"unhashed": {Name: "docker/scripts/node_modules/x.js", Typeflag: tar.TypeReg, Mode: 0o644},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Extract(evilTar(t, h, ""), dir); err == nil {
				t.Error("accepted")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("left %v behind", entries)
			}
		})
	}
	t.Run("control", func(t *testing.T) {
		if _, err := Extract(evilTar(t, tar.Header{Name: "docker/ok", Typeflag: tar.TypeReg, Mode: 0o644}, "x"), t.TempDir()); err != nil {
			t.Errorf("refused a plain file: %v", err)
		}
	})
}

func TestExtract_RefusesOversize(t *testing.T) {
	old := MaxSize
	MaxSize = 4
	defer func() { MaxSize = old }()
	_, err := Extract(evilTar(t, tar.Header{Name: "docker/big", Typeflag: tar.TypeReg, Mode: 0o644}, "12345"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("got %v, want a size error", err)
	}
}

func TestSnapshot_RefusesOversize(t *testing.T) {
	old := MaxSize
	MaxSize = 4
	defer func() { MaxSize = old }()
	dir := t.TempDir()
	if _, err := Snapshot(checkout(t), dir, nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("got %v, want a size error", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v behind", entries)
	}
}

func TestExtract_HugeEntryCantWrapTheTotal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "docker/a", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	// Only the header: the body never comes, but Extract must refuse first.
	tw.WriteHeader(&tar.Header{Name: "docker/b", Typeflag: tar.TypeReg, Mode: 0o644, Size: math.MaxInt64})
	tw.Flush()
	gz.Close()
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	os.WriteFile(p, buf.Bytes(), 0o644)
	if _, err := Extract(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("got %v, want a size error", err)
	}
}

func TestSnapshot_RefusesTooManyFiles(t *testing.T) {
	old := maxEntries
	maxEntries = 1
	defer func() { maxEntries = old }()
	if _, err := Snapshot(checkout(t), t.TempDir(), nil); err == nil {
		t.Error("snapshot over the file cap")
	}
}

func TestSnapshot_ReplacesATamperedRelease(t *testing.T) {
	repo, dir := checkout(t), t.TempDir()
	hash, err := Snapshot(repo, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(dir, hash, "matrix.yml")
	os.WriteFile(evil, []byte("tampered"), 0o644)
	if again, err := Snapshot(repo, dir, nil); err != nil || again != hash {
		t.Fatalf("again = %s %v", again, err)
	}
	if got, _ := Hash(filepath.Join(dir, hash)); got != hash {
		t.Errorf("the tampered tree was kept")
	}
	// A tree that can't be hashed at all is replaced too.
	os.Remove(evil)
	os.Symlink("/etc/passwd", evil)
	if again, err := Snapshot(repo, dir, nil); err != nil || again != hash {
		t.Fatalf("after symlink: %s %v", again, err)
	}
	if got, _ := Hash(filepath.Join(dir, hash)); got != hash {
		t.Errorf("the unhashable tree was kept")
	}
}
