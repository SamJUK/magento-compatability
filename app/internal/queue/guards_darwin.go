// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var freePercent = regexp.MustCompile(`free percentage:\s*(\d+)%`)

// MemoryFreePercent is the system-wide free memory percentage macOS reports,
// which counts reclaimable memory as free.
func MemoryFreePercent() (int, error) {
	// Every slot loop asks; a wedged one mustn't hang them.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "memory_pressure").Output()
	if err != nil {
		return 0, err
	}
	m := freePercent.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("memory_pressure: no free percentage in output")
	}
	return strconv.Atoi(string(m[1]))
}
