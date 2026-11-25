// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package images

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/samjuk/magento-compatability/internal/release"
)

const Registry = "ghcr.io/samjuk/magento-compatability-"

// Source is the repo the images are built from, for GHCR to link them.
const Source = "https://github.com/samjuk/magento-compatability"

const ContextLabel = "m2test.context"

// ContextHash identifies what a harness image is built from: its context's files, Dockerfile and build args.
func ContextHash(releaseDir string, h Harness) (string, error) {
	sum := sha256.New()
	add := func(kind, name string, exec bool, data []byte) {
		fmt.Fprintf(sum, "%s\x00%s\x00%t\x00%d\x00", kind, name, exec, len(data))
		sum.Write(data)
	}
	root := filepath.Join(releaseDir, filepath.FromSlash(h.Context))
	// WalkDir goes in lexical order, so the hash doesn't depend on the disk's.
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case release.Skipped(d.Name()) && d.IsDir():
			return filepath.SkipDir
		case release.Skipped(d.Name()) || d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		add("context", filepath.ToSlash(rel), info.Mode()&0o111 != 0, data)
		return nil
	})
	if err != nil {
		return "", err
	}
	// Outside the context for Playwright.
	df, err := os.ReadFile(filepath.Join(releaseDir, filepath.FromSlash(h.File)))
	if err != nil {
		return "", err
	}
	add("dockerfile", "", false, df)
	for _, k := range sortedKeys(h.Args) {
		add("arg", k, false, []byte(h.Args[k]))
	}
	for _, b := range sortedKeys(h.Bases) {
		if h.Bases[b] == "" {
			return "", fmt.Errorf("%s: base %s isn't in %s; run `builder images -lock`", h.Image, b, LockFile)
		}
		add("base", b, false, []byte(h.Bases[b]))
	}
	return hex.EncodeToString(sum.Sum(nil))[:16], nil
}

// Freeze fixes each harness image's hash as its files are now, so a sweep from a checkout keeps to the images it started with.
func Freeze(dir string, m Manifest) (Manifest, error) {
	m.Harness = slices.Clone(m.Harness)
	for i, h := range m.Harness {
		hash, err := ContextHash(dir, h)
		if err != nil {
			return m, err
		}
		m.Harness[i].Hash = hash
	}
	return m, nil
}

func hashOf(dir string, h Harness) (string, error) {
	if h.Hash != "" {
		return h.Hash, nil
	}
	return ContextHash(dir, h)
}

// RemoteRef is where a harness image with that hash lives on GHCR:
// m2test/php:8.4 becomes <Registry>php:8.4-<hash>.
func RemoteRef(h Harness, hash string) (string, error) {
	repo, tag, ok := strings.Cut(h.Image, ":")
	name, m2 := strings.CutPrefix(repo, "m2test/")
	if !ok || !m2 || tag == "" || name == "" || strings.ContainsAny(name+tag, "/:@") {
		return "", fmt.Errorf("harness image %s isn't m2test/<name>:<tag>", h.Image)
	}
	return Registry + name + ":" + tag + "-" + hash, nil
}
