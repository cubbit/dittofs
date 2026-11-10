package datapath

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

func ver(n uint64) core.Version { return core.Version{Counter: n} }

func ff(n int) []byte { return bytes.Repeat([]byte{0xFF}, n) }

// readFilled reads n bytes of f at off.
func readFilled(t *testing.T, c *Core, s *Share, f core.FileID, off int64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, _, _, _, err := c.ReadAt(context.Background(), s, f, off, buf); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return buf
}

// fillRecords returns the fill records on disk.
func fillRecords(t *testing.T, seam *kit.Seam) []onDisk {
	t.Helper()
	var out []onDisk
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == format.KindFill {
			out = append(out, r)
		}
	}
	return out
}

// offloadedAt reports the offloaded bit of the piece held at off.
func offloadedAt(t *testing.T, c *Core, f core.FileID, off int64) bool {
	t.Helper()
	for _, e := range c.Index().FileEntries(f) {
		if e.Kind == index.EntryHeld && e.Extent.Off <= off && off < e.Extent.Off+e.Extent.Len {
			return e.Offloaded
		}
	}
	t.Fatalf("no piece held at %d: %+v", off, c.Index().FileEntries(f))
	return false
}

func TestFillWritesOnlyAbsentParts(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{'a'}, 4096))
	mustWrite(t, c, s, f, 8192, bytes.Repeat([]byte{'b'}, 4096))

	if err := c.Fill(context.Background(), s, f, 0, ff(16384), c.Index().AsOf(f), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	want := slices2(bytes.Repeat([]byte{'a'}, 4096), ff(4096), bytes.Repeat([]byte{'b'}, 4096), ff(4096))
	if got := readFilled(t, c, s, f, 0, 16384); !bytes.Equal(got, want) {
		t.Fatal("the read back is not the held bytes with the gaps filled")
	}
	for off, offloaded := range map[int64]bool{0: false, 4096: true, 8192: false, 12288: true} {
		if got := offloadedAt(t, c, f, off); got != offloaded {
			t.Fatalf("offloaded bit at %d = %v, want %v", off, got, offloaded)
		}
	}
	if got := c.FilledBytes(); got != 8192 {
		t.Fatalf("FilledBytes = %d, want 8192", got)
	}
}

func slices2(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// A fill's charge for the gaps next to two held pieces of one record matches
// what it adds: the index gains one entry per gap.
func TestFillChargesGapsAdjacentToHeldPiece(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(2)
	// One record, cut into two held pieces by a loss between them.
	mustWrite(t, c, s, f, 4096, bytes.Repeat([]byte{'a'}, 16384))
	if _, err := c.Index().Lose(f, core.Extent{Off: 8192, Len: 8192}, ver(9)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	before := len(c.Index().FileEntries(f))
	if err := c.Fill(context.Background(), s, f, 0, ff(24576), c.Index().AsOf(f), ver(10)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	// The gaps are [0,4096), [8192,16384) and [20480,24576): one entry each.
	if added := len(c.Index().FileEntries(f)) - before; added != 3 {
		t.Fatalf("the index gained %d entries, want 3, one per gap", added)
	}
}

// A removal marker above v masks its part: that part is neither appended nor held.
func TestFillMaskedByNewerMarker(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(3)
	mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{'a'}, 1024))
	if _, err := c.Index().Lose(f, core.Extent{Off: 4096, Len: 4096}, ver(9)); err != nil {
		t.Fatalf("Lose: %v", err)
	}
	if err := c.Fill(context.Background(), s, f, 0, ff(12288), c.Index().AsOf(f), ver(2)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if _, missing := c.Index().Span(f, core.Extent{Off: 4096, Len: 4096}); len(missing) != 1 || missing[0].Len != 4096 {
		t.Fatalf("the masked part is held: missing = %v", missing)
	}
	var appended int64
	for _, r := range fillRecords(t, seam) {
		appended += r.Header.Extent.Len
	}
	// The gaps are [1024,4096) and [8192,12288).
	if appended != 3072+4096 {
		t.Fatalf("fill records carry %d bytes, want 7168", appended)
	}
}

func TestFillStale(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(4)
	mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{'a'}, 1024)) // the file's change version is now > 0
	before := len(readRecords(t, seam))

	err := c.Fill(context.Background(), s, f, 0, ff(4096), ver(0), ver(1))
	if !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill below the change version = %v, want ErrStaleFill", err)
	}
	// A file with no entry is checked against its bucket.
	c.Index().RaiseFloor(ver(7))
	g := testFile(5)
	if err := c.Fill(context.Background(), s, g, 0, ff(4096), ver(2), ver(2)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill below the floor = %v, want ErrStaleFill", err)
	}
	// Fully held, still checked.
	if err := c.Fill(context.Background(), s, f, 0, ff(1024), ver(0), ver(1)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill of a held extent below the change version = %v, want ErrStaleFill", err)
	}
	if got := len(readRecords(t, seam)); got != before {
		t.Fatalf("records on disk went %d -> %d across refused fills", before, got)
	}
	if got := c.FillRefused(); got != 3 {
		t.Fatalf("FillRefused = %d, want 3", got)
	}
}

func TestFillSetsBitAndWriteClearsIt(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(6)
	if err := c.Fill(context.Background(), s, f, 0, ff(4096), c.Index().AsOf(f), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if !offloadedAt(t, c, f, 0) {
		t.Fatal("a filled piece is not offloaded")
	}
	mustWrite(t, c, s, f, 1024, bytes.Repeat([]byte{'w'}, 1024))
	if offloadedAt(t, c, f, 1024) {
		t.Fatal("a write over a filled piece left the offloaded bit set")
	}
	if !offloadedAt(t, c, f, 0) || !offloadedAt(t, c, f, 3000) {
		t.Fatal("the untouched sides of the filled piece lost their offloaded bit")
	}
}

func TestFillDrawsNoVersion(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(7)
	mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{'a'}, 1024))
	asOf := c.Index().AsOf(f)
	drawn := c.counters.NextVersion()
	_, seq0 := lastSeq(t, seam)

	if err := c.Fill(context.Background(), s, f, 1024, ff(1024), asOf, ver(5)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if got := c.Index().AsOf(f); got != asOf {
		t.Fatalf("AsOf moved %v -> %v", asOf, got)
	}
	if got := c.counters.NextVersion(); got != drawn {
		t.Fatalf("the version counter moved %v -> %v", drawn, got)
	}
	recs := fillRecords(t, seam)
	if len(recs) != 1 || recs[0].Header.Version != ver(5) || recs[0].Header.Seq <= seq0 {
		t.Fatalf("fill records = %+v, want one at version 5 with seq above %d", recs, seq0)
	}
}

func lastSeq(t *testing.T, seam *kit.Seam) (int, core.Seq) {
	t.Helper()
	recs := readRecords(t, seam)
	return len(recs), recs[len(recs)-1].Header.Seq
}

func TestFillOtherShareRefusedFirst(t *testing.T) {
	c, _ := newTestCore(t)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f := testFile(8)
	mustWrite(t, c, s1, f, 0, bytes.Repeat([]byte{'a'}, 1024))
	for _, tc := range []struct {
		name string
		off  int64
		p    []byte
	}{{"ok", 0, ff(10)}, {"empty", 0, nil}, {"negative", -1, ff(10)}} {
		if err := c.Fill(context.Background(), s2, f, tc.off, tc.p, ver(0), ver(1)); !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("%s: Fill through another share = %v, want ErrOtherShare", tc.name, err)
		}
	}
}

func TestFillZeroLengthAndInvalidExtent(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(9)
	ctx := context.Background()
	if err := c.Fill(ctx, s, f, 0, nil, ver(0), ver(1)); err != nil {
		t.Fatalf("zero-length Fill = %v, want nil", err)
	}
	if err := c.Fill(ctx, s, f, -1, ff(4), ver(0), ver(1)); !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("negative offset = %v, want ErrInvalidExtent", err)
	}
	if err := c.Fill(ctx, s, f, 1<<63-2, ff(4), ver(0), ver(1)); !errors.Is(err, core.ErrInvalidExtent) {
		t.Fatalf("overflowing extent = %v, want ErrInvalidExtent", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.Fill(cancelled, s, f, 0, ff(4), ver(0), ver(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Fill = %v, want context.Canceled", err)
	}
	if n := len(fillRecords(t, seam)); n != 0 {
		t.Fatalf("%d fill records appended by refused fills", n)
	}
	if got := c.Index().FileEntries(f); len(got) != 0 {
		t.Fatalf("refused fills left entries: %+v", got)
	}
}

func TestFillFullyHeldAppendsNothing(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(10)
	mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{'a'}, 4096))
	if err := c.Fill(context.Background(), s, f, 1024, ff(1024), c.Index().AsOf(f), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if n := len(fillRecords(t, seam)); n != 0 || c.FilledBytes() != 0 {
		t.Fatalf("a fully held fill appended %d records, counted %d bytes", n, c.FilledBytes())
	}
}

func TestFillSpansSegmentBoundary(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(11)
	size := int(2*testSegmentCap + 1000)
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i * 7)
	}
	if err := c.Fill(context.Background(), s, f, 0, p, c.Index().AsOf(f), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if got := readFilled(t, c, s, f, 0, size); !bytes.Equal(got, p) {
		t.Fatal("the bytes read back differ across the segment boundaries")
	}
	if got := c.FilledBytes(); got != uint64(size) {
		t.Fatalf("FilledBytes = %d, want %d", got, size)
	}
	if !offloadedAt(t, c, f, int64(size-1)) {
		t.Fatal("the last piece is not offloaded")
	}
}

func TestFillOpensSpan(t *testing.T) {
	var mu sync.Mutex
	var ops []core.SpanOp
	var ends []error
	tracer := func(ctx context.Context, op core.SpanOp, _ ...slog.Attr) (context.Context, func(error)) {
		mu.Lock()
		defer mu.Unlock()
		ops = append(ops, op)
		i := len(ends)
		ends = append(ends, nil)
		return ctx, func(err error) { mu.Lock(); ends[i] = err; mu.Unlock() }
	}
	c, _ := newTestCore(t, func(o *coreOptions) { o.tracer = tracer })
	s := mustShare(t, c, 1)
	f := testFile(12)
	if err := c.Fill(context.Background(), s, f, 0, ff(64), ver(0), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	// A stale fill's span ends with its refusal.
	c.Index().RaiseFloor(ver(5))
	if err := c.Fill(context.Background(), s, testFile(13), 0, ff(64), ver(0), ver(1)); !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("stale Fill = %v, want ErrStaleFill", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var spans []error
	for i, op := range ops {
		if op == core.SpanFill {
			spans = append(spans, ends[i])
		}
	}
	if len(spans) != 2 || spans[0] != nil || !errors.Is(spans[1], core.ErrStaleFill) {
		t.Fatalf("fill spans ended with %v, want nil then ErrStaleFill", spans)
	}
}

func TestFillReservesLikeAWrite(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	// A limit a fill of 4 KiB cannot fit: the refusal names the share's limit.
	s := mustLimitShare(t, c, 1, 1024)
	err := c.Fill(ctx, s, testFile(13), 0, ff(4096), ver(0), ver(1))
	_ = refused(t, err, core.LimitShare)
	if c.ReservedBytes() != 0 {
		t.Fatalf("ReservedBytes = %d after a refused fill", c.ReservedBytes())
	}

	// Read-only refuses with the sentinel and reserves nothing.
	s2 := mustShare(t, c, 2)
	c.Durability().Enter(syscall.EROFS)
	census := seamCensus(seam)
	if err := c.Fill(ctx, s2, testFile(14), 0, ff(64), ver(0), ver(1)); !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("Fill on a read-only journal = %v, want ErrJournalReadOnly", err)
	}
	if c.ReservedBytes() != 0 || seamCensus(seam) != census {
		t.Fatal("a read-only refusal reserved or touched storage")
	}
}

func TestFillIgnoresVersionLimit(t *testing.T) {
	c, _ := newTestCore(t, func(o *coreOptions) { o.noLimit = true })
	s := mustShare(t, c, 1)
	f := testFile(15)
	if _, err := c.WriteAt(context.Background(), s, f, 0, ff(8), 1); err == nil {
		t.Fatal("a write with no version limit set was granted")
	}
	if err := c.Fill(context.Background(), s, f, 0, ff(8), ver(0), ver(1)); err != nil {
		t.Fatalf("Fill with no version left = %v, want nil", err)
	}
}

func TestFillRegisteredInLedger(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(16)
	h := seam.Hold(t, kit.OpSync, format.SegmentName(1))
	if err := c.Fill(context.Background(), s, f, 0, ff(4096), ver(0), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	p, _ := c.Index().Lookup(f, 0)
	rec := fillRecords(t, seam)[0]
	loc := segment.Location{Segment: p.Record.Segment, Off: rec.Header.Offset, Len: rec.Header.Size()}
	if c.Durability().Durable(loc) {
		t.Fatalf("the fill record %+v is durable while its segment's sync is held", loc)
	}
	synced := make(chan error, 1)
	go func() { synced <- c.Durability().Sync(context.Background(), f) }()
	eventually(t, "the sync to wait on the fill's segment", func() bool { return h.Waiting() > 0 })
	select {
	case err := <-synced:
		t.Fatalf("Sync returned (%v) while the fill's segment sync is held", err)
	case <-time.After(50 * time.Millisecond):
	}
	h.Release()
	if err := <-synced; err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// A write racing a fill of the same extent serialises on the append domain:
// the fill, holding it at the storage seam, finishes first, and the write
// that lands after it supersedes the fill's bytes.
func TestFillDoesNotOverwriteConcurrentWrite(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(17)
	// Warm the streams so the held write is a record's, not a segment's first.
	mustWrite(t, c, s, testFile(98), 0, bytes.Repeat([]byte{'x'}, 1))
	mustWrite(t, c, s, testFile(99), 0, bytes.Repeat([]byte{'x'}, 1))
	hold := seam.HoldFirst(t, kit.OpWrite, "*")

	filled := make(chan error, 1)
	go func() { filled <- c.Fill(context.Background(), s, f, 0, ff(4096), ver(0), ver(1)) }()
	eventually(t, "the fill's append to reach storage", func() bool { return hold.Waiting() > 0 })

	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, bytes.Repeat([]byte{'w'}, 4096), 1)
		wrote <- err
	}()
	select {
	case err := <-wrote:
		t.Fatalf("the write finished (%v) while the fill held the file's append domain", err)
	case <-time.After(50 * time.Millisecond):
	}
	hold.Release()
	if err := <-filled; err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if got := readFilled(t, c, s, f, 0, 4096); !bytes.Equal(got, bytes.Repeat([]byte{'w'}, 4096)) {
		t.Fatal("the write that followed the fill did not supersede it")
	}
	if offloadedAt(t, c, f, 0) {
		t.Fatal("the write's piece is offloaded")
	}

	// The other order: the write lands first, the fill writes nothing over it.
	g := testFile(18)
	mustWrite(t, c, s, g, 0, bytes.Repeat([]byte{'w'}, 4096))
	if err := c.Fill(context.Background(), s, g, 0, ff(4096), c.Index().AsOf(g), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if got := readFilled(t, c, s, g, 0, 4096); !bytes.Equal(got, bytes.Repeat([]byte{'w'}, 4096)) {
		t.Fatal("the fill overwrote the write")
	}
}

// With the index at its bound, a read-only journal refuses a fill with the
// read-only sentinel, not the index's refusal: the bound is checked after the
// read-only gate.
func TestFillReadOnlyBeforeIndexBound(t *testing.T) {
	c, _ := newTestCore(t, func(o *coreOptions) { o.extentLimit = 1 })
	s := mustShare(t, c, 1)
	mustWrite(t, c, s, testFile(60), 0, []byte("a")) // the one entry the bound allows
	c.Durability().Enter(syscall.EROFS)
	err := c.Fill(context.Background(), s, testFile(61), 0, ff(64), ver(0), ver(1))
	if !errors.Is(err, core.ErrJournalReadOnly) {
		t.Fatalf("Fill of a read-only journal at the index bound = %v, want ErrJournalReadOnly", err)
	}
}

// Several gaps cost a record each: a share limit that fits one record of all
// their bytes but not four records is refused naming the share, and a limit
// that fits the four admits the fill.
func TestFillChargesEveryGapsRecord(t *testing.T) {
	setup := func(t *testing.T, limit int64) (*Core, *Share, core.FileID) {
		c, _ := newTestCore(t)
		s := mustLimitShare(t, c, 1, limit)
		f := testFile(62)
		for off := int64(8); off < 64; off += 16 {
			mustWrite(t, c, s, f, off, bytes.Repeat([]byte{'h'}, 8))
		}
		return c, s, f
	}
	held := 4 * recordBytes(8)
	oneRecord := recordBytes(32) + recordBytes(0) // what four 8-byte gaps ask as one split
	fourRecords := 4 * (recordBytes(8) + recordBytes(0))

	c, s, f := setup(t, held+oneRecord)
	err := c.Fill(context.Background(), s, f, 0, ff(64), c.Index().AsOf(f), ver(1))
	_ = refused(t, err, core.LimitShare)

	c, s, f = setup(t, held+fourRecords)
	if err := c.Fill(context.Background(), s, f, 0, ff(64), c.Index().AsOf(f), ver(1)); err != nil {
		t.Fatalf("Fill within the share limit = %v, want nil", err)
	}
}

// A fill refused as stale at publish — a floor raise between its pre-check and
// its publish — leaves its record in a segment, so the index claims the file
// for the share all the same.
func TestFillRefusedAtPublishClaimsTheFile(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(70)
	mustWrite(t, c, s, testFile(98), 0, []byte("x"))
	mustWrite(t, c, s, testFile(99), 0, []byte("x"))
	hold := seam.HoldFirst(t, kit.OpWrite, "*")

	filled := make(chan error, 1)
	go func() { filled <- c.Fill(context.Background(), s, f, 0, ff(4096), ver(0), ver(1)) }()
	eventually(t, "the fill's append to reach storage", func() bool { return hold.Waiting() > 0 })
	c.Index().RaiseFloor(ver(5))
	hold.Release()

	if err := <-filled; !errors.Is(err, core.ErrStaleFill) {
		t.Fatalf("Fill = %v, want ErrStaleFill", err)
	}
	if got := c.FillRefused(); got != 1 {
		t.Fatalf("FillRefused = %d, want 1", got)
	}
	if n := len(fillRecords(t, seam)); n != 1 {
		t.Fatalf("%d fill records on disk, want the refused one", n)
	}
	if tag, ok := c.Index().Owner(f); !ok || tag != s.Tag {
		t.Fatalf("Owner = %v, %v, want the share that appended the record", tag, ok)
	}
}

// A fill is admitted by the entries its gaps add, not a write's: around a held
// piece it adds two, so with one entry of room it is refused naming the index,
// while a write of the same extent adds one and is granted.
func TestFillAdmittedByTheFillIndexCheck(t *testing.T) {
	c, _ := newTestCore(t, func(o *coreOptions) { o.extentLimit = 2 })
	s := mustShare(t, c, 1)
	f := testFile(71)
	mustWrite(t, c, s, f, 50, bytes.Repeat([]byte{'a'}, 100))
	err := c.Fill(context.Background(), s, f, 0, ff(200), c.Index().AsOf(f), ver(1))
	_ = refused(t, err, core.LimitIndex)
	if _, err := c.WriteAt(context.Background(), s, f, 0, bytes.Repeat([]byte{'w'}, 200), 1); err != nil {
		t.Fatalf("a write adding one entry with one entry of room = %v, want nil", err)
	}
}

// Two fills of different files pass Take's read-only index check at limit-1 and
// both reach storage; only one commits. The other is refused at publish naming
// the index, leaves its record noted and its file claimed, and holds nothing.
func TestFillsPastTheIndexBoundAdmitOnePerEntry(t *testing.T) {
	c, seam := newTestCore(t, func(o *coreOptions) { o.extentLimit = 2 })
	s := mustShare(t, c, 1)
	mustWrite(t, c, s, testFile(80), 0, []byte("x")) // one entry of the two
	hold := seam.Hold(t, kit.OpWrite, "*")

	// Both appends must reach the seam: on one stream the second would wait
	// behind the first's append.
	a, b := racePairPicker(t, c)()
	files := []core.FileID{a, b}
	errs := make(chan error, len(files))
	for _, f := range files {
		go func() { errs <- c.Fill(context.Background(), s, f, 0, ff(4096), ver(0), ver(1)) }()
	}
	eventually(t, "both fills' appends to reach storage", func() bool { return hold.Waiting() >= 2 })
	hold.Release()

	var ok, bounded int
	for range files {
		err := <-errs
		var ns *core.NoSpaceError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &ns) && ns.Limit == core.LimitIndex:
			bounded++
		default:
			t.Fatalf("Fill = %v, want nil or a refusal naming the index", err)
		}
	}
	if ok != 1 || bounded != 1 {
		t.Fatalf("%d fills committed and %d were refused by the bound, want 1 and 1", ok, bounded)
	}
	held := 0
	for _, f := range files {
		if tag, ok := c.Index().Owner(f); !ok || tag != s.Tag {
			t.Fatalf("Owner of %v = %v, %v, want the share that appended the record", f, tag, ok)
		}
		held += len(c.Index().FileEntries(f))
	}
	if held != 1 {
		t.Fatalf("the two files hold %d entries, want 1: the refused fill held its record", held)
	}
}
