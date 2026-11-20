//go:build unix

package install_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/install"
)

// A first-password file is read no further than a byte past its limit: a
// file that never ends, /dev/zero, refuses the step as one too long rather
// than being read without end.
func TestPasswordFileReadIsBounded(t *testing.T) {
	f := newFix(t)
	d := f.deps()
	d.PasswordFile = "/dev/zero"
	done := make(chan error, 1)
	go func() { done <- install.Run(t.Context(), d) }()
	// A bounded wait is the assertion: an unbounded read of /dev/zero never
	// returns, so a step that has not answered by then read past its limit.
	select {
	case err := <-done:
		if !errors.Is(err, install.ErrPassword) || !strings.Contains(err.Error(), "longer than 4096 bytes") {
			t.Fatalf("Run with /dev/zero as the password file: got %v, want ErrPassword naming the limit", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run with /dev/zero as the password file: no answer within 30 s, want the read bounded at the limit")
	}
}
