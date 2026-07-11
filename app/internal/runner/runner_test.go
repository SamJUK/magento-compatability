package runner

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

func TestSanitiseProjectName(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{
			id:   "magento-248-php83-mariadb114-apache",
			want: "m2test-f4d35bf7e3b05aca",
		},
		{
			id:   "foo!bar@baz#qux",
			want: "m2test-5c7bca84a9cd5955",
		},
		{
			id:   "foo_bar-baz",
			want: "m2test-e0542b6ce78fe203",
		},
		{
			id:   "FooBar",
			want: "m2test-0d749abe13775734",
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
	id := strings.Repeat("a", 60)
	got := sanitiseProjectName(id)
	if len(got) > 63 {
		t.Errorf("sanitiseProjectName: len=%d, want ≤63", len(got))
	}
}

func TestSanitiseProjectName_Format(t *testing.T) {
	got := sanitiseProjectName("magento-248-php83-mariadb114-apache")
	if ok, err := regexp.MatchString(`^m2test-[0-9a-f]{16}$`, got); err != nil {
		t.Fatalf("regexp failed: %v", err)
	} else if !ok {
		t.Fatalf("sanitiseProjectName(%q) = %q, want m2test-<16 hex chars>", "magento-248-php83-mariadb114-apache", got)
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
		c    matrix.Combination
		want string
	}{
		{
			c: matrix.Combination{SearchType: "opensearch", SearchVersion: "2"},
			want: "opensearch",
		},
		{
			c: matrix.Combination{SearchType: "elasticsearch", SearchVersion: "8.11"},
			want: "elasticsearch8",
		},
		{
			c: matrix.Combination{SearchType: "elasticsearch", SearchVersion: "7.17"},
			want: "elasticsearch7",
		},
		{
			c: matrix.Combination{SearchType: "elasticsearch"},
			want: "elasticsearch", // empty version: no suffix
		},
		{
			c: matrix.Combination{
				Package:       "magento/project-community-edition",
				Version:       "2.4.5-p9",
				SearchType:    "opensearch",
				SearchVersion: "2.19.5",
			},
			want: "elasticsearch7",
		},
	}
	for _, tc := range cases {
		got := searchConfigFlag(tc.c)
		if got != tc.want {
			t.Errorf("searchConfigFlag(...) = %q, want %q", got, tc.want)
		}
	}
}

func TestSearchHostFlagStyle(t *testing.T) {
	cases := []struct {
		name string
		c    matrix.Combination
		want string
	}{
		{
			name: "legacy magento opensearch uses elasticsearch flags",
			c: matrix.Combination{
				Package:    "magento/project-community-edition",
				Version:    "2.4.5-p9",
				SearchType: "opensearch",
			},
			want: "elasticsearch",
		},
		{
			name: "modern magento opensearch uses opensearch flags",
			c: matrix.Combination{
				Package:    "magento/project-community-edition",
				Version:    "2.4.6",
				SearchType: "opensearch",
			},
			want: "opensearch",
		},
		{
			name: "elasticsearch always uses elasticsearch flags",
			c: matrix.Combination{
				Package:    "magento/project-community-edition",
				Version:    "2.4.8",
				SearchType: "elasticsearch",
			},
			want: "elasticsearch",
		},
	}

	for _, tc := range cases {
		if got := searchHostFlagStyle(tc.c); got != tc.want {
			t.Errorf("%s: searchHostFlagStyle(...) = %q, want %q", tc.name, got, tc.want)
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
	env := buildMagentoEnv(c, searchConfigFlag(c))

	required := []string{
		"PRODUCT_PACKAGE=magento/project-community-edition",
		"PRODUCT_VERSION=2.4.8",
		"PHP_VERSION=8.3",
		"MIRROR_URL=https://mirror.example.com/",
		"SEARCH_TYPE=opensearch",
		"SEARCH_HOST_FLAG_STYLE=opensearch",
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

func TestIsTransientComposerNetworkFailure(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want bool
	}{
		{name: "curl 6", log: "curl error 6 while downloading packages.json", want: true},
		{name: "curl 7", log: "curl error 7 while downloading packages.json", want: true},
		{name: "curl 28", log: "curl error 28 while downloading packages.json", want: true},
		{name: "curl 56", log: "curl error 56 while downloading packages.json", want: true},
		{name: "different failure", log: "PHP Fatal error: something else", want: false},
	}

	for _, tc := range cases {
		if got := isTransientComposerNetworkFailure(tc.log); got != tc.want {
			t.Errorf("%s: isTransientComposerNetworkFailure(%q) = %v, want %v", tc.name, tc.log, got, tc.want)
		}
	}
}

func TestRunInstallWithRetries_RetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Setenv("TZ", "UTC")

	var attempts int
	var sleeps []time.Duration

	log, err := runInstallWithRetries(
		context.Background(),
		func() (string, error) {
			attempts++
			if attempts == 1 {
				return "curl error 28 while downloading packages.json\n", errors.New("transient")
			}
			return "[OK] Installation complete\n", nil
		},
		func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("runInstallWithRetries returned err = %v, want nil", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(sleeps) != 1 || sleeps[0] != 5*time.Second {
		t.Fatalf("sleeps = %v, want [5s]", sleeps)
	}
	if !strings.Contains(log, "Transient Composer network failure detected") {
		t.Fatalf("log missing retry warning: %q", log)
	}
	if !strings.Contains(log, "=== Install retry attempt 2/3 ===") {
		t.Fatalf("log missing retry attempt header: %q", log)
	}
	if !strings.Contains(log, "[OK] Installation complete") {
		t.Fatalf("log missing successful retry output: %q", log)
	}
}

func TestRunInstallWithRetries_DoesNotRetryNonTransientFailure(t *testing.T) {
	var attempts int
	var slept bool
	wantErr := errors.New("fatal")

	log, err := runInstallWithRetries(
		context.Background(),
		func() (string, error) {
			attempts++
			return "PHP Fatal error: broken install\n", wantErr
		},
		func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runInstallWithRetries err = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if slept {
		t.Fatal("sleep called for non-transient failure")
	}
	if strings.Contains(log, "Transient Composer network failure detected") {
		t.Fatalf("log unexpectedly contains retry warning: %q", log)
	}
}

func TestRunInstallWithRetries_StopsAfterMaxAttempts(t *testing.T) {
	var attempts int
	var sleeps []time.Duration
	wantErr := errors.New("still failing")

	log, err := runInstallWithRetries(
		context.Background(),
		func() (string, error) {
			attempts++
			return "curl error 56 while downloading packages.json\n", wantErr
		},
		func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runInstallWithRetries err = %v, want %v", err, wantErr)
	}
	if attempts != installRetryAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, installRetryAttempts)
	}
	if len(sleeps) != installRetryAttempts-1 {
		t.Fatalf("sleep count = %d, want %d", len(sleeps), installRetryAttempts-1)
	}
	if sleeps[0] != 5*time.Second || sleeps[1] != 10*time.Second {
		t.Fatalf("sleeps = %v, want [5s 10s]", sleeps)
	}
	if strings.Contains(log, "=== Install retry attempt 4/3 ===") {
		t.Fatalf("log shows impossible extra retry: %q", log)
	}
}
