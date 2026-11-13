package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jdemo.json")
	var id journal.JournalID
	copy(id[:], "state-round-trip") // 16 bytes
	st := stateFile{ID: id, Limit: 42}

	if err := saveState(path, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	// The encoding is pinned: JSON with the id as 32 lowercase hex chars.
	wantID := `"id":"` + hex.EncodeToString(id[:]) + `"`
	if !strings.Contains(string(raw), wantID) {
		t.Errorf("state file encoding: got %s, want it to contain %s", raw, wantID)
	}
	if !strings.Contains(string(raw), `"limit":42`) {
		t.Errorf("state file encoding: got %s, want it to contain \"limit\":42", raw)
	}

	got, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if got.ID != id {
		t.Errorf("loadState ID: got %x, want %x", got.ID, id)
	}
	if got.Limit != 42 {
		t.Errorf("loadState Limit: got %d, want 42", got.Limit)
	}
}

func TestLoadStateMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jdemo.json")
	if _, err := loadState(path); !os.IsNotExist(err) {
		t.Errorf("loadState of missing file: got err %v, want fs.ErrNotExist", err)
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jdemo.json")
	if err := os.WriteFile(path, []byte("{{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	_, err := loadState(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("loadState of corrupt file: got err %v, want an error naming %s", err, path)
	}
}
