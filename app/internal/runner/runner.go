// Package runner orchestrates the multi-step Docker Compose test pipeline for
// a single Combination.  It is the Go equivalent of run_combination() in
// orchestrate.sh.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

const defaultMaxLogBytes int64 = 1 << 20 // 1 MiB per service

// RunConfig holds configuration for a single combination run.
type RunConfig struct {
	ResultsDir    string
	ComposeDir    string
	PlaywrightDir string // path to tests/playwright; empty = skip playwright
	Force         bool
	MaxLogBytes   int64 // bytes to tail per container log; 0 = use default (1 MiB)
}

// searchConfigFlag returns the search type identifier for the Magento install
// command. Elasticsearch requires its major version as a suffix (e.g. "elasticsearch8").
func searchConfigFlag(searchType, searchVersion string) string {
	if searchType == "elasticsearch" && len(searchVersion) > 0 {
		return searchType + string(searchVersion[0])
	}
	return searchType
}

// buildMagentoEnv returns the KEY=VALUE environment pairs consumed by
// install.sh and the smoke / playwright test scripts.
func buildMagentoEnv(c matrix.Combination, searchFlag string) []string {
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
		"SEARCH_HOST=search",
		"SEARCH_PORT=9200",
		"CACHE_HOST=cache",
		"CACHE_PORT=6379",
		"QUEUE_HOST=queue",
		"QUEUE_PORT=5672",
		"QUEUE_USER=magento",
		"QUEUE_PASSWORD=magento",
		"MAGENTO_BASE_URL=http://localhost",
		"INSTALL_SAMPLE_DATA=0",
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

	magentoEnv := buildMagentoEnv(c, searchConfigFlag(c.SearchType, c.SearchVersion))

	cp, err := newCompose(c, cfg.ComposeDir, magentoEnv)
	if err != nil {
		return false, fmt.Errorf("runner: building compose for %s: %w", c.ID(), err)
	}

	steps := make(map[string]result.Step)
	overallStatus := result.StatusPass

	recordStep := func(name, status string, dur float64, log string) {
		steps[name] = result.Step{Status: status, DurationS: dur, Log: log}
		if status != result.StatusPass && status != result.StatusSkip {
			overallStatus = result.StatusFail
		}
	}

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
		recordStep("stack_up", result.StatusFail, dur, upLog)
		return true, writeResult(ctx, resultPath, c, steps, overallStatus, cp, maxLog)
	}
	recordStep("stack_up", result.StatusPass, dur, upLog)

	baseURL := resolveBaseURL(ctx, c, cp)

	t = time.Now()
	installLog, installErr := cp.Exec(ctx, "php-fpm", buildInstallArgs(magentoEnv, baseURL)...)
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
		pwLog, pwErr := runPlaywright(ctx, cfg.PlaywrightDir, baseURL, c.ID())
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
func playwrightEnv(baseURL string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MAGENTO_BASE_URL=") {
			env = append(env, kv)
		}
	}
	return append(env, "MAGENTO_BASE_URL="+baseURL)
}

// runPlaywright executes the Playwright test suite on the host machine.
// playwrightDir is the path to the tests/playwright directory.
func runPlaywright(ctx context.Context, playwrightDir, baseURL, combinationID string) (string, error) {
	npx, err := exec.LookPath("npx")
	if err != nil {
		return "npx not found in PATH — cannot run Playwright", fmt.Errorf("npx not found: %w", err)
	}

	outputDir := filepath.Join("test-results", combinationID)
	cmd := exec.CommandContext(ctx, npx, "playwright", "test", "--project", "chromium", "--output", outputDir)
	cmd.Dir = playwrightDir
	cmd.Env = playwrightEnv(baseURL)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	return buf.String(), cmd.Run()
}
