// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package release snapshots what a run reads from the checkout (matrix.yml
// and docker/) so a sweep, and every worker in it, runs one fixed copy.
package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

var Sources = []string{"matrix.yml", "docker"}

var skipped = map[string]bool{"node_modules": true, "test-results": true, "playwright-report": true, ".DS_Store": true}

// MaxSize caps an extracted release, so a bad tarball can't fill a disk.
var MaxSize int64 = 64 << 20

var maxEntries = 10000

type file struct {
	rel  string // slash separated
	exec bool
}

// files lists the regular files under root in a fixed order. Anything else
// (a symlink, a device) is an error: a release is plain files.
func files(root string, only []string) ([]file, error) {
	var out []file
	walk := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipped[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("release: %s is not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, file{rel: filepath.ToSlash(rel), exec: info.Mode()&0o111 != 0})
		return nil
	}
	if only == nil {
		if err := filepath.WalkDir(root, walk); err != nil {
			return nil, err
		}
	}
	for _, o := range only {
		if err := filepath.WalkDir(filepath.Join(root, o), walk); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

func hashFiles(root string, fl []file) (string, error) {
	h := sha256.New()
	for _, f := range fl {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%t\x00%d\x00", f.rel, f.exec, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func Hash(dir string) (string, error) {
	fl, err := files(dir, nil)
	if err != nil {
		return "", err
	}
	return hashFiles(dir, fl)
}

// Snapshot copies the release out of repoRoot, plus extra files keyed by release path, into dir/<hash> with a tarball beside it.
// One already there is reused.
func Snapshot(repoRoot, dir string, extra map[string][]byte) (string, error) {
	fl, err := files(repoRoot, Sources)
	if err != nil {
		return "", err
	}
	if len(fl)+len(extra) > maxEntries {
		return "", fmt.Errorf("release: over %d files", maxEntries)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".new-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	var total int64
	for _, f := range fl {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(f.rel)))
		if err != nil {
			return "", err
		}
		// Workers would refuse it; say so here, naming the file.
		if total += int64(len(data)); total > MaxSize {
			return "", fmt.Errorf("release: over %d bytes at %s", MaxSize, f.rel)
		}
		if err := put(tmp, f.rel, data, f.exec); err != nil {
			return "", err
		}
	}
	for rel, data := range extra {
		if total += int64(len(data)); total > MaxSize {
			return "", fmt.Errorf("release: over %d bytes at %s", MaxSize, rel)
		}
		if err := put(tmp, rel, data, false); err != nil {
			return "", err
		}
	}
	hash, err := settle(dir, tmp)
	if err != nil {
		return "", err
	}
	return hash, Pack(filepath.Join(dir, hash))
}

func settle(dir, tmp string) (string, error) {
	hash, err := Hash(tmp)
	if err != nil {
		return "", err
	}
	final := filepath.Join(dir, hash)
	// An existing tree is reused only if it still hashes to its name.
	if got, err := Hash(final); (err == nil && got != hash) || (err != nil && !errors.Is(err, os.ErrNotExist)) {
		if err := os.RemoveAll(final); err != nil {
			return "", err
		}
		os.Remove(final + ".tar.gz")
	}
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmp, final); err != nil {
			return "", err
		}
	}
	return hash, nil
}

// Pack writes dir's tarball afresh: one left by a crash may be cut short.
func Pack(dir string) error { return writeTar(dir, dir+".tar.gz") }

func put(root, rel string, data []byte, exec bool) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	return os.WriteFile(p, data, mode)
}

func writeTar(src, dst string) error {
	fl, err := files(src, nil)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	for _, f := range fl {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f.rel)))
		if err != nil {
			out.Close()
			return err
		}
		mode := int64(0o644)
		if f.exec {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.rel, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			out.Close()
			return err
		}
		if _, err := tw.Write(data); err != nil {
			out.Close()
			return err
		}
	}
	for _, c := range []io.Closer{tw, gz} {
		if err := c.Close(); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Extract unpacks a release tarball into dir/<hash> and returns the hash.
// Only plain files with clean relative paths are accepted.
func Extract(tarball, dir string) (string, error) {
	in, err := os.Open(tarball)
	if err != nil {
		return "", err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".new-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	tr := tar.NewReader(gz)
	var total int64
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name := h.Name
		// Skipped names aren't hashed, so they'd ride along unchecked.
		if h.Typeflag != tar.TypeReg || slices.ContainsFunc(strings.Split(name, "/"), func(c string) bool { return skipped[c] }) || name != path.Clean(name) || name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return "", fmt.Errorf("release: refusing tar entry %q", h.Name)
		}
		// Compared before adding, so a huge size can't wrap the total.
		if entries++; h.Size < 0 || h.Size > MaxSize-total || entries > maxEntries {
			return "", fmt.Errorf("release: over %d bytes or %d files", MaxSize, maxEntries)
		}
		total += h.Size
		data, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return "", err
		}
		if err := put(tmp, name, data, h.Mode&0o111 != 0); err != nil {
			return "", err
		}
	}
	return settle(dir, tmp)
}
