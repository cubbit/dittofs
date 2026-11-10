package datapath

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// syncWhileHeld arms a hold in the gap between a write's append and its
// publish. When the held write reaches the gap, the hold starts a sync of f
// through the durability layer and waits until that sync has been issued and
// counted, then lets the write publish. The sync's worker applies what it
// learned only once the write releases the file's append domain.
//
// If failing is set, the sync of every segment the stream may be on fails.
// arm must be called before the write under test; the returned wait collects
// the Sync call's error after it.
type syncWhileHeld struct {
	c       *Core
	f       core.FileID
	failing bool
	seam    *kit.Seam

	// nth is the call of the hold, counted from arm, that starts the sync: 0
	// or 1 is the first append of the armed write.
	nth int
	// failFirst makes the first append of the armed write start a sync that
	// fails, of segment failSeg when set and of any of the first sixteen
	// otherwise, before the nth one starts the one that does not.
	failFirst bool
	failSeg   core.SegmentID

	mu    sync.Mutex
	armed bool
	calls int
	done  chan error
	// gate, once set, makes every later Lock of a file's domain wait for it
	// to close, so the worker applies the sync only after the test has looked
	// at what the write alone left.
	gate     atomic.Pointer[chan struct{}]
	openOnce sync.Once
}

// Lock lets the core's own domain table through unless the gate is closed
// over it.
type gatedDomains struct {
	durability.Domains
	h *syncWhileHeld
}

func (g gatedDomains) Lock(id core.FileID) func() {
	if ch := g.h.gate.Load(); ch != nil {
		<-*ch
	}
	return g.Domains.Lock(id)
}

// newHeld builds a core whose writes call h's hold, with the gate in place.
func newHeld(t *testing.T, failing bool, extra ...func(*coreOptions)) (*syncWhileHeld, *Core) {
	t.Helper()
	h := &syncWhileHeld{failing: failing}
	opts := append([]func(*coreOptions){func(o *coreOptions) {
		o.afterAppend = h.hook
		o.wrapDomains = func(d durability.Domains) durability.Domains { return gatedDomains{d, h} }
	}}, extra...)
	c, seam := newTestCore(t, opts...)
	h.c, h.seam = c, seam
	// Runs before the core's own cleanup, so a test that fails with the gate
	// closed does not leave the worker stuck behind it.
	t.Cleanup(h.open)
	return h, c
}

// open lets the worker take the file's domain again.
func (h *syncWhileHeld) open() {
	h.openOnce.Do(func() {
		if ch := h.gate.Load(); ch != nil {
			close(*ch)
		}
	})
}

func (h *syncWhileHeld) hook() {
	h.mu.Lock()
	armed := h.armed
	h.calls++
	first := armed && h.failFirst && h.calls == 1
	fire := armed && !first && h.calls >= max(h.nth, 1)
	if fire {
		h.armed = false
	}
	h.mu.Unlock()
	switch {
	case first:
		h.syncNow(true)
	case fire && h.failFirst:
		// A Sync naming a file with a failed record answers at once with the
		// failure, so the second sync is the stream's own.
		if _, _, err := h.c.Streams().For(h.f).SyncSegment(); err != nil {
			panic(err)
		}
	case fire:
		h.syncNow(h.failing)
	}
}

// syncNow starts a sync of the file and waits until the stream's worker has
// counted it, failed or not. The gate goes up first.
func (h *syncWhileHeld) syncNow(fail bool) {
	if h.gate.Load() == nil {
		g := make(chan struct{})
		h.gate.Store(&g)
	}
	before := h.c.Durability().Stats()
	if fail {
		var rules []kit.Rule
		for id := core.SegmentID(1); id <= 16; id++ {
			if h.failSeg != 0 && id != h.failSeg {
				continue
			}
			rules = append(rules, kit.Failing(kit.OpSync, format.SegmentName(id), 1, errors.New("injected sync failure")))
			// The failed segment's pages do not read back either, so its
			// window cannot re-append the records from them and fails them.
			rules = append(rules, kit.Failing(kit.OpRead, format.SegmentName(id), 0, errors.New("injected read failure")))
		}
		if err := h.seam.Schedule(rules...); err != nil {
			panic(err)
		}
	}
	h.done = make(chan error, 1)
	done := h.done
	go func() { done <- h.c.Durability().Sync(context.Background(), h.f) }()
	// The sync runs on the stream's worker, which needs no lock this
	// goroutine holds; the deadline only bounds a failure.
	for deadline := time.Now().Add(5 * time.Second); ; runtime.Gosched() {
		st := h.c.Durability().Stats()
		if st.Syncs > before.Syncs || st.SyncFailures > before.SyncFailures {
			return
		}
		if time.Now().After(deadline) {
			panic("the held write's sync was never issued")
		}
	}
}

func (h *syncWhileHeld) arm() {
	h.mu.Lock()
	h.armed, h.calls = true, 0
	h.mu.Unlock()
}

func retainedBytes(c *Core, f core.FileID) int64 {
	var n int64
	for _, r := range c.Index().Retained(f) {
		n += r.Extent.Len
	}
	return n
}

// Content a newer write supersedes stays retained while the newer record is
// unsynced, and is released once the sync lands.
func TestWriteAt_SupersededIsRetainedUntilSynced(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	a := mustWrite(t, c, s, f, 0, make([]byte, 4096))
	b := mustWrite(t, c, s, f, 0, make([]byte, 4096))

	ret := c.Index().Retained(f)
	if len(ret) != 1 || ret[0].Version != a || ret[0].By != b || ret[0].Extent != (core.Extent{Off: 0, Len: 4096}) {
		t.Fatalf("Retained = %+v, want A's extent at version %v superseded by %v", ret, a, b)
	}
	if _, _, r := c.heldBytes(); r != 4096 {
		t.Fatalf("retained counter = %d, want 4096", r)
	}

	syncFiles(t, c, f)

	if ret := c.Index().Retained(f); len(ret) != 0 {
		t.Fatalf("Retained after the sync = %+v, want none", ret)
	}
	if _, _, r := c.heldBytes(); r != 0 {
		t.Fatalf("retained counter after the sync = %d, want 0", r)
	}
}

// A sync that completes after a write's append and before its publish, or
// fails there, is applied once the write has released the file: a completed
// one leaves nothing retained, a failed one leaves the write's own extent
// unheld and a loss record for it.
func TestWriteAt_SyncCompletesBetweenAppendAndPublish(t *testing.T) {
	t.Run("completes", func(t *testing.T) {
		h, c := newHeld(t, false)
		s := mustShare(t, c, 1)
		f := testFile(2)
		h.f = f
		mustWrite(t, c, s, f, 0, make([]byte, 4096))
		h.arm()
		b := mustWrite(t, c, s, f, 0, make([]byte, 4096))
		// The worker is still waiting for the file's domain, so what is
		// retained now is what the write alone left.
		if ret := c.Index().Retained(f); len(ret) != 0 {
			t.Fatalf("Retained after WriteAt returned = %+v, want none", ret)
		}
		if p, ok := c.Index().Lookup(f, 0); !ok || p.Version != b {
			t.Fatalf("Lookup = %+v, %v; want the write's piece at %v", p, ok, b)
		}
		if held, _, r := c.heldBytes(); held != 4096 || r != 0 {
			t.Fatalf("held/retained = %d/%d, want 4096/0", held, r)
		}
		h.open()
		if err := <-h.done; err != nil {
			t.Fatalf("Sync: %v", err)
		}
	})
	t.Run("fails", func(t *testing.T) {
		h, c := newHeld(t, true)
		s := mustShare(t, c, 1)
		f := testFile(3)
		h.f = f
		a := mustWrite(t, c, s, f, 0, make([]byte, 4096))
		syncFiles(t, c, f)
		h.arm()
		b := mustWrite(t, c, s, f, 0, make([]byte, 4096))
		// The failure is applied only once the worker has the domain, which
		// is after the publish. Its sync did not complete, so the write has
		// not lifted what it superseded: A is still retained.
		if ret := c.Index().Retained(f); len(ret) != 1 || ret[0].Version != a {
			t.Fatalf("Retained after WriteAt returned = %+v, want A's extent at %v", ret, a)
		}
		h.open()
		if err := <-h.done; err == nil {
			t.Fatal("Sync returned no error although its sync failed")
		}

		eventually(t, "A to be served again", func() bool {
			p, ok := c.Index().Lookup(f, 0)
			return ok && p.Version == a
		})
		if ret := c.Index().Retained(f); len(ret) != 0 {
			t.Fatalf("Retained = %+v, want none", ret)
		}
		var lost bool
		for _, ev := range c.Durability().Stats().Losses {
			if ev.File == f && ev.Version == b && ev.Reason == core.LossFailedWindow {
				lost = true
			}
		}
		if !lost {
			t.Fatalf("no loss record for the failed write at %v: %+v", b, c.Durability().Stats().Losses)
		}
		if held, _, _ := c.heldBytes(); held != 4096 {
			t.Fatalf("held = %d after the failed record was dropped, want A's 4096", held)
		}
	})
}

// pieceLoc is the ledger location of the held piece of f at off: where the
// index places it and the padded length of its record on disk.
func pieceLoc(t *testing.T, c *Core, seam *kit.Seam, f core.FileID, off int64) segment.Location {
	t.Helper()
	p, ok := c.Index().Lookup(f, off)
	if !ok {
		t.Fatalf("no held piece of %v at %d", f, off)
	}
	recs := fileWrites(t, seam, f)
	i := slices.IndexFunc(recs, func(r onDisk) bool {
		return r.Header.Offset == int64(p.Record.Offset) && r.Header.Extent.Off == p.Extent.Off
	})
	if i < 0 {
		t.Fatalf("no record on disk for piece %+v", p)
	}
	return segment.Location{Segment: p.Record.Segment, Off: recs[i].Header.Offset, Len: recs[i].Header.Size()}
}

// retainedIn is the retained bytes of f inside [lo, hi).
func retainedIn(c *Core, f core.FileID, lo, hi int64) int64 {
	var n int64
	for _, r := range c.Index().Retained(f) {
		n += max(0, min(r.Extent.Off+r.Extent.Len, hi)-max(r.Extent.Off, lo))
	}
	return n
}

// A write that spans segments gives every piece the same version, and a lift
// covers only the extent of the piece's own record. So a piece that is durable
// at its publish lifts what it superseded and nothing a sibling piece
// superseded: the content under an earlier piece whose sync failed stays
// retained, and is served again once that piece's window fails.
func TestWriteAt_DurablePieceLiftsOnlyItsOwnExtent(t *testing.T) {
	h, c := newHeld(t, false)
	h.nth, h.failFirst = 2, true
	s := mustShare(t, c, 1)
	f := testFile(7)
	h.f = f
	size := 2*testSegmentCap + 1000
	a := mustWrite(t, c, s, f, 0, make([]byte, size))
	syncFiles(t, c, f)
	// The next write's first piece lands in the segment A ended in, and the
	// sync that its hold starts for that piece fails; its second piece lands in
	// the next segment, and the hold's second sync of that one completes.
	last, ok := c.Index().Lookup(f, size-1)
	if !ok {
		t.Fatal("no held piece at the end of the first write")
	}
	h.failSeg = last.Record.Segment

	// The second write of the same size holds the three spares its pieces
	// roll into, taken from the pool before any byte moves; the refill
	// needs to have returned what the first write spent of them.
	for deadline := time.Now().Add(5 * time.Second); c.pool.ReadyContent() < 3; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pool never readied the three the next write holds: %+v / ready %d", c.pool.Counters(), c.pool.ReadyContent())
		}
	}

	h.arm()
	b := mustWrite(t, c, s, f, 0, make([]byte, size))
	// The sync of the second piece completed before it published; the first
	// piece's did not. The second piece lifted what it superseded, and the
	// first piece's content is still retained, whole.
	p1, ok := c.Index().Lookup(f, 0)
	if !ok || p1.Version != b {
		t.Fatalf("Lookup(0) = %+v, %v; want the first piece at %v", p1, ok, b)
	}
	p2, ok := c.Index().Lookup(f, p1.Extent.Len)
	if !ok || p2.Version != b || p2.Record.Segment == p1.Record.Segment {
		t.Fatalf("Lookup(%d) = %+v, %v; want a second piece at %v in another segment", p1.Extent.Len, p2, ok, b)
	}
	end2 := p2.Extent.Off + p2.Extent.Len
	if got := retainedIn(c, f, p1.Extent.Off, p1.Extent.Off+p1.Extent.Len); got != p1.Extent.Len {
		t.Fatalf("retained under the first piece = %d bytes, want its whole %d", got, p1.Extent.Len)
	}
	if got := retainedIn(c, f, p2.Extent.Off, end2); got != 0 {
		t.Fatalf("retained under the durable second piece = %d bytes, want none", got)
	}
	if got, want := retainedBytes(c, f), size-p2.Extent.Len; got != want {
		t.Fatalf("retained after WriteAt returned = %d bytes, want %d: everything but the second piece's", got, want)
	}
	h.open()
	// The Sync names the file whose first piece failed, so it reports it.
	if err := <-h.done; err == nil {
		t.Fatal("Sync returned no error although a piece of the file failed")
	}

	eventually(t, "the first piece's content to be served again", func() bool {
		p, ok := c.Index().Lookup(f, 0)
		return ok && p.Version == a
	})
	var lost bool
	for _, ev := range c.Durability().Stats().Losses {
		if ev.File == f && ev.Version == b && ev.Extent.Off == 0 {
			lost = true
		}
	}
	if !lost {
		t.Fatalf("no loss record for the first piece at %v: %+v", b, c.Durability().Stats().Losses)
	}
}

// Every piece of a write that spans segments is in the durability ledger. A
// location the ledger lacks reads as durable, so each piece is held non
// durable by blocking the sync of the segment it is in; a piece that was never
// tracked would read durable while its segment is held.
func TestWriteAt_EveryPieceReachesTheLedger(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(5)
	size := 2*testSegmentCap + 1000
	h1 := seam.Hold(t, kit.OpSync, format.SegmentName(1))
	h2 := seam.Hold(t, kit.OpSync, format.SegmentName(2))
	h3 := seam.Hold(t, kit.OpSync, format.SegmentName(3))

	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, size), 1)
		wrote <- err
	}()

	// The second piece's append rolls and so seals the first segment: the
	// write waits on that sync with only the first piece published.
	eventually(t, "the first segment's seal to wait", func() bool { return h1.Waiting() > 0 })
	loc1 := pieceLoc(t, c, seam, f, 0)
	if loc1.Segment != 1 {
		t.Fatalf("the first piece is in segment %d, want 1", loc1.Segment)
	}
	if c.Durability().Durable(loc1) {
		t.Fatalf("piece 1 %+v is durable while its segment's sync is held", loc1)
	}
	p1, _ := c.Index().Lookup(f, 0)
	h1.Release()

	eventually(t, "the second segment's seal to wait", func() bool { return h2.Waiting() > 0 })
	loc2 := pieceLoc(t, c, seam, f, p1.Extent.Len)
	if loc2.Segment != 2 {
		t.Fatalf("the second piece is in segment %d, want 2", loc2.Segment)
	}
	if c.Durability().Durable(loc2) {
		t.Fatalf("piece 2 %+v is durable while its segment's sync is held", loc2)
	}
	if !c.Durability().Durable(loc1) {
		t.Fatalf("piece 1 %+v is not durable after its segment's sync was released", loc1)
	}
	h2.Release()
	if err := <-wrote; err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	loc3 := pieceLoc(t, c, seam, f, size-1)
	if loc3.Segment != 3 {
		t.Fatalf("the last piece is in segment %d, want 3", loc3.Segment)
	}
	if c.Durability().Durable(loc3) {
		t.Fatalf("piece 3 %+v is durable while its segment's sync is held", loc3)
	}

	synced := make(chan error, 1)
	go func() { synced <- c.Durability().Sync(context.Background(), f) }()
	eventually(t, "the sync of the last segment to wait", func() bool { return h3.Waiting() > 0 })
	select {
	case err := <-synced:
		t.Fatalf("Sync returned (%v) while the last segment's sync is held", err)
	case <-time.After(50 * time.Millisecond):
	}
	h3.Release()
	if err := <-synced; err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, loc := range []segment.Location{loc1, loc2, loc3} {
		if !c.Durability().Durable(loc) {
			t.Fatalf("piece %+v is not durable after Sync returned", loc)
		}
		if err := c.Durability().WaitDurable(loc); err != nil {
			t.Fatalf("WaitDurable(%+v): %v", loc, err)
		}
	}
}

// The core's counters follow the index: the publish delta then the lift delta
// each equal the index's own change, and the final counters equal its totals.
func TestWriteAt_AccountSeesPublishAndLift(t *testing.T) {
	var mu sync.Mutex
	var seen []index.HeldDelta
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.accountSpy = func(d index.HeldDelta) { mu.Lock(); seen = append(seen, d); mu.Unlock() }
	})
	s := mustShare(t, c, 1)
	f := testFile(6)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	mu.Lock()
	seen = nil
	mu.Unlock()

	mustWrite(t, c, s, f, 1024, make([]byte, 1024))
	mu.Lock()
	publish := slices.Clone(seen)
	mu.Unlock()
	// Held grows by the new bytes less the 1024 they cover, dirty likewise, and
	// the covered 1024 become retained.
	if len(publish) != 1 || publish[0].File != f || publish[0].HeldBytes != 0 ||
		publish[0].DirtyBytes != 0 || publish[0].RetainedBytes != 1024 {
		t.Fatalf("publish deltas = %+v, want one of held/dirty/retained 0/0/1024", publish)
	}

	syncFiles(t, c, f)
	mu.Lock()
	after := slices.Clone(seen[len(publish):])
	mu.Unlock()
	var lift int64
	for _, d := range after {
		if d.HeldBytes != 0 || d.DirtyBytes != 0 {
			t.Fatalf("a delta after the sync moved held or dirty bytes: %+v", d)
		}
		lift += d.RetainedBytes
	}
	if len(after) == 0 || lift != -1024 {
		t.Fatalf("lift deltas = %+v, want retained changing by -1024 in all", after)
	}

	held, dirty, retained := c.heldBytes()
	var wantHeld, wantRetained int64
	for _, p := range heldPieces(t, c, f, 0, 4096) {
		wantHeld += p.Extent.Len
	}
	for _, r := range c.Index().Retained(f) {
		wantRetained += r.Extent.Len
	}
	if held != wantHeld || retained != wantRetained {
		t.Fatalf("counters held/retained = %d/%d, index totals %d/%d", held, retained, wantHeld, wantRetained)
	}
	if dirty < 0 {
		t.Fatalf("dirty = %d", dirty)
	}
}
