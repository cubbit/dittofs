//go:build unix

package bootstrap

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at node-id, or a symbolic link to one, must be refused: a plain open
// of it waits for a writer, so the start would hang with no error.
func TestNodeIDCopyThatIsAFIFO(t *testing.T) {
	for _, viaLink := range []bool{false, true} {
		name := "a FIFO"
		if viaLink {
			name = "a symbolic link to a FIFO"
		}

		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, nodeIDName)

			if viaLink {
				target := filepath.Join(t.TempDir(), "fifo")
				if err := syscall.Mkfifo(target, 0o600); err != nil {
					t.Fatal(err)
				}

				if err := os.Symlink(target, fifo); err != nil {
					t.Fatal(err)
				}
			} else if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)

			go func() {
				_, err := NodeID(bootstrapOf(nodeA, dir))
				done <- err
			}()

			select {
			case err := <-done:
				refused(t, name+" at node-id", err, errCopyUnreadable, dir)
			case <-time.After(10 * time.Second):
				// A writer's open frees the blocked reader so the test can end.
				w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				if err == nil {
					_ = w.Close()
				}

				t.Fatalf("NodeID on %s at node-id did not return: the open waits for a writer", name)
			}
		})
	}
}

// A dangling symbolic link at node-id reads as no copy, yet the link finds the
// name taken: the refusal says nothing is readable there, not that another
// identity is.
func TestWriteCopyOverADanglingLink(t *testing.T) {
	dir := t.TempDir()

	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, nodeIDName)); err != nil {
		t.Fatal(err)
	}

	_, err := writeCopy(osDir(dir), dir, nodeA)
	refused(t, "a dangling link at node-id", err, errCopyUnreadable, dir)
}
