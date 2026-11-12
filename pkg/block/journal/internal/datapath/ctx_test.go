package datapath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// A data-path call's context rules are the one helper every such call starts
// with. The two tests below pin what the helper owes: a done context is
// answered before any refusal check and moves nothing — no record written, no
// reservation taken — and a refusal below it, the capacity admission's as
// much as a shape's or a tag's, returns as itself, never wrapped.

// halfWritePayload returns a payload whose record cost is the largest not
// past half the record cost of an each-byte write, on the 8-byte record grid,
// so a share limited at that payload's own record bytes fills with one write
// to just under half of what an each-byte write asks.
func halfWritePayload(each int64) int64 {
	half := recordBytes(each) / 2
	p := half
	for recordBytes(p) > half {
		p -= 8
	}
	return p
}

// filledShare builds a core, one share whose limit one write of fillPayload
// bytes fills exactly, and the file holding that write, so the share sits at
// its limit and the next admitting call through it is the limit's to refuse.
func filledShare(t *testing.T, fillPayload int64) (*Core, *Share, core.FileID) {
	t.Helper()
	c, _ := newTestCore(t)
	s := mustLimitShare(t, c, 1, recordBytes(fillPayload))
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, fillPayload), 1); err != nil {
		t.Fatalf("the limit-filling write: %v", err)
	}
	awaitSettledSpares(t, c, 2*testStreams)
	return c, s, f
}

// TestDoneContextChangesNothingAndTakesNoReservation calls every
// context-taking data-path operation with an already-cancelled context and
// checks each answers the context's error having changed nothing: no seam
// call, no record on disk, and no reservation taken. The share-limit legs
// pin the order the two refusals come in: the done context wins over the
// capacity admission's, and the same write under a live context meets the
// admission's refusal as itself, never wrapped in the context's error.
func TestDoneContextChangesNothingAndTakesNoReservation(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error
	}{
		{"WriteAt", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.WriteAt(ctx, s, fa, 0, ff(4), 1)
			return err
		}},
		{"Fill", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			return c.Fill(ctx, s, fb, 0, ff(4), ver(0), ver(1))
		}},
		{"Truncate", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.Truncate(ctx, s, fa, 2)
			return err
		}},
		{"Deallocate", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.Deallocate(ctx, s, fb, core.Extent{Off: 0, Len: 4})
			return err
		}},
		{"Delete", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.Delete(ctx, s, fa)
			return err
		}},
		{"CloneTarget", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.CloneTarget(ctx, s, fb, core.CloneSpec{Src: testFile(9), SrcOff: 1 << 20, DstOff: 0, Len: 4, AsOf: ver(7)})
			return err
		}},
		{"Reserve", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.Reserve(ctx, s, 64, 1)
			return err
		}},
		{"WriteReserved", func(ctx context.Context, c *Core, s *Share, fa, fb core.FileID, r *capacity.Reservation) error {
			_, err := c.WriteReserved(ctx, s, r, fa, 0, ff(4), 1)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			fa, fb := testFile(1), testFile(2)
			if streamOf(c, fa) == streamOf(c, fb) {
				t.Fatal("the two fixture files must fall on the core's two streams")
			}
			// The reservation the reserved-write leg draws on, taken for
			// real through the same share before the context is cancelled,
			// so the leg's only refusal is the context's.
			r, err := c.Reserve(context.Background(), s, 4096, 1)
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			// The reservation's hold takes ready spares, and the pool
			// fills above its content target by what holds demand, so the
			// census window opens only once the refill has settled at the
			// target beside the holds — a make landing inside it would
			// read as the refused call's own.
			waitIn(t, 5*time.Second, "the pool to settle at its target beside the holds", func() bool {
				pc := c.pool.Counters()
				return pc.Spares == int64(2*testStreams)+pc.Held
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			before := seamCensus(seam)
			beforeReserved := c.ReservedBytes()

			err = tc.call(ctx, c, s, fa, fb, r)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with a done context = %v, want context.Canceled answered before any refusal", tc.name, err)
			}
			if after := seamCensus(seam); after != before {
				t.Fatalf("%s with a done context made %d seam calls, want none: the refusal changed state", tc.name, after-before)
			}
			if got := c.ReservedBytes(); got != beforeReserved {
				t.Fatalf("ReservedBytes = %d after the refused %s, want the %d it found: the refusal took a reservation", got, tc.name, beforeReserved)
			}
		})
	}

	// The order against the share's limit, checked from both sides: a done
	// context wins over the capacity admission at a share sitting at its
	// limit, and the same write under a live context meets the admission's
	// refusal as itself.
	const each = 4096
	fillPayload := halfWritePayload(each)
	t.Run("done context over the share's limit", func(t *testing.T) {
		c, seam := newTestCore(t)
		s := mustLimitShare(t, c, 1, recordBytes(fillPayload))
		f := testFile(1)
		if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, fillPayload), 1); err != nil {
			t.Fatalf("the limit-filling write: %v", err)
		}
		awaitSettledSpares(t, c, 2*testStreams)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := seamCensus(seam)
		beforeReserved := c.ReservedBytes()

		_, err := c.WriteAt(ctx, s, f, 1<<20, make([]byte, each), 1)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteAt with a done context on a full share = %v, want context.Canceled before the limit's refusal", err)
		}
		var ns *core.NoSpaceError
		if errors.As(err, &ns) {
			t.Fatalf("the done context was refused by the %s limit instead of the context", ns.Limit)
		}
		if after := seamCensus(seam); after != before {
			t.Fatalf("the refusal made %d seam calls, want none", after-before)
		}
		if got := c.ReservedBytes(); got != beforeReserved {
			t.Fatalf("ReservedBytes = %d after the refusal, want the %d it found", got, beforeReserved)
		}
	})
	t.Run("the share's limit under a live context", func(t *testing.T) {
		c, s, f := filledShare(t, fillPayload)
		_, err := c.WriteAt(context.Background(), s, f, 1<<20, make([]byte, each), 1)
		ns, ok := err.(*core.NoSpaceError)
		if !ok {
			t.Fatalf("WriteAt at the share's limit = %v (%T), want the NoSpaceError as itself, not wrapped", err, err)
		}
		if ns.Limit != core.LimitShare {
			t.Fatalf("the refusal names the %s limit, want %s", ns.Limit, core.LimitShare)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the limit's refusal was wrapped in a context error: %v", err)
		}
		if got := c.ReservedBytes(); got != 0 {
			t.Fatalf("ReservedBytes = %d after the refusal, want 0", got)
		}
	})
}

// TestRefusalPastContextReturnsAsItself checks every refusal that comes after
// the context check answers as itself under a live context: the shape
// sentinel, the other-share refusal carrying the two tags, and the capacity
// admission's NoSpaceError at a share sitting at its limit.
func TestRefusalPastContextReturnsAsItself(t *testing.T) {
	ctx := context.Background()

	t.Run("shape", func(t *testing.T) {
		c, _ := newTestCore(t)
		s := mustShare(t, c, 1)
		_, err := c.WriteAt(ctx, s, testFile(1), -1, ff(4), 1)
		if err != core.ErrInvalidExtent {
			t.Fatalf("WriteAt at a negative offset = %v (%T), want core.ErrInvalidExtent as itself", err, err)
		}
	})

	t.Run("other share's file", func(t *testing.T) {
		c, _ := newTestCore(t)
		one := mustShare(t, c, 1)
		two := mustShare(t, c, 2)
		f := testFile(1)
		if _, err := c.WriteAt(ctx, one, f, 0, ff(4), 1); err != nil {
			t.Fatalf("the claim write: %v", err)
		}
		_, err := c.WriteAt(ctx, two, f, 0, ff(4), 1)
		other, ok := err.(*core.OtherShareError)
		if !ok {
			t.Fatalf("WriteAt through another share's handle = %v (%T), want the OtherShareError as itself, not wrapped", err, err)
		}
		if !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("the refusal %v does not match core.ErrOtherShare", err)
		}
		if other.Tag != two.Tag || other.Owner != one.Tag || other.File != f {
			t.Fatalf("the refusal names {%x tag %d owner %d}, want {%x tag %d owner %d}", other.File.Bytes(), other.Tag, other.Owner, f.Bytes(), two.Tag, one.Tag)
		}
	})

	// The capacity admission at a share sitting at its limit: each admitting
	// call meets its NoSpaceError directly. The share's limit is the only
	// refusal in the order these meets — the headroom and the journal are
	// off the fixture and the asks are one record each.
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, c *Core, s *Share, f core.FileID) error
	}{
		{"WriteAt", func(ctx context.Context, c *Core, s *Share, f core.FileID) error {
			_, err := c.WriteAt(ctx, s, f, 1<<20, ff(4), 1)
			return err
		}},
		{"Fill", func(ctx context.Context, c *Core, s *Share, f core.FileID) error {
			return c.Fill(ctx, s, f, 1<<20, ff(4), c.Index().AsOf(f), ver(1))
		}},
		{"Reserve", func(ctx context.Context, c *Core, s *Share, f core.FileID) error {
			_, err := c.Reserve(ctx, s, recordBytes(1)+1, 1)
			return err
		}},
	} {
		t.Run(tc.name+" at the share's limit", func(t *testing.T) {
			c, s, f := filledShare(t, halfWritePayload(4096))
			err := tc.call(ctx, c, s, f)
			ns, ok := err.(*core.NoSpaceError)
			if !ok {
				t.Fatalf("%s at the share's limit = %v (%T), want the NoSpaceError as itself, not wrapped", tc.name, err, err)
			}
			if ns.Limit != core.LimitShare {
				t.Fatalf("the %s refusal names the %s limit, want %s", tc.name, ns.Limit, core.LimitShare)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the limit's refusal was wrapped in a context error: %v", err)
			}
		})
	}
}
