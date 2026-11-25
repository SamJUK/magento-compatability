// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package images

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samjuk/magento-compatability/internal/runner"
)

var docker = func(ctx context.Context, args ...string) ([]byte, error) {
	out, err := runner.Command(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func pinned(ref, digest string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(ref, ":"+r.Tag)
	return name + "@" + digest, nil
}

// Pull fetches every pinned image by digest and tags it as the compose files name it.
// One already here by digest is skipped: some Docker versions fail that with "cannot overwrite digest".
func Pull(ctx context.Context, m Manifest) error {
	for _, ref := range sortedKeys(m.Pinned) {
		p, err := pinned(ref, m.Pinned[ref])
		if err != nil {
			return err
		}
		_, err = docker(ctx, "image", "inspect", "--format", "{{.Id}}", p)
		if err != nil && !absent(err) {
			return err
		}
		if err != nil {
			if _, err := docker(ctx, "pull", "--quiet", p); err != nil {
				return err
			}
		}
		if _, err := docker(ctx, "tag", p, ref); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseLabel marks a harness image with the release it was built from.
const ReleaseLabel = "m2test.release"

// Build builds the harness images one at a time from releaseDir, on the
// pulled bases: --pull=false keeps a FROM from resolving its tag again.
// Harness tags are shared between releases, so each is labelled with its
// release's hash.
func Build(ctx context.Context, releaseDir, hash string, m Manifest) error {
	for _, h := range m.Harness {
		args := []string{"build", "--pull=false", "--quiet", "--label", ReleaseLabel + "=" + hash, "-t", h.Image, "-f", filepath.Join(releaseDir, h.File)}
		for _, k := range sortedKeys(h.Args) {
			args = append(args, "--build-arg", k+"="+h.Args[k])
		}
		if _, err := docker(ctx, append(args, filepath.Join(releaseDir, h.Context))...); err != nil {
			return err
		}
	}
	return nil
}

// different digest or from another release.
func Missing(ctx context.Context, hash string, m Manifest) ([]string, error) {
	var out []string
	for _, ref := range sortedKeys(m.Pinned) {
		raw, err := docker(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", ref)
		if err != nil {
			if absent(err) {
				out = append(out, ref)
				continue
			}
			return nil, err
		}
		var digests []string
		if err := json.Unmarshal(raw, &digests); err != nil {
			return nil, fmt.Errorf("inspecting %s: %w", ref, err)
		}
		found := false
		for _, d := range digests {
			found = found || strings.HasSuffix(d, "@"+m.Pinned[ref])
		}
		if !found {
			out = append(out, ref+" (other digest)")
		}
	}
	for _, h := range m.Harness {
		raw, err := docker(ctx, "image", "inspect", "--format", `{{index .Config.Labels "`+ReleaseLabel+`"}}`, h.Image)
		switch {
		case err != nil && absent(err):
			out = append(out, h.Image)
		case err != nil:
			return nil, err
		case strings.TrimSpace(string(raw)) != hash:
			out = append(out, h.Image+" (other release)")
		}
	}
	return out, nil
}

// Ready pulls and builds the release's images unless they are all here.
func Ready(ctx context.Context, releaseDir, hash string, m Manifest) error {
	missing, err := Missing(ctx, hash, m)
	if err != nil || len(missing) == 0 {
		return err
	}
	if err := Pull(ctx, m); err != nil {
		return err
	}
	return Build(ctx, releaseDir, hash, m)
}

func absent(err error) bool {
	return strings.Contains(err.Error(), "No such image") || strings.Contains(err.Error(), "No such object")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
