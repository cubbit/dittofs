package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// stateFile is what the demo daemon records durably beside the journal
// directory: the journal's identity, which Open requires, and the version
// limit it last set, which a caller that records its limit before raising it
// passes back as the open's floor. Without this file a journal on disk could
// never be reopened, because Open refuses an unknown expect unconditionally.
type stateFile struct {
	ID    journal.JournalID
	Limit uint64
}

// statePath returns the state file's sibling path: <dir>.jdemo.json, never
// inside the journal directory itself, so the journal's own listing of its
// directory stays exactly what the format owns.
func statePath(dir string) string {
	return filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".jdemo.json")
}

// saveState writes st to path as {"id": <32 hex>, "limit": n}.
func saveState(path string, st stateFile) error {
	b, err := json.Marshal(struct {
		ID    string `json:"id"`
		Limit uint64 `json:"limit"`
	}{hex.EncodeToString(st.ID[:]), st.Limit})
	if err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	return nil
}

// loadState reads the state file at path. A missing file answers the raw
// fs.ErrNotExist so the caller can tell a fresh start from a refused one;
// every other failure names the path.
func loadState(path string) (stateFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return stateFile{}, err
		}
		return stateFile{}, fmt.Errorf("state file %s: %w", path, err)
	}
	var raw struct {
		ID    string `json:"id"`
		Limit uint64 `json:"limit"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return stateFile{}, fmt.Errorf("state file %s: %w", path, err)
	}
	id, err := hex.DecodeString(raw.ID)
	if err != nil || len(id) != len(journal.JournalID{}) {
		return stateFile{}, fmt.Errorf("state file %s: id %q is not 32 hex chars", path, raw.ID)
	}
	var st stateFile
	copy(st.ID[:], id)
	st.Limit = raw.Limit
	return st, nil
}
