package engine

import (
	"path/filepath"
	"testing"
)

// The default free-space read answers a real directory's filesystem, and an
// error for a directory that does not exist, which journal_full's clear
// counts as no room.
func TestFreeSpaceReadsTheFilesystem(t *testing.T) {
	dir := t.TempDir()

	if n, err := freeSpace(dir); err != nil || n <= 0 {
		t.Errorf("freeSpace(%s) = %d, %v; want a positive figure", dir, n, err)
	}

	if _, err := freeSpace(filepath.Join(dir, "missing")); err == nil {
		t.Error("freeSpace of a missing directory answered no error")
	}
}
