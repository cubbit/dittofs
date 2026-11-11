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
