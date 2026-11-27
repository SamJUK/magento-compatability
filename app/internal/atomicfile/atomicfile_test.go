// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteReplacesTheFileAndLeavesNoTemp(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o022))
	path := filepath.Join(t.TempDir(), "a.json")
	for _, content := range []string{"one", "two"} {
		if err := Write(path, []byte(content), 0o666); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("read %q, want %q", got, content)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file", len(entries))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0666 less the umask", fi.Mode().Perm())
	}
}

func TestWriteKeepsTheModeItIsGiven(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o077))
	path := filepath.Join(t.TempDir(), "a")
	if err := Write(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v under umask 077, want 0600", fi.Mode().Perm())
	}
}

func TestWriteIntoAMissingDirectoryFailsCleanly(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "nope", "a"), nil, 0o600); err == nil {
		t.Error("wrote into a directory that isn't there")
	}
}
