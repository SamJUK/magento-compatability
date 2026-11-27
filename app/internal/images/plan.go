// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package images pins the upstream images a sweep runs on to digests, and
// readies the harness images (m2test/*) built on top of them.
package images

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/runner"
)

const File = "images.json"

const PlaywrightTests = "docker/scripts/tests/playwright"

type Manifest struct {
	Pinned     map[string]string `json:"pinned"` // upstream ref -> manifest list digest
	Harness    []Harness         `json:"harness"`
	Playwright string            `json:"playwright,omitempty"` // harness image the tests run in
	Build      bool              `json:"build,omitempty"`      // build harness images here rather than pull them
}

// Harness is an image built from a Dockerfile in the release: by CI, which
// pushes it to GHCR, or here with -build-images.
type Harness struct {
	Image   string            `json:"image"`
	Context string            `json:"context"` // release-relative
	File    string            `json:"file"`    // release-relative
	Args    map[string]string `json:"args,omitempty"`
	Bases   map[string]string `json:"bases,omitempty"` // FROM ref -> digest, from LockFile
	Hash    string            `json:"-"`               // set by Freeze
}

// Load reads a release's manifest, refusing refs that could pass as docker
// options and paths that leave the release.
func Load(releaseDir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(releaseDir, File))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	for _, pins := range append([]map[string]string{m.Pinned}, basesOf(m)...) {
		for ref, d := range pins {
			if _, err := ParseRef(ref); err != nil || strings.HasPrefix(ref, "-") || !validDigest(d) {
				return m, fmt.Errorf("%s: bad pin %q -> %q", File, ref, d)
			}
		}
	}
	if m.Playwright != "" && !slices.ContainsFunc(m.Harness, func(h Harness) bool { return h.Image == m.Playwright }) {
		return m, fmt.Errorf("%s: Playwright image %q isn't a harness image", File, m.Playwright)
	}
	for _, h := range m.Harness {
		_, err := ParseRef(h.Image)
		if err != nil || strings.HasPrefix(h.Image, "-") || !filepath.IsLocal(h.File) || !filepath.IsLocal(h.Context) {
			return m, fmt.Errorf("%s: bad harness image %+v", File, h)
		}
		for k := range h.Args {
			if strings.ContainsAny(k, "= ") || strings.HasPrefix(k, "-") {
				return m, fmt.Errorf("%s: bad build arg %q", File, k)
			}
		}
	}
	return m, nil
}

func basesOf(m Manifest) []map[string]string {
	var out []map[string]string
	for _, h := range m.Harness {
		out = append(out, h.Bases)
	}
	return out
}

// LockFile pins each harness image's FROM bases to a digest, so its hash names one image wherever built.
// `builder images -lock` adds missing entries.
const LockFile = "docker/bases.lock.json"

func ReadLock(dir string) (map[string]string, error) {
	lock := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(LockFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return lock, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &lock)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", LockFile, err)
	}
	return lock, nil
}

type composeFile struct {
	Services map[string]struct {
		Image string `yaml:"image"`
		Build *struct {
			Context    string            `yaml:"context"`
			Dockerfile string            `yaml:"dockerfile"`
			Args       map[string]string `yaml:"args"`
		} `yaml:"build"`
	} `yaml:"services"`
}

// Plan lists the upstream refs to pin and the harness images to build for combos under releaseDir.
func Plan(releaseDir string, combos []matrix.Combination, playwright bool) (refs []string, harness []Harness, pw string, err error) {
	composeDir := filepath.Join(releaseDir, "docker", "compose")
	upstream := map[string]bool{}
	built := map[string]Harness{}
	parsed := map[string]composeFile{}
	for _, c := range combos {
		files, err := runner.StackFiles(c, composeDir)
		if err != nil {
			return nil, nil, "", err
		}
		env := map[string]string{}
		for _, kv := range runner.StackEnv(c) {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		for _, f := range files {
			cf, ok := parsed[f]
			if !ok {
				data, err := os.ReadFile(f)
				if err != nil {
					return nil, nil, "", err
				}
				if err := yaml.Unmarshal(data, &cf); err != nil {
					return nil, nil, "", fmt.Errorf("%s: %w", f, err)
				}
				parsed[f] = cf
			}
			for name, svc := range cf.Services {
				if svc.Image == "" {
					continue
				}
				img, err := expand(svc.Image, env)
				if err != nil {
					return nil, nil, "", fmt.Errorf("%s: %s: %w", f, name, err)
				}
				if svc.Build == nil {
					upstream[img] = true
					continue
				}
				// Compose resolves paths against the first file's directory.
				h, err := harnessFor(releaseDir, filepath.Dir(files[0]), img, svc.Build.Context, svc.Build.Dockerfile, svc.Build.Args, env)
				if err != nil {
					return nil, nil, "", fmt.Errorf("%s: %s: %w", f, name, err)
				}
				built[img] = h
			}
		}
	}
	if playwright {
		h, err := playwrightHarness(releaseDir)
		if err != nil {
			return nil, nil, "", err
		}
		built[h.Image], pw = h, h.Image
	}
	lock, err := ReadLock(releaseDir)
	if err != nil {
		return nil, nil, "", err
	}
	for _, h := range built {
		bases, err := fromRefs(filepath.Join(releaseDir, h.File), h.Args)
		if err != nil {
			return nil, nil, "", err
		}
		// Left empty when unlocked: ContextHash says how to fix it.
		h.Bases = map[string]string{}
		for _, b := range bases {
			h.Bases[b] = lock[b]
		}
		harness = append(harness, h)
	}
	for r := range upstream {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	sort.Slice(harness, func(i, j int) bool { return harness[i].Image < harness[j].Image })
	return refs, harness, pw, nil
}

func harnessFor(releaseDir, composeDir, image, context, dockerfile string, args, env map[string]string) (Harness, error) {
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	ctx, err := expand(context, env)
	if err != nil {
		return Harness{}, err
	}
	abs := filepath.Join(composeDir, ctx)
	rel, err := filepath.Rel(releaseDir, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return Harness{}, fmt.Errorf("build context %s is outside the release", ctx)
	}
	h := Harness{Image: image, Context: filepath.ToSlash(rel), File: filepath.ToSlash(filepath.Join(rel, dockerfile)), Args: map[string]string{}}
	for k, v := range args {
		if h.Args[k], err = expand(v, env); err != nil {
			return Harness{}, err
		}
	}
	return h, nil
}

// playwrightHarness is the Playwright image for the locked version with the suite's dependencies; its tag carries the lockfile's hash.
func playwrightHarness(releaseDir string) (Harness, error) {
	lock, err := os.ReadFile(filepath.Join(releaseDir, PlaywrightTests, "package-lock.json"))
	if err != nil {
		return Harness{}, err
	}
	var l struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		return Harness{}, fmt.Errorf("package-lock.json: %w", err)
	}
	ver := l.Packages["node_modules/@playwright/test"].Version
	if ver == "" {
		return Harness{}, fmt.Errorf("package-lock.json has no @playwright/test")
	}
	sum := sha256.Sum256(lock)
	return Harness{
		Image:   fmt.Sprintf("m2test/playwright:%s-%s", ver, hex.EncodeToString(sum[:6])),
		Context: PlaywrightTests,
		File:    "docker/playwright/Dockerfile",
		Args:    map[string]string{"PLAYWRIGHT_VERSION": ver},
	}, nil
}

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func expand(s string, env map[string]string) (string, error) {
	var err error
	out := varRef.ReplaceAllStringFunc(s, func(m string) string {
		g := varRef.FindStringSubmatch(m)
		name, op, arg := g[1], g[2], g[3]
		if name == "" {
			name = g[4]
		}
		v, set := env[name]
		switch op {
		case ":-":
			if v == "" {
				return arg
			}
		case "-":
			if !set {
				return arg
			}
		case ":?":
			if v == "" && err == nil {
				err = fmt.Errorf("%s: %s", name, arg)
			}
		case "?":
			if !set && err == nil {
				err = fmt.Errorf("%s: %s", name, arg)
			}
		}
		return v
	})
	return out, err
}

func fromRefs(dockerfile string, buildArgs map[string]string) ([]string, error) {
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		return nil, err
	}
	args := map[string]string{}
	stages := map[string]bool{}
	var refs []string
	sawFrom := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "ARG":
			if sawFrom {
				continue // stage-local: not visible to FROM
			}
			k, v, _ := strings.Cut(f[1], "=")
			if b, ok := buildArgs[k]; ok {
				v = b
			}
			args[k] = v
		case "FROM":
			sawFrom = true
			rest := f[1:]
			for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				continue
			}
			ref, err := expand(rest[0], args)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", dockerfile, err)
			}
			if ref != "scratch" && !stages[strings.ToLower(ref)] {
				refs = append(refs, ref)
			}
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				stages[strings.ToLower(rest[2])] = true
			}
		}
	}
	return refs, sc.Err()
}
