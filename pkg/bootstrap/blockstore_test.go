package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockstoreJournalConfig_Validate_AcceptsZeroNewKnobs(t *testing.T) {
	// Zero for the new knobs means "apply the built-in default" (filled by
	// applyDefaults), so validate must accept a fully-zero config.
	c := BlockstoreJournalConfig{}
	if err := c.validate(); err != nil {
		t.Fatalf("validate(): got %v, want nil with zero new knobs", err)
	}
}

// max_log_bytes is the append-log pressure budget. Zero means "use the
// system-deduced default" (resolved at the runtime layer, since it depends on
// system memory), so applyDefaults must NOT rewrite a zero value, validate must
// accept zero and any positive value, and the key must round-trip through YAML
// and the DITTOFS_* reflective env binding.

func TestBlockstoreJournalConfig_ApplyDefaults_SetsAbsolutePath(t *testing.T) {
	c := BlockstoreJournalConfig{}
	c.applyDefaults()
	if c.Path == "" {
		t.Fatal("Path: got empty, want a default")
	}
	if !filepath.IsAbs(c.Path) {
		t.Errorf("Path: got %q, want an absolute path", c.Path)
	}

	if want := filepath.Join(getStateDir(), "blocks"); c.Path != want {
		t.Errorf("Path: got %q, want %q", c.Path, want)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_PreservesPath(t *testing.T) {
	c := BlockstoreJournalConfig{Path: "/srv/dittofs/blocks"}
	c.applyDefaults()
	if c.Path != "/srv/dittofs/blocks" {
		t.Errorf("Path: got %q, want the configured value preserved", c.Path)
	}
}

func TestBlockstoreJournalConfig_ApplyDefaults_ExpandsHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	c := BlockstoreJournalConfig{Path: "~/dittofs-blocks"}
	c.applyDefaults()
	if want := filepath.Join(home, "dittofs-blocks"); c.Path != want {
		t.Errorf("Path: got %q, want %q", c.Path, want)
	}
}

// A relative path would resolve against whatever directory the server happens
// to be started from, putting a share's only local copy somewhere the next
// start cannot find. validate must refuse it rather than let it through.
func TestBlockstoreJournalConfig_Validate_RejectsRelativePath(t *testing.T) {
	c := BlockstoreJournalConfig{Path: "blocks"}

	err := c.validate()
	if err == nil {
		t.Fatal("validate: got nil, want an error for a relative path")
	}
	if !strings.Contains(err.Error(), "blockstore.journal.path") {
		t.Errorf("validate: error %q does not name the offending key", err)
	}
}

// Empty means "apply the default" and is only reachable before applyDefaults,
// so it must not be reported as an operator error.
func TestBlockstoreJournalConfig_Validate_AcceptsEmptyPath(t *testing.T) {
	c := BlockstoreJournalConfig{}
	if err := c.validate(); err != nil {
		t.Errorf("validate: got %v, want nil for an unset path", err)
	}
}
