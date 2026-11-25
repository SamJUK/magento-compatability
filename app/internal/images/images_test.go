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
	for _, want := range []string{
		"mariadb:11.4", "opensearchproject/opensearch:2.19.5", "valkey/valkey:8-alpine", "rabbitmq:4.1-alpine",
		"php:8.4-fpm-alpine", "php:8.3-fpm-alpine", "composer/composer:2.2.26-bin", "composer/composer:2.9.8-bin",
		"nginx:1.28-alpine", "httpd:2.4-alpine", "varnish:7.7",
	} {
		if !slices.Contains(refs, want) {
			t.Errorf("refs lack %s: %v", want, refs)
		}
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
	if !strings.HasPrefix(pw, "m2test/playwright:1.") || !slices.ContainsFunc(refs, func(r string) bool { return strings.HasPrefix(r, "mcr.microsoft.com/playwright:v1.") }) {
		t.Errorf("playwright image %q, base missing from %v", pw, refs)
	}
	php := harness[slices.IndexFunc(harness, func(h Harness) bool { return h.Image == "m2test/php:8.4" })]
	if php.Context != "docker/php" || php.File != "docker/php/Dockerfile" || php.Args["PHP_VERSION"] != "8.4" {
		t.Errorf("php harness = %+v", php)
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
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if args[0] == "image" {
		ref := args[len(args)-1]
		d, ok := f.images[ref]
		if !ok {
			return nil, errors.New("Error response from daemon: No such image: " + ref)
		}
		return []byte(d), nil
	}
	if args[0] == "pull" {
		return nil, f.pullErr
	}
	return nil, nil
}

func useDocker(t *testing.T, f *fakeDocker) {
	old := docker
	docker = f.run
	t.Cleanup(func() { docker = old })
}

func TestPullAndBuild(t *testing.T) {
	f := &fakeDocker{}
	useDocker(t, f)
	m := Manifest{
		Pinned:  map[string]string{"mariadb:11.4": "sha256:aa", "docker.elastic.co/es/es:7": "sha256:bb"},
		Harness: []Harness{{Image: "m2test/php:8.4", Context: "docker/php", File: "docker/php/Dockerfile", Args: map[string]string{"PHP_VERSION": "8.4"}}},
	}
	if err := Pull(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := Build(context.Background(), "/rel", "h1", m); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"image", "inspect", "--format", "{{.Id}}", "docker.elastic.co/es/es@sha256:bb"}, {"pull", "--quiet", "docker.elastic.co/es/es@sha256:bb"}, {"tag", "docker.elastic.co/es/es@sha256:bb", "docker.elastic.co/es/es:7"},
		{"image", "inspect", "--format", "{{.Id}}", "mariadb@sha256:aa"}, {"pull", "--quiet", "mariadb@sha256:aa"}, {"tag", "mariadb@sha256:aa", "mariadb:11.4"},
		{"build", "--pull=false", "--quiet", "--label", "m2test.release=h1", "-t", "m2test/php:8.4", "-f", "/rel/docker/php/Dockerfile", "--build-arg", "PHP_VERSION=8.4", "/rel/docker/php"},
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

func TestMissing(t *testing.T) {
	m := Manifest{
		Pinned:  map[string]string{"mariadb:11.4": "sha256:aa", "redis:7": "sha256:cc", "nginx:1": "sha256:dd"},
		Harness: []Harness{{Image: "m2test/php:8.4"}, {Image: "m2test/varnish:7.7"}, {Image: "m2test/old:1"}},
	}
	useDocker(t, &fakeDocker{images: map[string]string{
		"mariadb:11.4":   `["mariadb@sha256:aa"]`,
		"redis:7":        `["redis@sha256:zz"]`,
		"m2test/php:8.4": "h1\n",
		"m2test/old:1":   "h0\n",
	}})
	got, err := Missing(context.Background(), "h1", m)
	want := []string{"nginx:1", "redis:7 (other digest)", "m2test/varnish:7.7", "m2test/old:1 (other release)"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %v %v, want %v", got, err, want)
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

func TestReady_OnlyWorksWhenSomethingIsMissing(t *testing.T) {
	m := Manifest{Pinned: map[string]string{"mariadb:11.4": "sha256:aa"}, Harness: []Harness{{Image: "m2test/php:8.4", Context: "c", File: "c/Dockerfile"}}}
	present := map[string]string{"mariadb:11.4": `["mariadb@sha256:aa"]`, "m2test/php:8.4": "h1\n"}
	f := &fakeDocker{images: present}
	useDocker(t, f)
	if err := Ready(context.Background(), "/rel", "h1", m); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c[0] == "pull" || c[0] == "build" {
			t.Errorf("all present, but ran %v", c)
		}
	}
	f = &fakeDocker{images: map[string]string{"mariadb:11.4": `["mariadb@sha256:aa"]`, "m2test/php:8.4": "h0\n"}}
	useDocker(t, f)
	if err := Ready(context.Background(), "/rel", "h1", m); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(f.calls, func(c []string) bool { return c[0] == "build" }) {
		t.Errorf("a harness image from another release wasn't rebuilt: %v", f.calls)
	}
}
