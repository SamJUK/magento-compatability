// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func MemoryFreePercent() (int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	kb := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			kb[name], _ = strconv.ParseUint(fields[0], 10, 64)
		}
	}
	if kb["MemTotal"] == 0 {
		return 0, fmt.Errorf("/proc/meminfo: no MemTotal")
	}
	return int(kb["MemAvailable"] * 100 / kb["MemTotal"]), nil
}
