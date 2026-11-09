package datapath

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

func TestReadNeverWrittenKeepsSentinel(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	id := core.NewFileID([16]byte{1})
	p := bytes.Repeat([]byte{0xA5}, 100)

	n, missing, held, _, err := c.ReadAt(context.Background(), s, id, 40, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if len(held) != 0 {
		t.Fatalf("held = %v, want none", held)
	}
	if want := []core.Extent{{Off: 40, Len: 100}}; len(missing) != 1 || missing[0] != want[0] {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	if !bytes.Equal(p, bytes.Repeat([]byte{0xA5}, 100)) {
		t.Fatal("p was touched")
	}
}

func TestReadOfNoBytesIsEmpty(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	id := core.NewFileID([16]byte{2})
	ctx := context.Background()

	n, missing, held, _, err := c.ReadAt(ctx, s, id, 7, nil)
	if err != nil || n != 0 || missing != nil || held != nil {
		t.Fatalf("empty read = %d %v %v %v", n, missing, held, err)
	}

	p := make([]byte, 4)
	if _, _, _, _, err := c.ReadAt(ctx, s, id, -1, p); !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("negative off: %v", err)
	}
	if _, _, _, _, err := c.ReadAt(ctx, s, id, math.MaxInt64-2, p); !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("overflowing end: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, _, err := c.ReadAt(cctx, s, id, 0, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, _, _, _, err := c.ReadAt(cctx, s, id, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled empty: %v", err)
	}
}

func TestReadResolvesAfterAsOfWithDomainReleased(t *testing.T) {
	var c *Core
	called := false
	c, _ = newTestCore(t, func(o *coreOptions) {
		o.beforeResolve = func() {
			called = true
			// A write needs the file's append domain: this would deadlock if
			// the read still held it.
			s := mustShare(t, c, 2)
			if _, err := c.WriteAt(context.Background(), s, core.NewFileID([16]byte{3}), 0, []byte("x"), 1); err != nil {
				t.Errorf("WriteAt in seam: %v", err)
			}
		}
	})
	s := mustShare(t, c, 3)
	if _, _, _, _, err := c.ReadAt(context.Background(), s, core.NewFileID([16]byte{3}), 0, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("beforeResolve not called")
	}
}
