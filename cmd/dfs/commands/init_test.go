package commands

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
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
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(dir, "store"), NodeID: testNode})
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

// Over a v1 Badger store at metadata.badger.path, dfs init refuses with the
// v1 sentence naming the directory, printing nothing, leaving v1's keys as
// they were and the journal directory empty.
func TestInitRefusesAV1MetadataStore(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	store := filepath.Join(dir, "store")
	writeV1Store(t, store)
	out, err := runDfs("init", "--config", config)
	if want := v1Sentence("a metadata store", store); err == nil || err.Error() != want || out != "" {
		t.Errorf("dfs init over a v1 store: got %q, %v; want nothing printed and %q", out, err, want)
	}
	checkV1Store(t, store)
	if ents, err := os.ReadDir(filepath.Join(dir, "device")); err != nil || len(ents) != 0 {
		t.Errorf("journal directory after the refusal: %d entries, %v; want it empty", len(ents), err)
	}
}

// Over a store kvstore.Init created that has since lost its format record,
// dfs init refuses it as foreign, never with the v1 sentence: its keys are
// this format's records.
func TestInitRefusesAStoreThatLostItsFormatRecord(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(dir, "store"), NodeID: testNode})
	if err != nil {
		t.Fatal(err)
	}
	err = kvstore.Init(t.Context(), db, [16]byte{1})
	if err == nil {
		err = db.Update(t.Context(), func(tx kv.Txn) error { return tx.Delete(keys.Format) })
	}
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := runDfs("init", "--config", config); !errors.Is(err, kvstore.ErrForeignStore) || strings.Contains(err.Error(), "from v1") {
		t.Errorf("dfs init over a store that lost its format record: got %v, want ErrForeignStore's wording, never the v1 sentence", err)
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

// skipUnsuitableJournalMount skips the test when the journal's platform
// check refuses the mount holding dir.
func skipUnsuitableJournalMount(t *testing.T, dir string) {
	t.Helper()
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(t.Context(), scratch, [16]byte{1}); errors.Is(err, journal.ErrUnsuitableStorage) {
		t.Skipf("the journal's platform check refuses the mount holding %s (%v); set TMPDIR to a directory on a suitable mount", dir, err)
	} else if err != nil {
		t.Fatalf("journal init on the scratch directory: %v", err)
	}
}

// keyFileConfig writes a bootstrap under dir whose storage key file,
// returned second, does not exist yet.
func keyFileConfig(t *testing.T, dir string) (string, string) {
	t.Helper()
	dev := filepath.Join(dir, "device")
	if err := os.Mkdir(dev, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "keys", "storage.key")
	if err := os.Mkdir(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	return bootstrapFile{store: filepath.Join(dir, "store"), journal: dev, node: nodeUUID, key: key}.write(t, dir), key
}

// With keys.storage.local.file naming no file, dfs init creates it with
// mode 0600, holding a master key the passphrase opens, prints once where
// it is and that it must be copied off the host, and initialises the
// installation; a second run prints nothing of the key file.
func TestInitCreatesTheStorageKeyFile(t *testing.T) {
	dir := t.TempDir()
	skipUnsuitableJournalMount(t, dir)
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", storagePassphrase)
	config, key := keyFileConfig(t, dir)
	out, err := runDfs("init", "--config", config)
	if err != nil {
		t.Fatalf("dfs init: %v", err)
	}
	if n := strings.Count(out, "created the storage key file "+key); n != 1 || strings.Count(out, "Copy it off this host") != 1 {
		t.Errorf("dfs init output %q: key file notice printed %d times, want once with the copy-off-host warning", out, n)
	}
	fi, err := os.Stat(key)
	if err != nil {
		t.Fatalf("stat the key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode: got %v, want 0600", fi.Mode().Perm())
	}
	p, err := keyprovider.NewProviderWithPassphrase(t.Context(), keyprovider.Config{Kind: keyprovider.KindLocal, File: key}, storagePassphrase)
	if err != nil {
		t.Fatalf("open the created key file with the passphrase: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if ents, err := os.ReadDir(filepath.Dir(key)); err != nil || len(ents) != 1 {
		t.Errorf("key directory after init: %d entries, %v; want only the key file", len(ents), err)
	}
	if out, err := runDfs("init", "--config", config); !errors.Is(err, install.ErrInstalled) || out != "" {
		t.Errorf("second dfs init: got %q, %v; want nothing printed and ErrInstalled", out, err)
	}
}

// An existing storage key file is used as it is: dfs init initialises the
// installation with it, leaving its bytes and mtime unchanged and printing
// nothing of it.
func TestInitUsesAnExistingStorageKeyFile(t *testing.T) {
	dir := t.TempDir()
	skipUnsuitableJournalMount(t, dir)
	config := initConfig(t, dir)
	key := filepath.Join(dir, "storage.key")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(key, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runDfs("init", "--config", config)
	if err != nil {
		t.Fatalf("dfs init: %v", err)
	}
	if strings.Contains(out, "storage key file") {
		t.Errorf("dfs init output %q: want no key file notice for an existing file", out)
	}
	after, err := os.ReadFile(key)
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("key file after init: %v, changed %v; want it byte-identical", err, !bytes.Equal(after, before))
	}
	if fi, err := os.Stat(key); err != nil || !fi.ModTime().Equal(past) {
		t.Errorf("key file mtime after init: got %v, %v; want %v", fi.ModTime(), err, past)
	}
}

// With no passphrase set and no key file, dfs init refuses, naming
// DITTOFS_ENCRYPTION_PASSPHRASE, printing nothing and creating no file.
func TestInitKeyFileNeedsThePassphrase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "")
	config, key := keyFileConfig(t, dir)
	out, err := runDfs("init", "--config", config)
	if !errors.Is(err, keyprovider.ErrPassphraseMissing) || !strings.Contains(err.Error(), "DITTOFS_ENCRYPTION_PASSPHRASE") || out != "" {
		t.Errorf("dfs init without a passphrase: got %q, %v; want nothing printed and ErrPassphraseMissing naming the variable", out, err)
	}
	if ents, err := os.ReadDir(filepath.Dir(key)); err != nil || len(ents) != 0 {
		t.Errorf("key directory after the refusal: %d entries, %v; want it empty", len(ents), err)
	}
}

// An existing key file the passphrase does not open is refused as the
// provider refuses it, and left as it was.
func TestInitRefusesAKeyFileTheWrongPassphraseCannotOpen(t *testing.T) {
	dir := t.TempDir()
	config := initConfig(t, dir)
	key := filepath.Join(dir, "storage.key")
	before, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", "not the passphrase")
	if out, err := runDfs("init", "--config", config); !errors.Is(err, keyprovider.ErrUnwrapFailed) || !strings.Contains(err.Error(), "opening the storage key") || out != "" {
		t.Errorf("dfs init with the wrong passphrase: got %q, %v; want nothing printed and ErrUnwrapFailed opening the storage key", out, err)
	}
	if after, err := os.ReadFile(key); err != nil || !bytes.Equal(after, before) {
		t.Errorf("key file after the refusal: %v, changed %v; want it byte-identical", err, !bytes.Equal(after, before))
	}
}
