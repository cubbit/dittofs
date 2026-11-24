package keyprovider

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const keyFilePassphrase = "key file test passphrase"

// opens fails t unless the passphrase opens the key file at path.
func opens(t *testing.T, path string) {
	t.Helper()
	p, err := NewProviderWithPassphrase(t.Context(), Config{Kind: KindLocal, File: path}, keyFilePassphrase)
	if err != nil {
		t.Fatalf("open the key file %s with the passphrase: %v", path, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// names lists dir's entries by name.
func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ns []string
	for _, e := range ents {
		ns = append(ns, e.Name())
	}
	return ns
}

// A run stopped between writing its temporary and linking it leaves the
// temporary and no key file: the next create writes a key file the
// passphrase opens and removes the temporary, and a create over an existing
// key file keeps that file's bytes and removes temporaries left after its
// link.
func TestCreateKeyFileAfterAStoppedRun(t *testing.T) {
	for _, existing := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "storage.key")
		var before []byte
		if existing {
			var err error
			if before, err = GenerateKeyFile(keyFilePassphrase); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stale := filepath.Join(dir, ".storage.key.tmp12345")
		if err := os.WriteFile(stale, []byte("a stopped run's wrapped key"), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := CreateKeyFile(path, keyFilePassphrase)
		if err != nil || created == existing {
			t.Fatalf("CreateKeyFile with an existing file %v: got created %v, %v; want created %v", existing, created, err, !existing)
		}
		opens(t, path)
		if ns := names(t, dir); len(ns) != 1 || ns[0] != "storage.key" {
			t.Errorf("directory after CreateKeyFile with an existing file %v: got %v, want only storage.key", existing, ns)
		}
		if after, err := os.ReadFile(path); existing && (err != nil || !bytes.Equal(after, before)) {
			t.Errorf("existing key file after CreateKeyFile: %v, changed %v; want it byte-identical", err, !bytes.Equal(after, before))
		}
	}
}

// Concurrent creators of one key file publish exactly one: one reports it
// created the file, the rest use it, none fails, and no temporary is left.
func TestCreateKeyFileConcurrentCreators(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.key")
	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		errs    []error
	)
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			c, err := CreateKeyFile(path, keyFilePassphrase)
			mu.Lock()
			defer mu.Unlock()
			if c {
				created++
			}
			if err != nil {
				errs = append(errs, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if created != 1 || len(errs) != 0 {
		t.Errorf("%d concurrent creators: got %d created, errors %v; want exactly one created and no error", n, created, errs)
	}
	opens(t, path)
	if ns := names(t, dir); len(ns) != 1 || ns[0] != "storage.key" {
		t.Errorf("directory after the concurrent creators: got %v, want only storage.key", ns)
	}
}
