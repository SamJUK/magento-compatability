// magento-compatibility — query the Astro compatibility site API from the CLI.
//
// Usage:
//
//	magento-compatibility [flags] <version>
//
// Examples:
//
//	magento-compatibility 2.4.8
//	magento-compatibility --product mageos 1.3.3
//	magento-compatibility --format json 2.4.8 | jq '.results[0]'
//	MCOMPAT_URL=https://m2compat.example.com magento-compatibility 2.4.8
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// ── API types (mirrors the Astro /api/v1/{product}/{version}.json shape) ─────

type apiResponse struct {
	Product string      `json:"product"`
	Version string      `json:"version"`
	Summary apiSummary  `json:"summary"`
	Results []apiResult `json:"results"`
}

type apiSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Unknown int `json:"unknown"`
}

type apiResult struct {
	ID            string            `json:"id"`
	OverallStatus string            `json:"overall_status"`
	Services      apiServices       `json:"services"`
	Steps         map[string]apiStep `json:"steps"`
	Timestamp     string            `json:"timestamp"`
}

type apiServices struct {
	PHP       string     `json:"php"`
	Webserver string     `json:"webserver"`
	DB        apiService `json:"db"`
	Search    apiService `json:"search"`
	Cache     apiService `json:"cache"`
	Queue     apiService `json:"queue"`
	Varnish   string     `json:"varnish"`
}

type apiService struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

type apiStep struct {
	Status    string  `json:"status"`
	DurationS float64 `json:"duration_s"`
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	fs := flag.NewFlagSet("magento-compatibility", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `magento-compatibility — query Magento compatibility data from the CLI

Usage:
  magento-compatibility [flags] <version>

Flags:`)
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Environment:
  MCOMPAT_URL   Base URL of the deployed compatibility site (overridden by --url)

Examples:
  magento-compatibility 2.4.8
  magento-compatibility --format json 2.4.8 | jq '.summary'
  magento-compatibility --product mageos 1.3.3
  MCOMPAT_URL=https://m2compat.example.com magento-compatibility 2.4.8-p4`)
	}

	flagURL     := fs.String("url", "", "Base URL of the compatibility site (e.g. https://m2compat.example.com)")
	flagProduct := fs.String("product", "magento", "Product name (magento|mageos)")
	flagFormat  := fs.String("format", "table", "Output format: table, json, csv")
	flagTimeout := fs.Duration("timeout", 15*time.Second, "HTTP request timeout")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	version := fs.Arg(0)

	baseURL := *flagURL
	if baseURL == "" {
		baseURL = os.Getenv("MCOMPAT_URL")
	}
	if baseURL == "" {
		fmt.Fprintln(os.Stderr, "error: no site URL provided — set --url or MCOMPAT_URL environment variable")
		os.Exit(1)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	// Validate the URL so we give a helpful error before making the request.
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid URL %q: %v\n", baseURL, err)
		os.Exit(1)
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s.json", baseURL, *flagProduct, version)

	client := &http.Client{Timeout: *flagTimeout}
	resp, err := client.Get(endpoint) //nolint:noctx
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: fetching %s: %v\n", endpoint, err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading response: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "error: version %q not found for product %q (HTTP 404)\n", version, *flagProduct)
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: HTTP %d from %s\n", resp.StatusCode, endpoint)
		os.Exit(1)
	}

	var data apiResponse
	if err := json.Unmarshal(body, &data); err != nil {
		fmt.Fprintf(os.Stderr, "error: parsing response JSON: %v\n", err)
		os.Exit(1)
	}

	switch *flagFormat {
	case "json":
		fmt.Println(string(body))
	case "csv":
		printCSV(os.Stdout, data)
	default:
		colour := term.IsTerminal(int(os.Stdout.Fd()))
		printTable(os.Stdout, data, colour)
	}

	// Exit 1 if every combination failed.
	if data.Summary.Total > 0 && data.Summary.Passed == 0 {
		os.Exit(1)
	}
}

// ── table output ──────────────────────────────────────────────────────────────

const (
	colReset  = "\033[0m"
	colGreen  = "\033[32m"
	colRed    = "\033[31m"
	colYellow = "\033[33m"
	colBold   = "\033[1m"
	colFaint  = "\033[2m"
)

func statusColour(status string, colour bool) string {
	if !colour {
		return strings.ToUpper(status)
	}
	switch status {
	case "pass":
		return colGreen + "PASS" + colReset
	case "fail":
		return colRed + "FAIL" + colReset
	case "skip":
		return colFaint + "SKIP" + colReset
	default:
		return colYellow + strings.ToUpper(status) + colReset
	}
}

func stepStatus(r apiResult, step string) string {
	if s, ok := r.Steps[step]; ok {
		return s.Status
	}
	return "skip"
}

func printTable(w io.Writer, data apiResponse, colour bool) {
	bold := func(s string) string {
		if colour {
			return colBold + s + colReset
		}
		return s
	}

	fmt.Fprintf(w, "\n%s  %s %s\n", bold("●"), bold(strings.ToUpper(data.Product)), bold(data.Version))
	fmt.Fprintf(w, "  %d total  %d passed  %d failed\n\n",
		data.Summary.Total, data.Summary.Passed, data.Summary.Failed)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, bold("STATUS\tPHP\tWEBSERVER\tDB\tSEARCH\tCACHE\tQUEUE\tVARNISH\tINSTALL\tSMOKE"))
	fmt.Fprintln(tw, strings.Repeat("─", 10)+"\t"+strings.Repeat("─", 5)+"\t"+
		strings.Repeat("─", 9)+"\t"+strings.Repeat("─", 12)+"\t"+
		strings.Repeat("─", 12)+"\t"+strings.Repeat("─", 10)+"\t"+
		strings.Repeat("─", 12)+"\t"+strings.Repeat("─", 8)+"\t"+
		strings.Repeat("─", 8)+"\t"+strings.Repeat("─", 8))

	for _, r := range data.Results {
		db := r.Services.DB.Type + " " + r.Services.DB.Version
		search := r.Services.Search.Type + " " + r.Services.Search.Version
		cache := r.Services.Cache.Type + " " + r.Services.Cache.Version
		queue := r.Services.Queue.Type + " " + r.Services.Queue.Version
		varnish := r.Services.Varnish
		if varnish == "none" || varnish == "" {
			varnish = "—"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			statusColour(r.OverallStatus, colour),
			r.Services.PHP,
			r.Services.Webserver,
			db, search, cache, queue, varnish,
			statusColour(stepStatus(r, "install"), colour),
			statusColour(stepStatus(r, "smoke"), colour),
		)
	}
	tw.Flush()
	fmt.Fprintln(w)
}

// ── CSV output ────────────────────────────────────────────────────────────────

func printCSV(w io.Writer, data apiResponse) {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "overall_status", "php", "webserver",
		"db_type", "db_version", "search_type", "search_version",
		"cache_type", "cache_version", "queue_type", "queue_version",
		"varnish", "install", "smoke", "playwright", "timestamp",
	})
	for _, r := range data.Results {
		_ = cw.Write([]string{
			r.ID,
			r.OverallStatus,
			r.Services.PHP,
			r.Services.Webserver,
			r.Services.DB.Type, r.Services.DB.Version,
			r.Services.Search.Type, r.Services.Search.Version,
			r.Services.Cache.Type, r.Services.Cache.Version,
			r.Services.Queue.Type, r.Services.Queue.Version,
			r.Services.Varnish,
			stepStatus(r, "install"),
			stepStatus(r, "smoke"),
			stepStatus(r, "playwright"),
			r.Timestamp,
		})
	}
	cw.Flush()
}
