package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

// baselineStepOrder controls the display order of steps in baseline output.
var baselineStepOrder = []string{"stack_up", "install", "smoke", "playwright"}

type stepResult struct {
	name   string
	status string
}

type baselineEntry struct {
	id       string
	overall  string
	steps    []stepResult
	failStep string
	failLog  string // last 20 lines of the first failed step log
}

// readBaselineEntry reads a result JSON for one combination and returns a
// baselineEntry. Safe to call when the file may not exist.
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

// filterSetupFailures returns the subset of combos whose existing result has
// stack_up status == "fail". Combos with no result file are excluded — they
// have never run.
func filterSetupFailures(combos []matrix.Combination, resultsDir string) []matrix.Combination {
	var out []matrix.Combination
	for _, c := range combos {
		path := filepath.Join(resultsDir, c.Product, c.ID()+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r result.TestResult
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		if step, ok := r.Steps["stack_up"]; ok && step.Status == result.StatusFail {
			out = append(out, c)
		}
	}
	return out
}

// printBaselineSummary prints the final pass/fail count. Returns true when all passed.
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
