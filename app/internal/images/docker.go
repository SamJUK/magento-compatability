// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package images

import (
	"context"
	"encoding/json"
	"errors"
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
func Pull(ctx context.Context, m Manifest) error { return pullPins(ctx, m.Pinned) }

func pullPins(ctx context.Context, pins map[string]string) error {
	for _, ref := range sortedKeys(pins) {
		p, err := pinned(ref, pins[ref])
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

// ErrUnpublished is a pull GHCR turned down: no such tag, or a package it won't show. Asking again won't help.
var ErrUnpublished = errors.New("not published")

// Harnesses readies each harness image from GHCR by its ContextHash: one already here is used, a missing one pulled, or built here with m.Build.
// Without m.Build a missing image is an error.
func Harnesses(ctx context.Context, releaseDir string, m Manifest) error {
	for _, h := range m.Harness {
		hash, err := hashOf(releaseDir, h)
		if err != nil {
			return err
		}
		remote, err := RemoteRef(h, hash)
		if err != nil {
			return err
		}
		// Only a copy labelled with this hash counts; another is replaced.
		raw, err := docker(ctx, "image", "inspect", "--format", `{{index .Config.Labels "`+ContextLabel+`"}}`, remote)
		switch {
		case err == nil && strings.TrimSpace(string(raw)) == hash:
		case err != nil && !absent(err):
			return err
		case m.Build:
			if err := Build(ctx, releaseDir, h, remote); err != nil {
				return err
			}
		default:
			if _, err := docker(ctx, "pull", "--quiet", remote); err != nil {
				if unpublished(ctx, remote) {
					err = fmt.Errorf("%w: %w", ErrUnpublished, err)
				}
				return fmt.Errorf("%s: pulling %s: %w; if it isn't published yet, run with -build-images (a private package gives the same error)", h.Image, remote, err)
			}
		}
		if _, err := docker(ctx, "tag", remote, h.Image); err != nil {
			return err
		}
	}
	return nil
}

func Build(ctx context.Context, releaseDir string, h Harness, remote string) error {
	hash, err := ContextHash(releaseDir, h)
	if err != nil {
		return err
	}
	if h.Hash != "" && h.Hash != hash {
		return fmt.Errorf("%s: its build files changed since the sweep started", h.Image)
	}
	if err := pullPins(ctx, h.Bases); err != nil {
		return err
	}
	// --pull=false keeps each FROM on its pinned base.
	args := []string{"build", "--pull=false", "--quiet", "--label", ContextLabel + "=" + hash, "--label", "org.opencontainers.image.source=" + Source, "-t", remote, "-f", filepath.Join(releaseDir, h.File)}
	for _, k := range sortedKeys(h.Args) {
		args = append(args, "--build-arg", k+"="+h.Args[k])
	}
	_, err = docker(ctx, append(args, filepath.Join(releaseDir, h.Context))...)
	return err
}

func Missing(ctx context.Context, releaseDir string, m Manifest) ([]string, error) {
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
		hash, err := hashOf(releaseDir, h)
		if err != nil {
			return nil, err
		}
		raw, err := docker(ctx, "image", "inspect", "--format", `{{index .Config.Labels "`+ContextLabel+`"}}`, h.Image)
		switch {
		case err != nil && absent(err):
			out = append(out, h.Image)
		case err != nil:
			return nil, err
		case strings.TrimSpace(string(raw)) != hash:
			out = append(out, h.Image+" (other build)")
		}
	}
	return out, nil
}

func Ready(ctx context.Context, releaseDir string, m Manifest) error {
	missing, err := Missing(ctx, releaseDir, m)
	if err != nil || len(missing) == 0 {
		return err
	}
	if err := Pull(ctx, m); err != nil {
		return err
	}
	if err := Harnesses(ctx, releaseDir, m); err != nil {
		return err
	}
	// A pulled tag is only trusted once its label says what it was built from.
	if missing, err = Missing(ctx, releaseDir, m); err == nil && len(missing) > 0 {
		err = fmt.Errorf("still not ready after readying: %s", strings.Join(missing, ", "))
	}
	return err
}

// Prune untags harness images m doesn't use, both names; one a container still uses stays.
func Prune(ctx context.Context, releaseDir string, m Manifest) error {
	keep := map[string]bool{}
	for _, h := range m.Harness {
		hash, err := hashOf(releaseDir, h)
		if err != nil {
			return err
		}
		remote, err := RemoteRef(h, hash)
		if err != nil {
			return err
		}
		keep[h.Image], keep[remote] = true, true
	}
	raw, err := docker(ctx, "image", "ls", "--filter", "label="+ContextLabel, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(string(raw)) {
		// Another name for one of ours, given by hand, isn't ours to remove.
		ours := strings.HasPrefix(ref, "m2test/") || strings.HasPrefix(ref, Registry)
		if ours && !keep[ref] && !strings.Contains(ref, "<none>") {
			docker(ctx, "image", "rm", ref)
		}
	}
	return nil
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
