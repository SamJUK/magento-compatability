// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
)

func TestPrintCSVEndsWithTheServiceChecks(t *testing.T) {
	r := apiResult{ID: "combo", Timestamp: "2026-11-27T10:00:00Z", Steps: map[string]apiStep{
		"queue": {Status: "fail"},
		"cron":  {Status: "pass"},
	}}

	var out bytes.Buffer
	printCSV(&out, apiResponse{Results: []apiResult{r}})

	rows, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv = %q, err %v", out.String(), err)
	}
	header, row := rows[0], rows[1]
	tail := len(header) - 4
	if got := strings.Join(header[tail:], ","); got != "timestamp,queue_check,cron_check,varnish_check" {
		t.Fatalf("header ends %q", got)
	}
	if got := strings.Join(row[tail:], ","); got != "2026-11-27T10:00:00Z,fail,pass,skip" {
		t.Fatalf("row ends %q", got)
	}
}
