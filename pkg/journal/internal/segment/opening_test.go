package segment_test

import (
	"context"
	"errors"
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
	const cap = 1 << 20
	seam, err := kit.NewSeam(t.TempDir())
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	set := segment.NewSet(cap, nil)
	maker := segment.NewMaker(seam.FS(), set, 8*cap, 0, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	pool := segment.NewPool(maker, segment.Targets{Content: 1}, nil, nil)
	s := segment.NewStream(1, streamIdentity, set, pool, cache, nopSink())
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown = %v, want nil", err)
		}
	})
	t.Cleanup(func() { _ = s.Close() })
	t.Cleanup(func() { pool.Stop() })

	makes := seam.Hold(t, kit.OpCreate, "*.spare.tmp")
	pool.Start()
	select {
	case <-makes.Reached():
	case <-time.After(30 * time.Second):
		t.Fatalf("the started pool's first make never reached the seam")
	}

	h, body := streamWriteHeader(streamFileID(1), 0, []byte("opening"), 1, core.Version{Counter: 1})
	_, err = s.Append(h, body)
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
