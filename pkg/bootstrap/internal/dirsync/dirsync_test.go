package dirsync

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestSyncAnExistingDirectory(t *testing.T) {
	if err := Sync(t.TempDir()); err != nil {
		t.Fatalf("Sync of an existing directory: %v", err)
	}
}

func TestSyncAMissingDirectory(t *testing.T) {
	err := Sync(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Sync of a missing directory: got %v, want fs.ErrNotExist", err)
	}
}
