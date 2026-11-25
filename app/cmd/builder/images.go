// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/samjuk/magento-compatability/internal/images"
	"github.com/samjuk/magento-compatability/internal/matrix"
)

type ghcrImage struct {
	Image  string `json:"image"`  // as the compose files name it
	Remote string `json:"remote"` // GHCR ref, with the context hash
}

// runImages lists the harness images CI pushes; -lock pins new FROM bases first, -build builds one.
func runImages(args []string) {
	fs := flag.NewFlagSet("images", flag.ExitOnError)
	root := fs.String("repo", findRepoRoot(), "Checkout to read matrix.yml and docker/ from")
	lock := fs.Bool("lock", false, "Pin FROM bases missing from "+images.LockFile+" to their digests now")
	build := fs.String("build", "", "Build the image with this GHCR ref")
	tag := fs.String("tag", "", "With -build, tag it as this instead")
	fs.Parse(args)
	ctx := context.Background()
	if *lock {
		n, err := lockBases(ctx, *root)
		if err != nil {
			fatalf("%v", err)
		}
		logInfo(fmt.Sprintf("pinned %d new bases in %s", n, images.LockFile))
		return
	}
	harness, list, err := ghcrImages(*root)
	if err != nil {
		fatalf("%v", err)
	}
	if *build == "" {
		writeJSON(os.Stdout, list)
		return
	}
	i := slices.IndexFunc(list, func(g ghcrImage) bool { return g.Remote == *build })
	if i < 0 {
		fatalf("%s isn't a harness image of this checkout", *build)
	}
	if *tag == "" {
		*tag = *build
	}
	if err := images.Build(ctx, *root, harness[i], *tag); err != nil {
		fatalf("%v", err)
	}
}

func planAll(root string) ([]images.Harness, error) {
	m, err := matrix.Load(filepath.Join(root, "matrix.yml"))
	if err != nil {
		return nil, err
	}
	combos := slices.Concat(matrix.BuildCombinations(m, matrix.Filter{}), matrix.BuildBaselineCombinations(m, matrix.Filter{}))
	_, harness, _, err := images.Plan(root, combos, true)
	return harness, err
}

func lockBases(ctx context.Context, root string) (int, error) {
	harness, err := planAll(root)
	if err != nil {
		return 0, err
	}
	lock, err := images.ReadLock(root)
	if err != nil {
		return 0, err
	}
	var refs []string
	for _, h := range harness {
		for b, d := range h.Bases {
			if d == "" && !slices.Contains(refs, b) {
				refs = append(refs, b)
			}
		}
	}
	if len(refs) == 0 {
		return 0, nil
	}
	pinned, err := resolve(ctx, refs)
	if err != nil {
		return 0, err
	}
	maps.Copy(lock, pinned)
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return 0, err
	}
	return len(refs), os.WriteFile(filepath.Join(root, filepath.FromSlash(images.LockFile)), append(data, '\n'), 0o644)
}

func ghcrImages(root string) ([]images.Harness, []ghcrImage, error) {
	harness, err := planAll(root)
	if err != nil {
		return nil, nil, err
	}
	list := []ghcrImage{}
	for _, h := range harness {
		hash, err := images.ContextHash(root, h)
		if err != nil {
			return nil, nil, err
		}
		remote, err := images.RemoteRef(h, hash)
		if err != nil {
			return nil, nil, err
		}
		list = append(list, ghcrImage{Image: h.Image, Remote: remote})
	}
	return harness, list, nil
}

func writeJSON(w io.Writer, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fatalf("%v", err)
	}
}
