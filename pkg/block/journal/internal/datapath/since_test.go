package datapath

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// sinceAll drains Since of f above from, failing the test on an error item.
func sinceAll(t *testing.T, c *Core, s *Share, f core.FileID, from core.Version) []core.Change {
	t.Helper()
	got, err := sinceErr(c, s, f, from)
	if err != nil {
		t.Fatalf("Since(%v): %v", from, err)
	}
	return got
}

// sinceErr drains Since, returning what it yielded before its error.
func sinceErr(c *Core, s *Share, f core.FileID, from core.Version) ([]core.Change, error) {
	var got []core.Change
	for ch, err := range c.Since(context.Background(), s, f, from) {
		if err != nil {
			return got, err
		}
		got = append(got, ch)
	}
	return got, nil
}

// brief is the part of a Change a test compares, without Over.
type brief struct {
	Kind    core.ChangeKind
	Extent  core.Extent
	Version core.Version
	Mtime   int64
}

func briefs(cs []core.Change) []brief {
	var out []brief
	for _, c := range cs {
		out = append(out, brief{c.Kind, c.Extent, c.Version, c.Mtime})
	}
	return out
}

func ex(off, n int64) core.Extent { return core.Extent{Off: off, Len: n} }

func TestSinceOrderAndMarkers(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	v1 := mustWrite(t, c, s, f, 0, make([]byte, 16<<10))
	v2, err := c.Deallocate(ctx, s, f, ex(4<<10, 4<<10))
	if err != nil {
		t.Fatal(err)
	}
	v3 := mustWrite(t, c, s, f, 20<<10, make([]byte, 4<<10))
	v4, err := c.Truncate(ctx, s, f, 22<<10)
	if err != nil {
		t.Fatal(err)
	}
	want := []brief{
		{core.ChangeWrite, ex(0, 4<<10), v1, 1},
		{core.ChangeWrite, ex(8<<10, 8<<10), v1, 1},
		{core.ChangeDeallocate, ex(4<<10, 4<<10), v2, 0},
		{core.ChangeWrite, ex(20<<10, 2<<10), v3, 1},
		{core.ChangeTruncate, ex(22<<10, math.MaxInt64-22<<10), v4, 0},
	}
	if got := briefs(sinceAll(t, c, s, f, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("Since(0) = %+v, want %+v", got, want)
	}
	// A removal's own version is excluded.
	if got := briefs(sinceAll(t, c, s, f, v2)); !slices.Equal(got, want[3:]) {
		t.Fatalf("Since(the deallocate's version) = %+v, want %+v", got, want[3:])
	}
	if got := sinceAll(t, c, s, f, v4); len(got) != 0 {
		t.Fatalf("Since(the last version) = %+v, want nothing", got)
	}
}

func TestSinceOverIsCarried(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4<<10))
	v2 := mustWrite(t, c, s, f, 0, make([]byte, 8<<10))
	got := sinceAll(t, c, s, f, core.Version{})
	if len(got) != 1 || got[0].Version != v2 || !slices.Equal(got[0].Over, []core.Extent{ex(0, 4<<10)}) {
		t.Fatalf("Since(0) = %+v, want the second write alone, with Over [0,4096)", got)
	}
}

func TestSinceMtime(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	ctx := context.Background()
	v1, err := c.WriteAt(ctx, s, f, 0, make([]byte, 4<<10), 1234)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := c.WriteAt(ctx, s, f, 4<<10, make([]byte, 4<<10), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, asOf, err := c.ReadAt(ctx, s, f, 0, make([]byte, 1))
	if err != nil {
		t.Fatal(err)
	}
	fv := v1 // the ref version a fill carries is any version; this one shares the first write's
	if err := c.Fill(ctx, s, f, 8<<10, make([]byte, 4<<10), asOf, fv); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	v3, err := c.Truncate(ctx, s, f, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	want := []brief{
		{core.ChangeWrite, ex(0, 4<<10), v1, 1234},
		{core.ChangeWrite, ex(8<<10, 4<<10), fv, 0},
		{core.ChangeWrite, ex(4<<10, 4<<10), v2, 0},
		{core.ChangeTruncate, ex(16<<10, math.MaxInt64-16<<10), v3, 0},
	}
	if got := briefs(sinceAll(t, c, s, f, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("Since(0) = %+v, want %+v (mtime as given, 0 suspended, for a fill and for a marker)", got, want)
	}
	// A fill at or below from is not yielded.
	if got := briefs(sinceAll(t, c, s, f, fv)); !slices.Equal(got, want[2:]) {
		t.Fatalf("Since(the fill's version) = %+v, want %+v", got, want[2:])
	}
}

// flipMtime flips the first byte of the record's body, its modification time.
func flipMtime(t *testing.T, seam *kit.Seam, r core.SegmentID, off uint32) {
	t.Helper()
	flip(t, seam, r, int64(off)+format.RecordHeaderSize)
}

func TestSinceCorruptMtimeDropsAndReportsLoss(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 3)
	f := testFile(1)
	v, err := c.WriteAt(context.Background(), s, f, 0, pattern(4<<10, 5), 77)
	if err != nil {
		t.Fatal(err)
	}
	syncFiles(t, c, f)
	r := recordOf(t, c, f, 0).Record
	flipMtime(t, seam, r.Segment, r.Offset)

	want := []brief{{core.ChangeLoss, ex(0, 4<<10), v, 0}}
	if got := briefs(sinceAll(t, c, s, f, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("Since(0) = %+v, want only the synced loss %+v in place of the write", got, want)
	}
	if _, ok := c.Index().Lookup(f, 0); ok {
		t.Fatal("the record with a damaged mtime is still held: Corrupt did not drop it")
	}
	ev := lossOf(t, c, f)
	if ev.Version != v || ev.Reason != core.LossCorrupt || !ev.Synced {
		t.Fatalf("loss = %+v, want version %v, corrupt, synced", ev, v)
	}
	// The marker the drop left is what a second Since yields, until a Settle
	// at or above it.
	if got := briefs(sinceAll(t, c, s, f, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("second Since(0) = %+v, want %+v", got, want)
	}
	c.Settle(s, f, v)
	if got := sinceAll(t, c, s, f, core.Version{}); len(got) != 0 {
		t.Fatalf("Since(0) after Settle(%v) = %+v, want nothing", v, got)
	}
}

func TestSinceLossRules(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	ctx := context.Background()

	// A synced loss is yielded.
	f1 := testFile(1)
	v1 := mustWrite(t, c, s, f1, 0, pattern(4<<10, 1))
	syncFiles(t, c, f1)
	lose := func(f core.FileID, v core.Version, seq core.Seq, reason core.LossReason, synced bool) {
		t.Helper()
		if _, err := c.Durability().Lose(ctx, durability.Loss{Share: 1, File: f, Extent: ex(8<<10, 4<<10), Version: v, Seq: seq, Reason: reason, Synced: synced}); err != nil {
			t.Fatalf("Lose: %v", err)
		}
	}
	lose(f1, v1, 900, core.LossCorrupt, true)
	// Of one version the loss, a marker, comes before the extent.
	want := []brief{{core.ChangeLoss, ex(8<<10, 4<<10), v1, 0}, {core.ChangeWrite, ex(0, 4<<10), v1, 1}}
	if got := briefs(sinceAll(t, c, s, f1, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("synced loss: Since(0) = %+v, want %+v", got, want)
	}

	// A superseded loss, synced or not, is not.
	f2 := testFile(2)
	v2 := mustWrite(t, c, s, f2, 0, pattern(4<<10, 2))
	syncFiles(t, c, f2)
	lose(f2, v2, 901, core.LossSuperseded, true)
	if got := sinceAll(t, c, s, f2, core.Version{}); len(got) != 1 || got[0].Kind != core.ChangeWrite {
		t.Fatalf("superseded loss: Since(0) = %+v, want the write alone", got)
	}

	// An unsynced loss is not: a dropped unsynced record yields nothing.
	f3 := testFile(3)
	hold := seam.Hold(t, kit.OpSync, "*")
	mustWrite(t, c, s, f3, 0, pattern(4<<10, 3))
	r := recordOf(t, c, f3, 0).Record
	flipMtime(t, seam, r.Segment, r.Offset)
	if got := sinceAll(t, c, s, f3, core.Version{}); len(got) != 0 {
		t.Fatalf("unsynced drop: Since(0) = %+v, want nothing", got)
	}
	hold.Release()
	if ev := lossOf(t, c, f3); ev.Synced {
		t.Fatalf("loss = %+v, want unsynced", ev)
	}
	if got := sinceAll(t, c, s, f3, core.Version{}); len(got) != 0 {
		t.Fatalf("unsynced drop: second Since(0) = %+v, want nothing", got)
	}
}

func TestSinceCancelled(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4<<10))
	// The second item is a marker, which reads no record, so only the check
	// between items can end the iteration before it.
	if _, err := c.Truncate(context.Background(), s, f, 2<<10); err != nil {
		t.Fatal(err)
	}

	// A cancelled ctx before the first item yields its error alone.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	var last error
	for _, err := range c.Since(ctx, s, f, core.Version{}) {
		n++
		last = err
	}
	if n != 1 || !errors.Is(last, context.Canceled) {
		t.Fatalf("pre-cancelled: %d items, last error %v, want one item with context.Canceled", n, last)
	}

	// A ctx cancelled while the consumer holds the first item ends the
	// iteration with its error before the second.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	var kinds []error
	items := 0
	for ch, err := range c.Since(ctx, s, f, core.Version{}) {
		kinds = append(kinds, err)
		if err == nil {
			items++
			_ = ch
			cancel()
		}
	}
	if items != 1 || len(kinds) != 2 || !errors.Is(kinds[1], context.Canceled) {
		t.Fatalf("cancelled mid-way: %d items, errors %v, want one item then context.Canceled", items, kinds)
	}
}

func TestSinceOtherShareAndClosed(t *testing.T) {
	c, _ := newTestCore(t)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f := testFile(1)
	mustWrite(t, c, s1, f, 0, make([]byte, 4<<10))
	got, err := sinceErr(c, s2, f, core.Version{})
	if len(got) != 0 {
		t.Fatalf("another share's file yielded %+v", got)
	}
	wantOtherShare(t, err, f, 2, 1)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sinceErr(c, s2, testFile(2), core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Since on a closed handle: %v, want ErrClosed", err)
	}
}

// TestSinceDoesNotBlockWriter pauses the consumer inside its first yield and
// shows a writer to the same file completes meanwhile: Since holds no lock
// while the consumer works.
func TestSinceDoesNotBlockWriter(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4<<10))
	mustWrite(t, c, s, f, 8<<10, make([]byte, 4<<10))

	inYield, release, done := make(chan struct{}), make(chan struct{}), make(chan []core.Change)
	go func() {
		var got []core.Change
		for ch, err := range c.Since(context.Background(), s, f, core.Version{}) {
			if err != nil {
				t.Errorf("Since: %v", err)
				break
			}
			if len(got) == 0 {
				close(inYield)
				<-release
			}
			got = append(got, ch)
		}
		done <- got
	}()
	<-inYield
	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, 4<<10), 1)
		wrote <- err
	}()
	// One generous backstop: a writer blocked behind the paused consumer
	// would never finish, so the wait is the assertion.
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("WriteAt during the iteration: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WriteAt to the file blocked while a consumer was paused inside Since")
	}
	close(release)
	if got := <-done; len(got) != 2 {
		t.Fatalf("Since yielded %d items, want the 2 of the view it copied before the write", len(got))
	}
}

// TestSinceReportsLossAnotherReadDropped covers a synced record that a
// concurrent read drops after Since copied the file's changes but before
// Since reached it: Since still yields the loss, since a consumer whose
// cursor passes the version would otherwise never learn of it. A second
// Since, begun at once after the drop with no wait for the worker, sees the
// loss marker too.
func TestSinceReportsLossAnotherReadDropped(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 3)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, pattern(4<<10, 1))
	vb := mustWrite(t, c, s, f, 8<<10, pattern(4<<10, 2))
	syncFiles(t, c, f)
	rb := recordOf(t, c, f, 8<<10).Record
	flip(t, seam, rb.Segment, payloadStart(rb)+10)

	inYield, release, done := make(chan struct{}), make(chan struct{}), make(chan []core.Change)
	go func() {
		var got []core.Change
		for ch, err := range c.Since(context.Background(), s, f, core.Version{}) {
			if err != nil {
				t.Errorf("Since: %v", err)
				break
			}
			if len(got) == 0 {
				close(inYield)
				<-release
			}
			got = append(got, ch)
		}
		done <- got
	}()
	<-inYield
	// The read drops the record the paused Since still holds in its copy.
	readUntouched(t, c, s, f, 8<<10, 4<<10, []core.Extent{ex(8<<10, 4<<10)})
	want := []brief{{core.ChangeLoss, ex(8<<10, 4<<10), vb, 0}}
	if got := briefs(sinceAll(t, c, s, f, core.Version{Counter: vb.Counter - 1})); !slices.Equal(got, want) {
		t.Fatalf("Since begun after the drop = %+v, want the loss %+v with no wait for the worker", got, want)
	}
	close(release)
	var got []core.Change
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the paused Since never finished")
	}
	if b := briefs(got); len(b) != 2 || b[0].Kind != core.ChangeWrite || b[1] != want[0] {
		t.Fatalf("paused Since = %+v, want the first write then the loss %+v", b, want[0])
	}
}

// TestSinceReadsMtimeFromAVerifiedCopy: a record that does not verify but has
// a copy that does is yielded with the copy's modification time.
func TestSinceReadsMtimeFromAVerifiedCopy(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	orig, _ := writeWithCopy(t, c, seam, s, f, pattern(4<<10, 12))
	flip(t, seam, orig.Record.Segment, payloadStart(orig.Record)+100)

	want := []brief{{core.ChangeWrite, ex(0, 4<<10), orig.Version, 1}}
	if got := briefs(sinceAll(t, c, s, f, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("Since = %+v, want %+v (the extent with the copy's mtime)", got, want)
	}
}

// TestSinceCopyThatFailsToVerifyYieldsMtimeZero: Corrupt names a copy, the copy
// does not verify when Since reads it, and the extent is yielded with an
// Mtime of 0 rather than dropped.
func TestSinceCopyThatFailsToVerifyYieldsMtimeZero(t *testing.T) {
	var cp segment.Location
	var seam *kit.Seam
	var c *Core
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterCorrupt = func() { flip(t, seam, cp.Segment, cp.Off+format.RecordHeaderSize+8+100) }
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	var orig index.Placed
	orig, cp = writeWithCopy(t, c, seam, s, f, pattern(4<<10, 13))
	flip(t, seam, orig.Record.Segment, payloadStart(orig.Record)+100)

	want := []brief{{core.ChangeWrite, ex(0, 4<<10), orig.Version, 0}}
	if got := briefs(sinceAll(t, c, s, f, core.Version{})); !slices.Equal(got, want) {
		t.Fatalf("Since = %+v, want %+v (the extent with mtime 0)", got, want)
	}
}

// pauseSince starts Since of f and holds its consumer inside the first yield.
// The returned func releases it and returns everything Since yielded.
func pauseSince(t *testing.T, c *Core, s *Share, f core.FileID) func() []core.Change {
	t.Helper()
	inYield, release, done := make(chan struct{}), make(chan struct{}), make(chan []core.Change, 1)
	go func() {
		var got []core.Change
		for ch, err := range c.Since(context.Background(), s, f, core.Version{}) {
			if err != nil {
				t.Errorf("Since: %v", err)
				break
			}
			if len(got) == 0 {
				close(inYield)
				<-release
			}
			got = append(got, ch)
		}
		done <- got
	}()
	<-inYield
	return func() []core.Change {
		close(release)
		select {
		case got := <-done:
			return got
		case <-time.After(30 * time.Second):
			t.Fatal("the paused Since never finished")
			return nil
		}
	}
}

// retiredSinceFile writes an earlier record, which Since yields first, then
// the sealed record at 0 of f, and returns the sealed record's placement.
func retiredSinceFile(t *testing.T, c *Core, s *Share, f core.FileID, data []byte) index.Placed {
	t.Helper()
	mustWrite(t, c, s, f, 1<<20, pattern(4<<10, 9))
	return sealedRecord(t, c, s, f, data)
}

// TestSinceRepointedRecordKeepsItsMtime retires the segment of a record that
// the Since copy still names, after a repack copied the record elsewhere and
// repointed the index: Since reads the copy and yields the write with the
// mtime it was given.
func TestSinceRepointedRecordKeepsItsMtime(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(4<<10, 21)
	orig := retiredSinceFile(t, c, s, f, data)
	resume := pauseSince(t, c, s, f)

	hdr := headerAt(t, seam, orig.Record)
	unlock := c.domains.Lock(f)
	pl, err := c.Streams().For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: hdr.Extent, Seq: hdr.Seq, Version: hdr.Version, Tag: hdr.Tag,
	}, format.RecordBody{Payload: data, MTime: 1})
	unlock()
	if err != nil || pl.Loc.Segment == orig.Record.Segment {
		t.Fatalf("Append of the copy: %+v %v", pl, err)
	}
	moved, hd, err := c.Index().RepointRecord(f, orig.Record, index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)})
	c.Account(hd)
	if err != nil || moved != 1 {
		t.Fatalf("RepointRecord = %d, %v", moved, err)
	}
	retire(t, c, seam, orig.Record.Segment)

	var got *core.Change
	all := resume()
	for i := range all {
		if all[i].Kind == core.ChangeWrite && all[i].Version == orig.Version {
			got = &all[i]
		}
	}
	if got == nil || got.Mtime != 1 {
		t.Fatalf("Since = %+v, want the repointed write yielded with Mtime 1", all)
	}
}

// TestSinceReportsLossOfDroppedThenRetiredRecord drops a synced record by a
// read after Since copied the changes, then retires its segment: Since finds
// the segment gone and still yields the loss, which a Settle at that version
// would otherwise erase unreported.
func TestSinceReportsLossOfDroppedThenRetiredRecord(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(4<<10, 21)
	orig := retiredSinceFile(t, c, s, f, data)
	flip(t, seam, orig.Record.Segment, payloadStart(orig.Record)+10)
	resume := pauseSince(t, c, s, f)

	readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{ex(0, 4<<10)})
	retire(t, c, seam, orig.Record.Segment)

	want := brief{core.ChangeLoss, ex(0, 4<<10), orig.Version, 0}
	all := resume()
	if !slices.Contains(briefs(all), want) {
		t.Fatalf("Since = %+v, want it to yield the loss %+v", briefs(all), want)
	}
	for _, ch := range all {
		if ch.Kind == core.ChangeWrite && ch.Version == orig.Version {
			t.Fatalf("Since yielded the dropped write %+v", ch)
		}
	}
}

// moveRecord appends a copy of the record at r, whose payload is data, to the
// stream's next segment, as a repack does, repoints the index to it and returns
// its place. The stream's current segment is sealed first, so the copy never
// shares a segment with a record that stays.
func moveRecord(t *testing.T, c *Core, seam *kit.Seam, f core.FileID, r index.RecordAt, data []byte) index.RecordAt {
	t.Helper()
	hdr := headerAt(t, seam, r)
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Errorf("Seal: %v", err)
		return r
	}
	unlock := c.domains.Lock(f)
	pl, err := c.Streams().For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: hdr.Extent, Seq: hdr.Seq, Version: hdr.Version, Tag: hdr.Tag,
	}, format.RecordBody{Payload: data, MTime: 1})
	unlock()
	if err != nil || pl.Loc.Segment == r.Segment {
		t.Errorf("Append of the copy: %+v %v", pl, err)
		return r
	}
	to := index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}
	moved, hd, err := c.Index().RepointRecord(f, r, to)
	c.Account(hd)
	if err != nil || moved == 0 {
		t.Errorf("RepointRecord = %d, %v", moved, err)
	}
	return to
}

// TestSinceFollowsARecordThroughTwoRepacks moves the record a second time,
// and retires the place it was first moved to, after Since sent its read to
// that place: the extent is still yielded with its mtime, from the third place.
func TestSinceFollowsARecordThroughTwoRepacks(t *testing.T) {
	var c *Core
	var seam *kit.Seam
	var first index.RecordAt
	data := pattern(4<<10, 21)
	fired := make(chan struct{})
	c, seam = newTestCore(t, func(o *coreOptions) {
		o.afterRetarget = func() {
			select {
			case <-fired:
				return
			default:
			}
			close(fired)
			moveRecord(t, c, seam, testFile(1), first, data)
			retire(t, c, seam, first.Segment)
		}
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	orig := retiredSinceFile(t, c, s, f, data)
	resume := pauseSince(t, c, s, f)

	first = moveRecord(t, c, seam, f, orig.Record, data)
	retire(t, c, seam, orig.Record.Segment)

	want := brief{core.ChangeWrite, ex(0, 4<<10), orig.Version, 1}
	all := resume()
	select {
	case <-fired:
	default:
		t.Fatal("Since never followed the record to its second place")
	}
	if !slices.Contains(briefs(all), want) {
		t.Fatalf("Since = %+v, want it to yield %+v from the record's third place", briefs(all), want)
	}
}

// TestSinceRetargetsAPieceAfterAnotherWasSuperseded splits one record into two
// pieces, supersedes the first, and moves the record, retiring its segment,
// after Since copied the changes: the second piece is still yielded with its
// mtime.
func TestSinceRetargetsAPieceAfterAnotherWasSuperseded(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(12<<10, 7)
	mustWrite(t, c, s, f, 1<<20, pattern(4<<10, 9))
	vr := mustWrite(t, c, s, f, 0, data)
	syncFiles(t, c, f)
	r := recordOf(t, c, f, 0).Record
	if _, err := c.Streams().For(f).Seal(true); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, c, s, f, 4<<10, pattern(4<<10, 3)) // splits the record into two pieces
	syncFiles(t, c, f)
	resume := pauseSince(t, c, s, f)

	mustWrite(t, c, s, f, 0, pattern(4<<10, 4)) // supersedes the first piece
	moveRecord(t, c, seam, f, r, data)
	retire(t, c, seam, r.Segment)

	want := brief{core.ChangeWrite, ex(8<<10, 4<<10), vr, 1}
	if all := resume(); !slices.Contains(briefs(all), want) {
		t.Fatalf("Since = %+v, want it to yield the second piece %+v", briefs(all), want)
	}
}
