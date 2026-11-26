package segment_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// TestRefusedFirstRollKeepsTheOpeningSpare appends to a stream before its
// pool has made any spare — every make parked at the seam — so the stream's
// first roll is refused `spare`. The stream has still taken no segment, so the
// pool keeps its opening spare: once the makes run, it settles at the target
// and the opening spare beside it, and the retried append's roll finds both.
// The retry runs under a one-spare hold, as an admitted first write does, and
// the hold rides inside the opening spare: the pool makes nothing for it.
func TestRefusedFirstRollKeepsTheOpeningSpare(t *testing.T) {
	seam, pool, s := openingRig(t, segment.Targets{Content: 1}, 8*openingCap)

	makes := seam.Hold(t, kit.OpCreate, "*.spare.tmp")
	pool.Start()
	select {
	case <-makes.Reached():
	case <-time.After(30 * time.Second):
		t.Fatalf("the started pool's first make never reached the seam")
	}

	h, body := streamWriteHeader(streamFileID(1), 0, []byte("opening"), 1, core.Version{Counter: 1})
	_, err := s.Append(h, body)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) || ns.Limit != core.LimitSpare {
		t.Fatalf("Append with no spare made = %v, want the `spare` refusal", err)
	}

	makes.Release()
	if err := pool.WaitSpares(t.Context()); err != nil {
		t.Fatalf("WaitSpares after the makes ran: %v", err)
	}
	if got, want := pool.Counters().Spares, int64(2); got != want {
		t.Fatalf("Spares = %d after a refused first roll, want %d: the target and the opening spare the stream has not taken", got, want)
	}
	// The retry runs under a hold of one spare, as an admitted first write
	// does: the hold is for the opening spare, so the pool, its makes parked
	// again, settles at once with nothing to make.
	seam.Hold(t, kit.OpCreate, "*.spare.tmp")
	hold, err := pool.Hold(1)
	if err != nil {
		t.Fatalf("Hold(1) over the target and the opening spare: %v", err)
	}
	defer hold.Close()
	// The backstop is the assertion: a pool that counted the hold on top of
	// the opening spare parks this wait behind its held make.
	settled, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := pool.WaitSpares(settled); err != nil {
		t.Fatalf("WaitSpares beside the first write's hold = %v, want nil: the hold rides inside the opening spare", err)
	}
	if _, err := s.AppendRoll(h, body, segment.Roll{Hold: hold}); err != nil {
		t.Fatalf("the retried Append: %v", err)
	}
	if got, want := pool.Counters().Spares, int64(1); got != want {
		t.Fatalf("Spares = %d after the first roll took its opening spare, want %d: the target in stock", got, want)
	}
}

// openingCap is the segment cap of the opening-spare rows.
const openingCap = 1 << 20

// openingRig builds a pool of targets, its maker capped at max bytes, and one
// stream over it, not yet started, so a row can set the pool's hooks first.
func openingRig(t *testing.T, targets segment.Targets, max int64) (*kit.Seam, *segment.Pool, *segment.Stream) {
	t.Helper()
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(openingCap, nil)
	maker := segment.NewMaker(seam.FS(), set, max, 0, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	pool := segment.NewPool(maker, targets, nil, nil)
	s := segment.NewStream(1, streamIdentity, set, pool, cache, nopSink())
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown = %v, want nil", err)
		}
	})
	t.Cleanup(func() { _ = s.Close() })
	t.Cleanup(func() { pool.Stop() })
	return seam, pool, s
}

// settledSpares fails t unless the pool settles at its goal holding want ready
// content spares.
func settledSpares(t *testing.T, pool *segment.Pool, what string, want int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := pool.WaitSpares(ctx); err != nil {
		t.Fatalf("WaitSpares %s = %v, want nil", what, err)
	}
	if got := pool.Counters().Spares; got != want {
		t.Fatalf("Spares = %d %s, want %d", got, what, want)
	}
}

// TestARefusedOpeningSpareIsMadeOnAnIdlePool gives the opening spare back
// once the refill has settled at the target alone, as it can between a first
// take's refusal and the give-back: the give-back wakes the refill, so the
// pool makes the opening spare rather than standing idle one short.
func TestARefusedOpeningSpareIsMadeOnAnIdlePool(t *testing.T) {
	_, pool, _ := openingRig(t, segment.Targets{Content: 1}, 8*openingCap)
	pool.Start()
	settledSpares(t, pool, "at open", 2)

	if !pool.OpenedForTest() {
		t.Fatal("the first stream's drop found no opening spare to drop")
	}
	for i := range 2 {
		if _, err := pool.Take(segment.Content); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	// The refill makes the target back and goes idle at it.
	awaitCounters(t, "the target alone, the refill idle", pool, func(c segment.PoolCounters) bool {
		return c.Spares == 1 && !pool.FillingForTest()
	})

	pool.OpeningRefusedForTest()
	settledSpares(t, pool, "after the opening spare was given back", 2)
}

// TestAnUnwoundFirstRollGivesTheOpeningSpareBack fails the successor header
// write of a stream's first roll after its take landed: the taken segment is
// unwound, the stream still has none, so the pool keeps its opening spare and
// makes it again.
func TestAnUnwoundFirstRollGivesTheOpeningSpareBack(t *testing.T) {
	seam, pool, s := openingRig(t, segment.Targets{Content: 1}, 8*openingCap)
	pool.Start()
	settledSpares(t, pool, "at open", 2)

	if err := seam.Schedule(kit.Failing(kit.OpWrite, "*", 1, syscall.EIO)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	h, body := streamWriteHeader(streamFileID(1), 0, []byte("opening"), 1, core.Version{Counter: 1})
	if _, err := s.Append(h, body); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Append with its header write failed = %v, want EIO", err)
	}
	if err := seam.Schedule(); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	settledSpares(t, pool, "after the first roll unwound", 2)
}

// TestAHeadroomFirstRollKeepsTheOpeningSpare rolls a stream's first record,
// a header-only one, onto a headroom spare while the content class is still
// empty: no content spare was taken, so the pool keeps its opening spare and
// fills the content class to the target and the opening spare.
func TestAHeadroomFirstRollKeepsTheOpeningSpare(t *testing.T) {
	_, pool, s := openingRig(t, segment.Targets{Content: 1, Headroom: 1}, 8*openingCap)
	// The refill makes the headroom's spare first; the second class it
	// chooses, content, parks until the roll has run.
	parked, release := make(chan struct{}), make(chan struct{})
	calls := 0
	pool.SetRefillClassHook(func() {
		if calls++; calls == 2 {
			close(parked)
			<-release
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	pool.Start()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the refill never chose its second class")
	}

	hdr := format.RecordHeader{Kind: format.KindStamp, File: streamFileID(1), Version: core.Version{Counter: 1}, Tag: 7}
	if _, err := s.AppendRoll(hdr, format.RecordBody{Cut: 4}, segment.Roll{}); err != nil {
		t.Fatalf("the header-only first append: %v", err)
	}
	if got := pool.Counters().HeadroomBytes; got != 0 {
		t.Fatalf("HeadroomBytes = %d after the header-only first roll, want 0: the roll took the headroom spare", got)
	}

	close(release)
	settledSpares(t, pool, "after a headroom first roll", 2)
}
