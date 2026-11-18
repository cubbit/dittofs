//go:build linux

package bootstrap

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// A socket path past Linux's socket-address limit, 107 bytes, is refused,
// and one at the limit binds: the bind, not a constant, says where the limit
// is.
func TestLoadSocketLimit(t *testing.T) {
	t.Parallel()

	pathOf := func(t *testing.T, n int) string {
		t.Helper()

		dir := t.TempDir()
		name := strings.Repeat("s", n-len(dir)-1)

		return filepath.Join(dir, name)
	}

	at := pathOf(t, 107)
	if b := loads(t, "api:\n  socket: "+at+"\n", nil); b.API.Socket != at {
		t.Errorf("api.socket: got %q, want %q", b.API.Socket, at)
	}

	l, err := net.Listen("unix", at)
	if err != nil {
		t.Fatalf("bind a 107-byte socket path: %v", err)
	}

	_ = l.Close()

	past := pathOf(t, 108)

	path, got := loadRefused(t, "api:\n  socket: "+past+"\n", nil)
	if want := wantReport(path,
		"P: api.socket: longer than 107 bytes, the socket address limit, got "+past,
	); got != want {
		t.Errorf("report:\ngot:\n%s\nwant:\n%s", got, want)
	}

	if l, err := net.Listen("unix", past); err == nil {
		_ = l.Close()

		t.Errorf("bind a 108-byte socket path: got no error, want the kernel's refusal")
	}
}
