package capacity

import (
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

// hrFake is the injected facts of a Headroom: the set's footprint, a plain
// read. Its zero value answers "nothing used", and its Admit is always
// writable.
type hrFake struct {
	footprint atomic.Int64
}

func (f *hrFake) foot() int64  { return f.footprint.Load() }
func (f *hrFake) admit() error { return nil }

// parkAtLimit parks the rig at the journal's limit — the one state in which
// the headroom is drawn.
func (f *hrFake) parkAtLimit() { f.footprint.Store(1 << 40) }

// newFull rigs a Headroom of records records over a fresh fake: the journal
// sits far below its maximum.
func newFull(t *testing.T, records int) (*Headroom, *hrFake) {
	t.Helper()
	f := &hrFake{}
	h := NewHeadroom(HeadroomConfig{
		Records:    records,
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
		got, err := h.Admit(c)
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
		got, err := h.Admit(c)
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
		_, err := h.Admit(c)
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
		got, err := h.Admit(c)
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
	got, err := h.Admit(Offloaded)
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
	_, err = h.Admit(Release)
	noSpaceOrFatal(t, err)
	o, err := h.Admit(Open)
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
	if _, err := h.Admit(Delete); err != nil {
		t.Fatalf("with one record over the reserve a removal was refused: %v", err)
	}

	// Exactly the reserve: a removal is refused, an Open is admitted.
	h2, f2 := newFull(t, bigRecords)
	f2.parkAtLimit()
	drawWith(t, h2, Delete, bigRecords-4096)
	_, err := h2.Admit(Delete)
	noSpaceOrFatal(t, err)
	o, err := h2.Admit(Open)
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
	_, err := h.Admit(Hold)
	noSpaceOrFatal(t, err)
	_, err = h.Admit(Delete)
	noSpaceOrFatal(t, err)
	if h.Draws() != drawsBefore {
		t.Fatalf("refused admissions moved headroom_draws from %d to %d", drawsBefore, h.Draws())
	}
	// The refusals drew nothing: an Open still finds the whole reserve.
	if _, err := h.Admit(Open); err != nil {
		t.Fatalf("a refusal drew on the open reserve anyway, an Open was then refused: %v", err)
	}
}

func TestHeadroomDrawsOnlyAtLimit(t *testing.T) {
	// The journal is at its limit when the footprint plus a segment cap
	// would pass the maximum; a record appended otherwise moves nothing.
	h, f := newFull(t, 1024)
	below, err := h.Admit(Release)
	if err != nil {
		t.Fatalf("the below-limit admission was refused: %v", err)
	}
	h.Drawn(below)
	if got := h.Draws(); got != 0 {
		t.Fatalf("a record appended below the limit moved headroom_draws to %d, want 0", got)
	}

	f.footprint.Store(1 << 40) // the maximum: any roll would pass it
	at, err := h.Admit(Release)
	if err != nil {
		t.Fatalf("the at-limit admission was refused: %v", err)
	}
	mid, err := h.Admit(Truncate)
	if err != nil {
		t.Fatalf("the at-limit truncate admission was refused: %v", err)
	}
	f.footprint.Store(0) // the journal leaves the limit between the admission and the landing
	h.Drawn(mid)
	h.Drawn(at)
	if got := h.Draws(); got != 2 {
		t.Fatalf("records drawn at the limit moved headroom_draws to %d, want 2", got)
	}

	below2, _ := h.Admit(Release)
	h.Drawn(below2)
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
	_, err := h.Admit(Delete)
	noSpaceOrFatal(t, err)

	drawsBefore := h.Draws()
	got, err := h.Admit(RepackCopy)
	if err != nil {
		t.Fatalf("a repack copy was refused: %v", err)
	}
	if !got.Append {
		t.Fatalf("a repack copy was told not to append")
	}
	h.Drawn(got)
	if h.Draws() != drawsBefore {
		t.Fatalf("the repack copy moved headroom_draws from %d to %d", drawsBefore, h.Draws())
	}
}

func TestAdmitThenDrawnChargesOneRecord(t *testing.T) {
	// Admit, then Drawn once the record is appended — the sequence a
	// header-only appender will run — must charge the record once, not twice: the free side a
	// later Admit reads moves by one record charge per appended record.
	// The journal sits at its limit the whole time, so every Drawn runs
	// its counter path and every appended record counts in headroom_draws.
	h, f := newFull(t, bigRecords)
	f.footprint.Store(1 << 40)
	for i := 0; i < bigRecords-4097; i++ {
		d, err := h.Admit(Release)
		if err != nil {
			t.Fatalf("draw %d toward the reserve edge failed: %v", i, err)
		}
		h.Drawn(d)
	}
	// reserve+1 records remain: the next removal is admitted and consumes
	// the last drawing-side record; the one after is refused.
	last, err := h.Admit(Release)
	if err != nil {
		t.Fatalf("with one record over the reserve a removal was refused: %v", err)
	}
	h.Drawn(last)
	if _, err := h.Admit(Release); !errors.Is(err, core.ErrNoSpace) {
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
		got, err := h.Admit(Release)
		if err != nil {
			t.Fatalf("admission %d below the limit was refused: %v", i+1, err)
		}
		if !got.Append {
			t.Fatalf("admission %d below the limit was told not to append", i+1)
		}
		h.Drawn(got)
	}
	if got := h.Draws(); got != 0 {
		t.Fatalf("records appended below the limit moved headroom_draws to %d, want 0", got)
	}

	f.parkAtLimit()
	for i := 0; i < bigRecords-4096; i++ {
		d, err := h.Admit(Release)
		if err != nil {
			t.Fatalf("draw %d at the limit toward the reserve edge failed: %v", i, err)
		}
		h.Drawn(d)
	}
	_, err := h.Admit(Release)
	noSpaceOrFatal(t, err)
	if got := h.Draws(); got != uint64(bigRecords-4096) {
		t.Fatalf("headroom_draws counted %d, want exactly the %d records appended at the limit", got, bigRecords-4096)
	}
}

func TestMeasureIsTheLedgerAlone(t *testing.T) {
	// The measure is the configured size less the records drawn: nothing a
	// pool reports moves it, so the headroom spare a header-only roll took
	// leaves the edge where it was — a removal admitted over the reserve,
	// refused at it, an Open admitted after.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4096-1)
	if _, err := h.Admit(Delete); err != nil {
		t.Fatalf("one record over the reserve a removal was refused: %v", err)
	}
	_, err := h.Admit(Delete)
	noSpaceOrFatal(t, err)
	if _, err := h.Admit(Open); err != nil {
		t.Fatalf("with the reserve left an Open was refused: %v", err)
	}
}

func TestNoRecordsAdmitsEveryClass(t *testing.T) {
	// A headroom of no records has nothing to measure: every class is
	// admitted at the limit, draws nothing and throttles no write.
	h, f := newFull(t, 0)
	f.parkAtLimit()
	for c := Release; c <= RepackCopy; c++ {
		d, err := h.Admit(c)
		if err != nil || !d.Append {
			t.Fatalf("%s with no records = %+v, %v; want admitted", c, d, err)
		}
		h.Drawn(d)
	}
	if h.Draws() != 0 || h.BelowHalf() {
		t.Fatalf("draws = %d, below half = %v; want none", h.Draws(), h.BelowHalf())
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
				got, err := h.Admit(Delete)
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
	// back — and only that admission's record — so the same count of
	// admissions fits again after the whole drawing side was drawn.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4097) // one record short of exhaustion

	// The last admission's append failed.
	last, err := h.Admit(Release)
	if err != nil {
		t.Fatalf("the last admission over the reserve edge was refused: %v", err)
	}
	h.Refund(last)
	if _, err := h.Admit(Release); err != nil {
		t.Fatalf("after a refund of the failed append's draw, the removal was still refused: %v", err)
	}
	// And the headroom_draws counter still counts what actually appended:
	// the refunded record never landed, so it was never counted.
	if got := h.Draws(); got != 0 {
		t.Fatalf("the refund moved headroom_draws to %d, want 0", got)
	}
}

func TestRefundAfterNoDrawIsNoop(t *testing.T) {
	h, f := newFull(t, bigRecords)

	// Below the limit nothing drew: a refund returns nothing, and the
	// drawing side stays whole for the later at-limit run.
	below, err := h.Admit(Release)
	if err != nil {
		t.Fatalf("the below-limit admission was refused: %v", err)
	}
	h.Refund(below)
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4096)
	_, err = h.Admit(Release)
	noSpaceOrFatal(t, err)

	// A repack copy drew on the repack reserve: nothing to refund.
	rc, err := h.Admit(RepackCopy)
	if err != nil {
		t.Fatalf("the repack copy was refused: %v", err)
	}
	h.Refund(rc)
	if _, err := h.Admit(Release); err == nil {
		t.Fatalf("refunding a repack copy returned a headroom record anyway")
	}

	// A refusal drew nothing — below half a Hold was refused — and an
	// Offloaded record below half was told not to append at all.
	h2, f2 := newFull(t, bigRecords)
	f2.parkAtLimit()
	drawWith(t, h2, Release, 2049)
	_, err = h2.Admit(Hold)
	noSpaceOrFatal(t, err)
	off, err := h2.Admit(Offloaded)
	if err != nil {
		t.Fatalf("below half, the Offloaded record was refused: %v", err)
	}
	if off.Append {
		t.Fatalf("below half, the Offloaded record was told to append")
	}
	h2.Refund(off) // a Draw never drawn: no-op by its token
	// The refunds returned nothing: the drawing side is still one record
	// past half, so a removal is admitted and the half refusal stands for
	// the class that frees nothing.
	if _, err := h2.Admit(Hold); err == nil {
		t.Fatalf("the refunds returned records the admissions never drew")
	}
	if _, err := h2.Admit(Release); err != nil {
		t.Fatalf("a refund of a refused admission corrupted the drawing side: %v", err)
	}
}

func TestRefundAtTheOpenReserveEdge(t *testing.T) {
	// An Open's draw on its reserve is refundable like any other: a failed
	// open loss record hands its record back. The token a Draw carries is
	// the fact that its admission drew, not a one-use right: refunding it
	// twice returns the record twice, so pairing one Refund with one failed
	// append is the caller's to keep — the clamp at zero is the only wall
	// the headroom holds.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Delete, bigRecords-4096) // the whole drawing side
	lastOpen, err := h.Admit(Open)
	if err != nil {
		t.Fatalf("the first Open on the reserve was refused: %v", err)
	}
	h.Refund(lastOpen)
	if _, err := h.Admit(Open); err != nil {
		t.Fatalf("a failed open record's draw was not refunded: %v", err)
	}
	h.Refund(lastOpen) // replayed: returns the same record again
	if _, err := h.Admit(Open); err != nil {
		t.Fatalf("a replayed refund returned nothing, want the record back once more")
	}
}

func TestRefundOfABelowLimitAdmissionStealsNoDraw(t *testing.T) {
	// A below-limit admission drew nothing, so its failed append has
	// nothing to refund — even once the journal has reached the limit and
	// other callers hold real draws: the refund must not return one of
	// theirs.
	h, f := newFull(t, bigRecords)
	below, err := h.Admit(Release)
	if err != nil {
		t.Fatalf("the below-limit admission was refused: %v", err)
	}
	f.parkAtLimit()
	drawWith(t, h, Release, bigRecords-4096) // other callers draw the side
	_, err = h.Admit(Release)
	noSpaceOrFatal(t, err)
	h.Refund(below) // the journal reached the limit in between: still its own draw token, drew nothing
	if _, err := h.Admit(Release); err == nil {
		t.Fatalf("the refund of an admission that drew nothing returned another caller's draw")
	}
}

func TestBelowHalfGatedAtTheLimit(t *testing.T) {
	// Below half is a state of the journal at its limit: headroom drawn in
	// a past episode must not throttle content writes once the journal left
	// the limit, and must re-arm the moment it is back.
	h, f := newFull(t, bigRecords)
	f.parkAtLimit()
	drawWith(t, h, Release, 3000) // half is 2,048: well below
	if !h.BelowHalf() {
		t.Fatalf("at the limit and below half, BelowHalf reads false")
	}
	f.footprint.Store(0)
	if h.BelowHalf() {
		t.Fatalf("below the limit, BelowHalf still reads true, throttling content writes from a past episode")
	}
	f.parkAtLimit()
	if !h.BelowHalf() {
		t.Fatalf("back at the limit, BelowHalf reads false over the same drawn figure")
	}
}
