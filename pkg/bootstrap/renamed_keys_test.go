package bootstrap

import (
	"path/filepath"
	"testing"
)

// A key under a renamed section names its replacement, whatever its case; a
// sibling section that only shares the letters, and a key never renamed,
// are not renamed.
func TestRenamedKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key  string
		want string
		ok   bool
	}{
		{"blockstore.local.max_log_bytes", "blockstore.journal.max_log_bytes", true},
		{"blockstore.local", "blockstore.journal", true},
		{"Blockstore.Local.Path", "blockstore.journal.path", true},
		{"blockstore.locality", "", false},
		{"lock.timeout", "", false},
	} {
		if got, ok := renamedKey(tc.key); got != tc.want || ok != tc.ok {
			t.Errorf("renamedKey(%q): got %q, %v, want %q, %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

// The renamed section in a file is refused naming the new key, and the new
// key loads.
func TestLoadRenamedBlockstoreSection(t *testing.T) {
	t.Parallel()

	path, got := loadRefused(t, "blockstore:\n  local:\n    path: /srv/blocks\n", nil)
	want := path + ": blockstore.local.path: no longer read; write blockstore.journal.path"

	if got != want {
		t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
	}

	dir := filepath.Join(t.TempDir(), "blocks")

	b := loads(t, "blockstore:\n  journal:\n    path: '"+dir+"'\n", nil)
	if b.Blockstore.Journal.Path != dir {
		t.Errorf("blockstore.journal.path: got %q, want %q", b.Blockstore.Journal.Path, dir)
	}
}
