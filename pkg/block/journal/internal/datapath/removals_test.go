package datapath

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// removalRecords returns the removal records on disk, in segment and offset
// order.
func removalRecords(t *testing.T, seam *kit.Seam) []onDisk {
	t.Helper()
	var out []onDisk
	for _, r := range readRecords(t, seam) {
		switch r.Header.Kind {
		case format.KindTruncate, format.KindDeallocate, format.KindDelete, format.KindCloneTarget:
			out = append(out, r)
		}
	}
	return out
}

// removalOf returns the one removal record on disk of kind k, failing the
// test when there is none or several.
func removalOf(t *testing.T, seam *kit.Seam, k format.Kind) onDisk {
	t.Helper()
	var got []onDisk
	for _, r := range removalRecords(t, seam) {
		if r.Header.Kind == k {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("records of kind %s on disk: got %d, want 1", k, len(got))
	}
	return got[0]
}

// markerOfKind returns the one unsettled marker of kind c the index holds for
// f, failing the test when there is none or several.
func markerOfKind(t *testing.T, c *Core, f core.FileID, k core.ChangeKind) index.Entry {
	t.Helper()
	var got []index.Entry
	for _, e := range c.Index().FileEntries(f) {
		if e.Kind == index.EntryMarker && e.Removal == k {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("markers of kind %d on %v: got %d, want 1\n%+v", k, f, len(got), c.Index().FileEntries(f))
	}
	return got[0]
}

// TestRemovalsAppendTheirHeaderValues drives each removal and checks the
// record it left on disk carries the header RFC 28 §2.3 gives it: a truncate
// and a delete write length 0, a deallocate its extent and a clone target its
// destination with the spec as its body, each under the version it returned.
func TestRemovalsAppendTheirHeaderValues(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))

	v, err := c.Truncate(ctx, s, f, 2048)
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	r := removalOf(t, seam, format.KindTruncate)
	if r.Header.Extent != (core.Extent{Off: 2048, Len: 0}) || r.Header.Version != v || r.Header.Tag != s.Tag || r.Header.File != f {
		t.Fatalf("truncate record = %+v, want extent (2048, 0) at %v under tag %d", r.Header, v, s.Tag)
	}

	v, err = c.Deallocate(ctx, s, f, core.Extent{Off: 1024, Len: 1024})
	if err != nil {
		t.Fatalf("Deallocate: %v", err)
	}
	r = removalOf(t, seam, format.KindDeallocate)
	if r.Header.Extent != (core.Extent{Off: 1024, Len: 1024}) || r.Header.Version != v {
		t.Fatalf("deallocate record = %+v, want extent (1024, 1024) at %v", r.Header, v)
	}

	v, err = c.Delete(ctx, s, f)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	r = removalOf(t, seam, format.KindDelete)
	if r.Header.Extent != (core.Extent{}) || r.Header.Version != v {
		t.Fatalf("delete record = %+v, want extent (0, 0) at %v", r.Header, v)
	}

	g := testFile(2)
	spec := core.CloneSpec{
		SrcShare: core.ShareID{0: 3},
		Src:      testFile(9),
		SrcOff:   1 << 20,
		DstOff:   4096,
		Len:      2048,
		AsOf:     ver(7),
	}
	v, err = c.CloneTarget(ctx, s, g, spec)
	if err != nil {
		t.Fatalf("CloneTarget: %v", err)
	}
	r = removalOf(t, seam, format.KindCloneTarget)
	if r.Header.Extent != (core.Extent{Off: 4096, Len: 2048}) || r.Header.Version != v {
		t.Fatalf("clone target record = %+v, want extent (4096, 2048) at %v", r.Header, v)
	}
	want := format.CloneSource{Share: spec.SrcShare, File: spec.Src, Off: spec.SrcOff, AsOf: spec.AsOf}
	if r.Body.Clone != want {
		t.Fatalf("clone target body = %+v, want %+v", r.Body.Clone, want)
	}
}

// TestRemovalsLeaveTheirMarkers checks each removal leaves the marker Since
// yields — the removed range at its version, the clone target with its spec —
// and stops holding what it covered.
func TestRemovalsLeaveTheirMarkers(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))

	v, err := c.Truncate(ctx, s, f, 2048)
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if m := markerOfKind(t, c, f, core.ChangeTruncate); m.Extent != toEnd(2048) || m.Version != v {
		t.Fatalf("truncate marker = %+v, want extent (2048, to end) at %v", m, v)
	}
	if _, missing := c.Index().Span(f, core.Extent{Off: 2048, Len: 2048}); len(missing) != 1 {
		t.Fatalf("the truncated tail is held: missing = %v", missing)
	}
	if got := c.Index().AsOf(f); got != v {
		t.Fatalf("asOf = %v after a truncate, want it raised to %v", got, v)
	}

	v, err = c.Deallocate(ctx, s, f, core.Extent{Off: 512, Len: 512})
	if err != nil {
		t.Fatalf("Deallocate: %v", err)
	}
	if m := markerOfKind(t, c, f, core.ChangeDeallocate); m.Extent != (core.Extent{Off: 512, Len: 512}) || m.Version != v {
		t.Fatalf("deallocate marker = %+v, want extent (512, 512) at %v", m, v)
	}

	spec := core.CloneSpec{SrcShare: core.ShareID{0: 2}, Src: testFile(9), DstOff: 1024, Len: 512, AsOf: ver(1)}
	v, err = c.CloneTarget(ctx, s, f, spec)
	if err != nil {
		t.Fatalf("CloneTarget: %v", err)
	}
	if m := markerOfKind(t, c, f, core.ChangeCloneTarget); m.Clone == nil || *m.Clone != spec || m.Version != v {
		t.Fatalf("clone target marker = %+v, want spec %+v at %v", m, spec, v)
	}

	v, err = c.Delete(ctx, s, f)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if m := markerOfKind(t, c, f, core.ChangeDelete); m.Extent != toEnd(0) || m.Version != v {
		t.Fatalf("delete marker = %+v, want extent (0, to end) at %v", m, v)
	}
	if placed, missing := c.Index().Span(f, core.Extent{Off: 0, Len: 4096}); len(placed) != 0 || len(missing) != 1 {
		t.Fatalf("content is held after a delete: placed = %v, missing = %v", placed, missing)
	}
}

// TestRemovalsRefuseShapesAndDrawNoVersion checks the shape refusals: a
// zero-length deallocate and clone target is refused with ErrZeroLength, a
// negative truncate and a deallocate whose end overflows with ErrInvalidExtent,
// each appends nothing and draws no version.
func TestRemovalsRefuseShapesAndDrawNoVersion(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		call func(*Core, *Share, core.FileID) (core.Version, error)
		want error
	}{
		"zero-length deallocate": {
			call: func(c *Core, s *Share, f core.FileID) (core.Version, error) {
				return c.Deallocate(ctx, s, f, core.Extent{Off: 4, Len: 0})
			},
			want: core.ErrZeroLength,
		},
		"zero-length clone target": {
			call: func(c *Core, s *Share, f core.FileID) (core.Version, error) {
				return c.CloneTarget(ctx, s, f, core.CloneSpec{DstOff: 4, Len: 0})
			},
			want: core.ErrZeroLength,
		},
		"negative truncate": {
			call: func(c *Core, s *Share, f core.FileID) (core.Version, error) {
				return c.Truncate(ctx, s, f, -1)
			},
			want: core.ErrInvalidExtent,
		},
		"deallocate offset overflow": {
			call: func(c *Core, s *Share, f core.FileID) (core.Version, error) {
				return c.Deallocate(ctx, s, f, core.Extent{Off: 4, Len: 1<<63 - 1})
			},
			want: core.ErrInvalidExtent,
		},
		"clone target offset overflow": {
			call: func(c *Core, s *Share, f core.FileID) (core.Version, error) {
				return c.CloneTarget(ctx, s, f, core.CloneSpec{DstOff: 4, Len: 1<<63 - 1})
			},
			want: core.ErrInvalidExtent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			f := testFile(1)
			next := c.counters.NextVersion()
			writes := seam.Calls(kit.OpWrite)
			v, err := tc.call(c, s, f)
			want := tc.want
			if !errors.Is(err, want) {
				t.Fatalf("%s = %v, want %v", name, err, want)
			}
			if v != (core.Version{}) {
				t.Fatalf("%s returned version %v", name, v)
			}
			if got := c.counters.NextVersion(); got != next {
				t.Fatalf("%s drew a version: next is %v, was %v", name, got, next)
			}
			if got := removalRecords(t, seam); len(got) != 0 {
				t.Fatalf("%s appended %d removal records", name, len(got))
			}
			if got := seam.Calls(kit.OpWrite); got != writes {
				t.Fatalf("%s made %d record writes", name, got-writes)
			}
			if got := c.Index().FileEntries(f); len(got) != 0 {
				t.Fatalf("%s left index entries: %+v", name, got)
			}
		})
	}
}

// TestRemovalsRefuseAnotherSharesFile checks a removal through a handle of
// another share's tag is refused with ErrOtherShare before anything is drawn
// or appended.
func TestRemovalsRefuseAnotherSharesFile(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	owner := mustShare(t, c, 1)
	other := mustShare(t, c, 2)
	f := testFile(1)
	mustWrite(t, c, owner, f, 0, make([]byte, 8))

	next, writes := c.counters.NextVersion(), seam.Calls(kit.OpWrite)
	for name, err := range map[string]error{
		"truncate":     func() error { _, err := c.Truncate(ctx, other, f, 0); return err }(),
		"deallocate":   func() error { _, err := c.Deallocate(ctx, other, f, core.Extent{Off: 0, Len: 8}); return err }(),
		"delete":       func() error { _, err := c.Delete(ctx, other, f); return err }(),
		"clone target": func() error { _, err := c.CloneTarget(ctx, other, f, core.CloneSpec{DstOff: 0, Len: 8}); return err }(),
	} {
		if !errors.Is(err, core.ErrOtherShare) {
			t.Fatalf("%s through another share = %v, want ErrOtherShare", name, err)
		}
	}
	if got := c.counters.NextVersion(); got != next {
		t.Fatalf("a refused removal drew a version: next is %v, was %v", got, next)
	}
	if got := seam.Calls(kit.OpWrite); got != writes {
		t.Fatalf("a refused removal made %d record writes", got-writes)
	}
}

// crashLosingUnsynced crashes the seam with no option, so every write no sync
// covered is lost: the bytes read back are the durable image alone.
func crashLosingUnsynced(t *testing.T, seam *kit.Seam) {
	t.Helper()
	crashedSeams.Store(seam, true)
	if err := seam.Crash(); err != nil {
		t.Fatalf("Crash: %v", err)
	}
}

// TestRemovalIsDurableOnReturn checks each removal's record is durable once
// the call returns: a crash at the seam right after it leaves the record in
// the segment's durable image.
func TestRemovalIsDurableOnReturn(t *testing.T) {
	for name, call := range map[string]func(*Core, *Share, core.FileID) error{
		"truncate": func(c *Core, s *Share, f core.FileID) error {
			_, err := c.Truncate(context.Background(), s, f, 0)
			return err
		},
		"deallocate": func(c *Core, s *Share, f core.FileID) error {
			_, err := c.Deallocate(context.Background(), s, f, core.Extent{Off: 0, Len: 8})
			return err
		},
		"delete": func(c *Core, s *Share, f core.FileID) error {
			_, err := c.Delete(context.Background(), s, f)
			return err
		},
		"clone target": func(c *Core, s *Share, f core.FileID) error {
			_, err := c.CloneTarget(context.Background(), s, f, core.CloneSpec{DstOff: 0, Len: 8})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			f := testFile(1)
			if err := call(c, s, f); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			rec := removalRecords(t, seam)
			if len(rec) != 1 {
				t.Fatalf("%s left %d removal records, want 1", name, len(rec))
			}
			loc := segment.Location{Segment: rec[0].Header.Segment, Off: rec[0].Header.Offset, Len: rec[0].Header.Size()}
			if !c.Durability().Durable(loc) {
				t.Fatalf("the %s record %+v is not durable on return", name, loc)
			}
		})
	}
}

// TestRemovalSurvivesACrashRightAfterItReturns crashes the seam with nothing
// kept right after a removal returns and reads the removal record back out of
// the durable image: a removal that returned before its record was synced
// loses it here.
func TestRemovalSurvivesACrashRightAfterItReturns(t *testing.T) {
	for name, call := range map[string]func(*Core, *Share, core.FileID) (core.Version, error){
		"truncate": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Truncate(context.Background(), s, f, 0)
		},
		"deallocate": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Deallocate(context.Background(), s, f, core.Extent{Off: 0, Len: 8})
		},
		"delete": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Delete(context.Background(), s, f)
		},
		"clone target": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.CloneTarget(context.Background(), s, f, core.CloneSpec{DstOff: 0, Len: 8})
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			f := testFile(1)
			v, err := call(c, s, f)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			crashLosingUnsynced(t, seam)
			var found bool
			for _, r := range removalRecords(t, seam) {
				if r.Header.File == f && r.Header.Version == v {
					found = true
				}
			}
			if !found {
				t.Fatalf("the %s record at version %v is not on disk after a crash", name, v)
			}
		})
	}
}

// TestRemovalAfterATimerFailedWindowLeavesItsMarkForSync fails the sync bound's
// own sync of a file, then truncates the file: the truncate's record is in a
// new segment and returns nil, never the window's SyncError, and the file's
// next Sync still reports the failure it was owed.
func TestRemovalAfterATimerFailedWindowLeavesItsMarkForSync(t *testing.T) {
	clk := kit.NewStopwatchClock()
	c, seam := newTestCore(t, func(o *coreOptions) { o.clock = clk })
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	seg := format.SegmentName(recordOf(t, c, f, 0).Record.Segment)
	// The failed segment's pages do not read back either, so its window
	// cannot re-append the write and fails it, which marks the file.
	if err := seam.Schedule(
		kit.Failing(kit.OpSync, seg, 1, errors.New("sync boom")),
		kit.Failing(kit.OpRead, seg, 0, errors.New("read boom")),
	); err != nil {
		t.Fatalf("the seam refused the failing sync and read rules: %v", err)
	}
	eventually(t, "the sync bound's timer to arm", func() bool { return clk.Pending() > 0 })
	clk.Advance(core.SyncBound)
	eventually(t, "the timer's sync to fail", func() bool { return c.Durability().Stats().SyncFailures == 1 })
	// The window's failure lands once its record is dropped, which leaves the
	// file owed. The failed segment reads back again then, so the scan below
	// sees it.
	eventually(t, "the failed window's write to be dropped", func() bool {
		_, ok := c.Index().Lookup(f, 0)
		return !ok
	})
	if err := seam.Schedule(); err != nil {
		t.Fatalf("the seam refused to clear its rules: %v", err)
	}

	v, err := c.Truncate(context.Background(), s, f, 2048)
	if err != nil {
		t.Fatalf("the truncate after a failed window = %v, want the old window's failure left for Sync", err)
	}
	if rec := removalOf(t, seam, format.KindTruncate); rec.Header.Version != v {
		t.Fatalf("the record on disk carries %v, the call returned %v", rec.Header.Version, v)
	}
	if err := c.Durability().Sync(context.Background(), f); !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("Sync after the truncate = %v, want the failed window's SyncError", err)
	}
}

// TestRemovalInAFailedWindowIsReappended checks a truncate whose record is in a
// segment whose sync failed survives it: the record is re-appended to a new
// segment, the wait follows it and returns nil, and no loss is recorded.
func TestRemovalInAFailedWindowIsReappended(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	seg := recordOf(t, c, f, 0).Record.Segment

	hold := seam.HoldFirst(t, kit.OpSync, format.SegmentName(seg))
	done := make(chan struct {
		v   core.Version
		err error
	}, 1)
	go func() {
		v, err := c.Truncate(context.Background(), s, f, 2048)
		done <- struct {
			v   core.Version
			err error
		}{v, err}
	}()
	eventually(t, "the removal's sync to reach storage", func() bool { return hold.Waiting() > 0 })
	// The rule is consulted after the hold, so it fails the held sync.
	if err := seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(seg), 1, errors.New("sync boom"))); err != nil {
		t.Fatalf("the seam refused the failing sync rule: %v", err)
	}
	gen := c.Durability().Generation()
	hold.Release()

	got := within(t, done, "the removal whose segment's sync failed")
	if got.err != nil {
		t.Fatalf("the removal = %v, want nil once its record was re-appended", got.err)
	}
	var reappended bool
	for _, r := range removalRecords(t, seam) {
		if r.Header.Kind == format.KindTruncate && r.Header.File == f && r.Header.Segment != seg && r.Header.Version == got.v {
			reappended = true
		}
	}
	if !reappended {
		t.Fatalf("the truncate's record is not on disk in a segment after the failed one %d", seg)
	}
	for _, r := range readRecords(t, seam) {
		if r.Header.Kind == format.KindLoss {
			t.Fatalf("a loss record was written for a re-appended removal: %+v", r.Header)
		}
	}
	if g := c.Durability().Generation(); g != gen {
		t.Fatalf("the loss generation moved from %d to %d: a re-append raises none", gen, g)
	}
}

// TestRemovalWaitingWhenTheJournalTurnsReadOnly checks a removal parked in its
// durability wait returns ErrJournalReadOnly, never a *SyncError and never a
// hang, once the journal turns read-only, and still names the version it drew.
func TestRemovalWaitingWhenTheJournalTurnsReadOnly(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	// Every sync is held, so the record waits whichever sync reaches it.
	hold := seam.Hold(t, kit.OpSync, "*")
	done := make(chan struct {
		v   core.Version
		err error
	}, 1)
	go func() {
		v, err := c.Truncate(context.Background(), s, f, 0)
		done <- struct {
			v   core.Version
			err error
		}{v, err}
	}()
	eventually(t, "the removal's wait to reach its sync", func() bool { return hold.Waiting() > 0 })
	c.Durability().Enter(syscall.EROFS)
	hold.Release()
	got := within(t, done, "the removal waiting when the journal turned read-only")

	if !errors.Is(got.err, core.ErrJournalReadOnly) {
		t.Fatalf("the removal = %v, want ErrJournalReadOnly", got.err)
	}
	var se *core.SyncError
	if errors.As(got.err, &se) {
		t.Fatalf("the removal returned %v, a *core.SyncError", got.err)
	}
	if rec := removalOf(t, seam, format.KindTruncate); got.v == (core.Version{}) || rec.Header.Version != got.v {
		t.Fatalf("the removal returned %v, want the record's version %v", got.v, rec.Header.Version)
	}
}

// TestRemovalsAreInTheLedger checks each removal's record is in the durability
// ledger while it is unsynced, named by the place the record landed at.
func TestRemovalsAreInTheLedger(t *testing.T) {
	for name, call := range map[string]func(*Core, *Share, core.FileID) (core.Version, error){
		"truncate": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Truncate(context.Background(), s, f, 0)
		},
		"deallocate": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Deallocate(context.Background(), s, f, core.Extent{Off: 0, Len: 8})
		},
		"delete": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.Delete(context.Background(), s, f)
		},
		"clone target": func(c *Core, s *Share, f core.FileID) (core.Version, error) {
			return c.CloneTarget(context.Background(), s, f, core.CloneSpec{DstOff: 0, Len: 8})
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, seam := newTestCore(t)
			s := mustShare(t, c, 1)
			f := testFile(1)
			// Every sync is held, so the removal's own record cannot be
			// completed by a sync the hold did not stop.
			hold := seam.Hold(t, kit.OpSync, "*")
			done := make(chan struct {
				v   core.Version
				err error
			}, 1)
			go func() {
				v, err := call(c, s, f)
				done <- struct {
					v   core.Version
					err error
				}{v, err}
			}()
			eventually(t, "the removal's sync to reach storage", func() bool { return hold.Waiting() > 0 })
			rec := removalRecords(t, seam)
			if len(rec) != 1 {
				t.Fatalf("%s left %d removal records on disk, want 1", name, len(rec))
			}
			at := index.RecordAt{Segment: rec[0].Header.Segment, Offset: uint32(rec[0].Header.Offset)}
			w, ok := c.Durability().Tracked(f, at)
			if !ok || w.Kind != rec[0].Header.Kind || w.Version != rec[0].Header.Version {
				t.Fatalf("%s: the ledger holds %+v, %v, want the record at %+v", name, w, ok, at)
			}
			hold.Release()
			if got := within(t, done, "the removal after its sync was released"); got.err != nil {
				t.Fatalf("%s: %v", name, got.err)
			}
		})
	}
}

// TestRemovalReturnsItsVersionWhenCancelledAfterTheAppend checks a removal
// whose ctx is cancelled while its wait runs still returns its version and no
// error: the record is on disk, and the context is not consulted after the
// append.
func TestRemovalReturnsItsVersionWhenCancelledAfterTheAppend(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	hold := seam.HoldFirst(t, kit.OpSync, "*")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct {
		v   core.Version
		err error
	}, 1)
	go func() {
		v, err := c.Truncate(ctx, s, f, 0)
		done <- struct {
			v   core.Version
			err error
		}{v, err}
	}()
	eventually(t, "the removal's wait to reach its sync", func() bool { return hold.Waiting() > 0 })
	cancel()
	hold.Release()
	got := within(t, done, "the removal whose ctx was cancelled during its wait")
	if got.err != nil {
		t.Fatalf("the removal = %v, want the result and no context error", got.err)
	}
	rec := removalOf(t, seam, format.KindTruncate)
	if got.v == (core.Version{}) || rec.Header.Version != got.v {
		t.Fatalf("the removal returned %v, want the record's version %v", got.v, rec.Header.Version)
	}
}

// TestRemovalRefusedOnAReadOnlyJournal checks a removal on a read-only journal
// is refused at once with ErrJournalReadOnly, drawing no version, appending
// nothing and touching no storage.
func TestRemovalRefusedOnAReadOnlyJournal(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	c.Durability().Enter(syscall.EROFS)
	next, writes, census := c.counters.NextVersion(), seam.Calls(kit.OpWrite), seamCensus(seam)

	for name, err := range map[string]error{
		"truncate": func() error { _, err := c.Truncate(context.Background(), s, f, 0); return err }(),
		"deallocate": func() error {
			_, err := c.Deallocate(context.Background(), s, f, core.Extent{Off: 0, Len: 8})
			return err
		}(),
		"delete": func() error { _, err := c.Delete(context.Background(), s, f); return err }(),
		"clone target": func() error {
			_, err := c.CloneTarget(context.Background(), s, f, core.CloneSpec{DstOff: 0, Len: 8})
			return err
		}(),
	} {
		if !errors.Is(err, core.ErrJournalReadOnly) {
			t.Fatalf("%s on a read-only journal = %v, want ErrJournalReadOnly", name, err)
		}
	}
	if got := c.counters.NextVersion(); got != next {
		t.Fatalf("a read-only refusal drew a version: next is %v, was %v", got, next)
	}
	if got := seam.Calls(kit.OpWrite); got != writes || seamCensus(seam) != census {
		t.Fatal("a read-only refusal appended or touched storage")
	}
}

// TestRemovalsOnOneFileTakeVersionsInAppendOrder runs two removals on one file
// concurrently and checks their versions and their records' order on disk
// agree: the earlier version is the earlier record.
func TestRemovalsOnOneFileTakeVersionsInAppendOrder(t *testing.T) {
	ctx := context.Background()
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 8192))

	type result struct {
		v   core.Version
		err error
	}
	done := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		v, err := c.Truncate(ctx, s, f, 4096)
		done <- result{v, err}
	}()
	go func() {
		defer wg.Done()
		v, err := c.Deallocate(ctx, s, f, core.Extent{Off: 0, Len: 512})
		done <- result{v, err}
	}()
	wg.Wait()
	close(done)
	var vs []core.Version
	for r := range done {
		if r.err != nil {
			t.Fatalf("a removal = %v, want no error", r.err)
		}
		vs = append(vs, r.v)
	}
	if vs[0] == vs[1] {
		t.Fatalf("two removals drew one version %v", vs[0])
	}
	var on []core.Version
	for _, r := range removalRecords(t, seam) {
		on = append(on, r.Header.Version)
	}
	if len(on) != 2 {
		t.Fatalf("%d removal records on disk, want 2", len(on))
	}
	if on[0].Compare(on[1]) >= 0 {
		t.Fatalf("records are not in version order: %v then %v", on[0], on[1])
	}
	if (vs[0] != on[0] || vs[1] != on[1]) && (vs[0] != on[1] || vs[1] != on[0]) {
		t.Fatalf("the removals returned versions %v, but the records on disk carry %v", vs, on)
	}
}

// TestRemovalReleasesTheFileAppendDomainBeforeItsWait parks a removal in its
// durability wait and checks a second write of the same file runs to its end
// meanwhile: the file's append domain is released before the wait, so a
// removal never waits on the worker that resolves the file's window under it.
func TestRemovalReleasesTheFileAppendDomainBeforeItsWait(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	mustWrite(t, c, s, f, 0, make([]byte, 4096))
	hold := seam.HoldFirst(t, kit.OpSync, "*")

	removed := make(chan error, 1)
	go func() {
		_, err := c.Truncate(context.Background(), s, f, 2048)
		removed <- err
	}()
	eventually(t, "the removal's wait to reach its sync", func() bool { return hold.Waiting() > 0 })

	// The write needs the file's append domain the removal must have
	// released: it would block here if the removal held it across the wait.
	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, []byte("abcd"), 1)
		wrote <- err
	}()
	if err := within(t, wrote, "a write of the removed file while the removal waits"); err != nil {
		t.Fatalf("the write beside a waiting removal: %v", err)
	}
	hold.Release()
	if err := within(t, removed, "the removal after its sync was released"); err != nil {
		t.Fatalf("the removal: %v", err)
	}
}

// TestRemovalOfOneFileTakesNoLockAnotherFilesIOHolds holds file B's record
// write at storage and checks a removal of file A returns meanwhile: a removal
// takes its own file's append domain and no lock another file's I/O holds. An
// offload pass runs its callback with no journal lock held and its uploads
// under the file's storage guard, so the lock a removal must not wait on is
// the one of the file another's I/O holds; offload is not wired in this
// package yet, so the held record write stands in for it.
func TestRemovalOfOneFileTakesNoLockAnotherFilesIOHolds(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	b := testFile(2)
	a := otherStreamFile(c, b)
	mustWrite(t, c, s, a, 0, make([]byte, 4096))
	mustWrite(t, c, s, b, 0, make([]byte, 4096))
	// The hold names B's segment, so A's removal, on the other stream, writes
	// past it.
	hold := seam.Hold(t, kit.OpWrite, format.SegmentName(recordOf(t, c, b, 0).Record.Segment))
	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, b, 1<<20, []byte("w"), 1)
		wrote <- err
	}()
	within(t, hold.Reached(), "B's record write up to storage")

	removed := make(chan error, 1)
	go func() {
		_, err := c.Truncate(context.Background(), s, a, 2048)
		removed <- err
	}()
	if err := within(t, removed, "a removal of A while B's record write is held"); err != nil {
		t.Fatalf("the removal of A: %v", err)
	}
	hold.Release()
	if err := within(t, wrote, "B's write after the hold was released"); err != nil {
		t.Fatalf("B's write: %v", err)
	}
}

// otherStreamFile returns a test file whose appends land on a stream other
// than f's, failing the test when the core's two streams offer none.
func otherStreamFile(c *Core, f core.FileID) core.FileID {
	for b := byte(1); b < 40; b++ {
		if g := testFile(b); g != f && streamOf(c, g) != streamOf(c, f) {
			return g
		}
	}
	panic("the test core's two streams place every file on one")
}

// TestRemovalDropsOffloadedContent checks a removal drops content whatever
// its offloaded bit: a deallocate over an offloaded piece stops holding it,
// and the pieces outside its extent keep theirs.
func TestRemovalDropsOffloadedContent(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	// Filled pieces are held offloaded: the fill path sets the bit, and the
	// datapath's offload path is not this task's. The fill's version is below
	// the one the removal below draws, which only content below a marker's
	// version is excluded by.
	if _, err := c.Draw(); err != nil {
		t.Fatalf("Draw: %v", err)
	}
	if err := c.Fill(ctx, s, f, 0, ff(12288), c.Index().AsOf(f), ver(1)); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if !offloadedAt(t, c, f, 0) || !offloadedAt(t, c, f, 8192) {
		t.Fatal("the filled pieces are not offloaded")
	}
	if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 0, Len: 4096}); err != nil {
		t.Fatalf("Deallocate over offloaded content: %v", err)
	}
	if _, missing := c.Index().Span(f, core.Extent{Off: 0, Len: 4096}); len(missing) != 1 {
		t.Fatalf("the offloaded content removed is still held: missing = %v", missing)
	}
	if !offloadedAt(t, c, f, 8192) {
		t.Fatal("the piece outside the removal lost its offloaded bit")
	}
}
