package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/block/journal"
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
