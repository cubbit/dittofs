package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// TestDittofsInit_FilesLoadAndOpenTheKey guards the files and environment
// the setup hands dfs init and dfs start: the real loader and validator read
// them as the server does, so a refused variable name, an upper-case level,
// a missing node identity, protocol or storage key, logs sent away from the
// server's log, or a key file the passphrase does not open fails here instead
// of failing every cell of a managed run at setup.
func TestDittofsInit_FilesLoadAndOpenTheKey(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	confDir := filepath.Join(home, ".config", "dittofs")

	configPath, passwordFile, err := writeDittofsInit(confDir, data)
	if err != nil {
		t.Fatalf("writeDittofsInit: %v", err)
	}

	env := append(dittofsEnv(), "HOME="+home)

	// The key provider reads its passphrase from the process, as dfs init's does.
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}

	cfg, err := bootstrap.Load(configPath, env)
	if err != nil {
		t.Fatalf("bootstrap.Load over the setup's files and environment: %v", err)
	}

	if err := cfg.Validate(t.Context(), env); err != nil {
		t.Fatalf("Validate over the setup's files and environment: %v", err)
	}

	if got, want := cfg.Node.ID, entity.NodeID(uuid.MustParse(dittofsNodeID)); got != want {
		t.Errorf("node.id: got %s, want %s", uuid.UUID(got), uuid.UUID(want))
	}

	if got, want := cfg.Logging.Level, "warn"; got != want {
		t.Errorf("logging level: got %q, want %q", got, want)
	}

	if got, want := cfg.Logging.Output, "stderr"; got != want {
		t.Errorf("logging output: got %q, want %q", got, want)
	}

	if got, want := cfg.Metadata.Badger.Path, filepath.Join(data, "meta"); got != want {
		t.Errorf("metadata.badger.path: got %q, want %q", got, want)
	}

	if len(cfg.Journals) != 1 || cfg.Journals[0].Path != filepath.Join(data, "blocks") {
		t.Errorf("journals: got %v, want one at %q", cfg.Journals, filepath.Join(data, "blocks"))
	}

	key, err := keyprovider.NewProvider(t.Context(), cfg.Keys.Storage.Provider())
	if err != nil {
		t.Fatalf("opening keys.storage with the environment's passphrase: %v", err)
	}

	t.Cleanup(func() {
		_ = key.Close()
	})

	password, err := os.ReadFile(passwordFile)
	if err != nil {
		t.Fatalf("reading the password file: %v", err)
	}

	if got := string(password); got != dittofsAdminPass {
		t.Errorf("password file: got %q, want the login's %q", got, dittofsAdminPass)
	}
}

// TestDittofsDurabilityFlags_EachTierIsADistinctPair guards the matrix: nine
// registered backends differ only by metadata engine and tier, so two tiers
// expanding to the same pair would report cells that measured the same thing.
func TestDittofsDurabilityFlags_EachTierIsADistinctPair(t *testing.T) {
	seen := map[string]string{}
	for _, tier := range []string{"local", "writeback", "remote"} {
		ack, relaxed, err := dittofsDurabilityFlags(tier)
		if err != nil {
			t.Fatalf("dittofsDurabilityFlags(%q): %v", tier, err)
		}
		key := fmt.Sprintf("%s/%t", ack, relaxed)
		if prev, dup := seen[key]; dup {
			t.Errorf("tiers %q and %q both expand to %s", prev, tier, key)
		}
		seen[key] = tier
	}
}

// TestDittofsDurabilityFlags_RejectsAnUnknownTier keeps an unrecognised name
// from silently taking a default, which would label a run with a tier it never
// exercised.
func TestDittofsDurabilityFlags_RejectsAnUnknownTier(t *testing.T) {
	if _, _, err := dittofsDurabilityFlags("block-store"); err == nil {
		t.Fatal("got nil, want an error for a tier name that is not in the matrix")
	}
}
