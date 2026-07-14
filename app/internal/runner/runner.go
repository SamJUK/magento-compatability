// Package runner orchestrates the multi-step Docker Compose test pipeline for
// a single Combination.  It is the Go equivalent of run_combination() in
// orchestrate.sh.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

const defaultMaxLogBytes int64 = 1 << 20 // 1 MiB per service

const (
	installRetryAttempts = 3
	installRetryDelay    = 5 * time.Second
)

var transientComposerCurlErrors = []string{
	"curl error 6",
	"curl error 7",
	"curl error 28",
	"curl error 56",
}

var retryableInstallFailureSignatures = []string{
	"could not connect to the amqp server",
	"no alive nodes found in your cluster",
	"corrupted zip archive (0 bytes), try again",
	"does not contain valid json",
}

func classifyStepFailure(stepName, log string) *result.Failure {
	return classifyStepFailureForCombination(matrix.Combination{}, stepName, log)
}

func classifyStepFailureForCombination(c matrix.Combination, stepName, log string) *result.Failure {
	text := compactWhitespace(strings.ToLower(log))

	switch {
	case stepName == "install" && isTransientComposerNetworkFailure(text):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "composer_network",
			Summary:     "Composer download failed with a transient curl/network error.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "could not connect to the amqp server"):
		return &result.Failure{
			Category:    "harness",
			Code:        "queue_not_ready",
			Summary:     "The queue service was reachable but not yet ready when Magento validated AMQP connectivity.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "search engine cluster not healthy"):
		return &result.Failure{
			Category:    "harness",
			Code:        "search_cluster_unhealthy",
			Summary:     "The search service never reached cluster health before the harness timeout elapsed.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "/usr/bin/unzip") &&
		strings.Contains(text, "exceeded the timeout of 300 seconds"):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "package_extract_timeout",
			Summary:     "Composer package extraction exceeded the archive unzip timeout.",
			LikelyFlaky: true,
		}
	case stepName == "install" && isKnownElasticsearch8CompatibilityFailure(c, text):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "elasticsearch8_unsupported",
			Summary:     "This product version could not complete setup:install against Elasticsearch 8.x.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "search engine 'elasticsearch") &&
		strings.Contains(text, "is not an available search engine"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "search_engine_unsupported",
			Summary:     "This product version does not support the requested search engine identifier.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") && strings.Contains(text, "current version of rdbms is not supported"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "db_version_unsupported",
			Summary:     "The product version does not support the selected database version.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "your requirements could not be resolved to an installable set of packages") &&
		strings.Contains(text, "sebastian/comparator"):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_dependency_conflict",
			Summary:     "Unpinned Composer dependencies resolved to a set that conflicts with the harness root requirements.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "your php version") &&
		strings.Contains(text, "does not satisfy that requirement"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "php_version_unsupported",
			Summary:     "The product's Composer constraints do not allow this PHP version.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "glob_brace"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "glob_brace_unsupported",
			Summary:     "The application references an undefined GLOB_BRACE constant during setup bootstrap.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") && (strings.Contains(text, "could not scan for classes inside") &&
		strings.Contains(text, "does not appear to be a file nor a folder") ||
		strings.Contains(text, "failed to open stream: no such file or directory") ||
		strings.Contains(text, "file doesn't exist")):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "package_layout_invalid",
			Summary:     "Installed package contents were incomplete or inconsistent for this release.",
			LikelyFlaky: false,
		}
	case stepName == "install" && (strings.Contains(text, "the \"--opensearch-host\" option does not exist") ||
		strings.Contains(text, "the \"--opensearch-port\" option does not exist")):
		return &result.Failure{
			Category:    "harness",
			Code:        "legacy_search_flags",
			Summary:     "The harness invoked setup:install with OpenSearch host flags that this Magento version does not support.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "no alive nodes found in your cluster"):
		return &result.Failure{
			Category:    "harness",
			Code:        "search_not_ready",
			Summary:     "The search service was reachable but not yet ready when Magento validated the search backend.",
			LikelyFlaky: true,
		}
	case stepName == "install" && (strings.Contains(text, "corrupted zip archive (0 bytes)") ||
		strings.Contains(text, "does not contain valid json")):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_cache_corruption",
			Summary:     "Shared Composer cache state became corrupted during the harness run.",
			LikelyFlaky: true,
		}
	case (stepName == "stack_up" || stepName == "install") && strings.Contains(text, "no space left on device"):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "disk_space",
			Summary:     "The run exhausted host or Docker disk space.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && (strings.Contains(text, "already in use by container") ||
		strings.Contains(text, "endpoint with name") && strings.Contains(text, "already exists in network") ||
		strings.Contains(text, "network with name") && strings.Contains(text, "already exists")):
		return &result.Failure{
			Category:    "harness",
			Code:        "docker_name_conflict",
			Summary:     "Docker Compose found stale project resources with colliding container or network names.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && (strings.Contains(text, "no such container") ||
		strings.Contains(text, "network has active endpoints") ||
		strings.Contains(text, "resource is still in use") ||
		(strings.Contains(text, "container ") && strings.Contains(text, " is not running"))):
		return &result.Failure{
			Category:    "harness",
			Code:        "docker_cleanup_race",
			Summary:     "Docker Compose hit a cleanup/startup race with stale containers, networks, or volumes.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && (strings.Contains(text, "php-fpm is missing dependency") ||
		(strings.Contains(text, "failed to start") && strings.Contains(text, "exited (")) ||
		strings.Contains(text, "container ") && strings.Contains(text, " exited (") ||
		strings.Contains(text, "sigtrap")):
		return &result.Failure{
			Category:    "harness",
			Code:        "service_startup",
			Summary:     "A dependency container exited during stack startup before Magento install began.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && text == "":
		return &result.Failure{
			Category:    "harness",
			Code:        "stack_up_no_output",
			Summary:     "Docker Compose stack startup failed before the harness captured a specific error message.",
			LikelyFlaky: true,
		}
	case stepName == "install" && (strings.Contains(text, "pluginblockedexception") ||
		strings.Contains(text, "allow-plugins") ||
		strings.Contains(text, "contains a composer plugin which is blocked")):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_allow_plugins",
			Summary:     "Composer plugin execution was blocked by harness configuration.",
			LikelyFlaky: true,
		}
	case stepName == "install" && (strings.Contains(text, "block-insecure") ||
		strings.Contains(text, "security vulnerability advis") ||
		strings.Contains(text, "run composer audit")):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_audit_policy",
			Summary:     "Composer audit policy blocked the install in the harness.",
			LikelyFlaky: true,
		}
	case stepName == "smoke" && strings.Contains(text, "implicitly marking parameter") &&
		strings.Contains(text, "nullable type must be used instead"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "php84_nullable_deprecation",
			Summary:     "The product code hits PHP 8.4 implicit-nullable deprecations during compilation.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") && strings.Contains(text, "syntax error, unexpected"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "php_syntax_incompatible",
			Summary:     "The product code uses PHP syntax unsupported by the selected PHP runtime.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") &&
		strings.Contains(text, "class \"") && strings.Contains(text, "\" not found"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "compile_class_missing",
			Summary:     "The installed code references classes that are missing for this release combination.",
			LikelyFlaky: false,
		}
	default:
		return nil
	}
}

func isKnownElasticsearch8CompatibilityFailure(c matrix.Combination, log string) bool {
	return c.SearchType == "elasticsearch" &&
		strings.HasPrefix(c.SearchVersion, "8.") &&
		strings.Contains(log, "could not validate a connection to") &&
		strings.Contains(log, "no alive nodes found in") &&
		strings.Contains(log, "cluster")
}

// RunConfig holds configuration for a single combination run.
type RunConfig struct {
	ResultsDir        string
	ComposeDir        string
	PlaywrightDir     string // path to tests/playwright; empty = skip playwright
	InstallSampleData bool
	Force             bool
	MaxLogBytes       int64 // bytes to tail per container log; 0 = use default (1 MiB)
}

// searchConfigFlag returns the search type identifier for the Magento install
// command. Elasticsearch requires its major version as a suffix (e.g.
// "elasticsearch8"), and some Magento OpenSearch releases still expect the
// legacy "elasticsearch7" identifier.
func searchConfigFlag(c matrix.Combination) string {
	if usesLegacyMagentoOpenSearchInstall(c) {
		return "elasticsearch7"
	}
	if c.SearchType == "elasticsearch" && len(c.SearchVersion) > 0 {
		return c.SearchType + string(c.SearchVersion[0])
	}
	return c.SearchType
}

// searchHostFlagStyle selects which setup:install host/port option family to
// use. Some Magento 2.4.4/2.4.5 OpenSearch releases still require the legacy
// --elasticsearch-host/port flags even when search-engine=opensearch.
func searchHostFlagStyle(c matrix.Combination) string {
	if c.SearchType != "opensearch" {
		return "elasticsearch"
	}
	if usesLegacyMagentoOpenSearchInstall(c) {
		return "elasticsearch"
	}
	return "opensearch"
}

func usesLegacyMagentoOpenSearchInstall(c matrix.Combination) bool {
	return c.Package == "magento/project-community-edition" &&
		c.SearchType == "opensearch" &&
		compareMagentoVersion(c.Version, "2.4.6") < 0
}

func magentoVersionBetween(version, minVersion, maxVersion string) bool {
	return compareMagentoVersion(version, minVersion) >= 0 && compareMagentoVersion(version, maxVersion) <= 0
}

func compareMagentoVersion(a, b string) int {
	av, ok := parseMagentoVersion(a)
	if !ok {
		return strings.Compare(a, b)
	}
	bv, ok := parseMagentoVersion(b)
	if !ok {
		return strings.Compare(a, b)
	}

	switch {
	case av.major != bv.major:
		if av.major < bv.major {
			return -1
		}
	case av.minor != bv.minor:
		if av.minor < bv.minor {
			return -1
		}
	case av.patch != bv.patch:
		if av.patch < bv.patch {
			return -1
		}
	case av.patchLevel != bv.patchLevel:
		if av.patchLevel < bv.patchLevel {
			return -1
		}
	default:
		return 0
	}
	return 1
}

type magentoVersion struct {
	major      int
	minor      int
	patch      int
	patchLevel int
}

func parseMagentoVersion(version string) (magentoVersion, bool) {
	base := version
	patchLevel := 0

	if idx := strings.Index(version, "-p"); idx >= 0 {
		base = version[:idx]
		n, err := strconv.Atoi(version[idx+2:])
		if err != nil {
			return magentoVersion{}, false
		}
		patchLevel = n
	}

	var parsed magentoVersion
	if _, err := fmt.Sscanf(base, "%d.%d.%d", &parsed.major, &parsed.minor, &parsed.patch); err != nil {
		return magentoVersion{}, false
	}
	parsed.patchLevel = patchLevel
	return parsed, true
}

// buildMagentoEnv returns the KEY=VALUE environment pairs consumed by
// install.sh and the smoke / playwright test scripts.
func buildMagentoEnv(c matrix.Combination, searchFlag string, installSampleData bool) []string {
	sampleDataValue := "0"
	if installSampleData {
		sampleDataValue = "1"
	}
	return []string{
		"PRODUCT_PACKAGE=" + c.Package,
		"PRODUCT_VERSION=" + c.Version,
		"PHP_VERSION=" + c.PHP,
		"MIRROR_URL=" + c.Mirror,
		"DB_HOST=db",
		"DB_PORT=3306",
		"DB_NAME=magento",
		"DB_USER=magento",
		"DB_PASSWORD=magento",
		"SEARCH_TYPE=" + searchFlag,
		"SEARCH_HOST_FLAG_STYLE=" + searchHostFlagStyle(c),
		"SEARCH_HOST=search",
		"SEARCH_PORT=9200",
		"CACHE_HOST=cache",
		"CACHE_PORT=6379",
		"QUEUE_HOST=queue",
		"QUEUE_PORT=5672",
		"QUEUE_USER=magento",
		"QUEUE_PASSWORD=magento",
		"MAGENTO_BASE_URL=http://localhost",
		"INSTALL_SAMPLE_DATA=" + sampleDataValue,
	}
}

// resolveBaseURL discovers the host-side mapped port and returns the base URL
// to use for the Magento install and browser tests. Varnish fronts HTTP when
// enabled, otherwise the webserver port is used. Falls back to port 80.
func resolveBaseURL(ctx context.Context, c matrix.Combination, cp *Compose) string {
	portSvc := "webserver"
	if c.Varnish != "none" && c.Varnish != "" {
		portSvc = "varnish"
	}
	port, err := cp.Port(ctx, portSvc, 80)
	if err != nil || port == "" {
		port = "80"
	}
	return "http://localhost:" + port
}

// buildInstallArgs constructs the exec argv for running install.sh inside the
// php-fpm container with the Magento environment set and the correct base URL.
func buildInstallArgs(env []string, baseURL string) []string {
	args := make([]string, 0, len(env)+3)
	args = append(args, "env")
	args = append(args, env...)
	args = append(args, "MAGENTO_BASE_URL="+baseURL+"/")
	args = append(args, "bash", "/scripts/install.sh")
	return args
}

func isTransientComposerNetworkFailure(log string) bool {
	log = compactWhitespace(strings.ToLower(log))
	for _, sig := range transientComposerCurlErrors {
		if strings.Contains(log, sig) {
			return true
		}
	}
	return false
}

func isRetryableInstallFailure(log string) bool {
	text := compactWhitespace(strings.ToLower(log))
	if isTransientComposerNetworkFailure(text) {
		return true
	}
	for _, sig := range retryableInstallFailureSignatures {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

func compactWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func formatStackUpFailureLog(projectName, upLog, psLog, composeLogs string) string {
	var parts []string

	if trimmed := strings.TrimSpace(upLog); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if trimmed := strings.TrimSpace(psLog); trimmed != "" {
		parts = append(parts, "=== docker compose ps -a ===\n"+trimmed)
	}
	if trimmed := strings.TrimSpace(composeLogs); trimmed != "" {
		parts = append(parts, "=== docker compose logs ===\n"+trimmed)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("[ERROR] docker compose up failed for project %s but produced no stdout/stderr output", projectName)
	}
	return strings.Join(parts, "\n\n")
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runInstallWithRetries(
	ctx context.Context,
	execInstall func() (string, error),
	sleep func(context.Context, time.Duration) error,
) (string, error) {
	var combined strings.Builder

	for attempt := 1; attempt <= installRetryAttempts; attempt++ {
		if attempt > 1 {
			fmt.Fprintf(&combined, "\n=== Install retry attempt %d/%d ===\n", attempt, installRetryAttempts)
		}

		out, err := execInstall()
		combined.WriteString(out)
		if err == nil {
			return combined.String(), nil
		}
		if !isRetryableInstallFailure(out) || attempt == installRetryAttempts {
			return combined.String(), err
		}

		delay := installRetryDelay * time.Duration(attempt)
		fmt.Fprintf(
			&combined,
			"\n[WARN] Retryable install failure detected; retrying install in %s (attempt %d/%d)\n",
			delay,
			attempt+1,
			installRetryAttempts,
		)
		if err := sleep(ctx, delay); err != nil {
			return combined.String(), err
		}
	}

	return combined.String(), nil
}

// Run executes the full test pipeline for one combination and writes a result
// JSON file.  It returns (true, nil) when the result was written, (false, nil)
// when skipped (already exists and Force=false).
func Run(ctx context.Context, c matrix.Combination, cfg RunConfig) (ran bool, err error) {
	maxLog := cfg.MaxLogBytes
	if maxLog == 0 {
		maxLog = defaultMaxLogBytes
	}

	resultPath := filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json")

	if !cfg.Force {
		if _, statErr := os.Stat(resultPath); statErr == nil {
			return false, nil
		}
	}

	magentoEnv := buildMagentoEnv(c, searchConfigFlag(c), cfg.InstallSampleData)

	cp, err := newCompose(c, cfg.ComposeDir, magentoEnv)
	if err != nil {
		return false, fmt.Errorf("runner: building compose for %s: %w", c.ID(), err)
	}

	steps := make(map[string]result.Step)
	overallStatus := result.StatusPass

	recordStep := func(name, status string, dur float64, log string) {
		step := result.Step{Status: status, DurationS: dur, Log: log}
		if status == result.StatusFail || status == result.StatusError {
			step.Failure = classifyStepFailureForCombination(c, name, log)
		}
		steps[name] = step
		if status != result.StatusPass && status != result.StatusSkip {
			overallStatus = result.StatusFail
		}
	}

	// Reruns reuse the same deterministic project name, so clear any leftover
	// resources from earlier failed attempts before bringing the stack up again.
	_ = cp.Down(context.Background())

	defer func() {
		_ = cp.Down(context.Background())
		// Don't persist a partial result from a cancelled run — it would
		// block re-runs without --force.
		if ctx.Err() != nil {
			os.Remove(resultPath)
			ran = false
			err = nil
		}
	}()

	t := time.Now()
	upLog, upErr := cp.Up(ctx)
	dur := time.Since(t).Seconds()
	if upErr != nil {
		psLog, _ := cp.run(ctx, "ps", "-a")
		composeLogs, _ := cp.run(ctx, "logs", "--no-color", "--timestamps")
		upLog = formatStackUpFailureLog(cp.ProjectName(), upLog, psLog, composeLogs)
		recordStep("stack_up", result.StatusFail, dur, upLog)
		return true, writeResult(ctx, resultPath, c, steps, overallStatus, cp, maxLog)
	}
	recordStep("stack_up", result.StatusPass, dur, upLog)

	baseURL := resolveBaseURL(ctx, c, cp)
	installArgs := buildInstallArgs(magentoEnv, baseURL)

	t = time.Now()
	installLog, installErr := runInstallWithRetries(
		ctx,
		func() (string, error) {
			return cp.Exec(ctx, "php-fpm", installArgs...)
		},
		sleepWithContext,
	)
	dur = time.Since(t).Seconds()
	if installErr != nil {
		recordStep("install", result.StatusFail, dur, installLog)
		return true, writeResult(ctx, resultPath, c, steps, overallStatus, cp, maxLog)
	}
	recordStep("install", result.StatusPass, dur, installLog)

	t = time.Now()
	smokeLog, smokeErr := cp.Exec(ctx, "php-fpm", "bash", "/scripts/tests/smoke.sh")
	dur = time.Since(t).Seconds()
	if smokeErr != nil {
		recordStep("smoke", result.StatusFail, dur, smokeLog)
		return true, writeResult(ctx, resultPath, c, steps, overallStatus, cp, maxLog)
	}
	recordStep("smoke", result.StatusPass, dur, smokeLog)

	if cfg.PlaywrightDir == "" {
		recordStep("playwright", result.StatusSkip, 0, "Playwright skipped — no playwright dir configured")
	} else {
		t = time.Now()
		pwLog, pwErr := runPlaywright(ctx, cfg.PlaywrightDir, baseURL, c.ID(), cfg.InstallSampleData)
		dur = time.Since(t).Seconds()
		if pwErr != nil {
			recordStep("playwright", result.StatusFail, dur, pwLog)
		} else {
			recordStep("playwright", result.StatusPass, dur, pwLog)
		}
	}

	return true, writeResult(ctx, resultPath, c, steps, overallStatus, cp, maxLog)
}

// writeResult captures container logs then persists the result file.
func writeResult(
	ctx context.Context,
	path string,
	c matrix.Combination,
	steps map[string]result.Step,
	overallStatus string,
	cp *Compose,
	maxLog int64,
) error {
	containerLogs := captureContainerLogs(ctx, cp, maxLog)

	r := result.TestResult{
		ID:            c.ID(),
		Product:       c.Product,
		Version:       c.Version,
		OverallStatus: overallStatus,
		Services: result.Services{
			PHP:       c.PHP,
			Webserver: c.WebserverType,
			DB:        result.DBService{Type: c.DBType, Version: c.DBVersion},
			Search:    result.DBService{Type: c.SearchType, Version: c.SearchVersion},
			Cache:     result.DBService{Type: c.CacheType, Version: c.CacheVersion},
			Queue:     result.DBService{Type: c.QueueType, Version: c.QueueVersion},
			Varnish:   c.Varnish,
		},
		Steps:         steps,
		ContainerLogs: containerLogs,
		Timestamp:     result.Now(),
	}

	return result.Write(path, r)
}

// captureContainerLogs collects per-service logs while the stack is still up.
// Returns nil map if the stack is already gone.
func captureContainerLogs(ctx context.Context, cp *Compose, maxLog int64) map[string]string {
	svcs, err := cp.Services(ctx)
	if err != nil || len(svcs) == 0 {
		return nil
	}

	logs := make(map[string]string, len(svcs))
	for _, svc := range svcs {
		log, _ := cp.Logs(ctx, svc, maxLog)
		logs[svc] = log
	}
	return logs
}

// playwrightEnv returns the subprocess environment for Playwright: the current
// process env with MAGENTO_BASE_URL replaced so tests hit the right stack.
// PLAYWRIGHT_BROWSERS_PATH is intentionally left alone — let it use the cache.
func playwrightEnv(baseURL, reportFile string, sampleDataEnabled bool) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MAGENTO_BASE_URL=") &&
			!strings.HasPrefix(kv, "PLAYWRIGHT_REPORT_FILE=") &&
			!strings.HasPrefix(kv, "PLAYWRIGHT_SAMPLE_DATA=") &&
			!strings.HasPrefix(kv, "FORCE_COLOR=") &&
			!strings.HasPrefix(kv, "NO_COLOR=") {
			env = append(env, kv)
		}
	}
	sampleDataValue := "0"
	if sampleDataEnabled {
		sampleDataValue = "1"
	}
	return append(env,
		"MAGENTO_BASE_URL="+baseURL,
		"PLAYWRIGHT_REPORT_FILE="+reportFile,
		"PLAYWRIGHT_SAMPLE_DATA="+sampleDataValue,
	)
}

// runPlaywright executes the Playwright test suite on the host machine.
// playwrightDir is the path to the tests/playwright directory.
func runPlaywright(ctx context.Context, playwrightDir, baseURL, combinationID string, sampleDataEnabled bool) (string, error) {
	npx, err := exec.LookPath("npx")
	if err != nil {
		return "npx not found in PATH — cannot run Playwright", fmt.Errorf("npx not found: %w", err)
	}

	outputDir := filepath.Join("test-results", combinationID)
	reportFile := filepath.Join("playwright-report", combinationID+".json")
	cmd := exec.CommandContext(ctx, npx, "playwright", "test", "--project", "chromium", "--output", outputDir)
	cmd.Dir = playwrightDir
	cmd.Env = playwrightEnv(baseURL, reportFile, sampleDataEnabled)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err = cmd.Run()
	return summarisePlaywrightRun(buf.String(), filepath.Join(playwrightDir, reportFile), baseURL), err
}

type playwrightReport struct {
	Stats struct {
		Expected   int `json:"expected"`
		Skipped    int `json:"skipped"`
		Unexpected int `json:"unexpected"`
		Flaky      int `json:"flaky"`
	} `json:"stats"`
	Suites []playwrightSuite `json:"suites"`
}

type playwrightSuite struct {
	Title  string            `json:"title"`
	Specs  []playwrightSpec  `json:"specs"`
	Suites []playwrightSuite `json:"suites"`
}

type playwrightSpec struct {
	Title  string           `json:"title"`
	Status string           `json:"status"`
	Tests  []playwrightTest `json:"tests"`
}

type playwrightTest struct {
	Results []playwrightResult `json:"results"`
}

type playwrightResult struct {
	Status string `json:"status"`
}

func summarisePlaywrightRun(stdout, reportPath, baseURL string) string {
	stdout = strings.TrimSpace(stdout)

	reportSummary, err := readPlaywrightReportSummary(reportPath, baseURL)
	switch {
	case err != nil && stdout != "":
		return stdout + "\n\n[WARN] Unable to read Playwright report: " + err.Error()
	case err != nil:
		return "[WARN] Unable to read Playwright report: " + err.Error()
	case stdout == "":
		return reportSummary
	default:
		return stdout + "\n\n" + reportSummary
	}
}

func readPlaywrightReportSummary(reportPath, baseURL string) (string, error) {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return "", err
	}

	var report playwrightReport
	if err := json.Unmarshal(data, &report); err != nil {
		return "", err
	}

	failed := collectFailedPlaywrightSpecs(report.Suites)

	var buf strings.Builder
	fmt.Fprintf(&buf, "Playwright base URL: %s\n", baseURL)
	fmt.Fprintf(&buf, "Playwright summary: expected=%d unexpected=%d flaky=%d skipped=%d\n",
		report.Stats.Expected,
		report.Stats.Unexpected,
		report.Stats.Flaky,
		report.Stats.Skipped,
	)

	if len(failed) == 0 {
		buf.WriteString("Playwright failures: none")
		return buf.String(), nil
	}

	buf.WriteString("Playwright failures:\n")
	for _, title := range failed {
		buf.WriteString(" - ")
		buf.WriteString(title)
		buf.WriteByte('\n')
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func collectFailedPlaywrightSpecs(suites []playwrightSuite) []string {
	var failed []string
	var walk func([]playwrightSuite)
	walk = func(suites []playwrightSuite) {
		for _, suite := range suites {
			for _, spec := range suite.Specs {
				if playwrightSpecFailed(spec) {
					failed = append(failed, spec.Title)
				}
			}
			if len(suite.Suites) > 0 {
				walk(suite.Suites)
			}
		}
	}
	walk(suites)
	return failed
}

func playwrightSpecFailed(spec playwrightSpec) bool {
	for _, test := range spec.Tests {
		for _, result := range test.Results {
			if result.Status == "failed" || result.Status == "timedOut" || result.Status == "interrupted" {
				return true
			}
		}
	}
	return false
}
