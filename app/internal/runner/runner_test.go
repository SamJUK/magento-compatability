package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

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

func TestUniqueProjectName_Format(t *testing.T) {
	got := uniqueProjectName("magento-248-php83-mariadb114-apache")
	if ok, err := regexp.MatchString(`^m2test-[0-9a-f]{16}-[0-9a-f]{8}$`, got); err != nil {
		t.Fatalf("regexp failed: %v", err)
	} else if !ok {
		t.Fatalf("uniqueProjectName(...) = %q, want m2test-<16 hex chars>-<8 hex chars>", got)
	}
}

func TestUniqueProjectName_DiffersBetweenRuns(t *testing.T) {
	first := uniqueProjectName("magento-248-php83-mariadb114-apache")
	second := uniqueProjectName("magento-248-php83-mariadb114-apache")
	if first == second {
		t.Fatalf("uniqueProjectName(...) returned %q twice, want distinct values", first)
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

func TestClassifyStepFailureForCombination_KnownCompatibilityIssue(t *testing.T) {
	c := matrix.Combination{
		Product:       "magento",
		Version:       "2.4.6-p11",
		SearchType:    "elasticsearch",
		SearchVersion: "8.11.4",
	}

	got := classifyStepFailureForCombination(
		c,
		"install",
		"Could not validate a connection to the OpenSearch.\nNo alive nodes found in your cluster",
	)
	if got == nil {
		t.Fatal("classifyStepFailureForCombination(...) = nil, want classification")
	}

	want := result.Failure{
		Category:    "compatibility",
		Code:        "elasticsearch8_unsupported",
		Summary:     "This product version could not complete setup:install against Elasticsearch 8.x.",
		LikelyFlaky: false,
	}
	if *got != want {
		t.Fatalf("classifyStepFailureForCombination(...) = %#v, want %#v", *got, want)
	}
}

func TestClassifyStepFailureForCombination_Elasticsearch8CompatibilityFailureUsesElasticsearchMessage(t *testing.T) {
	c := matrix.Combination{
		Product:       "magento",
		Version:       "2.4.7-p10",
		SearchType:    "elasticsearch",
		SearchVersion: "8.19.15",
	}

	got := classifyStepFailureForCombination(
		c,
		"install",
		"Could not validate a connection to Elasticsearch.\nNo alive nodes found in your cluster",
	)
	if got == nil {
		t.Fatal("classifyStepFailureForCombination(...) = nil, want classification")
	}

	want := result.Failure{
		Category:    "compatibility",
		Code:        "elasticsearch8_unsupported",
		Summary:     "This product version could not complete setup:install against Elasticsearch 8.x.",
		LikelyFlaky: false,
	}
	if *got != want {
		t.Fatalf("classifyStepFailureForCombination(...) = %#v, want %#v", *got, want)
	}
}

func TestClassifyStepFailureForCombination_PHPVersionUnsupported(t *testing.T) {
	got := classifyStepFailureForCombination(
		matrix.Combination{},
		"install",
		"Your requirements could not be resolved to an installable set of packages.\n"+
			"your php version (8.3.32) does not satisfy that requirement.",
	)
	if got == nil {
		t.Fatal("classifyStepFailureForCombination(...) = nil, want classification")
	}

	want := result.Failure{
		Category:    "compatibility",
		Code:        "php_version_unsupported",
		Summary:     "The product's Composer constraints do not allow this PHP version.",
		LikelyFlaky: false,
	}
	if *got != want {
		t.Fatalf("classifyStepFailureForCombination(...) = %#v, want %#v", *got, want)
	}
}

func TestClassifyStepFailureForCombination_LegacySearchFlagsHarnessIssue(t *testing.T) {
	got := classifyStepFailureForCombination(
		matrix.Combination{},
		"install",
		"The \"--opensearch-host\" option does not exist.",
	)
	if got == nil {
		t.Fatal("classifyStepFailureForCombination(...) = nil, want classification")
	}

	want := result.Failure{
		Category:    "harness",
		Code:        "legacy_search_flags",
		Summary:     "The harness invoked setup:install with OpenSearch host flags that this Magento version does not support.",
		LikelyFlaky: false,
	}
	if *got != want {
		t.Fatalf("classifyStepFailureForCombination(...) = %#v, want %#v", *got, want)
	}
}

func TestComposeUpEnsuresSharedVolumes(t *testing.T) {
	var calls [][]string
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		env:         []string{"COMPOSE_PROJECT_NAME=m2test-abcd"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			call := append([]string{name}, args...)
			calls = append(calls, call)
			return exec.CommandContext(ctx, "true")
		},
	}

	if _, err := cp.Up(context.Background()); err != nil {
		t.Fatalf("Up() error = %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("Up() made %d commands, want 3", len(calls))
	}

	if got, want := calls[0], []string{"docker", "volume", "create", "m2test-composer-cache"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first command = %v, want %v", got, want)
	}
	if got, want := calls[1], []string{"docker", "volume", "create", "m2test-vendor-cache"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second command = %v, want %v", got, want)
	}
	if got, want := calls[2], []string{"docker", "compose", "-f", "/tmp/base.yml", "up", "-d", "--wait", "--wait-timeout", "120"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compose command = %v, want %v", got, want)
	}
}

func TestNewComposeSetsComposeParallelLimit(t *testing.T) {
	cp, err := newCompose(
		matrix.Combination{
			Product:       "magento",
			Version:       "2.4.8-p5",
			PHP:           "8.4",
			WebserverType: "apache",
			DBType:        "mariadb",
			DBVersion:     "11.4",
			SearchType:    "opensearch",
			SearchVersion: "2.19.5",
			CacheType:     "valkey",
			CacheVersion:  "8",
			QueueType:     "rabbitmq",
			QueueVersion:  "4.2",
			Varnish:       "7.7",
		},
		"/tmp/compose",
		nil,
	)
	if err != nil {
		t.Fatalf("newCompose() error = %v", err)
	}

	if !containsString(cp.env, "COMPOSE_PARALLEL_LIMIT=1") {
		t.Fatalf("compose env missing COMPOSE_PARALLEL_LIMIT=1: %v", cp.env)
	}
}

func TestSearchConfigFlag(t *testing.T) {
	cases := []struct {
		c    matrix.Combination
		want string
	}{
		{
			c:    matrix.Combination{SearchType: "opensearch", SearchVersion: "2"},
			want: "opensearch",
		},
		{
			c:    matrix.Combination{SearchType: "elasticsearch", SearchVersion: "8.11"},
			want: "elasticsearch8",
		},
		{
			c:    matrix.Combination{SearchType: "elasticsearch", SearchVersion: "7.17"},
			want: "elasticsearch7",
		},
		{
			c:    matrix.Combination{SearchType: "elasticsearch"},
			want: "elasticsearch", // empty version: no suffix
		},
		{
			c: matrix.Combination{
				Package:       "magento/project-community-edition",
				Version:       "2.4.2-p2",
				SearchType:    "opensearch",
				SearchVersion: "2.19.5",
			},
			want: "elasticsearch7",
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
				Version:    "2.4.2-p2",
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

func TestPlaywrightEnv_ReplacesBaseURLAndReportFile(t *testing.T) {
	t.Setenv("MAGENTO_BASE_URL", "http://stale.example")
	t.Setenv("PLAYWRIGHT_REPORT_FILE", "old-report.json")
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("NO_COLOR", "1")

	env := playwrightEnv("http://localhost:4321", "playwright-report/fresh.json")
	envSet := make(map[string]bool, len(env))
	for _, kv := range env {
		envSet[kv] = true
	}

	if !envSet["MAGENTO_BASE_URL=http://localhost:4321"] {
		t.Fatalf("playwrightEnv: missing updated base URL")
	}
	if !envSet["PLAYWRIGHT_REPORT_FILE=playwright-report/fresh.json"] {
		t.Fatalf("playwrightEnv: missing updated report file")
	}
	if envSet["MAGENTO_BASE_URL=http://stale.example"] {
		t.Fatalf("playwrightEnv: stale base URL leaked into subprocess environment")
	}
	if envSet["PLAYWRIGHT_REPORT_FILE=old-report.json"] {
		t.Fatalf("playwrightEnv: stale report path leaked into subprocess environment")
	}
	if envSet["FORCE_COLOR=1"] {
		t.Fatalf("playwrightEnv: FORCE_COLOR should not leak into subprocess environment")
	}
	if envSet["NO_COLOR=1"] {
		t.Fatalf("playwrightEnv: NO_COLOR should not leak into subprocess environment")
	}
}

func TestReadPlaywrightReportSummary(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "results.json")
	report := `{
  "stats": {
    "expected": 3,
    "unexpected": 1,
    "flaky": 0,
    "skipped": 1
  },
  "suites": [
    {
      "title": "storefront.spec.ts",
      "specs": [],
      "suites": [
        {
          "title": "Storefront",
          "specs": [
            {
              "title": "homepage renders a real Magento storefront shell",
              "tests": [
                {
                  "results": [
                    { "status": "passed" }
                  ]
                }
              ]
            },
            {
              "title": "guest checkout places an order",
              "tests": [
                {
                  "results": [
                    { "status": "failed" }
                  ]
                }
              ]
            }
          ],
          "suites": []
        }
      ]
    }
  ]
}`
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := readPlaywrightReportSummary(reportPath, "http://localhost:8080")
	if err != nil {
		t.Fatalf("readPlaywrightReportSummary: %v", err)
	}

	if !strings.Contains(got, "Playwright base URL: http://localhost:8080") {
		t.Fatalf("summary missing base URL:\n%s", got)
	}
	if !strings.Contains(got, "expected=3 unexpected=1 flaky=0 skipped=1") {
		t.Fatalf("summary missing stats:\n%s", got)
	}
	if !strings.Contains(got, "guest checkout places an order") {
		t.Fatalf("summary missing failed spec title:\n%s", got)
	}
}

func TestSummarisePlaywrightRun_FallsBackToReportWhenStdoutEmpty(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "results.json")
	report := `{
  "stats": {
    "expected": 1,
    "unexpected": 0,
    "flaky": 0,
    "skipped": 0
  },
  "suites": []
}`
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := summarisePlaywrightRun("", reportPath, "http://localhost:8080")
	if !strings.Contains(got, "Playwright summary: expected=1 unexpected=0 flaky=0 skipped=0") {
		t.Fatalf("summary missing fallback report output:\n%s", got)
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

func TestIsRetryableInstallFailure(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want bool
	}{
		{name: "composer network", log: "curl error 28 while downloading packages.json", want: true},
		{name: "queue not ready", log: "Could not connect to the Amqp Server.", want: true},
		{name: "search not ready", log: "Could not validate a connection to the OpenSearch.\nNo alive nodes found in   your cluster", want: true},
		{name: "corrupted composer zip", log: "corrupted zip archive (0 bytes), try again.", want: true},
		{name: "invalid composer cache config", log: "\"/composer-cache/config.json\" does not contain valid JSON", want: true},
		{name: "hard product failure", log: "Class \"Magento\\Framework\\Exception\\NotFoundException\" not found", want: false},
	}

	for _, tc := range cases {
		if got := isRetryableInstallFailure(tc.log); got != tc.want {
			t.Errorf("%s: isRetryableInstallFailure(%q) = %v, want %v", tc.name, tc.log, got, tc.want)
		}
	}
}

func TestFormatStackUpFailureLog(t *testing.T) {
	t.Run("keeps original output and appends diagnostics", func(t *testing.T) {
		got := formatStackUpFailureLog(
			"m2test-abcd",
			"dependency failed to start",
			"NAME STATUS",
			"db | booting",
		)
		for _, want := range []string{
			"dependency failed to start",
			"=== docker compose ps -a ===\nNAME STATUS",
			"=== docker compose logs ===\ndb | booting",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("formatStackUpFailureLog(...) missing %q in %q", want, got)
			}
		}
	})

	t.Run("synthesises fallback when compose is silent", func(t *testing.T) {
		got := formatStackUpFailureLog("m2test-abcd", "", "", "")
		want := "[ERROR] docker compose up failed for project m2test-abcd but produced no stdout/stderr output"
		if got != want {
			t.Fatalf("formatStackUpFailureLog(...) = %q, want %q", got, want)
		}
	})
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
	if !strings.Contains(log, "Retryable install failure detected") {
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
	if strings.Contains(log, "Retryable install failure detected") {
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

func TestClassifyStepFailure(t *testing.T) {
	cases := []struct {
		name     string
		stepName string
		log      string
		want     *result.Failure
	}{
		{
			name:     "composer network",
			stepName: "install",
			log:      "curl error 28 while downloading packages.json",
			want: &result.Failure{
				Category:    "infrastructure",
				Code:        "composer_network",
				Summary:     "Composer download failed with a transient curl/network error.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "disk full",
			stepName: "install",
			log:      "write /tmp/cache: no space left on device",
			want: &result.Failure{
				Category:    "infrastructure",
				Code:        "disk_space",
				Summary:     "The run exhausted host or Docker disk space.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "cleanup race",
			stepName: "stack_up",
			log:      "dependency failed to start: Error response from daemon: No such container: deadbeef",
			want: &result.Failure{
				Category:    "harness",
				Code:        "docker_cleanup_race",
				Summary:     "Docker Compose hit a cleanup/startup race with stale containers, networks, or volumes.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "service startup exit",
			stepName: "stack_up",
			log:      "dependency failed to start: container m2test-queue-1 exited (0)",
			want: &result.Failure{
				Category:    "harness",
				Code:        "service_startup",
				Summary:     "A dependency container exited during stack startup before Magento install began.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "service startup missing dependency",
			stepName: "stack_up",
			log:      "php-fpm is missing dependency search",
			want: &result.Failure{
				Category:    "harness",
				Code:        "service_startup",
				Summary:     "A dependency container exited during stack startup before Magento install began.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "docker name conflict",
			stepName: "stack_up",
			log:      "Error response from daemon: Conflict. The container name \"/m2test-magento-db-1\" is already in use by container",
			want: &result.Failure{
				Category:    "harness",
				Code:        "docker_name_conflict",
				Summary:     "Docker Compose found stale project resources with colliding container or network names.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "docker endpoint conflict",
			stepName: "stack_up",
			log:      "Error response from daemon: failed to set up container networking: endpoint with name m2test-abcd-varnish-1 already exists in network m2test-abcd_magento",
			want: &result.Failure{
				Category:    "harness",
				Code:        "docker_name_conflict",
				Summary:     "Docker Compose found stale project resources with colliding container or network names.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "allow plugins",
			stepName: "install",
			log:      "PluginBlockedException: package contains a Composer plugin which is blocked by your allow-plugins config",
			want: &result.Failure{
				Category:    "harness",
				Code:        "composer_allow_plugins",
				Summary:     "Composer plugin execution was blocked by harness configuration.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "audit policy",
			stepName: "install",
			log:      "found 1 security vulnerability advisory affecting 1 package; run composer audit for a full list of advisories",
			want: &result.Failure{
				Category:    "harness",
				Code:        "composer_audit_policy",
				Summary:     "Composer audit policy blocked the install in the harness.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "queue not ready",
			stepName: "install",
			log:      "Could not connect to the Amqp Server.",
			want: &result.Failure{
				Category:    "harness",
				Code:        "queue_not_ready",
				Summary:     "The queue service was reachable but not yet ready when Magento validated AMQP connectivity.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "search not ready",
			stepName: "install",
			log:      "Could not validate a connection to the OpenSearch.\nNo alive nodes found in   your cluster",
			want: &result.Failure{
				Category:    "harness",
				Code:        "search_not_ready",
				Summary:     "The search service was reachable but not yet ready when Magento validated the search backend.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "composer cache corruption",
			stepName: "install",
			log:      "\"/composer-cache/config.json\" does not contain valid JSON",
			want: &result.Failure{
				Category:    "harness",
				Code:        "composer_cache_corruption",
				Summary:     "Shared Composer cache state became corrupted during the harness run.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "search cluster unhealthy",
			stepName: "install",
			log:      "[ERROR] Search engine cluster not healthy at http://search:9200/_cluster/health after 180s",
			want: &result.Failure{
				Category:    "harness",
				Code:        "search_cluster_unhealthy",
				Summary:     "The search service never reached cluster health before the harness timeout elapsed.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "package extract timeout",
			stepName: "install",
			log:      "In Process.php line 1205: The process \"'/usr/bin/unzip' ...\" exceeded the timeout of 300 seconds.",
			want: &result.Failure{
				Category:    "infrastructure",
				Code:        "package_extract_timeout",
				Summary:     "Composer package extraction exceeded the archive unzip timeout.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "search engine unsupported",
			stepName: "install",
			log:      "Search engine 'elasticsearch8' is not an available search engine.",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "search_engine_unsupported",
				Summary:     "This product version does not support the requested search engine identifier.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "db version unsupported",
			stepName: "smoke",
			log:      "Current version of RDBMS is not supported. Used Version: 10.11.16-MariaDB",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "db_version_unsupported",
				Summary:     "The product version does not support the selected database version.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "composer dependency conflict",
			stepName: "install",
			log:      "Your requirements could not be resolved to an installable set of packages. phpunit/phpunit requires sebastian/comparator ^4.0.10 but it conflicts with your root composer.json require (<=4.0.6).",
			want: &result.Failure{
				Category:    "harness",
				Code:        "composer_dependency_conflict",
				Summary:     "Unpinned Composer dependencies resolved to a set that conflicts with the harness root requirements.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "glob brace unsupported",
			stepName: "install",
			log:      "Undefined constant \"Magento\\\\Framework\\\\Setup\\\\Mvc\\\\GLOB_BRACE\"",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "glob_brace_unsupported",
				Summary:     "The application references an undefined GLOB_BRACE constant during setup bootstrap.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "package layout invalid",
			stepName: "install",
			log:      "Could not scan for classes inside \"/var/www/html/vendor/dg/bypass-finals/src/\" which does not appear to be a file nor a folder",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "package_layout_invalid",
				Summary:     "Installed package contents were incomplete or inconsistent for this release.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "smoke php84 nullable deprecation",
			stepName: "smoke",
			log:      "Implicitly marking parameter $scopeConfig as null able is deprecated, the explicit nullable type must be used instead",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "php84_nullable_deprecation",
				Summary:     "The product code hits PHP 8.4 implicit-nullable deprecations during compilation.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "smoke php syntax incompatible",
			stepName: "smoke",
			log:      "syntax error, unexpected '|', expecting ';' or '{'",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "php_syntax_incompatible",
				Summary:     "The product code uses PHP syntax unsupported by the selected PHP runtime.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "install php syntax incompatible",
			stepName: "install",
			log:      "syntax error, unexpected identifier \"TABLE_NAME\", expecting \"=\"",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "php_syntax_incompatible",
				Summary:     "The product code uses PHP syntax unsupported by the selected PHP runtime.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "compile class missing",
			stepName: "smoke",
			log:      "Class \"Magento\\\\Framework\\\\Exception\\\\NotFoundException\" not found",
			want: &result.Failure{
				Category:    "compatibility",
				Code:        "compile_class_missing",
				Summary:     "The installed code references classes that are missing for this release combination.",
				LikelyFlaky: false,
			},
		},
		{
			name:     "stack up no output",
			stepName: "stack_up",
			log:      "",
			want: &result.Failure{
				Category:    "harness",
				Code:        "stack_up_no_output",
				Summary:     "Docker Compose stack startup failed before the harness captured a specific error message.",
				LikelyFlaky: true,
			},
		},
		{
			name:     "unknown failure remains unclassified",
			stepName: "smoke",
			log:      "totally unknown failure signature",
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStepFailure(tc.stepName, tc.log)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("classifyStepFailure(...) = %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("classifyStepFailure(...) = nil, want classification")
			}
			if *got != *tc.want {
				t.Fatalf("classifyStepFailure(...) = %#v, want %#v", *got, *tc.want)
			}
		})
	}
}
