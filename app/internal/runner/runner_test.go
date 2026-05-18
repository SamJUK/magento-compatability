package runner

import (
	"strings"
	"testing"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

func TestSanitiseProjectName(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{
			// short ID — no truncation
			id:   "magento-248-php83-mariadb114-apache",
			want: "m2test-magento-248-php83-mariadb114-apache",
		},
		{
			// special chars stripped
			id:   "foo!bar@baz#qux",
			want: "m2test-foobarbazqux",
		},
		{
			// underscores and hyphens kept
			id:   "foo_bar-baz",
			want: "m2test-foo_bar-baz",
		},
		{
			// uppercase kept
			id:   "FooBar",
			want: "m2test-FooBar",
		},
	}

	for _, tc := range cases {
		got := sanitiseProjectName(tc.id)
		if got != tc.want {
			t.Errorf("sanitiseProjectName(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestSanitiseProjectName_Truncates(t *testing.T) {
	// ID long enough to push past 63 chars after "m2test-" prefix.
	id := strings.Repeat("a", 60)
	got := sanitiseProjectName(id)
	if len(got) > 63 {
		t.Errorf("sanitiseProjectName: len=%d, want ≤63", len(got))
	}
}

func TestParseHostPort(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// standard docker compose output format: "HOST:PORT\n"
		{"0.0.0.0:32768\n", "32768"},
		{"127.0.0.1:8080\n", "8080"},
		// bare port (no host prefix)
		{"32768\n", "32768"},
		{"  32768  ", "32768"},
	}

	for _, tc := range cases {
		got := parseHostPort(tc.raw)
		if got != tc.want {
			t.Errorf("parseHostPort(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestByteCountSI(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{999, "999 B"},
		{1_000, "1.0 kB"},
		{1_500, "1.5 kB"},
		{999_999, "1000.0 kB"},
		{1_000_000, "1.0 MB"},
		{1_500_000, "1.5 MB"},
		{1_000_000_000, "1.0 GB"},
	}

	for _, tc := range cases {
		got := byteCountSI(tc.input)
		if got != tc.want {
			t.Errorf("byteCountSI(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestComposeFileMap_AllKnownTypes(t *testing.T) {
	for typ, file := range composeFileMap {
		if file == "" {
			t.Errorf("composeFileMap[%q] is empty", typ)
		}
		if !strings.HasSuffix(file, ".yml") {
			t.Errorf("composeFileMap[%q] = %q: expected .yml suffix", typ, file)
		}
	}
}

func TestSearchConfigFlag(t *testing.T) {
	cases := []struct {
		searchType    string
		searchVersion string
		want          string
	}{
		{"opensearch", "2", "opensearch"},
		{"elasticsearch", "8.11", "elasticsearch8"},
		{"elasticsearch", "7.17", "elasticsearch7"},
		{"elasticsearch", "", "elasticsearch"}, // empty version: no suffix
	}
	for _, tc := range cases {
		got := searchConfigFlag(tc.searchType, tc.searchVersion)
		if got != tc.want {
			t.Errorf("searchConfigFlag(%q, %q) = %q, want %q", tc.searchType, tc.searchVersion, got, tc.want)
		}
	}
}

func TestBuildMagentoEnv_ContainsExpectedKeys(t *testing.T) {
	c := matrix.Combination{
		Package:       "magento/project-community-edition",
		Version:       "2.4.8",
		PHP:           "8.3",
		Mirror:        "https://mirror.example.com/",
		SearchType:    "opensearch",
		SearchVersion: "2",
	}
	env := buildMagentoEnv(c, searchConfigFlag(c.SearchType, c.SearchVersion))

	required := []string{
		"PRODUCT_PACKAGE=magento/project-community-edition",
		"PRODUCT_VERSION=2.4.8",
		"PHP_VERSION=8.3",
		"MIRROR_URL=https://mirror.example.com/",
		"SEARCH_TYPE=opensearch",
		"INSTALL_SAMPLE_DATA=0",
	}
	envSet := make(map[string]bool, len(env))
	for _, kv := range env {
		envSet[kv] = true
	}
	for _, want := range required {
		if !envSet[want] {
			t.Errorf("buildMagentoEnv: missing %q", want)
		}
	}
}

func TestBuildInstallArgs(t *testing.T) {
	env := []string{"FOO=bar", "BAZ=qux"}
	baseURL := "http://localhost:32768"
	args := buildInstallArgs(env, baseURL)

	if args[0] != "env" {
		t.Errorf("buildInstallArgs: first arg = %q, want %q", args[0], "env")
	}
	if args[len(args)-1] != "/scripts/install.sh" {
		t.Errorf("buildInstallArgs: last arg = %q, want %q", args[len(args)-1], "/scripts/install.sh")
	}
	wantURL := "MAGENTO_BASE_URL=" + baseURL + "/"
	found := false
	for _, a := range args {
		if a == wantURL {
			found = true
		}
	}
	if !found {
		t.Errorf("buildInstallArgs: missing %q", wantURL)
	}
}
