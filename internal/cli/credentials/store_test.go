package credentials

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// update runs fn under the lock on the file at path, failing the test on an
// error.
func update(t *testing.T, path string, fn func(*Store) error) {
	t.Helper()
	if err := Update(t.Context(), path, nil, fn); err != nil {
		t.Fatalf("Update %s: %v", path, err)
	}
}

// set saves ctx as the context name of the file at path.
func set(t *testing.T, path, name string, ctx *Context) {
	t.Helper()
	update(t, path, func(s *Store) error { return s.SetContext(name, ctx) })
}

// read reads the file at path, failing the test on an error.
func read(t *testing.T, path string) Snapshot {
	t.Helper()
	s, err := Read(path)
	if err != nil {
		t.Fatalf("Read %s: %v", path, err)
	}
	return s
}

// Every change is saved: a store read afresh sees the contexts, the current
// one and the tokens as the last change left them.
func TestStoreChangesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dfsctl", "config.json")
	if names := read(t, path).ListContexts(); len(names) != 0 {
		t.Fatalf("a missing file's contexts: %v, want none", names)
	}
	set(t, path, "a", &Context{ServerURL: "https://a.example", Username: "eli", CACert: "/ca.pem"})
	set(t, path, "b", &Context{ServerURL: "/run/dittofs/api.sock"})
	update(t, path, func(s *Store) error {
		for _, err := range []error{s.SetTokens("a", "access", "refresh"), s.UseContext("a"), s.RenameContext("a", "prod"), s.DeleteContext("b")} {
			if err != nil {
				return err
			}
		}
		if err := s.SetTokens("b", "x", "y"); err != ErrContextNotFound {
			t.Errorf("SetTokens on a missing context: %v, want ErrContextNotFound", err)
		}
		return nil
	})

	got := read(t, path)
	if name := got.GetCurrentContextName(); name != "prod" {
		t.Errorf("current context: %q, want prod", name)
	}
	c, err := got.GetCurrentContext()
	if err != nil {
		t.Fatalf("GetCurrentContext: %v", err)
	}
	want := Context{ServerURL: "https://a.example", Username: "eli", CACert: "/ca.pem", AccessToken: "access", RefreshToken: "refresh"}
	if *c != want {
		t.Errorf("context prod: %+v, want %+v", *c, want)
	}
	if _, err := got.GetContext("b"); err != ErrContextNotFound {
		t.Errorf("deleted context b: %v, want ErrContextNotFound", err)
	}
}

// The file is its owner's alone, 0600 in a 0700 directory, a group-writable
// directory that already existed included, and a save leaves
// no temporary file beside it, only the lock file.
func TestStoreFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes; the file's ACL on Windows is not checked")
	}
	dir := filepath.Join(t.TempDir(), "dfsctl")
	path := filepath.Join(dir, "config.json")
	// An existing directory wider than 0700 is narrowed by the save.
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	set(t, path, "a", &Context{ServerURL: "https://a.example", AccessToken: "secret"})
	for _, tc := range []struct {
		path string
		want os.FileMode
	}{{path, FilePermissions}, {dir, DirPermissions}} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if fi.Mode().Perm() != tc.want {
			t.Errorf("%s: mode %o, want %o", tc.path, fi.Mode().Perm(), tc.want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the directory holds %d entries, want config.json and its lock", len(entries))
	}
}

// A file a context of which says tls_skip_verify: true is refused, naming
// the field and --cacert; false is read as absent.
func TestStoreRefusesSkipVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(skip string) {
		t.Helper()
		body := `{"current_context":"a","contexts":{"a":{"server_url":"https://a.example","tls_skip_verify":` + skip + `}}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("true")
	_, err := Read(path)
	if err == nil || !strings.Contains(err.Error(), "tls_skip_verify") || !strings.Contains(err.Error(), "--cacert") {
		t.Errorf("a file holding tls_skip_verify true: %v, want a refusal naming tls_skip_verify and --cacert", err)
	}
	write("false")
	if _, err := Read(path); err != nil {
		t.Errorf("a file holding tls_skip_verify false: %v, want it read", err)
	}
}

// While one Update holds the lock, another waits, saying so once, and runs
// only after the first returns, reading what the first saved.
func TestUpdateWaitsForTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	held, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- Update(t.Context(), path, nil, func(s *Store) error {
			close(held)
			<-release
			return s.SetContext("a", &Context{ServerURL: "https://a.example"})
		})
	}()
	<-held
	waited := make(chan string, 1)
	second := make(chan error, 1)
	go func() {
		second <- Update(t.Context(), path, func(lock string) { waited <- lock }, func(s *Store) error {
			if _, err := s.GetContext("a"); err != nil {
				return fmt.Errorf("the context the first Update saved: %w", err)
			}
			return nil
		})
	}()
	select {
	case lock := <-waited:
		if lock != path+".lock" {
			t.Errorf("waited on %s, want %s.lock", lock, path)
		}
	case err := <-second:
		t.Fatalf("the second Update returned %v while the first held the lock, want it waiting", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Update: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("second Update: %v", err)
	}
}
