package result_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file is .../app/internal/result/result_test.go; root is three levels up
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func TestReadAll_ExistingResults(t *testing.T) {
	resultsDir := filepath.Join(repoRoot(t), "results")
	if _, err := os.Stat(resultsDir); os.IsNotExist(err) {
		t.Skip("no results directory")
	}

	results, err := result.ReadAll(resultsDir)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	for _, r := range results {
		if r.ID == "" {
			t.Error("result with empty ID")
		}
		if r.Product == "" {
			t.Error("result with empty Product")
		}
		if r.Version == "" {
			t.Error("result with empty Version")
		}
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	tmp := t.TempDir()

	c := matrix.Combination{
		Product:       "magento",
		Package:       "magento/project-community-edition",
		Mirror:        "https://example.com/",
		Version:       "2.4.8",
		PHP:           "8.3",
		WebserverType: "apache",
		WebserverVersion: "2.4",
		DBType:        "mariadb",
		DBVersion:     "11.4",
		SearchType:    "opensearch",
		SearchVersion: "3",
		CacheType:     "valkey",
		CacheVersion:  "8",
		QueueType:     "rabbitmq",
		QueueVersion:  "4.2",
		Varnish:       "7.7",
	}

	steps := map[string]result.Step{
		"stack_up": {Status: result.StatusPass, DurationS: 12.5, Log: "All services healthy"},
		"install":  {Status: result.StatusPass, DurationS: 180.2, Log: "Installation complete"},
		"smoke":    {Status: result.StatusPass, DurationS: 45.0, Log: "Smoke passed"},
		"playwright": {Status: result.StatusSkip, DurationS: 0, Log: "Playwright disabled"},
	}

	r := result.TestResult{
		ID:            c.ID(),
		Product:       c.Product,
		Version:       c.Version,
		OverallStatus: result.OverallStatus(steps),
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
		ContainerLogs: map[string]string{"php-fpm": "PHP started OK"},
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
	}

	path := filepath.Join(tmp, "magento", r.ID+".json")
	if err := result.Write(path, r); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var got result.TestResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if got.ID != r.ID {
		t.Errorf("ID mismatch: got %q want %q", got.ID, r.ID)
	}
	if got.OverallStatus != result.StatusPass {
		t.Errorf("expected pass, got %q", got.OverallStatus)
	}
	if got.ContainerLogs["php-fpm"] != "PHP started OK" {
		t.Errorf("container log not preserved")
	}
}

func TestWrite_ContainerLogWithSpecialChars(t *testing.T) {
	// Ensures logs with characters that break --argjson are handled correctly.
	tmp := t.TempDir()

	steps := map[string]result.Step{
		"stack_up": {Status: result.StatusFail, DurationS: 5, Log: `Error: "container" failed with 'exit code' 1 & \n special`},
	}

	r := result.TestResult{
		ID:            "test-special-chars",
		Product:       "magento",
		Version:       "2.4.8",
		OverallStatus: result.StatusFail,
		Steps:         steps,
		ContainerLogs: map[string]string{
			"php-fpm": `line with "quotes" and 'apostrophes' and backslash \ and newlines`,
		},
		Timestamp: result.Now(),
	}

	path := filepath.Join(tmp, r.ID+".json")
	if err := result.Write(path, r); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	var got result.TestResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("JSON with special chars failed to unmarshal: %v", err)
	}
	if !strings.Contains(got.ContainerLogs["php-fpm"], "quotes") {
		t.Error("special chars not preserved in container logs")
	}
}

func TestOverallStatus(t *testing.T) {
	cases := []struct {
		steps  map[string]result.Step
		expect string
	}{
		{map[string]result.Step{
			"install": {Status: "pass"},
			"smoke":   {Status: "pass"},
		}, "pass"},
		{map[string]result.Step{
			"install":    {Status: "pass"},
			"playwright": {Status: "skip"},
		}, "pass"},
		{map[string]result.Step{
			"install": {Status: "fail"},
		}, "fail"},
	}
	for _, tc := range cases {
		got := result.OverallStatus(tc.steps)
		if got != tc.expect {
			t.Errorf("OverallStatus(%v) = %q, want %q", tc.steps, got, tc.expect)
		}
	}
}
