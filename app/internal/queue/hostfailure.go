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
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var r result.TestResult
	if json.Unmarshal(data, &r) != nil {
		return ""
	}
	for _, st := range r.Steps {
		if st.Failure != nil && hostCodes[st.Failure.Code] {
			return st.Failure.Code
		}
	}
	return ""
}
