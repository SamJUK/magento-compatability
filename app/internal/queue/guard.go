// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import "fmt"

func DiskGuard(path string, minFreeGB uint64) func() string {
	return func() string {
		bytes, inodes, hasInodes, err := DiskFree(path)
		switch {
		case err != nil:
			return fmt.Sprintf("can't read free space on %s: %v", path, err)
		case bytes < minFreeGB<<30:
			return fmt.Sprintf("under %dGB free on %s", minFreeGB, path)
		case hasInodes && inodes < 100_000:
			return fmt.Sprintf("under 100k inodes free on %s", path)
		}
		return ""
	}
}
