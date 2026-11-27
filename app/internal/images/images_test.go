// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

var repo = filepath.Join("..", "..", "..")

func combo(php, web, varnish string) matrix.Combination {
	return matrix.Combination{Product: "magento", Version: "2.4.8", PHP: php,
		WebserverType: web, WebserverVersion: map[string]string{"nginx": "1.28", "apache": "2.4"}[web],
		DBType: "mariadb", DBVersion: "11.4", SearchType: "opensearch", SearchVersion: "2.19.5",
		CacheType: "valkey", CacheVersion: "8", QueueType: "rabbitmq", QueueVersion: "4.1", Varnish: varnish}
}

func TestPlan_RealComposeTree(t *testing.T) {
	refs, harness, pw, err := Plan(repo, []matrix.Combination{combo("8.4", "nginx", "7.7"), combo("8.3", "apache", "none")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mariadb:11.4", "opensearchproject/opensearch:2.19.5", "rabbitmq:4.1-alpine", "valkey/valkey:8-alpine"}; !reflect.DeepEqual(refs, want) {
		t.Errorf("refs %v, want only the services: harness bases come from the lock", refs)
	}
	for _, r := range refs {
		if strings.HasPrefix(r, "m2test/") || r == "composer-lts" || strings.Contains(r, "$") {
			t.Errorf("%s is not an upstream image", r)
		}
	}
	var images []string
	for _, h := range harness {
		images = append(images, h.Image)
	}
	for _, want := range []string{"m2test/php:8.4", "m2test/php:8.3", "m2test/webserver-nginx:1.28", "m2test/webserver-apache:2.4", "m2test/varnish:7.7", pw} {
		if !slices.Contains(images, want) {
			t.Errorf("harness lacks %s: %v", want, images)
		}
	}
	if !strings.HasPrefix(pw, "m2test/playwright:1.") {
		t.Errorf("playwright image %q", pw)
	}
	lock, _ := ReadLock(repo)
	php := harness[slices.IndexFunc(harness, func(h Harness) bool { return h.Image == "m2test/php:8.4" })]
	if php.Context != "docker/php" || php.File != "docker/php/Dockerfile" || php.Args["PHP_VERSION"] != "8.4" ||
		!validDigest(php.Bases["php:8.4-fpm-alpine"]) || php.Bases["php:8.4-fpm-alpine"] != lock["php:8.4-fpm-alpine"] || len(php.Bases) != 3 {
		t.Errorf("php harness = %+v", php)
	}
	nginx := harness[slices.IndexFunc(harness, func(h Harness) bool { return h.Image == "m2test/webserver-nginx:1.28" })]
	if d, ok := nginx.Bases["nginx:1.28-alpine"]; !ok || d != "" {
		t.Errorf("unlocked base = %q %t, want listed with no digest", d, ok)
	}
}

// Pins are pulled for this host's architecture; a service asking for another
// makes compose pull its tag, which may have moved since the release.
func TestCompose_NoServiceNamesAPlatform(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(repo, "docker", "compose", "*", "*.yml"))
	files = append(files, filepath.Join(repo, "docker", "compose", "base.yml"))
	if len(files) < 10 {
		t.Fatalf("found only %v", files)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var cf struct {
			Services map[string]struct {
				Platform string `yaml:"platform"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &cf); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, svc := range cf.Services {
			if svc.Platform != "" {
				t.Errorf("%s: %s asks for %s", f, name, svc.Platform)
			}
		}
	}
}

func TestPlan_NoPlaywright(t *testing.T) {
	refs, _, pw, err := Plan(repo, []matrix.Combination{combo("8.4", "nginx", "none")}, false)
	if err != nil || pw != "" || slices.ContainsFunc(refs, func(r string) bool { return strings.Contains(r, "playwright") }) {
		t.Errorf("pw=%q err=%v refs=%v", pw, err, refs)
	}
}

func TestExpand(t *testing.T) {
	env := map[string]string{"SET": "x", "EMPTY": ""}
	for in, want := range map[string]string{
		"a:${SET}": "a:x", "${SET:-d}": "x", "${EMPTY:-d}": "d", "${EMPTY-d}": "", "${UNSET-d}": "d", "$SET/y": "x/y",
	} {
		if got, err := expand(in, env); err != nil || got != want {
			t.Errorf("expand(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"${EMPTY:?need it}", "${UNSET?need it}"} {
		if _, err := expand(in, env); err == nil {
			t.Errorf("expand(%q) did not fail", in)
		}
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]Ref{
		"mariadb:11.4":                          {"registry-1.docker.io", "library/mariadb", "11.4"},
		"valkey/valkey:8-alpine":                {"registry-1.docker.io", "valkey/valkey", "8-alpine"},
		"docker.elastic.co/elasticsearch/es:7":  {"docker.elastic.co", "elasticsearch/es", "7"},
		"localhost:5000/x":                      {"localhost:5000", "x", "latest"},
		"mcr.microsoft.com/playwright:v1-noble": {"mcr.microsoft.com", "playwright", "v1-noble"},
		"docker.io/library/php:8":               {"registry-1.docker.io", "library/php", "8"},
	} {
		if got, err := ParseRef(in); err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v %v, want %+v", in, got, err, want)
		}
	}
	if _, err := ParseRef("x@sha256:abc"); err == nil {
		t.Error("a pinned ref parsed")
	}
}

func fakeRegistry(t *testing.T, headDigest bool) (*httptest.Server, string, *[]string) {
	body := []byte(`{"manifests":[]}`)
	sum := sha256.Sum256(body)
	d := "sha256:" + hex.EncodeToString(sum[:])
	var seen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:library/mariadb:pull" {
				http.Error(w, "bad scope", 400)
				return
			}
			w.Write([]byte(`{"token":"t1"}`))
		case r.Header.Get("Authorization") != "Bearer t1":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:library/mariadb:pull"`)
			w.WriteHeader(401)
		case r.URL.Path == "/v2/library/mariadb/manifests/11.4":
			if headDigest {
				w.Header().Set("Docker-Content-Digest", d)
			}
			if r.Method == http.MethodGet {
				w.Write(body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, d, &seen
}

func TestResolve_HeadWithToken(t *testing.T) {
	srv, want, seen := fakeRegistry(t, true)
	scheme = "http"
	defer func() { scheme = "https" }()
	host := strings.TrimPrefix(srv.URL, "http://")
	got, err := Resolve(context.Background(), []string{host + "/library/mariadb:11.4"})
	if err != nil || got[host+"/library/mariadb:11.4"] != want {
		t.Fatalf("got %v %v, want %s", got, err, want)
	}
	if slices.ContainsFunc(*seen, func(s string) bool { return strings.HasPrefix(s, "GET /v2/") }) {
		t.Errorf("fetched the manifest body when HEAD had the digest: %v", *seen)
	}
}

func TestUnpublishedAsksTheRegistry(t *testing.T) {
	srv, _, _ := fakeRegistry(t, true)
	scheme = "http"
	defer func() { scheme = "https" }()
	host := strings.TrimPrefix(srv.URL, "http://")
	if unpublished(context.Background(), host+"/library/mariadb:11.4") {
		t.Error("a published tag counted as unpublished")
	}
	if !unpublished(context.Background(), host+"/library/mariadb:missing") {
		t.Error("a missing tag counted as published")
	}
	srv.Close()
	if unpublished(context.Background(), host+"/library/mariadb:missing") {
		t.Error("an unreachable registry counted as unpublished")
	}
}

func TestResolve_FallsBackToHashingTheBody(t *testing.T) {
	srv, want, _ := fakeRegistry(t, false)
	scheme = "http"
	defer func() { scheme = "https" }()
	host := strings.TrimPrefix(srv.URL, "http://")
	got, err := Resolve(context.Background(), []string{host + "/library/mariadb:11.4"})
	if err != nil || got[host+"/library/mariadb:11.4"] != want {
		t.Fatalf("got %v %v, want %s", got, err, want)
	}
	if _, err := Resolve(context.Background(), []string{host + "/library/mariadb:missing"}); err == nil {
		t.Error("a missing tag resolved")
	}
	maxManifest = 4
	defer func() { maxManifest = 4 << 20 }()
	if _, err := Resolve(context.Background(), []string{host + "/library/mariadb:11.4"}); err == nil {
		t.Error("hashed a truncated manifest")
	}
}

type fakeDocker struct {
	calls   [][]string
	images  map[string]string // ref -> RepoDigests JSON
	pullErr error
	list    string            // image ls output
	pullAs  map[string]string // ref -> what inspect shows once pulled
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if args[0] == "image" && args[1] == "ls" {
		return []byte(f.list), nil
	}
	if args[0] == "image" && args[1] == "rm" {
		return nil, nil
	}
	if args[0] == "image" {
		ref := args[len(args)-1]
		d, ok := f.images[ref]
		if !ok {
			return nil, errors.New("Error response from daemon: No such image: " + ref)
		}
		return []byte(d), nil
	}
	if args[0] == "pull" {
		if f.pullErr == nil && f.pullAs[args[2]] != "" {
			f.images[args[2]] = f.pullAs[args[2]]
		}
		return nil, f.pullErr
	}
	if args[0] == "build" && f.images != nil {
		// Labelled as the build asked.
		label := args[slices.Index(args, "--label")+1]
		f.images[args[slices.Index(args, "-t")+1]] = strings.TrimPrefix(label, ContextLabel+"=") + "\n"
	}
	if args[0] == "tag" && f.images != nil {
		if d, ok := f.images[args[1]]; ok {
			f.images[args[2]] = d
		}
	}
	return nil, nil
}

func useDocker(t *testing.T, f *fakeDocker) {
	old := docker
	docker = f.run
	t.Cleanup(func() { docker = old })
}

func TestPull(t *testing.T) {
	f := &fakeDocker{}
	useDocker(t, f)
	m := Manifest{Pinned: map[string]string{"mariadb:11.4": "sha256:aa", "docker.elastic.co/es/es:7": "sha256:bb"}}
	if err := Pull(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"image", "inspect", "--format", "{{.Id}}", "docker.elastic.co/es/es@sha256:bb"}, {"pull", "--quiet", "docker.elastic.co/es/es@sha256:bb"}, {"tag", "docker.elastic.co/es/es@sha256:bb", "docker.elastic.co/es/es:7"},
		{"image", "inspect", "--format", "{{.Id}}", "mariadb@sha256:aa"}, {"pull", "--quiet", "mariadb@sha256:aa"}, {"tag", "mariadb@sha256:aa", "mariadb:11.4"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%v\nwant:\n%v", f.calls, want)
	}
}

func TestPullSkipsAnImageAlreadyHereByDigest(t *testing.T) {
	f := &fakeDocker{images: map[string]string{"mysql@sha256:aa": "sha256:id"}, pullErr: errors.New("cannot overwrite digest sha256:aa")}
	useDocker(t, f)
	m := Manifest{Pinned: map[string]string{"mysql:8.0": "sha256:aa", "redis:7": "sha256:cc"}}
	err := Pull(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "cannot overwrite") {
		t.Fatalf("Pull = %v, want the redis pull error", err)
	}
	for _, c := range f.calls {
		if c[0] == "pull" && c[2] == "mysql@sha256:aa" {
			t.Error("pulled mysql, already here by digest")
		}
	}
	want := []string{"tag", "mysql@sha256:aa", "mysql:8.0"}
	if !reflect.DeepEqual(f.calls[1], want) {
		t.Errorf("call 1 = %v, want %v", f.calls[1], want)
	}
}

func TestPullFailsWhenInspectFailsForAnotherReason(t *testing.T) {
	useDocker(t, &fakeDocker{})
	docker = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "image" {
			return nil, errors.New("Cannot connect to the Docker daemon")
		}
		t.Errorf("ran docker %v after inspect failed", args)
		return nil, nil
	}
	if err := Pull(context.Background(), Manifest{Pinned: map[string]string{"mysql:8.0": "sha256:aa"}}); err == nil {
		t.Error("Pull ignored an inspect error")
	}
}

func phpRelease(t *testing.T) (string, Harness) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "docker/php/conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "docker/php/Dockerfile"), []byte("ARG PHP_VERSION\nFROM php:${PHP_VERSION}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docker/php/custom.ini"), []byte("memory_limit=2G\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docker/php/conf/a.conf"), []byte("a\n"), 0o644)
	return dir, Harness{Image: "m2test/php:8.4", Context: "docker/php", File: "docker/php/Dockerfile", Args: map[string]string{"PHP_VERSION": "8.4"},
		Bases: map[string]string{"php:8.4": "sha256:" + strings.Repeat("a", 64)}}
}

func mustHash(t *testing.T, dir string, h Harness) string {
	t.Helper()
	hash, err := ContextHash(dir, h)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestContextHash_StableAcrossCopiesAndTimes(t *testing.T) {
	dir, h := phpRelease(t)
	want := mustHash(t, dir, h)
	// Another checkout: files written in another order, other mtimes, and
	// what a release leaves out.
	other := t.TempDir()
	for _, f := range []string{"docker/php/conf/a.conf", "docker/php/custom.ini", "docker/php/Dockerfile"} {
		data, _ := os.ReadFile(filepath.Join(dir, f))
		os.MkdirAll(filepath.Dir(filepath.Join(other, f)), 0o755)
		os.WriteFile(filepath.Join(other, f), data, 0o644)
		os.Chtimes(filepath.Join(other, f), time.Unix(1, 0), time.Unix(1, 0))
	}
	os.WriteFile(filepath.Join(other, "docker/php/.DS_Store"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(other, "docker/php/node_modules/x"), 0o755)
	os.WriteFile(filepath.Join(other, "docker/php/node_modules/x/y"), []byte("x"), 0o644)
	if got := mustHash(t, other, h); got != want {
		t.Errorf("hash %s in another copy, want %s", got, want)
	}
	if len(want) != 16 {
		t.Errorf("hash %q isn't 16 hex characters", want)
	}
}

func TestContextHash_ChangesWithEveryInput(t *testing.T) {
	base, h := phpRelease(t)
	want := mustHash(t, base, h)
	for name, change := range map[string]func(dir string, h *Harness){
		"context file": func(dir string, _ *Harness) {
			os.WriteFile(filepath.Join(dir, "docker/php/custom.ini"), []byte("memory_limit=4G\n"), 0o644)
		},
		"nested file": func(dir string, _ *Harness) {
			os.WriteFile(filepath.Join(dir, "docker/php/conf/a.conf"), []byte("b\n"), 0o644)
		},
		"new file": func(dir string, _ *Harness) { os.WriteFile(filepath.Join(dir, "docker/php/new"), nil, 0o644) },
		"renamed file": func(dir string, _ *Harness) {
			os.Rename(filepath.Join(dir, "docker/php/conf/a.conf"), filepath.Join(dir, "docker/php/conf/b.conf"))
		},
		"exec bit": func(dir string, _ *Harness) { os.Chmod(filepath.Join(dir, "docker/php/custom.ini"), 0o755) },
		"dockerfile": func(dir string, _ *Harness) {
			os.WriteFile(filepath.Join(dir, "docker/php/Dockerfile"), []byte("FROM php\n"), 0o644)
		},
		"arg value": func(_ string, h *Harness) { h.Args = map[string]string{"PHP_VERSION": "8.3"} },
		"arg added": func(_ string, h *Harness) { h.Args = map[string]string{"PHP_VERSION": "8.4", "X": ""} },
		"base digest": func(_ string, h *Harness) {
			h.Bases = map[string]string{"php:8.4": "sha256:" + strings.Repeat("b", 64)}
		},
		"base added": func(_ string, h *Harness) { h.Bases["alpine:3"] = "sha256:" + strings.Repeat("c", 64) },
	} {
		dir, h := phpRelease(t)
		change(dir, &h)
		if got := mustHash(t, dir, h); got == want {
			t.Errorf("%s: hash unchanged", name)
		}
	}
}

func TestContextHash_RefusesAnUnlockedBase(t *testing.T) {
	dir, h := phpRelease(t)
	h.Bases["alpine:3"] = ""
	if _, err := ContextHash(dir, h); err == nil || !strings.Contains(err.Error(), "alpine:3") || !strings.Contains(err.Error(), "images -lock") {
		t.Errorf("ContextHash = %v, want the base and the fix", err)
	}
}

func TestContextHash_CoversADockerfileOutsideTheContext(t *testing.T) {
	dir, h := phpRelease(t)
	os.MkdirAll(filepath.Join(dir, "docker/pw"), 0o755)
	os.WriteFile(filepath.Join(dir, "docker/pw/Dockerfile"), []byte("FROM a\n"), 0o644)
	h.File = "docker/pw/Dockerfile"
	want := mustHash(t, dir, h)
	os.WriteFile(filepath.Join(dir, "docker/pw/Dockerfile"), []byte("FROM b\n"), 0o644)
	if mustHash(t, dir, h) == want {
		t.Error("hash ignores a Dockerfile outside the context")
	}
}

func TestRemoteRef(t *testing.T) {
	for image, want := range map[string]string{
		"m2test/php:8.4":               Registry + "php:8.4-abc",
		"m2test/webserver-nginx:1.30":  Registry + "webserver-nginx:1.30-abc",
		"m2test/playwright:1.60-0f0f0": Registry + "playwright:1.60-0f0f0-abc",
		"php:8.4":                      "",
		"m2test/php":                   "",
		"m2test/a/b:1":                 "",
		"ghcr.io/m2test/php:8.4":       "",
	} {
		got, err := RemoteRef(Harness{Image: image}, "abc")
		if got != want || (err == nil) != (want != "") {
			t.Errorf("RemoteRef(%s) = %q, %v; want %q", image, got, err, want)
		}
	}
}

func TestHarnesses_PullsAndTagsByDefault(t *testing.T) {
	dir, h := phpRelease(t)
	remote, _ := RemoteRef(h, mustHash(t, dir, h))
	f := &fakeDocker{}
	useDocker(t, f)
	if err := Harnesses(context.Background(), dir, Manifest{Harness: []Harness{h}}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"image", "inspect", "--format", labelFormat, remote}, {"pull", "--quiet", remote}, {"tag", remote, "m2test/php:8.4"}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%v\nwant:\n%v", f.calls, want)
	}
}

func TestHarnesses_MissingWithoutTheFlagNamesIt(t *testing.T) {
	dir, h := phpRelease(t)
	f := &fakeDocker{pullErr: errors.New(`failed to resolve reference "ghcr.io/x": not found`)}
	useDocker(t, f)
	missing := true
	old := unpublished
	unpublished = func(context.Context, string) bool { return missing }
	t.Cleanup(func() { unpublished = old })
	err := Harnesses(context.Background(), dir, Manifest{Harness: []Harness{h}})
	if err == nil || !strings.Contains(err.Error(), "m2test/php:8.4") || !strings.Contains(err.Error(), "-build-images") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Harnesses = %v, want the image, the cause and the flag", err)
	}
	if !errors.Is(err, ErrUnpublished) {
		t.Errorf("%v isn't ErrUnpublished", err)
	}
	missing = false
	f.pullErr = errors.New("failed to register layer: permission denied")
	if err := Harnesses(context.Background(), dir, Manifest{Harness: []Harness{h}}); err == nil || errors.Is(err, ErrUnpublished) {
		t.Errorf("control: a pull failing with the tag published gave %v", err)
	}
	for _, c := range f.calls {
		if c[0] == "build" || c[0] == "tag" {
			t.Errorf("ran %v after the pull failed", c)
		}
	}
}

func TestHarnesses_BuildsWithTheFlag(t *testing.T) {
	dir, h := phpRelease(t)
	hash := mustHash(t, dir, h)
	remote, _ := RemoteRef(h, hash)
	f := &fakeDocker{}
	useDocker(t, f)
	if err := Harnesses(context.Background(), dir, Manifest{Harness: []Harness{h}, Build: true}); err != nil {
		t.Fatal(err)
	}
	base := "php@sha256:" + strings.Repeat("a", 64)
	want := [][]string{
		{"image", "inspect", "--format", labelFormat, remote},
		{"image", "inspect", "--format", "{{.Id}}", base}, {"pull", "--quiet", base}, {"tag", base, "php:8.4"},
		{"build", "--pull=false", "--quiet", "--label", ContextLabel + "=" + hash, "--label", "org.opencontainers.image.source=" + Source, "-t", remote, "-f", filepath.Join(dir, "docker/php/Dockerfile"), "--build-arg", "PHP_VERSION=8.4", filepath.Join(dir, "docker/php")},
		{"tag", remote, "m2test/php:8.4"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%v\nwant:\n%v", f.calls, want)
	}
}

func TestHarnesses_UsesTheTagAlreadyHere(t *testing.T) {
	dir, h := phpRelease(t)
	remote, _ := RemoteRef(h, mustHash(t, dir, h))
	for _, build := range []bool{false, true} {
		f := &fakeDocker{images: map[string]string{remote: mustHash(t, dir, h) + "\n"}, pullErr: errors.New("must not pull")}
		useDocker(t, f)
		if err := Harnesses(context.Background(), dir, Manifest{Harness: []Harness{h}, Build: build}); err != nil {
			t.Fatal(err)
		}
		want := [][]string{{"image", "inspect", "--format", labelFormat, remote}, {"tag", remote, "m2test/php:8.4"}}
		if !reflect.DeepEqual(f.calls, want) {
			t.Errorf("build %t: calls %v, want %v", build, f.calls, want)
		}
	}
}

const labelFormat = `{{index .Config.Labels "` + ContextLabel + `"}}`

func TestHarnesses_ReplacesACachedTagWithTheWrongLabel(t *testing.T) {
	dir, h := phpRelease(t)
	hash := mustHash(t, dir, h)
	remote, _ := RemoteRef(h, hash)
	for _, build := range []bool{false, true} {
		f := &fakeDocker{images: map[string]string{remote: "0123456789abcdef\n"}, pullAs: map[string]string{remote: hash + "\n"}}
		useDocker(t, f)
		if err := Ready(context.Background(), dir, Manifest{Harness: []Harness{h}, Build: build}); err != nil {
			t.Errorf("build %t: %v", build, err)
		}
		want := map[bool]string{false: "pull", true: "build"}[build]
		i := slices.IndexFunc(f.calls, func(c []string) bool { return c[0] == want })
		j := slices.IndexFunc(f.calls, func(c []string) bool { return c[0] == "tag" && c[1] == remote })
		if i < 0 || j < i {
			t.Errorf("build %t: cached wrong label not replaced by %s then tagged: %v", build, want, f.calls)
		}
	}
}

func TestHarnesses_InspectErrorIsFatal(t *testing.T) {
	dir, h := phpRelease(t)
	useDocker(t, &fakeDocker{})
	docker = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "image" {
			return nil, errors.New("Cannot connect to the Docker daemon")
		}
		t.Errorf("ran docker %v after inspect failed", args)
		return nil, nil
	}
	if err := Harnesses(context.Background(), dir, Manifest{Harness: []Harness{h}, Build: true}); err == nil {
		t.Error("Harnesses ignored an inspect error")
	}
}

func TestMissing(t *testing.T) {
	dir, h := phpRelease(t)
	hash := mustHash(t, dir, h)
	varnish, old := h, h
	varnish.Image, old.Image = "m2test/varnish:7.7", "m2test/old:1"
	m := Manifest{
		Pinned:  map[string]string{"mariadb:11.4": "sha256:aa", "redis:7": "sha256:cc", "nginx:1": "sha256:dd"},
		Harness: []Harness{h, varnish, old},
	}
	useDocker(t, &fakeDocker{images: map[string]string{
		"mariadb:11.4":   `["mariadb@sha256:aa"]`,
		"redis:7":        `["redis@sha256:zz"]`,
		"m2test/php:8.4": hash + "\n",
		"m2test/old:1":   "0123456789abcdef\n",
	}})
	got, err := Missing(context.Background(), dir, m)
	want := []string{"nginx:1", "redis:7 (other digest)", "m2test/varnish:7.7", "m2test/old:1 (other build)"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %v %v, want %v", got, err, want)
	}
}

func TestReady_OnlyWorksWhenSomethingIsMissing(t *testing.T) {
	dir, h := phpRelease(t)
	hash := mustHash(t, dir, h)
	remote, _ := RemoteRef(h, hash)
	m := Manifest{Pinned: map[string]string{"mariadb:11.4": "sha256:aa"}, Harness: []Harness{h}}
	f := &fakeDocker{images: map[string]string{"mariadb:11.4": `["mariadb@sha256:aa"]`, "m2test/php:8.4": hash + "\n"}}
	useDocker(t, f)
	if err := Ready(context.Background(), dir, m); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c[0] != "image" {
			t.Errorf("all present, but ran %v", c)
		}
	}
	// Built from other files: the tag now names another image.
	f = &fakeDocker{images: map[string]string{"mariadb:11.4": `["mariadb@sha256:aa"]`, "m2test/php:8.4": "0123456789abcdef\n"}, pullAs: map[string]string{remote: hash + "\n"}}
	useDocker(t, f)
	if err := Ready(context.Background(), dir, m); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(f.calls, func(c []string) bool { return c[0] == "pull" && c[2] == remote }) {
		t.Errorf("an image from other build files wasn't replaced: %v", f.calls)
	}
}

func TestReady_DistrustsAPulledImageWithTheWrongLabel(t *testing.T) {
	dir, h := phpRelease(t)
	remote, _ := RemoteRef(h, mustHash(t, dir, h))
	for label, ok := range map[string]bool{mustHash(t, dir, h) + "\n": true, "0123456789abcdef\n": false, "\n": false} {
		useDocker(t, &fakeDocker{images: map[string]string{}, pullAs: map[string]string{remote: label}})
		err := Ready(context.Background(), dir, Manifest{Harness: []Harness{h}})
		if (err == nil) != ok {
			t.Errorf("pulled label %q: Ready = %v", label, err)
		}
	}
}

func TestFreeze_KeepsToTheImagesASweepStartedWith(t *testing.T) {
	dir, h := phpRelease(t)
	hash := mustHash(t, dir, h)
	m, err := Freeze(dir, Manifest{Harness: []Harness{h}})
	if err != nil || m.Harness[0].Hash != hash {
		t.Fatalf("Freeze = %+v %v", m, err)
	}
	os.WriteFile(filepath.Join(dir, "docker/php/custom.ini"), []byte("changed\n"), 0o644)
	remote, _ := RemoteRef(h, hash)
	useDocker(t, &fakeDocker{images: map[string]string{}, pullAs: map[string]string{remote: hash + "\n"}})
	if err := Ready(context.Background(), dir, m); err != nil {
		t.Errorf("pulling the image the sweep started with: %v", err)
	}
	m.Build = true
	useDocker(t, &fakeDocker{})
	if err := Harnesses(context.Background(), dir, m); err == nil || !strings.Contains(err.Error(), "changed since the sweep started") {
		t.Errorf("built changed files under the old hash: %v", err)
	}
}

func TestPrune_KeepsOnlyWhatTheReleaseUses(t *testing.T) {
	dir, h := phpRelease(t)
	remote, _ := RemoteRef(h, mustHash(t, dir, h))
	f := &fakeDocker{list: strings.Join([]string{"m2test/php:8.4", remote, Registry + "php:8.4-0123456789abcdef", "m2test/php:8.3", "<none>:<none>", "mine/php:dev"}, "\n")}
	useDocker(t, f)
	if err := Prune(context.Background(), dir, Manifest{Harness: []Harness{h}}); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range f.calls {
		if c[0] == "image" && c[1] == "rm" {
			removed = append(removed, c[2])
		}
	}
	if want := []string{Registry + "php:8.4-0123456789abcdef", "m2test/php:8.3"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
}

// M2TEST_LIVE=1 checks Resolve against the real registries.
func TestResolve_Live(t *testing.T) {
	if os.Getenv("M2TEST_LIVE") == "" {
		t.Skip("set M2TEST_LIVE=1")
	}
	refs := []string{"mariadb:11.4", "valkey/valkey:8-alpine", "docker.elastic.co/elasticsearch/elasticsearch:7.17.28", "mcr.microsoft.com/playwright:v1.60.0-noble"}
	got, err := Resolve(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if !validDigest(got[r]) {
			t.Errorf("%s -> %q", r, got[r])
		}
		t.Logf("%s -> %s", r, got[r])
	}
}

func TestLoad_RefusesUnsafeManifests(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	h := `{"image":"m2test/php:8.4","context":"docker/php","file":"docker/php/Dockerfile"}`
	for name, body := range map[string]string{
		"option ref":    `{"pinned":{"--privileged":"` + d + `"}}`,
		"bad digest":    `{"pinned":{"mariadb:11.4":"sha256:zz"}}`,
		"escaping file": `{"harness":[{"image":"m2test/php:8.4","context":"docker/php","file":"../../etc/Dockerfile"}]}`,
		"absolute ctx":  `{"harness":[{"image":"m2test/php:8.4","context":"/","file":"docker/php/Dockerfile"}]}`,
		"option image":  `{"harness":[{"image":"-q","context":"docker/php","file":"docker/php/Dockerfile"}]}`,
		"option arg":    `{"harness":[{"image":"m2test/php:8.4","context":"docker/php","file":"docker/php/Dockerfile","args":{"X=1 --network":"host"}}]}`,
		"stray pw":      `{"playwright":"--privileged","harness":[` + h + `]}`,
		"bad base":      `{"harness":[{"image":"m2test/php:8.4","context":"docker/php","file":"docker/php/Dockerfile","bases":{"--x":"` + d + `"}}]}`,
		"bad base hash": `{"harness":[{"image":"m2test/php:8.4","context":"docker/php","file":"docker/php/Dockerfile","bases":{"php:8.4":"sha256:zz"}}]}`,
		"control":       `{"pinned":{"mariadb:11.4":"` + d + `"},"harness":[` + h + `],"playwright":"m2test/php:8.4"}`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, File), []byte(body), 0o644)
		_, err := Load(dir)
		if (err == nil) != (name == "control") {
			t.Errorf("%s: Load = %v", name, err)
		}
	}
}
