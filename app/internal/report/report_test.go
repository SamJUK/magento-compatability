// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package report

import (
	"bytes"
	encsv "encoding/csv"
	"strings"
	"testing"

	"github.com/samjuk/magento-compatability/internal/result"
)

func TestRowPassedCountsServiceChecks(t *testing.T) {
	steps := func(queue string) map[string]result.Step {
		return map[string]result.Step{
			"install":    {Status: result.StatusPass},
			"smoke":      {Status: result.StatusPass},
			"playwright": {Status: result.StatusPass},
			"queue":      {Status: queue},
		}
	}

	if !rowPassed(result.TestResult{Steps: steps(result.StatusPass)}) {
		t.Fatal("rowPassed with every step passing = false, want true")
	}
	if !rowPassed(result.TestResult{Steps: steps(result.StatusSkip)}) {
		t.Fatal("rowPassed with a skipped check = false, want true")
	}
	if rowPassed(result.TestResult{Steps: steps(result.StatusFail)}) {
		t.Fatal("rowPassed with a failed check = true, want false")
	}
}

func TestReportsShowServiceChecks(t *testing.T) {
	r := result.TestResult{ID: "combo", Timestamp: "2026-11-27T10:00:00Z", Steps: map[string]result.Step{
		"install":    {Status: result.StatusPass},
		"smoke":      {Status: result.StatusPass},
		"playwright": {Status: result.StatusPass},
		"queue":      {Status: result.StatusFail},
		"cron":       {Status: result.StatusPass},
	}}

	var md, csv, out bytes.Buffer
	if err := WriteMD(&md, "magento", []result.TestResult{r}); err != nil {
		t.Fatal(err)
	}
	if err := WriteCSV(&csv, "magento", []result.TestResult{r}); err != nil {
		t.Fatal(err)
	}
	StdoutSummary(&out, []result.TestResult{r}, false)

	if !strings.Contains(md.String(), "| ❌ | ✅ | ⏭️ |") {
		t.Errorf("markdown row does not end with the check columns:\n%s", md.String())
	}
	if !strings.Contains(out.String(), "queue:❌ cron:✅ varnish:⏭️") {
		t.Errorf("summary does not show the checks:\n%s", out.String())
	}

	rows, err := encsv.NewReader(&csv).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv rows = %v, err %v", rows, err)
	}
	header, row := rows[0], rows[1]
	tail := len(header) - 4
	if got := strings.Join(header[tail:], ","); got != "timestamp,queue_check,cron_check,varnish_check" {
		t.Errorf("csv header ends %q", got)
	}
	if got := strings.Join(row[tail:], ","); got != "2026-11-27T10:00:00Z,FAIL,PASS,SKIP" {
		t.Errorf("csv row ends %q", got)
	}
}
