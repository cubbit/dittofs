package credentials

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// open reads the store at path, failing the test on an error.
func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := NewStoreAt(path)
	if err != nil {
		t.Fatalf("NewStoreAt(%s): %v", path, err)
	}
	return s
}

// Every change is saved: a store read afresh sees the contexts, the current
// one and the tokens as the last change left them.
func TestStoreChangesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dfsctl", "config.json")
	s := open(t, path)
	if names := s.ListContexts(); len(names) != 0 {
		t.Fatalf("a missing file's contexts: %v, want none", names)
	}
	if err := s.SetContext("a", &Context{ServerURL: "https://a.example", Username: "eli", CACert: "/ca.pem"}); err != nil {
		t.Fatalf("SetContext a: %v", err)
	}
	if err := s.SetContext("b", &Context{ServerURL: "/run/dittofs/api.sock"}); err != nil {
		t.Fatalf("SetContext b: %v", err)
	}
	if err := s.SetTokens("a", "access", "refresh"); err != nil {
		t.Fatalf("SetTokens: %v", err)
	}
	if err := s.UseContext("a"); err != nil {
		t.Fatalf("UseContext: %v", err)
	}
	if err := s.RenameContext("a", "prod"); err != nil {
		t.Fatalf("RenameContext: %v", err)
	}
	if err := s.DeleteContext("b"); err != nil {
		t.Fatalf("DeleteContext: %v", err)
	}

	got := open(t, path)
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
	if err := got.SetTokens("b", "x", "y"); err != ErrContextNotFound {
		t.Errorf("SetTokens on a missing context: %v, want ErrContextNotFound", err)
	}
}

// The file is its owner's alone, 0600 in a 0700 directory, and a save leaves
// no temporary file beside it.
func TestStoreFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes; the file's ACL on Windows is not checked")
	}
	dir := filepath.Join(t.TempDir(), "dfsctl")
	path := filepath.Join(dir, "config.json")
	if err := open(t, path).SetContext("a", &Context{ServerURL: "https://a.example", AccessToken: "secret"}); err != nil {
		t.Fatalf("SetContext: %v", err)
	}
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
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only config.json", len(entries))
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
	_, err := NewStoreAt(path)
	if err == nil || !strings.Contains(err.Error(), "tls_skip_verify") || !strings.Contains(err.Error(), "--cacert") {
		t.Errorf("a file holding tls_skip_verify true: %v, want a refusal naming tls_skip_verify and --cacert", err)
	}
	write("false")
	if _, err := NewStoreAt(path); err != nil {
		t.Errorf("a file holding tls_skip_verify false: %v, want it read", err)
	}
}
