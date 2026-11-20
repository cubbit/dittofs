package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// initConfig writes a bootstrap under dir naming a store, one journal
// directory and a storage key file, its passphrase set for the test.
func initConfig(t *testing.T, dir string) string {
	t.Helper()
	dev := filepath.Join(dir, "device")
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatal(err)
	}
	return bootstrapFile{store: filepath.Join(dir, "store"), journal: dev, node: nodeUUID, key: storageKeyFile(t, dir)}.write(t, dir)
}

// dfs init hands its password flags to the initialisation step: an empty
// file refuses, naming it; a flag given an empty value refuses, naming the
// flag, rather than generating a password; and naming both flags is refused.
func TestInitPasswordFlags(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runDfs("init", "--config", config, "--admin-password-file", empty); !errors.Is(err, install.ErrPassword) || !strings.Contains(err.Error(), empty) {
		t.Errorf("dfs init --admin-password-file %s: got %v, want ErrPassword naming the file", empty, err)
	}
	for _, flag := range []string{"--admin-password-file", "--admin-password-env"} {
		if out, err := runDfs("init", "--config", config, flag, ""); !errors.Is(err, install.ErrPassword) || !strings.Contains(err.Error(), flag) || out != "" {
			t.Errorf("dfs init %s \"\": got %q, %v; want ErrPassword naming the flag, nothing printed", flag, out, err)
		}
	}
	if _, err := runDfs("init", "--config", config, "--admin-password-file", empty, "--admin-password-env", "X"); err == nil ||
		!strings.Contains(err.Error(), "admin-password-env") {
		t.Errorf("dfs init with both password flags: got %v, want them refused as exclusive", err)
	}
}

// dfs init reads --admin-password-file to RFC 23 §5.4's 4 KiB: a file a
// byte longer refuses, naming the file, printing nothing and leaving the
// store not initialised and the journal directory empty; a file of exactly
// 4 KiB then initialises the installation with its password, printing none.
// A host whose mount the journal's platform check refuses skips the
// accepted run.
func TestInitPasswordFileLimit(t *testing.T) {
	const limit = 4 << 10
	dir := t.TempDir()
	config := initConfig(t, dir)
	file := filepath.Join(dir, "admin-password")
	if err := os.WriteFile(file, []byte(strings.Repeat("p", limit+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runDfs("init", "--config", config, "--admin-password-file", file)
	if !errors.Is(err, install.ErrPassword) || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "longer than 4096 bytes") || out != "" {
		t.Fatalf("dfs init with a %d-byte password file: got %q, %v; want ErrPassword naming the file and the limit, nothing printed", limit+1, out, err)
	}
	if ents, err := os.ReadDir(filepath.Join(dir, "device")); err != nil || len(ents) != 0 {
		t.Errorf("journal directory after the refusal: %d entries, %v; want it empty", len(ents), err)
	}
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(dir, "store"), NodeID: testNode, Clock: time.Now})
	if err != nil {
		t.Fatalf("open the store after the refusal: %v", err)
	}
	if _, err := kvstore.Open(t.Context(), db); !errors.Is(err, kvstore.ErrNotInitialised) {
		t.Errorf("store after the refusal: open got %v, want it not initialised", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(t.Context(), scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", dir, err)
	} else if err != nil {
		t.Fatalf("journal init on the scratch directory: %v", err)
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("p", limit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runDfs("init", "--config", config, "--admin-password-file", file); err != nil || out != "" {
		t.Fatalf("dfs init with a %d-byte password file: got %q, %v; want it initialised, nothing printed", limit, out, err)
	}
	if _, err := runDfs("init", "--config", config); !errors.Is(err, install.ErrInstalled) {
		t.Errorf("second dfs init: got %v, want ErrInstalled", err)
	}
}

// dfs init initialises the installation the bootstrap names, printing the
// generated password on its standard output, and refuses a second run. A
// host whose mount the journal's platform check refuses skips the test.
func TestInitThenAgain(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(t.Context(), scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", dir, err)
	} else if err != nil {
		t.Fatalf("journal init on the scratch directory: %v", err)
	}
	config := initConfig(t, dir)
	out, err := runDfs("init", "--config", config)
	if err != nil {
		t.Fatalf("dfs init: %v", err)
	}
	if !strings.Contains(out, "generated password") {
		t.Errorf("dfs init output %q: want the generated password", out)
	}
	if out, err := runDfs("init", "--config", config); !errors.Is(err, install.ErrInstalled) || out != "" {
		t.Errorf("second dfs init: got %q, %v; want nothing printed and ErrInstalled", out, err)
	}
}

// dfs init reads the password from the variable --admin-password-env names
// even when that name starts with DITTOFS_, which the bootstrap's scan
// would refuse as unknown or retired: the variable holds a password, not a
// setting. The run initialises the installation and prints no password.
func TestInitPasswordEnvDittofsName(t *testing.T) {
	for _, name := range []string{"DITTOFS_ADMIN_INITIAL_PASSWORD", "DITTOFS_ADMIN_PW"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			scratch := filepath.Join(dir, "scratch")
			if err := os.Mkdir(scratch, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Init(t.Context(), scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
				t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", dir, err)
			} else if err != nil {
				t.Fatalf("journal init on the scratch directory: %v", err)
			}
			t.Setenv(name, "a-first-password")
			config := initConfig(t, dir)
			if out, err := runDfs("init", "--config", config, "--admin-password-env", name); err != nil || out != "" {
				t.Fatalf("dfs init --admin-password-env %s: got %q, %v; want it initialised, nothing printed", name, out, err)
			}
		})
	}
}
