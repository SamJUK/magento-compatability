// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"encoding/json"
	"os"

	"github.com/samjuk/magento-compatability/internal/result"
)

// hostCodes are failures that describe the host, not the combination.
var hostCodes = map[string]bool{"docker_name_conflict": true, "docker_cleanup_race": true, "disk_space": true}

// HostFailure returns the code of a host failure recorded in the result at
// path, or "" when it has none or can't be read.
func HostFailure(path string) string {
	r, ok := loadResult(path)
	if !ok {
		return ""
	}
	return hostFailureOf(r)
}

func loadResult(path string) (r result.TestResult, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return r, false
	}
	return r, json.Unmarshal(data, &r) == nil
}

func hostFailureOf(r result.TestResult) string {
	for _, st := range r.Steps {
		if st.Failure != nil && hostCodes[st.Failure.Code] {
			return st.Failure.Code
		}
	}
	return ""
}

// PlaywrightSkipped says whether a result passed the smoke test but skipped
// Playwright, which doesn't satisfy a sweep that runs it.
func PlaywrightSkipped(r result.TestResult) bool {
	return r.Steps["smoke"].Status == result.StatusPass && r.Steps["playwright"].Status == result.StatusSkip
}
