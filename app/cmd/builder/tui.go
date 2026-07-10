package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/samjuk/magento-compatability/internal/result"
)

// comboStatus represents the lifecycle state of one test combination.
type comboStatus int

const (
	comboPending comboStatus = iota
	comboRunning
	comboPass
	comboFail
	comboSkip
)

// comboEntry tracks live state for one combination row in the TUI.
type comboEntry struct {
	id      string
	status  comboStatus
	started time.Time
	dur     time.Duration
	errMsg  string
	steps   []stepResult
}

// progressUI renders a live progress table to stderr.
// It has no interactive keyboard input — all navigation is removed to keep
// the terminal in normal (cooked) mode, which avoids raw-mode artefacts.
type progressUI struct {
	mu           sync.Mutex
	entries      []*comboEntry
	byID         map[string]*comboEntry
	concurrency  int
	startedAt    time.Time
	tty          bool
	lastLines    int
	baselines    []*baselineEntry // accumulates for printBaselineSummary; not used for render
	baselineMode bool
}

func newProgressUI(ids []string, concurrency int, tty bool, baselineMode bool) *progressUI {
	p := &progressUI{
		byID:         make(map[string]*comboEntry, len(ids)),
		concurrency:  concurrency,
		startedAt:    time.Now(),
		tty:          tty,
		baselineMode: baselineMode,
	}
	for _, id := range ids {
		e := &comboEntry{id: id, status: comboPending}
		p.entries = append(p.entries, e)
		p.byID[id] = e
	}
	if tty {
		p.redrawLocked()
	}
	return p
}

// startTicker redraws once per second so running timers stay current.
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
		e.errMsg = errMsg
		e.steps = steps
	}
	if !p.tty {
		p.mu.Unlock()
		switch status {
		case comboFail:
			logError(fmt.Sprintf("Combination failed: %s — %s", id, errMsg))
		case comboSkip:
			if !p.baselineMode {
				logInfo(fmt.Sprintf("Skipping (result exists): %s", id))
			}
		}
		return
	}
	p.redrawLocked()
	p.mu.Unlock()
}

// addBaseline records a completed baseline result for the final summary.
// In non-TTY mode it also prints the result immediately.
func (p *progressUI) addBaseline(b *baselineEntry) {
	p.mu.Lock()
	p.baselines = append(p.baselines, b)
	p.mu.Unlock()

	if !p.tty {
		col, txt := colGreen, "PASS"
		if b.overall != result.StatusPass {
			col, txt = colRed, "FAIL"
		}
		fmt.Fprintf(os.Stderr, "%s[BASELINE]%s %s — %s%s%s  |  %s\n",
			col, colReset, b.id, col, txt, colReset, renderSteps(b.steps))
	}
}

// redraw repaints the TUI. Safe to call from any goroutine.
func (p *progressUI) redraw() {
	if !p.tty {
		return
	}
	p.mu.Lock()
	p.redrawLocked()
	p.mu.Unlock()
}

// redrawLocked repaints the TUI. Caller must hold p.mu.
func (p *progressUI) redrawLocked() {
	output := p.render()

	// Clamp to terminal height so cursor-up never overshoots the top.
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
	w.WriteString(output)
	fmt.Fprint(os.Stderr, w.String())
	p.lastLines = newLines
}

const tuiBarWidth = 40

// render produces the full TUI string for the current state.
func (p *progressUI) render() string {
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

	var sb strings.Builder

	if !p.baselineMode {
		filled := done * tuiBarWidth / max(total, 1)
		pct := done * 100 / max(total, 1)
		bar := colGreen + strings.Repeat("█", filled) + colReset + strings.Repeat("░", tuiBarWidth-filled)
		fmt.Fprintf(&sb, "\n  [%s]  %d/%d  (%d%%)\n", bar, done, total, pct)
	} else {
		sb.WriteString("\n\n")
	}

	// Show last maxRows entries so the display fits the terminal.
	const headerLines, footerLines = 3, 3
	maxRows := max(termH-headerLines-footerLines, 3)
	start := max(len(p.entries)-maxRows, 0)
	if start > 0 {
		fmt.Fprintf(&sb, "  %s↑ %d more above%s\n", colYellow, start, colReset)
	}

	for i := start; i < len(p.entries); i++ {
		e := p.entries[i]
		sym, statusTxt, durStr := comboStatusDisplay(e, now)
		fmt.Fprintf(&sb, "  %s  %-54s  %s  %s\n", sym, truncateID(e.id), statusTxt, durStr)
	}

	timingLine := buildTimingLine(ranDurs, pending+running, p.concurrency, time.Since(p.startedAt))
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

// comboStatusDisplay returns the symbol, status text, and elapsed duration for one row.
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
			statusTxt = colGreen + "PASS" + colReset
		}
		durStr = formatDur(e.dur)
	case comboFail:
		sym = colRed + "✗" + colReset
		if len(e.steps) > 0 {
			statusTxt = renderSteps(e.steps)
		} else if e.errMsg != "" {
			statusTxt = colRed + "FAIL" + colReset + "  " + truncateError(e.errMsg, 50)
		} else {
			statusTxt = colRed + "FAIL" + colReset
		}
		durStr = formatDur(e.dur)
	case comboSkip:
		sym = colYellow + "⊖" + colReset
		statusTxt = colYellow + "SKIP" + colReset
	}
	return
}

// renderSteps formats a step slice as "step:status  step:status …".
func renderSteps(steps []stepResult) string {
	parts := make([]string, 0, len(steps))
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

// buildTimingLine constructs the elapsed/avg/ETA footer text.
func buildTimingLine(ranDurs []float64, remaining, concurrency int, elapsed time.Duration) string {
	s := "elapsed " + formatDur(elapsed.Round(time.Second))
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
		eta := avg * float64(remaining) / float64(max(concurrency, 1))
		s += "  ·  ~" + formatDur(time.Duration(eta*float64(time.Second))) + " remaining"
	}
	return s
}

// truncateID shortens a combination ID to fit the fixed-width TUI column.
func truncateID(id string) string {
	if len(id) > 52 {
		return id[:49] + "..."
	}
	return id
}

// truncateError strips newlines and truncates an error message for inline display.
func truncateError(msg string, maxLen int) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > maxLen {
		return msg[:maxLen-1] + "…"
	}
	return msg
}

// formatDur formats a duration as a concise human-readable string.
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
