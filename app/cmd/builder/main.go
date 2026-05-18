// magento-compatibility-builder — orchestrate test runs and aggregate reports.
//
// Usage:
//
//	magento-compatibility-builder <subcommand> [flags]
//
// Subcommands:
//
//	test    Run compatibility test combinations
//	report  Aggregate results into MD / CSV / JSON artefacts
//	help    Show this help
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/term"

	"github.com/samjuk/magento-compatability/internal/matrix"
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
  report  Aggregate results into MD / CSV / JSON artefacts
  help    Show this help

Run 'magento-compatibility-builder test --help' or
    'magento-compatibility-builder report --help'
for subcommand-specific flags.
`)
}

// ─── test subcommand ──────────────────────────────────────────────────────────

func runTest(args []string) {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: magento-compatibility-builder test [flags]

Flags:`)
		fs.PrintDefaults()
	}

	var (
		flagProduct     = fs.String("product", "", "Filter by product name (magento|mageos)")
		flagVersion     = fs.String("version", "", "Filter by product version (e.g. 2.4.8)")
		flagPHP         = fs.String("php", "", "Filter by PHP version (e.g. 8.3)")
		flagWebserver   = fs.String("webserver", "", "Filter by webserver type (nginx|apache)")
		flagDB          = fs.String("db", "", "Filter by database (type:version, e.g. mariadb:11.4)")
		flagSearch      = fs.String("search", "", "Filter by search engine (type:version, e.g. opensearch:3)")
		flagCache       = fs.String("cache", "", "Filter by cache (type:version, e.g. valkey:8)")
		flagQueue       = fs.String("queue", "", "Filter by queue (type:version, e.g. rabbitmq:4.2)")
		flagVarnish     = fs.String("varnish", "", "Filter by varnish version or \"none\"")
		flagConcurrency = fs.Int("concurrency", 1, "Number of combinations to run in parallel")
		flagForce       = fs.Bool("force", false, "Re-run combinations that already have a result on disk")
		flagListJSON    = fs.Bool("list-json", false, "Print matching combinations as JSON and exit")
		flagDryRun      = fs.Bool("dry-run", false, "Print combinations without running them")
		flagMaxLogBytes = fs.Int64("max-log-bytes", 1<<20, "Maximum bytes to capture per container log (0 = unlimited)")
		flagMatrixFile  = fs.String("matrix", "", "Path to matrix.yml (default: auto-detect from script location)")
		flagResultsDir  = fs.String("results-dir", "", "Path to results directory (default: <repo-root>/results)")
		flagComposeDir  = fs.String("compose-dir", "", "Path to compose directory (default: <repo-root>/compose)")
		flagPlaywright  = fs.Bool("playwright", true, "Run Playwright E2E tests after smoke tests (default: true)")
		flagBaselines   = fs.Bool("baseline", false, "Run only the baseline combination(s) — one per product version — and print a structured pass/fail summary")
		flagNoTUI       = fs.Bool("no-tui", false, "Disable TUI — plain log output suitable for CI")
	)

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	repoRoot := findRepoRoot()

	matrixFile := *flagMatrixFile
	if matrixFile == "" {
		matrixFile = filepath.Join(repoRoot, "matrix.yml")
	}
	resultsDir := *flagResultsDir
	if resultsDir == "" {
		resultsDir = filepath.Join(repoRoot, "results")
	}
	composeDir := *flagComposeDir
	if composeDir == "" {
		composeDir = filepath.Join(repoRoot, "docker", "compose")
	}

	m, err := matrix.Load(matrixFile)
	if err != nil {
		fatalf("loading matrix: %v", err)
	}

	f := matrix.Filter{
		Product:   *flagProduct,
		Version:   *flagVersion,
		PHP:       *flagPHP,
		Webserver: *flagWebserver,
		DB:        *flagDB,
		Search:    *flagSearch,
		Cache:     *flagCache,
		Queue:     *flagQueue,
		Varnish:   *flagVarnish,
	}

	var combos []matrix.Combination
	if *flagBaselines {
		combos = matrix.BuildBaselineCombinations(m, f)
	} else {
		combos = matrix.BuildCombinations(m, f)
	}
	logStep(fmt.Sprintf("Building combination list from %s", matrixFile))
	logInfo(fmt.Sprintf("Total combinations matching filters: %d", len(combos)))

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

	// ── Signal-aware context ────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(*flagConcurrency)

	cfg := runner.RunConfig{
		ResultsDir: resultsDir,
		ComposeDir: composeDir,
		PlaywrightDir: func() string {
			if *flagPlaywright {
				return filepath.Join(repoRoot, "docker", "scripts", "tests", "playwright")
			}
			return ""
		}(),
		Force:       *flagForce,
		MaxLogBytes: *flagMaxLogBytes,
	}

	comboIDs := make([]string, len(combos))
	for i, c := range combos {
		comboIDs[i] = c.ID()
	}
	prog := newProgressUI(comboIDs, *flagConcurrency, isTerminal(os.Stderr) && !*flagNoTUI, *flagBaselines)

	tickerCtx, cancelTicker := context.WithCancel(context.Background())
	prog.startTicker(tickerCtx)
	prog.startInput(stop)

	for _, c := range combos {
		g.Go(func() error {
			prog.started(c.ID())
			comboStart := time.Now()
			ran, err := runner.Run(ctx, c, cfg)
			comboDur := time.Since(comboStart)
			var b *baselineEntry
			if ran {
				b = readBaselineEntry(c, resultsDir)
			}
			if *flagBaselines && b != nil {
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

	prog.cleanup()

	// Prune dangling images and stopped containers accumulated during the run.
	// image prune without --all only removes untagged (dangling) images,
	// leaving the tagged m2test/* images intact.
	// @TODO: DO NOY LIKE THIS - TARGET TO ONLY PRUNE IMAGES CREATED BY THIS TOOL
	exec.Command("docker", "image", "prune", "-f").Run()     //nolint:errcheck
	exec.Command("docker", "container", "prune", "-f").Run() //nolint:errcheck

	if *flagBaselines {
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

// ─── baseline summary ─────────────────────────────────────────────────────────

var baselineStepOrder = []string{"stack_up", "install", "smoke", "playwright"}

// readBaselineEntry reads a result JSON for one combination and returns a
// baselineEntry ready for display. Safe to call when the file may not exist.
func readBaselineEntry(c matrix.Combination, resultsDir string) *baselineEntry {
	path := filepath.Join(resultsDir, c.Product, c.ID()+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return &baselineEntry{id: c.ID(), overall: "missing"}
	}
	var r result.TestResult
	if err := json.Unmarshal(data, &r); err != nil {
		return &baselineEntry{id: c.ID(), overall: "error"}
	}
	b := &baselineEntry{id: c.ID(), overall: r.OverallStatus}
	for _, stepName := range baselineStepOrder {
		step, ok := r.Steps[stepName]
		if !ok {
			continue
		}
		b.steps = append(b.steps, stepResult{name: stepName, status: step.Status})
		if step.Status == result.StatusFail && b.failStep == "" && step.Log != "" {
			b.failStep = stepName
			b.failLog = strings.Join(logTail(step.Log, 20), "\n")
		}
	}
	return b
}

// printBaselineSummary prints the final overall pass/fail count.
// Per-combo detail is shown live via addBaseline during the run.
// Returns true when all baselines passed.
func printBaselineSummary(baselines []*baselineEntry) bool {
	passed, failed := 0, 0
	for _, b := range baselines {
		if b.overall == result.StatusPass {
			passed++
		} else {
			failed++
		}
	}
	total := passed + failed
	fmt.Fprintln(os.Stderr)
	if failed == 0 {
		logOK(fmt.Sprintf("All %d baselines passed", total))
	} else {
		logError(fmt.Sprintf("%d/%d baselines failed", failed, total))
	}
	return failed == 0
}

// logTail returns the last n lines of s.
func logTail(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// ─── report subcommand ────────────────────────────────────────────────────────

func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: magento-compatibility-builder report [flags]

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

	// Filter by product if requested.
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

	// Stdout summary (colour when TTY).
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
			if err := writeFile(filepath.Join(outDir, product+".md"), func(f *os.File) error {
				return report.WriteMD(f, product, results)
			}); err != nil {
				logError(fmt.Sprintf("writing %s.md: %v", product, err))
			} else {
				logOK(fmt.Sprintf("Wrote %s", filepath.Join(outDir, product+".md")))
			}
		}
		if writeCSV {
			if err := writeFile(filepath.Join(outDir, product+".csv"), func(f *os.File) error {
				return report.WriteCSV(f, product, results)
			}); err != nil {
				logError(fmt.Sprintf("writing %s.csv: %v", product, err))
			} else {
				logOK(fmt.Sprintf("Wrote %s", filepath.Join(outDir, product+".csv")))
			}
		}
	}

	if writeJSON {
		if err := writeFile(filepath.Join(outDir, "report.json"), func(f *os.File) error {
			return report.WriteJSON(f, byProduct)
		}); err != nil {
			logError(fmt.Sprintf("writing report.json: %v", err))
		} else {
			logOK(fmt.Sprintf("Wrote %s", filepath.Join(outDir, "report.json")))
		}
	}

	if anyFail {
		logError("One or more test combinations FAILED. See reports/ for details.")
		os.Exit(1)
	}
	logOK("All reported combinations passed.")
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func findRepoRoot() string {
	// Walk upward from the executable's directory looking for matrix.yml.
	// Falls back to the working directory.
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

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// ─── TUI progress display ────────────────────────────────────────────────────

type comboStatus int

const (
	comboPending comboStatus = iota
	comboRunning
	comboPass
	comboFail
	comboSkip
)

type comboEntry struct {
	id      string
	status  comboStatus
	started time.Time
	dur     time.Duration
	errMsg  string
	steps   []stepResult
}

type stepResult struct {
	name   string
	status string
}

type baselineEntry struct {
	id       string
	overall  string
	steps    []stepResult
	failStep string
	failLog  string // last 20 lines of first failed step log
}

type progressUI struct {
	mu           sync.Mutex
	entries      []*comboEntry
	byID         map[string]*comboEntry
	concurrency  int
	startedAt    time.Time
	tty          bool
	lastLines    int // lines drawn in last redraw, for cursor-up erase
	errors       []string
	baselines    []*baselineEntry
	baselineMode bool
	// keyboard navigation (baseline mode only)
	termState    *term.State
	selectedRow  int
	scrollOffset int
	expandedRows map[string]bool
}

func newProgressUI(ids []string, concurrency int, tty bool, baselineMode bool) *progressUI {
	p := &progressUI{
		byID:         make(map[string]*comboEntry, len(ids)),
		concurrency:  concurrency,
		startedAt:    time.Now(),
		tty:          tty,
		baselineMode: baselineMode,
		expandedRows: make(map[string]bool),
	}
	for _, id := range ids {
		e := &comboEntry{id: id, status: comboPending}
		p.entries = append(p.entries, e)
		p.byID[id] = e
	}
	if tty {
		p.redrawLocked() // establish the TUI area
	}
	return p
}

// startTicker runs a background goroutine that redraws every second so that
// elapsed timers on running combinations update in real time.
func (p *progressUI) startTicker(ctx context.Context) {
	if !p.tty {
		return
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.redraw()
			}
		}
	}()
}

// started marks a combination as running and updates the display.
func (p *progressUI) started(id string) {
	p.mu.Lock()
	if e, ok := p.byID[id]; ok {
		e.status = comboRunning
		e.started = time.Now()
	}
	if !p.tty {
		p.mu.Unlock()
		if !p.baselineMode {
			logStep(fmt.Sprintf("Running: %s", id))
		}
		return
	}
	p.redrawLocked()
	p.mu.Unlock()
}

// done marks a combination finished. errMsg is non-empty only on failure.
func (p *progressUI) done(id string, status comboStatus, dur time.Duration, errMsg string, steps []stepResult) {
	p.mu.Lock()
	if e, ok := p.byID[id]; ok {
		e.status = status
		e.dur = dur
		if errMsg != "" {
			e.errMsg = errMsg
		}
		e.steps = steps
	}
	if errMsg != "" {
		p.errors = append(p.errors, fmt.Sprintf("%s — %s", id, errMsg))
	}
	if !p.tty {
		p.mu.Unlock()
		if status == comboFail {
			logError(fmt.Sprintf("Combination failed: %s — %s", id, errMsg))
		} else if !p.baselineMode && status == comboSkip {
			logInfo(fmt.Sprintf("Skipping (result exists): %s", id))
		}
		return
	}
	p.redrawLocked()
	p.mu.Unlock()
}

// addBaseline records a completed baseline result and updates the display.
func (p *progressUI) addBaseline(b *baselineEntry) {
	p.mu.Lock()
	p.baselines = append(p.baselines, b)
	if p.tty {
		p.redrawLocked()
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	// Non-TTY: print the entry immediately.
	overallCol, overallTxt := colGreen, "PASS"
	if b.overall != result.StatusPass {
		overallCol, overallTxt = colRed, "FAIL"
	}
	fmt.Fprintf(os.Stderr, "%s[BASELINE]%s %s — %s%s%s  |  %s\n",
		overallCol, colReset, b.id, overallCol, overallTxt, colReset,
		renderSteps(b.steps))
}

// cleanup restores the terminal from raw mode. Idempotent.
func (p *progressUI) cleanup() {
	p.mu.Lock()
	s := p.termState
	p.termState = nil
	p.mu.Unlock()
	if s != nil {
		term.Restore(int(os.Stdin.Fd()), s)
	}
}

// startInput puts stdin into raw mode and starts a goroutine that translates
// arrow keys and Enter/Space into row selection / expansion events. ^C calls
// cancel so the in-progress runs are cancelled cleanly.
func (p *progressUI) startInput(cancel context.CancelFunc) {
	if !p.tty {
		return
	}
	s, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return
	}
	p.mu.Lock()
	p.termState = s
	p.mu.Unlock()

	go func() {
		buf := make([]byte, 16)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			p.handleInput(buf[:n], cancel)
		}
	}()
}

// handleInput processes a raw keystroke sequence from the input goroutine.
func (p *progressUI) handleInput(b []byte, cancel context.CancelFunc) {
	if len(b) >= 1 && b[0] == 3 { // ^C in raw mode
		cancel()
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var nRows int
	if p.baselineMode {
		nRows = len(p.baselines)
	} else {
		nRows = len(p.entries)
	}

	switch {
	case len(b) == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'A': // up arrow
		if p.selectedRow > 0 {
			p.selectedRow--
		}
	case len(b) == 3 && b[0] == 27 && b[1] == '[' && b[2] == 'B': // down arrow
		if p.selectedRow < nRows-1 {
			p.selectedRow++
		}
	case len(b) == 1 && (b[0] == 13 || b[0] == 32): // enter or space — toggle expand
		if nRows > 0 && p.selectedRow < nRows {
			var id string
			if p.baselineMode {
				id = p.baselines[p.selectedRow].id
			} else {
				id = p.entries[p.selectedRow].id
			}
			p.expandedRows[id] = !p.expandedRows[id]
		}
	}
	p.redrawLocked()
}

// redraw acquires the lock then repaints the TUI block.
func (p *progressUI) redraw() {
	if !p.tty {
		return
	}
	p.mu.Lock()
	p.redrawLocked()
	p.mu.Unlock()
}

// redrawLocked repaints the TUI block. Caller must hold p.mu.
func (p *progressUI) redrawLocked() {
	output := p.render()

	// Clamp output to terminal height so cursor-up never overshoots the top.
	if _, h, err := term.GetSize(int(os.Stderr.Fd())); err == nil && h > 1 {
		maxLines := h - 1
		if n := strings.Count(output, "\n"); n > maxLines {
			lines := strings.Split(output, "\n")
			output = strings.Join(lines[len(lines)-maxLines:], "\n")
		}
	}

	newLines := strings.Count(output, "\n")
	var w strings.Builder
	if p.lastLines > 0 {
		fmt.Fprintf(&w, "\033[%dA\033[J", p.lastLines)
	}
	// term.MakeRaw disables OPOST so \n no longer implies \r — fix it.
	if p.termState != nil {
		output = strings.ReplaceAll(output, "\n", "\r\n")
	}
	w.WriteString(output)
	fmt.Fprint(os.Stderr, w.String())
	p.lastLines = newLines
}

const tuiBarWidth = 40

func (p *progressUI) render() string {
	if p.baselineMode {
		return p.renderBaseline()
	}
	return p.renderTest()
}

func (p *progressUI) renderTest() string {
	now := time.Now()

	_, termH, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || termH <= 0 {
		termH = 30
	}

	var passed, failed, skipped, running, pending int
	var ranDurs []float64
	for _, e := range p.entries {
		switch e.status {
		case comboRunning:
			running++
		case comboPass:
			passed++
			ranDurs = append(ranDurs, e.dur.Seconds())
		case comboFail:
			failed++
			ranDurs = append(ranDurs, e.dur.Seconds())
		case comboSkip:
			skipped++
		case comboPending:
			pending++
		}
	}

	total := len(p.entries)
	done := passed + failed + skipped
	filled := done * tuiBarWidth / max(total, 1)
	pct := done * 100 / max(total, 1)
	bar := colGreen + strings.Repeat("█", filled) + colReset + strings.Repeat("░", tuiBarWidth-filled)
	timingLine := p.buildTimingLine(ranDurs, pending+running)

	// Height budget: header(2) + scroll indicators(2) + footer(3) + buffer(1)
	tableH := max(termH-8, 0)
	viewEnd := p.clampViewport(len(p.entries), tableH)

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n  [%s]  %d/%d  (%d%%)\n", bar, done, total, pct)

	if p.scrollOffset > 0 {
		fmt.Fprintf(&sb, "  %s↑ %d more above (↑/↓ to scroll)%s\n", colYellow, p.scrollOffset, colReset)
	}

	for i := p.scrollOffset; i < viewEnd; i++ {
		e := p.entries[i]
		isSelected := i == p.selectedRow
		isExpanded := p.expandedRows[e.id]
		sym, statusTxt, durStr := comboStatusDisplay(e, now)
		prefix := "  "
		if isSelected {
			prefix = colBold + "> " + colReset
		}
		fmt.Fprintf(&sb, "%s%s  %-54s  %s  %s%s\n",
			prefix, sym, truncateID(e.id), statusTxt, durStr,
			rowExpandHint(isSelected, e.errMsg != "", isExpanded, ""))
		if isExpanded && e.errMsg != "" {
			writeLogBox(&sb, "error", e.errMsg)
		}
	}

	if below := len(p.entries) - viewEnd; below > 0 {
		fmt.Fprintf(&sb, "  %s↓ %d more below (↑/↓ to scroll)%s\n", colYellow, below, colReset)
	}

	fmt.Fprintf(&sb, "\n  %s✓%s %d  %s✗%s %d  %s⊖%s %d  %s↻%s %d  %s⊙%s %d  |  %s\n\n",
		colGreen, colReset, passed,
		colRed, colReset, failed,
		colYellow, colReset, skipped,
		colCyan, colReset, running,
		colYellow, colReset, pending,
		timingLine,
	)
	return sb.String()
}

// renderBaseline returns a height-bounded table of baseline results with
// keyboard-navigable rows and expandable failure logs.
// Must be called with p.mu held (same as render).
func (p *progressUI) renderBaseline() string {
	now := time.Now()

	_, termH, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || termH <= 0 {
		termH = 30
	}

	var running []*comboEntry
	var ranDurs []float64
	var passed, failed, skipped, pending int
	for _, e := range p.entries {
		switch e.status {
		case comboRunning:
			running = append(running, e)
		case comboPass:
			passed++
			ranDurs = append(ranDurs, e.dur.Seconds())
		case comboFail:
			failed++
			ranDurs = append(ranDurs, e.dur.Seconds())
		case comboSkip:
			skipped++
		case comboPending:
			pending++
		}
	}

	timingLine := p.buildTimingLine(ranDurs, pending+len(running))

	// Height budget: 1 top + running rows + 2 scroll indicators + 1 pending + 3 footer + 1 buffer.
	tableH := max(termH-(1+len(running)+2+1+3+1), 0)
	viewEnd := p.clampViewport(len(p.baselines), tableH)

	var sb strings.Builder
	sb.WriteString("\n")

	for _, e := range running {
		fmt.Fprintf(&sb, "  %s↻%s  %-54s  %sRUNNING%s  %s\n",
			colCyan, colReset, truncateID(e.id), colCyan, colReset,
			formatDur(now.Sub(e.started).Round(time.Second)))
	}

	if p.scrollOffset > 0 {
		fmt.Fprintf(&sb, "  %s↑ %d more above (↑/↓ to scroll)%s\n", colYellow, p.scrollOffset, colReset)
	}

	for i := p.scrollOffset; i < viewEnd; i++ {
		b := p.baselines[i]
		isSelected := i == p.selectedRow
		isExpanded := p.expandedRows[b.id]

		sym, overallTxt := colGreen+"✓"+colReset, colGreen+"PASS"+colReset
		if b.overall != result.StatusPass {
			sym, overallTxt = colRed+"✗"+colReset, colRed+"FAIL"+colReset
		}
		prefix := "  "
		if isSelected {
			prefix = colBold + "> " + colReset
		}
		fmt.Fprintf(&sb, "%s%s  %-54s  %s  %s%s\n",
			prefix, sym, truncateID(b.id), overallTxt, renderSteps(b.steps),
			rowExpandHint(isSelected, b.failLog != "", isExpanded, "log"))
		if isExpanded && b.failLog != "" {
			writeLogBox(&sb, b.failStep+" log", b.failLog)
		}
	}

	if below := len(p.baselines) - viewEnd; below > 0 {
		fmt.Fprintf(&sb, "  %s↓ %d more below (↑/↓ to scroll)%s\n", colYellow, below, colReset)
	}
	if pending > 0 {
		fmt.Fprintf(&sb, "  %s⊙ %d pending%s\n", colYellow, pending, colReset)
	}

	fmt.Fprintf(&sb, "\n  %s✓%s %d  %s✗%s %d  %s↻%s %d  %s⊙%s %d  |  %s\n\n",
		colGreen, colReset, passed,
		colRed, colReset, failed,
		colCyan, colReset, len(running),
		colYellow, colReset, pending,
		timingLine,
	)
	_ = skipped // tracked but not shown in baseline view
	return sb.String()
}

// buildTimingLine constructs the elapsed/avg/ETA footer text shared by both TUI modes.
func (p *progressUI) buildTimingLine(ranDurs []float64, remaining int) string {
	elapsed := time.Since(p.startedAt).Round(time.Second)
	s := "elapsed " + formatDur(elapsed)
	if len(ranDurs) == 0 {
		return s
	}
	var sum float64
	for _, d := range ranDurs {
		sum += d
	}
	avg := sum / float64(len(ranDurs))
	s += "  ·  avg " + formatDur(time.Duration(avg*float64(time.Second))) + "/combo"
	if remaining > 0 {
		eta := avg * float64(remaining) / float64(max(p.concurrency, 1))
		s += "  ·  ~" + formatDur(time.Duration(eta*float64(time.Second))) + " remaining"
	}
	return s
}

// clampViewport adjusts scrollOffset so selectedRow stays visible, then
// returns the exclusive upper bound of the visible row window.
func (p *progressUI) clampViewport(nRows, tableH int) int {
	if p.selectedRow >= nRows && nRows > 0 {
		p.selectedRow = nRows - 1
	}
	if p.selectedRow < p.scrollOffset {
		p.scrollOffset = p.selectedRow
	}
	if p.selectedRow >= p.scrollOffset+tableH {
		p.scrollOffset = p.selectedRow - tableH + 1
	}
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
	viewEnd := p.scrollOffset + tableH
	if viewEnd > nRows {
		viewEnd = nRows
	}
	return viewEnd
}

// truncateID shortens a combination ID to fit the fixed-width TUI table column.
func truncateID(id string) string {
	if len(id) > 52 {
		return id[:49] + "..."
	}
	return id
}

// writeLogBox appends a framed log block to sb with a labelled header.
func writeLogBox(sb *strings.Builder, header, body string) {
	fmt.Fprintf(sb, "  %s┌─ %s ──%s\n", colBold, header, colReset)
	for _, line := range strings.Split(body, "\n") {
		fmt.Fprintf(sb, "  │  %s\n", line)
	}
	fmt.Fprintf(sb, "  %s└────────────%s\n", colBold, colReset)
}

// rowExpandHint returns the keyboard hint for a selected, expandable row.
// expandLabel customises the expand prompt (e.g. "log"); collapse is always "collapse".
func rowExpandHint(isSelected, canExpand, isExpanded bool, expandLabel string) string {
	if !isSelected || !canExpand {
		return ""
	}
	if isExpanded {
		return "  " + colYellow + "[space to collapse]" + colReset
	}
	label := "expand"
	if expandLabel != "" {
		label = "expand " + expandLabel
	}
	return "  " + colYellow + "[space to " + label + "]" + colReset
}

// comboStatusDisplay returns the symbol, status text, and elapsed duration
// string for a combo entry row in the test TUI.
func comboStatusDisplay(e *comboEntry, now time.Time) (sym, statusTxt, durStr string) {
	switch e.status {
	case comboPending:
		sym = colYellow + "⊙" + colReset
		statusTxt = colYellow + "PENDING" + colReset
	case comboRunning:
		sym = colCyan + "↻" + colReset
		statusTxt = colCyan + "RUNNING" + colReset
		durStr = formatDur(now.Sub(e.started).Round(time.Second))
	case comboPass:
		sym = colGreen + "✓" + colReset
		if len(e.steps) > 0 {
			statusTxt = renderSteps(e.steps)
		} else {
			statusTxt = colGreen + "PASS   " + colReset
		}
		durStr = formatDur(e.dur)
	case comboFail:
		sym = colRed + "✗" + colReset
		if len(e.steps) > 0 {
			statusTxt = renderSteps(e.steps)
		} else {
			statusTxt = colRed + "FAIL   " + colReset
		}
		durStr = formatDur(e.dur)
	case comboSkip:
		sym = colYellow + "⊖" + colReset
		statusTxt = colYellow + "SKIP   " + colReset
	}
	return
}

func renderSteps(steps []stepResult) string {
	var parts []string
	for _, s := range steps {
		c := colGreen
		switch s.status {
		case result.StatusFail:
			c = colRed
		case result.StatusSkip:
			c = colYellow
		}
		parts = append(parts, fmt.Sprintf("%s:%s%s%s", s.name, c, s.status, colReset))
	}
	return strings.Join(parts, "  ")
}

func formatDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
