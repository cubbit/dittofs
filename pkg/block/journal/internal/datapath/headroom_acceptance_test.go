package datapath

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// Header-only appenders do not exist yet, so these drive the admission a
// removal path will call (Core.headroom) over a journal really at its
// limit; the content-write side goes through WriteAt.

// acceptanceRecords is the headroom of the cores below: forty records, so
// the open reserve is twenty, the drawing side twenty and half falls at ten
// drawn.
const acceptanceRecords = 40

// atTheLimit returns a core whose journal holds its limit with its pool
// settled, a share on it, and the file the warm write went to. opts adjust
// the core before it is built.
func atTheLimit(t *testing.T, opts ...func(*coreOptions)) (*Core, *kit.Seam, *Share, core.FileID) {
	t.Helper()
	c, seam := newTestCore(t, append([]func(*coreOptions){
		capacityCoreOption(5 * testSegmentCap),
		func(o *coreOptions) { o.headroomRecords = acceptanceRecords },
	}, opts...)...)
	s := mustShare(t, c, 1)
	f := testFile(1)
	if _, err := c.WriteAt(context.Background(), s, f, 0, make([]byte, roomAfter(0)+1), 1); err != nil {
		t.Fatalf("the warm write: %v", err)
	}
	waitIn(t, 5*time.Second, "the pool to settle at its last two spares", func() bool {
		pc := c.pool.Counters()
		return pc.Spares == 2 && pc.Held == 0
	})
	return c, seam, s, f
}

// admit admits one record of class k, counting it as appended when it is.
func admit(t *testing.T, c *Core, k capacity.RecordClass) (capacity.Draw, error) {
	t.Helper()
	d, err := c.headroom.Admit(k)
	if err == nil && d.Append {
		c.headroom.Drawn(d)
	}
	return d, err
}

// mustAdmit fails the test unless k is admitted for appending.
func mustAdmit(t *testing.T, c *Core, k capacity.RecordClass) {
	t.Helper()
	d, err := admit(t, c, k)
	if err != nil || !d.Append {
		t.Fatalf("%s admission = %+v, %v; want it admitted", k, d, err)
	}
}

// TestRFC1B_RecordsWithoutBytesAtTheLimit_Admission checks every class of
// record without bytes, bar content, is admitted at the journal's limit
// with no refusal naming the share, the journal or the spares, and that
// headroom_draws counts each.
func TestRFC1B_RecordsWithoutBytesAtTheLimit_Admission(t *testing.T) {
	c, _, _, _ := atTheLimit(t)
	// The classes that free nothing go first: they are refused below half.
	classes := []capacity.RecordClass{
		capacity.CloneTarget, capacity.Hold, capacity.Stamp,
		capacity.Release, capacity.Truncate, capacity.Deallocate, capacity.Delete,
		capacity.Offloaded, capacity.Unmark, capacity.Loss, capacity.Unhold,
		capacity.Forget, capacity.Reconciled,
	}
	for _, k := range classes {
		_, err := admit(t, c, k)
		var ns *core.NoSpaceError
		if errors.As(err, &ns) {
			t.Fatalf("%s at the limit was refused naming %s", k, ns.Limit)
		}
		if err != nil {
			t.Fatalf("%s at the limit: %v", k, err)
		}
	}
	if got := c.headroom.Draws(); got != uint64(len(classes)) {
		t.Fatalf("headroom_draws = %d, want %d", got, len(classes))
	}
}

// TestEdge_RecordsWithoutBytesAtMaximum checks that at the journal's
// maximum a content write whose roll the filesystem refuses reports the
// device's ENOSPC and not ErrNoSpace, and a record without bytes is still
// admitted beside it.
func TestEdge_RecordsWithoutBytesAtMaximum(t *testing.T) {
	c, seam, s, f := atTheLimit(t)
	if err := seam.Schedule(kit.Failing(kit.OpWrite, "*", 0, syscall.ENOSPC)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	_, err := c.WriteAt(context.Background(), s, f, 1<<20, make([]byte, roomAfter(recordBytes(1))+1), 1)
	if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("the write = %v, want the device's ENOSPC and not ErrNoSpace", err)
	}
	mustAdmit(t, c, capacity.Release)
}

// TestEdge_SealAtMaximum checks that at the journal's maximum a stream's
// seal writes its marker and footer, refused neither with ErrNoSpace nor
// with an error of its own, and moves neither the footprint nor the headroom.
func TestEdge_SealAtMaximum(t *testing.T) {
	c, _, _, f := atTheLimit(t)
	used := c.set.Counters().UsedBytes
	res, err := c.Streams().For(f).Seal(false)
	if errors.Is(err, core.ErrNoSpace) {
		t.Fatalf("the seal at the maximum was refused: %v", err)
	}
	if err != nil || res.TailErr != nil {
		t.Fatalf("Seal = %+v, %v; want a clean seal", res, err)
	}
	if got := c.set.Counters().UsedBytes; got != used {
		t.Fatalf("the seal moved the footprint %d -> %d", used, got)
	}
	if got := c.headroom.Draws(); got != 0 {
		t.Fatalf("the seal drew on the headroom: headroom_draws = %d", got)
	}
	mustAdmit(t, c, capacity.Release)
}

// TestRFC1B_OffloadedRecordsBelowHalfTheHeadroom_Admission checks that below
// half an Offloaded admission says not to append and moves no counter, and a
// hold, stamp and clone target are refused naming the headroom.
func TestRFC1B_OffloadedRecordsBelowHalfTheHeadroom_Admission(t *testing.T) {
	c, _, _, _ := atTheLimit(t)
	for i := 0; i < 11; i++ {
		mustAdmit(t, c, capacity.Release)
	}
	if !c.headroom.BelowHalf() {
		t.Fatal("eleven records drawn left the headroom above half")
	}
	draws := c.headroom.Draws()
	d, err := c.headroom.Admit(capacity.Offloaded)
	if err != nil || d.Append {
		t.Fatalf("Offloaded below half = %+v, %v; want admitted without an append", d, err)
	}
	c.headroom.Drawn(d)
	for _, k := range []capacity.RecordClass{capacity.Hold, capacity.Stamp, capacity.CloneTarget} {
		_, err := admit(t, c, k)
		_ = refused(t, err, core.LimitHeadroom)
	}
	if got := c.headroom.Draws(); got != draws {
		t.Fatalf("the dropped and refused admissions moved headroom_draws %d -> %d", draws, got)
	}
}

// TestRFC1B_RecordsWithoutBytesAtTheLimit_ReserveEdge checks the open
// reserve where it is half the headroom: a delete is admitted with one
// record over the reserve left, refused naming the headroom with exactly the
// reserve left, and an open's record is still admitted.
func TestRFC1B_RecordsWithoutBytesAtTheLimit_ReserveEdge(t *testing.T) {
	c, _, _, _ := atTheLimit(t)
	reserve := capacity.OpenReserveRecords(acceptanceRecords)
	// The last of these is taken with reserve+1 left: the last a delete may take.
	for i := 0; i < acceptanceRecords-reserve; i++ {
		mustAdmit(t, c, capacity.Delete)
	}
	draws := c.headroom.Draws()
	_, err := admit(t, c, capacity.Delete)
	_ = refused(t, err, core.LimitHeadroom)
	if got := c.headroom.Draws(); got != draws {
		t.Fatalf("the refusal moved headroom_draws %d -> %d", draws, got)
	}
	mustAdmit(t, c, capacity.Open)
}

// TestRFC1B_RepackBesideShareAtLimit_Admission checks a repack copy beside a
// share at its limit is admitted and draws nothing from the headroom: the
// reserve's edge, drawn to just over, is where it was after.
//
// ponytail: counting a header-only record into a share's UsedBytes is untested,
// as no appender makes one yet; add it once a removal path appends.
func TestRFC1B_RepackBesideShareAtLimit_Admission(t *testing.T) {
	c, _, _, _ := atTheLimit(t)
	lim := mustLimitShare(t, c, 2, 1)
	_ = refused(t, func() error {
		_, err := c.WriteAt(context.Background(), lim, testFile(2), 1<<20, []byte("w"), 1)
		return err
	}(), core.LimitShare)
	reserve := capacity.OpenReserveRecords(acceptanceRecords)
	for i := 0; i < acceptanceRecords-reserve-1; i++ {
		mustAdmit(t, c, capacity.Delete)
	}
	for i := 0; i < 5; i++ {
		mustAdmit(t, c, capacity.RepackCopy)
	}
	if got := c.headroom.Draws(); got != uint64(acceptanceRecords-reserve-1) {
		t.Fatalf("headroom_draws = %d: a repack copy drew", got)
	}
	mustAdmit(t, c, capacity.Delete) // still reserve+1 left
}

// TestRFC1B_HeadroomSurvivesItsSpareLeavingThePool checks the headroom is
// not read off the pool's ready spares: with the headroom spare taken, as a
// header-only roll at the limit takes it, a release and an open's record are
// still admitted and the content throttle does not refuse on that alone.
func TestRFC1B_HeadroomSurvivesItsSpareLeavingThePool(t *testing.T) {
	c, _, s, f := atTheLimit(t)
	if _, err := c.pool.Take(segment.Headroom); err != nil {
		t.Fatalf("taking the headroom spare: %v", err)
	}
	if got := c.pool.Counters().HeadroomBytes; got != 0 {
		t.Fatalf("the pool still holds %d headroom bytes after the take", got)
	}
	mustAdmit(t, c, capacity.Release)
	mustAdmit(t, c, capacity.Open)
	if c.headroom.BelowHalf() {
		t.Fatal("the headroom spare leaving the pool put the headroom below half")
	}
	// A content write is not refused naming the headroom; it may meet
	// another limit, which is not what is under test.
	_, err := c.WriteAt(context.Background(), s, f, 1<<20, []byte("w"), 1)
	var ns *core.NoSpaceError
	if errors.As(err, &ns) && ns.Limit == core.LimitHeadroom {
		t.Fatalf("the write was refused naming the headroom: %v", err)
	}
}

// TestHeadroomTarget_CountsEachStreamsSeal checks the pool's headroom target
// holds a seal's marker, footer entry and trailer per stream on top of the
// records: 2048 records at the charge fill seven segments exactly, so the two
// streams' seals need an eighth.
func TestHeadroomTarget_CountsEachStreamsSeal(t *testing.T) {
	const records = 2048 // 2048 x 224 B = 7 x 64 KiB
	c, _ := newTestCore(t, func(o *coreOptions) { o.headroomRecords = records })
	waitIn(t, 5*time.Second, "the pool to ready eight headroom spares", func() bool {
		return c.pool.Counters().HeadroomBytes >= 8*testSegmentCap
	})
}
