// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// magento-compatibility-builder — orchestrate test runs and aggregate reports.
//
// Usage:
//
//	magento-compatibility-builder <subcommand> [flags]
//
// Subcommands:
//
//	test    Run compatibility test combinations
//	queue   Work through a sweep unattended, resumably
//	report  Aggregate results into MD / CSV / JSON artefacts
//	help    Show this help
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/report"
	"github.com/samjuk/magento-compatability/internal/result"
	"github.com/samjuk/magento-compatability/internal/runner"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "test":
		runTest(os.Args[2:])
	case "queue":
		runQueue(os.Args[2:])
	case "report":
		runReport(os.Args[2:])
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`magento-compatibility-builder — Magento/MageOS compatibility test runner

Usage:
  magento-compatibility-builder <subcommand> [flags]

Subcommands:
  test    Run compatibility test combinations
  queue   Work through a sweep unattended, resumably
  report  Aggregate results into MD / CSV / JSON artefacts
  help    Show this help

Run 'magento-compatibility-builder test --help' or
    'magento-compatibility-builder report --help'
for subcommand flags and examples.
`)
}

// ─── test subcommand ──────────────────────────────────────────────────────────

func runTest(args []string) {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: magento-compatibility-builder test [flags]

Filters (all optional, combinable):
  -product    magento|mageos
  -version    e.g. 2.4.8
  -php        e.g. 8.3
  -webserver  nginx|apache
  -db         type:version  e.g. mariadb:11.4
  -search     type:version  e.g. opensearch:3
  -cache      type:version  e.g. valkey:8
  -queue      type:version  e.g. rabbitmq:4.2
  -varnish    version or "none"

Examples:
  # Run all PHP 8.3 combinations
  builder test -php 8.3

  # Baseline run for Magento 2.4.8 with concurrency 4
  builder test -product magento -version 2.4.8 -baseline -concurrency 4

  # Retry stack setup failures
  builder test -retry-setup-failures

Flags:`)
		fs.PrintDefaults()
	}

	var (
		sel                    = addSelectionFlags(fs, 0)
		flagConcurrency        = fs.Int("concurrency", 1, "Number of combinations to run in parallel")
		flagForce              = fs.Bool("force", false, "Re-run combinations that already have a result on disk")
		flagListJSON           = fs.Bool("list-json", false, "Print matching combinations as JSON and exit")
		flagDryRun             = fs.Bool("dry-run", false, "Print combinations without running them")
		flagNoTUI              = fs.Bool("no-tui", false, "Disable TUI — plain log output suitable for CI (also set by $CI env var)")
		flagRetrySetupFailures = fs.Bool("retry-setup-failures", false, "Re-run only combinations whose stack_up step previously failed (implies -force)")
		flagIgnoreQueue        = fs.Bool("ignore-queue", false, "Run combinations even while a queue is running them")
	)

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	combos := sel.load()
	resultsDir := sel.resultsDir
	logStep(fmt.Sprintf("Building combination list from %s", sel.matrixFile))
	logInfo(fmt.Sprintf("Total combinations matching filters: %d", len(combos)))

	if *flagRetrySetupFailures {
		before := len(combos)
		combos = filterSetupFailures(combos, resultsDir)
		logInfo(fmt.Sprintf("retry-setup-failures: %d of %d combinations had stack_up failures", len(combos), before))
		if len(combos) == 0 {
			logWarn("No setup failures found — nothing to re-run.")
			return
		}
	}

	if *flagListJSON {
		type comboWithID struct {
			ID string `json:"id"`
			matrix.Combination
		}
		out := make([]comboWithID, len(combos))
		for i, c := range combos {
			out[i] = comboWithID{ID: c.ID(), Combination: c}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fatalf("encoding combinations: %v", err)
		}
		return
	}

	if len(combos) == 0 {
		logWarn("No combinations match the given filters.")
		return
	}

	if *flagDryRun {
		logInfo("Dry-run mode — combinations that would run:")
		for _, c := range combos {
			fmt.Printf("  %s\n", c.ID())
		}
		return
	}

	if *flagConcurrency > 1 && len(combos) > 5 {
		logWarn(fmt.Sprintf("Running %d combinations with concurrency=%d. Each stack uses ~4-6 GB RAM.", len(combos), *flagConcurrency))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(*flagConcurrency)

	cfg := sel.runConfig()
	cfg.Force = *flagForce || *flagRetrySetupFailures

	// Auto-disable TUI in CI environments.
	noTUI := *flagNoTUI || os.Getenv("CI") != ""

	comboIDs := make([]string, len(combos))
	for i, c := range combos {
		comboIDs[i] = c.ID()
	}
	prog := newProgressUI(comboIDs, *flagConcurrency, isTerminal(os.Stderr) && !noTUI, sel.baseline)

	tickerCtx, cancelTicker := context.WithCancel(context.Background())
	prog.startTicker(tickerCtx)

	lg, err := queue.OpenLedger(queueStateDir())
	if err != nil {
		fatalf("%v", err)
	}
	defer lg.Close()

	for _, c := range combos {
		g.Go(func() error {
			if *flagIgnoreQueue {
				// A remote worker's run of it is fenced off and aborted.
				if err := lg.Override(resultsDir, c.ID()); err != nil {
					return err
				}
			} else {
				ok, err := lg.Lease(resultsDir, c.ID())
				if err != nil {
					return err
				}
				if !ok {
					logWarn(fmt.Sprintf("%s: a queue is running it; skipped (-ignore-queue runs it anyway)", c.ID()))
					prog.done(c.ID(), comboSkip, 0, "", nil)
					return nil
				}
			}
			defer lg.Unlease(resultsDir, c.ID())
			prog.started(c.ID())
			comboStart := time.Now()
			ran, err := runner.Run(ctx, c, cfg)
			comboDur := time.Since(comboStart)

			var b *baselineEntry
			if ran {
				b = readBaselineEntry(c, resultsDir)
			}
			if sel.baseline && b != nil {
				prog.addBaseline(b)
			}
			var steps []stepResult
			if b != nil {
				steps = b.steps
			}

			if err != nil {
				prog.done(c.ID(), comboFail, comboDur, err.Error(), steps)
				return err
			}
			if !ran {
				prog.done(c.ID(), comboSkip, 0, "", nil)
			} else {
				prog.done(c.ID(), comboPass, comboDur, "", steps)
			}
			return nil
		})
	}

	waitErr := g.Wait()
	cancelTicker()
	prog.redraw() // final repaint before any trailing log lines

	if sel.baseline {
		allPassed := printBaselineSummary(prog.baselines)
		if !allPassed || waitErr != nil {
			os.Exit(1)
		}
		return
	}

	if waitErr != nil {
		logError(fmt.Sprintf("One or more combinations failed: %v", waitErr))
		os.Exit(1)
	}

	logStep("All combinations complete")
	logInfo(fmt.Sprintf("Results written to: %s", resultsDir))
}

// ─── report subcommand ────────────────────────────────────────────────────────

func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: magento-compatibility-builder report [flags]

Examples:
  # Generate all report formats
  builder report

  # Generate only markdown for magento
  builder report -product magento -format md

Flags:`)
		fs.PrintDefaults()
	}

	var (
		flagProduct    = fs.String("product", "", "Filter to a single product (default: all)")
		flagOutDir     = fs.String("out-dir", "", "Output directory for reports (default: <repo-root>/reports)")
		flagFormat     = fs.String("format", "all", "Output formats: md, csv, json, all")
		flagResultsDir = fs.String("results-dir", "", "Path to results directory (default: <repo-root>/results)")
	)

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	repoRoot := findRepoRoot()
	resultsDir := *flagResultsDir
	if resultsDir == "" {
		resultsDir = filepath.Join(repoRoot, "results")
	}
	outDir := *flagOutDir
	if outDir == "" {
		outDir = filepath.Join(repoRoot, "reports")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatalf("creating output directory: %v", err)
	}

	allResults, err := result.ReadAll(resultsDir)
	if err != nil {
		fatalf("reading results: %v", err)
	}

	if *flagProduct != "" {
		var filtered []result.TestResult
		for _, r := range allResults {
			if r.Product == *flagProduct {
				filtered = append(filtered, r)
			}
		}
		allResults = filtered
	}

	if len(allResults) == 0 {
		logWarn("No results found.")
		return
	}

	byProduct := report.Aggregate(allResults)

	isTTY := isTerminal(os.Stdout)
	for _, results := range byProduct {
		report.StdoutSummary(os.Stdout, results, isTTY)
	}

	writeMD := *flagFormat == "all" || *flagFormat == "md"
	writeCSV := *flagFormat == "all" || *flagFormat == "csv"
	writeJSON := *flagFormat == "all" || *flagFormat == "json"

	anyFail := false

	for product, results := range byProduct {
		if report.AnyFailed(results) {
			anyFail = true
		}

		if writeMD {
			path := filepath.Join(outDir, product+".md")
			if err := writeFile(path, func(f *os.File) error { return report.WriteMD(f, product, results) }); err != nil {
				logError(fmt.Sprintf("writing %s.md: %v", product, err))
			} else {
				logOK(fmt.Sprintf("Wrote %s", path))
			}
		}
		if writeCSV {
			path := filepath.Join(outDir, product+".csv")
			if err := writeFile(path, func(f *os.File) error { return report.WriteCSV(f, product, results) }); err != nil {
				logError(fmt.Sprintf("writing %s.csv: %v", product, err))
			} else {
				logOK(fmt.Sprintf("Wrote %s", path))
			}
		}
	}

	if writeJSON {
		path := filepath.Join(outDir, "report.json")
		if err := writeFile(path, func(f *os.File) error { return report.WriteJSON(f, byProduct) }); err != nil {
			logError(fmt.Sprintf("writing report.json: %v", err))
		} else {
			logOK(fmt.Sprintf("Wrote %s", path))
		}
	}

	if anyFail {
		logError("One or more test combinations FAILED. See reports/ for details.")
		os.Exit(1)
	}
	logOK("All reported combinations passed.")
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// findRepoRoot walks upward from the executable looking for matrix.yml.
// Falls back to the working directory.
func findRepoRoot() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	dir := filepath.Dir(exe)
	for {
		if _, err := os.Stat(filepath.Join(dir, "matrix.yml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	wd, _ := os.Getwd()
	return wd
}

func writeFile(path string, fn func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return fn(f)
}

func fatalf(format string, a ...any) {
	logError(fmt.Sprintf(format, a...))
	os.Exit(1)
}

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// ─── logging ──────────────────────────────────────────────────────────────────

const (
	colReset  = "\033[0m"
	colCyan   = "\033[36m"
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
	colBold   = "\033[1m"
)

func logInfo(msg string)  { fmt.Fprintf(os.Stderr, "%s[INFO]%s  %s\n", colCyan, colReset, msg) }
func logOK(msg string)    { fmt.Fprintf(os.Stderr, "%s[OK]%s    %s\n", colGreen, colReset, msg) }
func logWarn(msg string)  { fmt.Fprintf(os.Stderr, "%s[WARN]%s  %s\n", colYellow, colReset, msg) }
func logError(msg string) { fmt.Fprintf(os.Stderr, "%s[ERROR]%s %s\n", colRed, colReset, msg) }
func logStep(msg string) {
	fmt.Fprintf(os.Stderr, "\n%s━━━ %s ━━━%s\n", colBold, msg, colReset)
}
