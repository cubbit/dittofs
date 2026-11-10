package capacity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// bigRecords is a headroom at the 8,191/8,192 edge: its open reserve is the
// fixed 4,096 records, its drawing side holds 4,096 and the half rule's half
// is 2,048.
const bigRecords = 8192

// hrFake is the injected facts of a Headroom: the pool's headroom bytes,
// the set's footprint and the read-only check, each a plain read. Its zero
// value answers "writable, no headroom byte, nothing used".
type hrFake struct {
	headroomBytes atomic.Int64
	footprint     atomic.Int64
	admitErr      atomic.Value // error, nil until a test stores one
}

func (f *hrFake) pool() int64 { return f.headroomBytes.Load() }
func (f *hrFake) foot() int64 { return f.footprint.Load() }
func (f *hrFake) admit() error {
	if err, _ := f.admitErr.Load().(error); err != nil {
		return err
	}
	return nil
}

// newFull rigs a Headroom of records records over a fresh fake: the pool
// answers the configured size and the journal sits far below its maximum.
func newFull(t *testing.T, records int) (*Headroom, *hrFake) {
	t.Helper()
	f := &hrFake{}
	f.headroomBytes.Store(int64(records) * RecordCharge)
	h := NewHeadroom(HeadroomConfig{
		Records:    records,
		Pool:       f.pool,
		Footprint:  f.foot,
		MaxBytes:   1 << 40,
		SegmentCap: 1 << 20,
		Admit:      f.admit,
	})
	return h, f
}

func drawWith(t *testing.T, h *Headroom, c RecordClass, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		got, err := h.Admit(context.Background(), c)
		if err != nil {
			t.Fatalf("draw %d of %d as %s failed: %v", i+1, n, c, err)
		}
		if !got.Append {
			t.Fatalf("draw %d of %d as %s was told not to append", i+1, n, c)
		}
	}
}

// noSpaceOrFatal fails the test unless err is ErrNoSpace naming the
// headroom with at least one entry missing.
func noSpaceOrFatal(t *testing.T, err error) {
	t.Helper()
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("the admission answered %v, want ErrNoSpace", err)
	}
	if ns.Limit != core.LimitHeadroom {
		t.Fatalf("the refusal named the %s limit, want the headroom", ns.Limit)
	}
	if ns.Missing < 1 {
		t.Fatalf("the refusal named %d %s missing, want at least one entry", ns.Missing, ns.Unit)
	}
}

func TestOpenReserveSizes(t *testing.T) {
	for _, tc := range []struct{ records, want int }{
		{65536, 4096},
		{8192, 4096},
		{8191, 4095},
		{100, 50},
	} {
		if got := OpenReserveRecords(tc.records); got != tc.want {
			t.Fatalf("the open reserve of a headroom of %d records is %d, want %d", tc.records, got, tc.want)
		}
	}
}

func TestAdmitClassesAboveHalf(t *testing.T) {
	h, _ := newFull(t, bigRecords)
	for _, c := range []RecordClass{
		Release, Truncate, Deallocate, Delete, CloneTarget, Offloaded,
		Unmark, Loss, Hold, Unhold, Stamp, Forget, Reconciled, Open,
		RepackCopy,
	} {
		got, err := h.Admit(context.Background(), c)
		if err != nil {
			t.Fatalf("%s above half answered %v, want no refusal", c, err)
		}
		if !got.Append {
			t.Fatalf("%s above half was told not to append", c)
		}
	}
}

func TestAdmitBelowHalf(t *testing.T) {
	// The fixed figures of the rig: 8,192 records, a 4,096 open reserve, so
	// the half rule falls at 2,048 records drawn.
	h, _ := newFull(t, bigRecords)
	drawWith(t, h, Release, 2048) // the drawing side exactly at half
	if h.BelowHalf() {
		t.Fatalf("at exactly half the headroom already reads below half")
	}
	drawWith(t, h, Release, 1) // one record past the edge
	if !h.BelowHalf() {
		t.Fatalf("one record past half the headroom does not read below half")
	}

	// What frees nothing is refused naming the headroom.
	for _, c := range []RecordClass{Hold, Stamp, CloneTarget} {
		_, err := h.Admit(context.Background(), c)
		if !errors.Is(err, core.ErrNoSpace) {
			t.Fatalf("%s below half answered %v, want ErrNoSpace", c, err)
		}
		noSpaceOrFatal(t, err)
	}

	// Everything that returns space draws on the rest.
	for _, c := range []RecordClass{
		Release, Truncate, Deallocate, Delete, Unmark, Loss, Unhold,
		Forget, Reconciled,
	} {
		got, err := h.Admit(context.Background(), c)
		if err != nil {
			t.Fatalf("%s below half answered %v, want no refusal", c, err)
		}
		if !got.Append {
			t.Fatalf("%s below half was told not to append", c)
		}
	}

	// An offloaded record is neither appended nor refused: the bits are set
	// in memory only and no counter moves.
	drawsBefore := h.Draws()
	got, err := h.Admit(context.Background(), Offloaded)
	if err != nil {
		t.Fatalf("below half, Offloaded answered %v, want no refusal", err)
	}
	if got.Append {
		t.Fatalf("below half, Offloaded was told to append, want the bits in memory only")
	}
	if h.Draws() != drawsBefore {
		t.Fatalf("below half, Offloaded moved headroom_draws to %d, want %d", h.Draws(), drawsBefore)
	}

	// The Offloaded admission drew nothing — 2049 + 9 records are drawn —
	// so the drawing side still runs to exactly the reserve: the next
	// removal is refused, and an Open is admitted on the reserve.
	drawWith(t, h, Release, bigRecords-4096-2049-9)
	_, err = h.Admit(context.Background(), Release)
	noSpaceOrFatal(t, err)
	o, err := h.Admit(context.Background(), Open)
	if err != nil {
		t.Fatalf("with exactly the reserve left an Open was refused: %v", err)
	}
	if !o.Append {
		t.Fatalf("with exactly the reserve left an Open was told not to append")
	}
}

func TestReserveEdge(t *testing.T) {
	// One record over the reserve: a removal is admitted (it consumes the
	// last drawing-side record) and an Open is admitted beside it.
	h, _ := newFull(t, bigRecords)
	drawWith(t, h, Delete, bigRecords-4096-1)
	if _, err := h.Admit(context.Background(), Delete); err != nil {
		t.Fatalf("with one record over the reserve a removal was refused: %v", err)
	}

	// Exactly the reserve: a removal is refused, an Open is admitted.
	h2, _ := newFull(t, bigRecords)
	drawWith(t, h2, Delete, bigRecords-4096)
	_, err := h2.Admit(context.Background(), Delete)
	noSpaceOrFatal(t, err)
	o, err := h2.Admit(context.Background(), Open)
	if err != nil {
		t.Fatalf("with exactly the reserve left an Open was refused: %v", err)
	}
	if !o.Append {
		t.Fatalf("with exactly the reserve left an Open was told not to append")
	}
}

func TestRefusalMovesNothing(t *testing.T) {
	h, _ := newFull(t, bigRecords)
	// Park the drawing side at exactly the reserve, below half.
	drawWith(t, h, Release, bigRecords-4096)
	drawsBefore := h.Draws()
	_, err := h.Admit(context.Background(), Hold)
	noSpaceOrFatal(t, err)
	_, err = h.Admit(context.Background(), Delete)
	noSpaceOrFatal(t, err)
	if h.Draws() != drawsBefore {
		t.Fatalf("refused admissions moved headroom_draws from %d to %d", drawsBefore, h.Draws())
	}
	// The refusals drew nothing: an Open still finds the whole reserve.
	if _, err := h.Admit(context.Background(), Open); err != nil {
		t.Fatalf("a refusal drew on the open reserve anyway, an Open was then refused: %v", err)
	}
}

func TestHeadroomDrawsOnlyAtLimit(t *testing.T) {
	// The journal is at its limit when the footprint plus a segment cap
	// would pass the maximum; a record drawn otherwise moves nothing.
	h, f := newFull(t, 1024)
	h.Drawn(Release)
	if got := h.Draws(); got != 0 {
		t.Fatalf("a record drawn below the limit moved headroom_draws to %d, want 0", got)
	}

	f.footprint.Store(1 << 40) // the maximum: any roll would pass it
	h.Drawn(Release)
	h.Drawn(Truncate)
	if got := h.Draws(); got != 2 {
		t.Fatalf("records drawn at the limit moved headroom_draws to %d, want 2", got)
	}

	f.footprint.Store(0)
	h.Drawn(Release)
	if got := h.Draws(); got != 2 {
		t.Fatalf("a record drawn below the limit moved headroom_draws to %d, want 2", got)
	}
}

func TestRepackCopyNeverRefused(t *testing.T) {
	// The headroom exhausted and the journal at its limit — the two
	// refusals a repack copy stands beside — and it is still admitted and
	// moves no headroom counter.
	h, f := newFull(t, bigRecords)
	drawWith(t, h, Release, bigRecords-4096)
	_, err := h.Admit(context.Background(), Delete)
	noSpaceOrFatal(t, err)
	f.footprint.Store(1 << 40)

	drawsBefore := h.Draws()
	got, err := h.Admit(context.Background(), RepackCopy)
	if err != nil {
		t.Fatalf("a repack copy was refused: %v", err)
	}
	if !got.Append {
		t.Fatalf("a repack copy was told not to append")
	}
	h.Drawn(RepackCopy)
	if h.Draws() != drawsBefore {
		t.Fatalf("the repack copy moved headroom_draws from %d to %d", drawsBefore, h.Draws())
	}
}

func TestConcurrentAdmitNeverOverdraws(t *testing.T) {
	// A small headroom, so the race window is wide: every goroutine races
	// to draw the few records beside the reserve, and no interleaving may
	// admit one more of them than the drawing side holds.
	records := 64
	h, _ := newFull(t, records)

	var wg sync.WaitGroup
	admitted := &atomic.Int64{}
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				got, err := h.Admit(context.Background(), Delete)
				if err == nil && got.Append {
					admitted.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	drawingSide := int64(records - OpenReserveRecords(records))
	if got := admitted.Load(); got > drawingSide {
		t.Fatalf("%d concurrent admissions drew more than the %d records beside the reserve", got, drawingSide)
	}
}

func TestAdmitThenDrawnChargesOneRecord(t *testing.T) {
	// The sequence Task 2 wires — Admit, then Drawn once the record is
	// appended — must charge the record once, not twice: the free side a
	// later Admit reads moves by one record charge per appended record.
	// The journal sits at its limit the whole time, so every Drawn runs
	// its counter path and every appended record counts in headroom_draws.
	h, f := newFull(t, bigRecords)
	f.footprint.Store(1 << 40)
	for i := 0; i < bigRecords-4097; i++ {
		if _, err := h.Admit(context.Background(), Release); err != nil {
			t.Fatalf("draw %d toward the reserve edge failed: %v", i, err)
		}
		h.Drawn(Release)
	}
	// reserve+1 records remain: the next removal is admitted and consumes
	// the last drawing-side record; the one after is refused.
	if _, err := h.Admit(context.Background(), Release); err != nil {
		t.Fatalf("with one record over the reserve a removal was refused: %v", err)
	}
	h.Drawn(Release)
	if _, err := h.Admit(context.Background(), Release); !errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("after the reserve-edge record landed, the next removal answered %v, want ErrNoSpace", err)
	}
	// Every appended record was drawn at the limit: the drawing side's
	// 4,096 records, no more, no fewer.
	if got := h.Draws(); got != uint64(bigRecords-4096) {
		t.Fatalf("headroom_draws counted %d records, want the %d appended", got, bigRecords-4096)
	}
}
