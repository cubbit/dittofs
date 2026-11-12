//go:build engine_conformance

package integration

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// A restart and a crash start the filesystem only once the engine's start has
// finished: while the new engine's start is held inside its start steps, the
// filesystem built for the new process refuses a call as not serving, and
// once the start is let through it serves the call.
func TestTheFilesystemStartsAfterTheEngine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(c *composition)
	}{
		{"restart", func(c *composition) { c.Restart() }},
		{"crash", func(c *composition) { c.Crash() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := compose(t)
			h, _ := c.File("f")
			entered, release := c.HoldNextStart()
			started := make(chan struct{})
			during := make(chan error, 1)
			gone := make(chan struct{})
			go func() {
				defer close(gone)
				defer release()
				select {
				case <-entered:
				case <-started:
					during <- errors.New("the engine's start never reached its hold")
					return
				}
				// Built before the engine's start, so the hold orders this read.
				_, err := c.fs.GetAttr(t.Context(), c.admin, h)
				during <- err
			}()
			// A start that fails ends the test here, so the deferred close
			// still lets the goroutine go.
			func() {
				defer func() { close(started); <-gone }()
				tc.start(c)
			}()
			if err := <-during; !errors.Is(err, vfs.ErrUnavailable) {
				t.Errorf("a call while the engine starts: %v, want ErrUnavailable", err)
			}
			if _, err := c.fs.GetAttr(t.Context(), c.admin, h); err != nil {
				t.Errorf("a call once the engine started: %v", err)
			}
		})
	}
}
