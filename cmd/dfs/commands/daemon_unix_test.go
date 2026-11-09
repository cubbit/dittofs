//go:build !windows

package commands

import (
	"fmt"
	"os"
	"testing"
)

// A --log-file that cannot be read back from its end, such as a pipe or a
// terminal, gives no tail; it must not stop the start.
func TestOpenLogTail_Pipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if f := openLogTail(fmt.Sprintf("/dev/fd/%d", r.Fd())); f != nil {
		_ = f.Close()
		t.Fatal("a handle on a pipe, which cannot be read back from its end")
	}
}
