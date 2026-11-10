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
// fixed 4,096 records, its drawing side holds 4,096 and the half rule falls
// at 2,048 records drawn.
const bigRecords = 8192

// hrFake is the injected facts of a Headroom: the pool's headroom bytes and
// the set's footprint, each a plain read. Its zero value answers "no
// headroom byte, nothing used", and its Admit is always writable.
type hrFake struct {
	headroomBytes atomic.Int64
	footprint     atomic.Int64
}

func (f *hrFake) pool() int64  { return f.headroomBytes.Load() }
func (f *hrFake) foot() int64  { return f.footprint.Load() }
func (f *hrFake) admit() error { return nil }

// parkAtLimit parks the rig at the journal's limit — the one state in which
// the headroom is drawn.
func (f *hrFake) parkAtLimit() { f.footprint.Store(1 << 40) }

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
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
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
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
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
	// last drawing-side record) beside an Open.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Delete, bigRecords-4096-1)
	if _, err := h.Admit(context.Background(), Delete); err != nil {
		t.Fatalf("with one record over the reserve a removal was refused: %v", err)
	}

	// Exactly the reserve: a removal is refused, an Open is admitted.
	h2, f2 := newFull(t, bigRecords)
	f2.parkAtLimit()
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
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
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
	// would pass the maximum; a record appended otherwise moves nothing.
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
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4096)
	_, err := h.Admit(context.Background(), Delete)
	noSpaceOrFatal(t, err)

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

func TestAdmitBelowTheLimit(t *testing.T) {
	// Below the limit a record without bytes lands in a content spare, so
	// the headroom draws nothing and counts nothing — however many land —
	// and the whole drawing side remains once the journal reaches the
	// limit, where the refusal arrives after the exact expected count.
	h, f := newFull(t, bigRecords)
	for i := 0; i < 5000; i++ {
		got, err := h.Admit(context.Background(), Release)
		if err != nil {
			t.Fatalf("admission %d below the limit was refused: %v", i+1, err)
		}
		if !got.Append {
			t.Fatalf("admission %d below the limit was told not to append", i+1)
		}
		h.Drawn(Release)
	}
	if got := h.Draws(); got != 0 {
		t.Fatalf("records appended below the limit moved headroom_draws to %d, want 0", got)
	}

	f.parkAtLimit()
	for i := 0; i < bigRecords-4096; i++ {
		if _, err := h.Admit(context.Background(), Release); err != nil {
			t.Fatalf("draw %d at the limit toward the reserve edge failed: %v", i, err)
		}
		h.Drawn(Release)
	}
	_, err := h.Admit(context.Background(), Release)
	noSpaceOrFatal(t, err)
	if got := h.Draws(); got != uint64(bigRecords-4096) {
		t.Fatalf("headroom_draws counted %d, want exactly the %d records appended at the limit", got, bigRecords-4096)
	}
}

func TestEmptyPool(t *testing.T) {
	// An empty pool at the limit names a headroom of nothing: every class
	// is refused — an Open included, for the reserve holds no record
	// either — but a repack copy, whose reserve is elsewhere, is not.
	h, f := newFull(t, bigRecords)
	f.headroomBytes.Store(0)
	f.parkAtLimit()
	for _, c := range []RecordClass{
		Release, Truncate, Deallocate, Delete, CloneTarget, Offloaded,
		Unmark, Loss, Hold, Unhold, Stamp, Forget, Reconciled, Open,
	} {
		_, err := h.Admit(context.Background(), c)
		noSpaceOrFatal(t, err)
	}
	if _, err := h.Admit(context.Background(), RepackCopy); err != nil {
		t.Fatalf("beside an empty headroom a repack copy was refused: %v", err)
	}
}

func TestPoolShortOfConfigured(t *testing.T) {
	// The size is the smaller of the pool's bytes and the configured
	// records: a pool holding only 4,200 records' bytes leaves a drawing
	// side of 104 — not the configured 4,096 — whatever Records says.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	f.headroomBytes.Store(4200 * RecordCharge)
	drawWith(t, h, Release, 4200-OpenReserveRecords(bigRecords))
	_, err := h.Admit(context.Background(), Release)
	noSpaceOrFatal(t, err)
	// The open reserve of the configured size still holds: an Open is
	// admitted beside the drawing side the pool fell short of.
	if _, err := h.Admit(context.Background(), Open); err != nil {
		t.Fatalf("with the pool short and the reserve left an Open was refused: %v", err)
	}
}

func TestConcurrentAdmitNeverOverdraws(t *testing.T) {
	// A small headroom at the limit, so the race window is wide: every
	// goroutine races to draw the few records beside the reserve, and the
	// admissions that land are exactly the drawing side — no more, and no
	// fewer, for a class that refuses what it should draw is as wrong as
	// one that draws what it should refuse.
	records := 64
	h, f := newFull(t, records)
	f.parkAtLimit()

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
	if got := admitted.Load(); got != drawingSide {
		t.Fatalf("%d concurrent admissions landed, want exactly the %d records beside the reserve", got, drawingSide)
	}
}

func TestRefundRestoresOneRecord(t *testing.T) {
	// The append an admission drew for failed: Refund hands the record
	// back, and the same count of admissions fits again after the whole
	// drawing side has been drawn and refunded once.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4096)

	// The last admission's append failed; the one beside it, withdrew.
	h.Refund(Release)
	if _, err := h.Admit(context.Background(), Release); err != nil {
		t.Fatalf("after a refund of the failed append's draw, the removal was still refused: %v", err)
	}
	// And the headroom_draws counter still counts what actually appended:
	// the refunded record never landed, so it was never drawn here.
	if got := h.Draws(); got != 0 {
		t.Fatalf("the refund moved headroom_draws to %d, want 0", got)
	}
}

func TestRefundAfterNoDrawIsNoop(t *testing.T) {
	h, f := newFull(t, bigRecords)

	// Below the limit nothing drew: a refund returns nothing, and the
	// drawing side stays whole for the later at-limit run.
	h.Refund(Release)
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4096)
	for i := 0; i < bigRecords-4096; i++ {
		h.Drawn(Release) // each record appended: its draw is settled
	}
	_, err := h.Admit(context.Background(), Release)
	noSpaceOrFatal(t, err)

	// A repack copy drew on the repack reserve: nothing to refund.
	h.Refund(RepackCopy)
	if _, err := h.Admit(context.Background(), Release); err == nil {
		t.Fatalf("refunding a repack copy returned a headroom record anyway")
	}

	// A refusal drew nothing — below half a Hold was refused — and an
	// Offloaded record below half was told not to append at all.
	h2, f2 := newFull(t, bigRecords)
	f2.parkAtLimit()
	drawWith(t, h2, Release, 2049)
	for i := 0; i < 2049; i++ {
		h2.Drawn(Release)
	}
	_, err = h2.Admit(context.Background(), Hold)
	noSpaceOrFatal(t, err)
	h2.Refund(Hold)
	h2.Refund(Offloaded)
	// Both refunds returned nothing: the drawing side is still one record
	// past half, so a removal is admitted and the half refusal stands for
	// the class that frees nothing.
	if _, err := h2.Admit(context.Background(), Hold); err == nil {
		t.Fatalf("the refunds returned records the admissions never drew")
	}
	if _, err := h2.Admit(context.Background(), Release); err != nil {
		t.Fatalf("a refund of a refused admission corrupted the drawing side: %v", err)
	}
}

func TestRefundAtTheOpenReserveEdge(t *testing.T) {
	// An Open's draw on its reserve is refundable like any other: a failed
	// open loss record hands its reserve record back, and a refund past
	// every draw clamps at zero.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Delete, bigRecords-4096) // the whole drawing side
	drawWith(t, h, Open, 4096)              // the whole open reserve
	h.Refund(Open)
	if _, err := h.Admit(context.Background(), Open); err != nil {
		t.Fatalf("a failed open record's draw was not refunded: %v", err)
	}
	h.Refund(Open) // settled: nothing outstanding; must clamp, not go below zero
	h.Refund(Open)
	if _, err := h.Admit(context.Background(), Open); err != nil {
		t.Fatalf("a refund past the settled draws returned headroom anyway: %v", err)
	}
}
