// Package result defines the canonical TestResult type and helpers for reading
// and writing result JSON files (results/{product}/*.json).
//
// The JSON schema is intentionally kept identical to what the Astro site
// expects — do not rename JSON fields without updating site/src/lib/types.ts.
package result

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// Status values used across steps and overall_status.
const (
	StatusPass  = "pass"
	StatusFail  = "fail"
	StatusSkip  = "skip"
	StatusError = "error"
)

// Step holds the outcome of one orchestration step.
type Step struct {
	Status    string  `json:"status"`
	DurationS float64 `json:"duration_s"`
	Log       string  `json:"log"`
}

// DBService describes a typed service (db, search, cache, queue).
type DBService struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

// Services mirrors the services sub-object in the result JSON.
type Services struct {
	PHP       string    `json:"php"`
	Webserver string    `json:"webserver"`
	DB        DBService `json:"db"`
	Search    DBService `json:"search"`
	Cache     DBService `json:"cache"`
	Queue     DBService `json:"queue"`
	Varnish   string    `json:"varnish"`
}

// TestResult is the canonical result record written to disk and read by the
// Astro site and the reporter.
type TestResult struct {
	ID            string            `json:"id"`
	Product       string            `json:"product"`
	Version       string            `json:"version"`
	OverallStatus string            `json:"overall_status"`
	Services      Services          `json:"services"`
	Steps         map[string]Step   `json:"steps"`
	ContainerLogs map[string]string `json:"container_logs,omitempty"`
	Timestamp     string            `json:"timestamp"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// OverallStatus derives pass/fail from a step map.
// Returns "pass" only when every step is "pass" or "skip".
func OverallStatus(steps map[string]Step) string {
	for _, s := range steps {
		if s.Status != StatusPass && s.Status != StatusSkip {
			return StatusFail
		}
	}
	return StatusPass
}

// Now returns the current UTC time formatted as an ISO-8601 timestamp.
func Now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ── Write ─────────────────────────────────────────────────────────────────────

// Write serialises r as indented JSON to path, creating parent directories as
// needed. This replaces write_result_json in scripts/lib.sh and, crucially,
// never calls jq — eliminating the --argjson failures caused by unescaped log
// content.
func Write(path string, r TestResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("result: mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("result: marshalling %s: %w", r.ID, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("result: writing %s: %w", path, err)
	}
	return nil
}

// ── ReadAll ───────────────────────────────────────────────────────────────────

// ReadAll walks resultsDir/{product}/*.json and returns every valid result.
// Malformed files are silently skipped (matching the Astro site behaviour).
func ReadAll(resultsDir string) ([]TestResult, error) {
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("result: reading %s: %w", resultsDir, err)
	}

	var results []TestResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		productDir := filepath.Join(resultsDir, e.Name())
		files, err := os.ReadDir(productDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") || f.Name() == ".gitkeep" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(productDir, f.Name()))
			if err != nil {
				continue
			}
			var r TestResult
			if err := json.Unmarshal(data, &r); err != nil {
				continue
			}
			if r.ID != "" && r.Product != "" && r.Version != "" {
				results = append(results, r)
			}
		}
	}
	return results, nil
}
