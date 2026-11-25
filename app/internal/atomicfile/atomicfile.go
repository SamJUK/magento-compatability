// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package atomicfile replaces a file so a reader sees the old content or the new, never a mix, and a power cut can't leave it empty.
package atomicfile

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// Write replaces path with data. It writes to a temp file in the same
// directory, which ends in .tmp so a directory scan for *.json skips it,
// syncs it, renames it over path and syncs the directory. mode is the
// file's, less the umask.
func Write(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), rand.Uint64()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// SyncDir makes a rename in dir survive a power cut.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
