// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import "syscall"

// DiskFree returns the bytes and inodes available to unprivileged users on
// the filesystem holding path. hasInodes is false on filesystems with no
// fixed inode table, such as btrfs, which report zero of both.
func DiskFree(path string) (bytes, inodes uint64, hasInodes bool, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Ffree), st.Files > 0, nil
}
