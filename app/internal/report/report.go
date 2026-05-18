// Package report aggregates TestResult slices and writes MD, CSV, and JSON
// output files — a direct Go replacement of reporter.sh.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/samjuk/magento-compatability/internal/result"
)

// ── Step helpers ──────────────────────────────────────────────────────────────

func stepIcon(status string) string {
	switch status {
	case result.StatusPass:
		return "✅"
	case result.StatusFail:
		return "❌"
	case result.StatusSkip:
		return "⏭️"
	case result.StatusError:
		return "⚠️"
	default:
		return "❓"
	}
}

func stepPlain(status string) string {
	switch status {
	case result.StatusPass:
		return "PASS"
	case result.StatusFail:
		return "FAIL"
	case result.StatusSkip:
		return "SKIP"
	case result.StatusError:
		return "ERROR"
	default:
		return "?"
	}
}

func stepStatus(r result.TestResult, step string) string {
	if s, ok := r.Steps[step]; ok {
		return s.Status
	}
	return result.StatusSkip
}

func rowPassed(r result.TestResult) bool {
	install := stepStatus(r, "install")
	smoke := stepStatus(r, "smoke")
	pw := stepStatus(r, "playwright")
	return install == result.StatusPass &&
		smoke == result.StatusPass &&
		(pw == result.StatusPass || pw == result.StatusSkip)
}

// ── Aggregate ─────────────────────────────────────────────────────────────────

// Aggregate groups results by product name, sorted by ID within each group.
func Aggregate(results []result.TestResult) map[string][]result.TestResult {
	m := make(map[string][]result.TestResult)
	for _, r := range results {
		m[r.Product] = append(m[r.Product], r)
	}
	for product := range m {
		sort.Slice(m[product], func(i, j int) bool {
			return m[product][i].ID < m[product][j].ID
		})
	}
	return m
}

// AnyFailed returns true if any result in the slice is a failure.
func AnyFailed(results []result.TestResult) bool {
	for _, r := range results {
		if !rowPassed(r) {
			return true
		}
	}
	return false
}

// Summary returns (passed, total) counts for a slice.
func Summary(results []result.TestResult) (passed, total int) {
	total = len(results)
	for _, r := range results {
		if rowPassed(r) {
			passed++
		}
	}
	return
}

// ── WriteMD ───────────────────────────────────────────────────────────────────

// WriteMD writes a GitHub Flavoured Markdown table to w.
func WriteMD(w io.Writer, product string, results []result.TestResult) error {
	fmt.Fprintf(w, "# %s Test Results\n\n", product)
	fmt.Fprintln(w, "| Combo ID | Version | PHP | Webserver | DB | Search | Cache | Queue | Varnish | Stack Up | Install | Smoke | Playwright |")
	fmt.Fprintln(w, "|----------|---------|-----|-----------|-----|--------|-------|-------|---------|----------|---------|-------|------------|")

	for _, r := range results {
		db := r.Services.DB.Type + ":" + r.Services.DB.Version
		search := r.Services.Search.Type + ":" + r.Services.Search.Version
		cache := r.Services.Cache.Type + ":" + r.Services.Cache.Version
		queue := r.Services.Queue.Type + ":" + r.Services.Queue.Version

		fmt.Fprintf(w, "| `%s` | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.ID,
			r.Version,
			r.Services.PHP,
			r.Services.Webserver,
			db, search, cache, queue,
			r.Services.Varnish,
			stepIcon(stepStatus(r, "stack_up")),
			stepIcon(stepStatus(r, "install")),
			stepIcon(stepStatus(r, "smoke")),
			stepIcon(stepStatus(r, "playwright")),
		)
	}

	passed, total := Summary(results)
	fmt.Fprintf(w, "\n_%d/%d combinations passed — generated %s_\n",
		passed, total, time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	return nil
}

// ── WriteCSV ──────────────────────────────────────────────────────────────────

// WriteCSV writes a CSV file with the same column layout as reporter.sh.
func WriteCSV(w io.Writer, product string, results []result.TestResult) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"id", "product", "version", "php", "webserver",
		"db", "search", "cache", "queue", "varnish",
		"stack_up", "install", "smoke", "playwright", "timestamp",
	}); err != nil {
		return err
	}
	for _, r := range results {
		db := r.Services.DB.Type + ":" + r.Services.DB.Version
		search := r.Services.Search.Type + ":" + r.Services.Search.Version
		cache := r.Services.Cache.Type + ":" + r.Services.Cache.Version
		queue := r.Services.Queue.Type + ":" + r.Services.Queue.Version

		if err := cw.Write([]string{
			r.ID, r.Product, r.Version, r.Services.PHP, r.Services.Webserver,
			db, search, cache, queue, r.Services.Varnish,
			stepPlain(stepStatus(r, "stack_up")),
			stepPlain(stepStatus(r, "install")),
			stepPlain(stepStatus(r, "smoke")),
			stepPlain(stepStatus(r, "playwright")),
			r.Timestamp,
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// ── WriteJSON ─────────────────────────────────────────────────────────────────

// AggregatedReport is the structure written by WriteJSON.
type AggregatedReport struct {
	GeneratedAt string                   `json:"generated_at"`
	Products    map[string]ProductReport `json:"products"`
}

// ProductReport summarises one product's results.
type ProductReport struct {
	Passed  int                 `json:"passed"`
	Total   int                 `json:"total"`
	Results []result.TestResult `json:"results"`
}

// WriteJSON writes an aggregated JSON report suitable for `| jq` piping or
// GHA step outputs.
func WriteJSON(w io.Writer, byProduct map[string][]result.TestResult) error {
	report := AggregatedReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Products:    make(map[string]ProductReport, len(byProduct)),
	}
	for product, results := range byProduct {
		passed, total := Summary(results)
		report.Products[product] = ProductReport{
			Passed:  passed,
			Total:   total,
			Results: results,
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// ── StdoutSummary ─────────────────────────────────────────────────────────────

// ANSI colour codes (used when output is a TTY).
const (
	ansiReset = "\033[0m"
	ansiGreen = "\033[32m"
	ansiRed   = "\033[31m"
	ansiBold  = "\033[1m"
	ansiCyan  = "\033[36m"
)

// StdoutSummary writes a coloured per-combination summary line to w.
// Set colour=false when writing to a non-TTY.
func StdoutSummary(w io.Writer, results []result.TestResult, colour bool) {
	cf := func(code, s string) string {
		if colour {
			return code + s + ansiReset
		}
		return s
	}

	for _, r := range results {
		icon := "✅"
		lineColour := ansiGreen
		if !rowPassed(r) {
			icon = "❌"
			lineColour = ansiRed
		}

		install := stepStatus(r, "install")
		smoke := stepStatus(r, "smoke")
		pw := stepStatus(r, "playwright")

		line := fmt.Sprintf("%s %s | install:%s smoke:%s pw:%s",
			icon, r.ID,
			stepIcon(install), stepIcon(smoke), stepIcon(pw),
		)
		fmt.Fprintln(w, cf(lineColour, line))
	}

	passed, total := Summary(results)
	fmt.Fprintf(w, "%s", cf(ansiBold, fmt.Sprintf("  %d/%d combinations passed\n", passed, total)))
}
