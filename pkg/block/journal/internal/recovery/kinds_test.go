package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// lossSink stores what the kinds pass handed Place and answers the failed
// list the check chose — an output sink, the one stand-in the testing rules
// allow: it records what the code under test delivered and answers nothing
// in a dependency's place.
type lossSink struct {
	failed  []core.Seq
	placed  []core.LossEvent
	dropped []core.Seq
}

// Failed returns the failed list the check chose, sorted as the check chose.
func (s *lossSink) Failed() []core.Seq { return s.failed }

// Place stores the event and the seq of the record it drops.
func (s *lossSink) Place(ev core.LossEvent, dropped core.Seq) {
	s.placed = append(s.placed, ev)
	s.dropped = append(s.dropped, dropped)
}

// asideRun is the rig's Run over Deps carrying the loss sink, the shape every
// kinds check drives.
func (r *rig) asideRun(sink LossSink) (State, error) {
	r.t.Helper()
	d := r.runDeps()
	d.Losses = sink
	return Run(context.Background(), d, core.Version{}, journalID, nil)
}

// forgetRec builds one forget record: it names only its share tag, no file,
// no version and no extent, and carries the seq the check chose.
func forgetRec(tag uint32, seq uint64) raw.Record {
	return rec(14, func(rc *raw.Record) {
		rc.FileID = [16]byte{}
		rc.Version = raw.Version{}
		rc.Offset, rc.Length = 0, 0
		rc.Tag = tag
		rc.Seq = seq
	})
}

// TestRunPlacesAnUnsyncedLossRecordWithEveryField holds the placement rule:
// a loss record with a decodable body publishes exactly one event through
// the sink, every field of it read from the record the disk holds — the
// header's file, extent, version and share tag, the body's reason and the
// bits it carries — and AtOpen set, while the event's number stays for the
// sink to assign. The OpenLoss it collects carries the same event and the
// seq of the record it drops, and holds no segments yet: the feed that
// collects them is not built.
func TestRunPlacesAnUnsyncedLossRecordWithEveryField(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(2)
	seg := buildSeg(id,
		rec(1, func(rc *raw.Record) { // the write the loss names, over [0,64)
			rc.Offset, rc.Length = 0, 64
			rc.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, 64)}
		}),
		rec(10, func(rc *raw.Record) { // the loss naming it, unsynced, corrupt
			rc.Offset, rc.Length = 0, 64
			rc.Seq = 101
			rc.Version = raw.Version{Counter: 201}
			rc.Loss = &raw.LossBody{Dropped: 100, Flags: 1 << 8}
		}))
	r.plantSeg(id, seg)

	sink := &lossSink{}
	st, err := r.asideRun(sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := core.LossEvent{
		Share:     7,
		File:      core.NewFileID([16]byte{0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x0a, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f}),
		Extent:    core.Extent{Off: 0, Len: 64},
		Version:   core.Version{Counter: 201},
		Reason:    core.LossCorrupt,
		Offloaded: false,
		Synced:    false,
		AtOpen:    true,
	}
	if len(sink.placed) != 1 {
		t.Fatalf("Run: the sink was handed %d loss events, want the one the loss record names: %+v", len(sink.placed), sink.placed)
	}
	if got := sink.placed[0]; got != want {
		t.Errorf("Run: the placed event got %+v, want %+v", got, want)
	}
	if got := sink.dropped; !slices.Equal(got, []core.Seq{100}) {
		t.Errorf("Run: the placed events name the dropped records %v, want [100]", got)
	}
	if len(st.Kinds.Losses) != 1 {
		t.Fatalf("Run: State.Kinds.Losses holds %d losses, want the one placed", len(st.Kinds.Losses))
	}
	if got := st.Kinds.Losses[0]; got.Ev != want || got.Dropped != 100 {
		t.Errorf("Run: the collected OpenLoss got %+v, want the placed event %+v of the dropped record 100", got, want)
	}
	if got := st.Kinds.Losses[0].Segments; !slices.Equal(got, []core.SegmentID{2}) {
		t.Errorf("Run: the collected loss holds segments %v, want [2]: the segment of the one withheld copy the feed saw", got)
	}
}

// kindsFile is the file the seeding rig's default record of kind k names, so
// a test's records and the aside records naming them agree on one file.
func kindsFile(k byte) core.FileID { return core.NewFileID(rec(k, nil).FileID) }

// writeRec builds one write record of its own numbers over [off, off+n),
// carrying the payload its length names so a verification pass can check it.
func writeRec(seq, counter uint64, off, n int64) raw.Record {
	return rec(1, func(rc *raw.Record) {
		rc.Seq, rc.Version = seq, raw.Version{Counter: counter}
		rc.Offset, rc.Length = uint64(off), uint64(n)
		rc.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, n)}
	})
}

// lossRec builds one loss record: its header names the file, extent and
// version its event reads, and its body the dropped seq and the flag word.
func lossRec(file core.FileID, dropped, seq, counter uint64, off, n int64, flags uint64) raw.Record {
	return rec(10, func(rc *raw.Record) {
		rc.FileID = file.Bytes()
		rc.Seq, rc.Version = seq, raw.Version{Counter: counter}
		rc.Offset, rc.Length = uint64(off), uint64(n)
		rc.Loss = &raw.LossBody{Dropped: dropped, Flags: flags}
	})
}

// kindsRun opens the rig's journal with no loss sink and returns the state,
// refusing the test on the open's own refusal.
func kindsRun(t *testing.T, r *rig) State {
	t.Helper()
	st, err := r.asideRun(nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

// TestRunWithholdsTheWholeRecordAnUnsyncedLossNames holds the feed filter's
// whole-loss rule: a loss record whose payload says unsynced names a record
// by sequence number, so the record is withheld whole however intact its
// payload — repaired bytes under a position the loss named still belong to a
// record the journal reported lost — while the write it superseded, Q over
// the same extent, feeds again at its own version, precedence as if R never
// was. An unsynced loss cuts nothing and marks nothing.
func TestRunWithholdsTheWholeRecordAnUnsyncedLossNames(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	seg := buildSeg(2,
		writeRec(10, 3, 0, 64),                  // Q at v3
		writeRec(11, 5, 0, 64),                  // R at v5, the record the loss names
		lossRec(file, 11, 12, 201, 0, 64, 1<<8), // unsynced, corrupt
	)
	r.plantSeg(2, seg)

	st := kindsRun(t, r)

	p, ok := st.Rebuilt.Index.Lookup(file, 0)
	if !ok {
		t.Fatalf("Lookup over [0,64) missed, want Q re-held at its own version")
	}
	if p.Version.Counter != 3 {
		t.Errorf("the record over [0,64) holds version %d, want 3: R is withheld whole and Q re-held, precedence as if R never was", p.Version.Counter)
	}
	for _, ch := range st.Rebuilt.Index.Since(file, core.Version{}) {
		if ch.Kind != core.ChangeWrite {
			t.Errorf("Since yielded %+v over a file an unsynced loss names, want the held write alone: an unsynced loss cuts nothing and marks nothing", ch)
		}
	}
	if got := st.Kinds.Losses[0].Segments; !slices.Equal(got, []core.SegmentID{2}) {
		t.Errorf("the loss names segments %v, want [2]: the home of the one withheld copy", got)
	}
}

// TestRunWithholdsEveryCopyOfALossNamedRecord holds the by-sequence reach of
// a loss record against copies: two healthy copies of the named record sit
// in two segments and neither feeds — a surviving twin would resurrect a
// loss the journal already reported — so the rebuilt index holds nothing of
// the file and no marker or cut brings any copy back.
func TestRunWithholdsEveryCopyOfALossNamedRecord(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	r.plantSeg(1, buildSeg(1, writeRec(11, 5, 0, 64)))
	r.plantSeg(2, buildSeg(2, writeRec(11, 5, 0, 64))) // the carried copy
	r.plantSeg(3, buildSeg(3, lossRec(file, 11, 12, 201, 0, 64, 1<<8)))

	st := kindsRun(t, r)

	if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup over the loss-named record's extent hit, want a miss: a twin does not resurrect a reported loss")
	}
	if got := st.Rebuilt.Index.FileEntries(file); len(got) != 0 {
		t.Errorf("the index holds %d entries of the loss-named file, want none: every copy is withheld", len(got))
	}
	if got := st.Kinds.Losses[0].Segments; !slices.Equal(got, []core.SegmentID{1, 2}) {
		t.Errorf("the loss names segments %v, want [1 2]: the home of every withheld copy", got)
	}
}

// TestRunWithholdsAForgottenTagBelowTheForgetsSeq holds the forget rule over
// the feed: no record of the forgotten tag below the forget's seq feeds,
// whatever its kind — writes, a truncate, an offloaded mark — so the withheld
// truncate builds no removal marker and the rebuilt file of the tag holds
// nothing below the forget, while a write above the forget's seq feeds
// normally. The forget collects the home segment of every record it withheld.
func TestRunWithholdsAForgottenTagBelowTheForgetsSeq(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	truncate := rec(4, func(rc *raw.Record) {
		rc.Seq, rc.Version = 13, raw.Version{Counter: 13}
		rc.Offset, rc.Length = 0x100, 0
	})
	offloaded := rec(8, func(rc *raw.Record) {
		rc.Seq, rc.Version = 14, raw.Version{Counter: 14}
		rc.Offset, rc.Length = 0x200, 16
	})
	r.plantSeg(1, buildSeg(1, writeRec(10, 10, 0, 16), writeRec(11, 11, 0x80, 16), truncate))
	r.plantSeg(2, buildSeg(2, writeRec(12, 12, 0x300, 16), offloaded))
	r.plantSeg(3, buildSeg(3, forgetRec(7, 30), writeRec(40, 40, 0x400, 16)))

	st := kindsRun(t, r)

	for _, off := range []int64{0, 0x80, 0x100, 0x200, 0x300} {
		if _, ok := st.Rebuilt.Index.Lookup(file, off); ok {
			t.Errorf("Lookup at %d hit, want a miss: the record of the tag is below the forget's seq and withholds", off)
		}
	}
	for _, ch := range st.Rebuilt.Index.Since(file, core.Version{}) {
		if ch.Kind != core.ChangeWrite {
			t.Errorf("Since yielded %+v over a forgotten file, want the held write above the forget alone: a withheld truncate builds no marker", ch)
		}
	}
	if p, ok := st.Rebuilt.Index.Lookup(file, 0x400); !ok {
		t.Fatalf("Lookup at 0x400 missed, want the write above the forget's seq to feed")
	} else if p.Seq != 40 {
		t.Errorf("the record at 0x400 holds seq %d, want 40: a write above the forget feeds with its own numbers", p.Seq)
	}
	if len(st.Kinds.Forgets) != 1 {
		t.Fatalf("State.Kinds.Forgets holds %d entries, want the one forget of the tag", len(st.Kinds.Forgets))
	}
	if got := st.Kinds.Forgets[0]; got.Tag != 7 || got.Seq != 30 {
		t.Errorf("the forget got tag %d at seq %d, want tag 7 at seq 30", got.Tag, got.Seq)
	}
	if got := st.Kinds.Forgets[0].Segments; !slices.Equal(got, []core.SegmentID{1, 2}) {
		t.Errorf("the forget of tag 7 names segments %v, want [1 2]: the home of every record it withheld", got)
	}
}

// TestRunWithholdsARecordTheFailedListNamesAsAReadOnlyDrop holds the
// read-only-failure rule: each seq the loss sink's failed list names that a
// scan finds on disk withholds whole — in every segment holding it, one note
// per seq — and the note carries the found record's own file, extent,
// version and tag with the reason a failed sync window takes, unsynced. No
// ring event is raised for it: Place is never handed one.
func TestRunWithholdsARecordTheFailedListNamesAsAReadOnlyDrop(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	failed := writeRec(77, 5, 0, 32)
	r.plantSeg(1, buildSeg(1, failed))
	r.plantSeg(2, buildSeg(2, writeRec(77, 5, 0, 32), writeRec(78, 6, 0x4100, 16))) // the copy, and G feeding

	sink := &lossSink{failed: []core.Seq{77}}
	st, err := r.asideRun(sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup over the failed record's extent hit, want a miss: the failed seq withholds whole")
	}
	if p, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok {
		t.Fatalf("Lookup at 0x4100 missed, want the record no rule names to feed")
	} else if p.Seq != 78 {
		t.Errorf("the record at 0x4100 holds seq %d, want 78", p.Seq)
	}
	if len(sink.placed) != 0 {
		t.Errorf("the sink was handed %d loss events for a failed-list drop, want none: no ring event and no generation raise", len(sink.placed))
	}
	if len(st.Kinds.ReadOnlyDrops) != 1 {
		t.Fatalf("State.Kinds.ReadOnlyDrops holds %d drops, want the one note of the failed seq: %+v", len(st.Kinds.ReadOnlyDrops), st.Kinds.ReadOnlyDrops)
	}
	got := st.Kinds.ReadOnlyDrops[0]
	want := ReadOnlyDrop{
		Ev: core.LossEvent{
			Share:   7,
			File:    file,
			Extent:  core.Extent{Off: 0, Len: 32},
			Version: core.Version{Counter: 5},
			Reason:  core.LossFailedWindow,
		},
		Dropped: 77,
	}
	if got.Ev != want.Ev || got.Dropped != want.Dropped {
		t.Errorf("the read-only drop got %+v of seq %d, want %+v of seq 77: the found record's own header with the failed-window reason, unsynced", got, got.Dropped, want)
	}
	if !slices.Equal(got.Segments, []core.SegmentID{1, 2}) {
		t.Errorf("the read-only drop names segments %v, want [1 2]: every segment still holding the record", got.Segments)
	}
}

// TestRunFeedsTheRemainderASyncedSupersededLossNames holds the clip rule over
// content: a superseded loss reaches the record it names only over the loss's
// own extent, so the record's runs outside the union of the extents naming
// it feed clipped under its own numbers, the runs inside it hold nothing,
// a removal the same loss names feeds whole — its own marker runs to the end
// of the file — and a record the loss extents cover whole feeds nothing at
// all, folding its numbers into Withheld for the counters that open above
// what the feed saw.
func TestRunFeedsTheRemainderASyncedSupersededLossNames(t *testing.T) {
	t.Run("the kept runs of an intact record", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		file := kindsFile(1)
		r.plantSeg(2, buildSeg(2,
			writeRec(60, 5, 0, 128),
			lossRec(file, 60, 61, 9, 0, 64, 1|4<<8), // synced, superseded
		))

		st := kindsRun(t, r)

		if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
			t.Errorf("Lookup at 0 hit, want a miss: the loss's extent covers the record's first half")
		}
		p, ok := st.Rebuilt.Index.Lookup(file, 64)
		if !ok {
			t.Fatalf("Lookup at 64 missed, want the kept run [64,128) to feed clipped")
		}
		if p.Extent != (core.Extent{Off: 64, Len: 64}) || p.Seq != 60 {
			t.Errorf("the kept run got %+v of seq %d, want the clipped extent [64,128) under the record's own seq 60", p.Extent, p.Seq)
		}
		if got := st.Kinds.Losses[0].Segments; !slices.Equal(got, []core.SegmentID{2}) {
			t.Errorf("the loss names segments %v, want [2]: superseded or whole, the record it names is its liveness", got)
		}
	})
	t.Run("a removal the loss names feeds whole", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		file := kindsFile(4)
		r.plantSeg(1, buildSeg(1,
			rec(4, func(rc *raw.Record) { // a truncate: Off is the new size
				rc.Seq, rc.Version = 62, raw.Version{Counter: 62}
				rc.Offset, rc.Length = 0x100, 0
			}),
			lossRec(file, 62, 63, 9, 0x40, 16, 1|4<<8),
		))

		st := kindsRun(t, r)

		var truncate int
		for _, ch := range st.Rebuilt.Index.Since(file, core.Version{}) {
			if ch.Kind == core.ChangeTruncate {
				truncate++
			}
		}
		if truncate != 1 {
			t.Errorf("Since yielded %d truncate markers, want the one the removal record left: a superseded loss naming a removal clips nothing", truncate)
		}
	})
	t.Run("a record the loss extents cover whole", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		file := kindsFile(1)
		r.plantSeg(2, buildSeg(2,
			writeRec(60, 5, 0, 64),
			lossRec(file, 60, 61, 9, 0, 64, 1|4<<8),
		))

		st := kindsRun(t, r)

		if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
			t.Errorf("Lookup at 0 hit, want a miss: the loss extent covers the record whole, so no run feeds")
		}
		if got, want := st.Kinds.Withheld, (index.Numbers{Version: core.Version{Counter: 5}, Seq: 60}); got != want {
			t.Errorf("Withheld got %+v, want %+v: the numbers of the whole record no run fed", got, want)
		}
	})
}

// TestRunAppliesASyncedLossRecordAsItsCut holds the cut each synced loss
// record applies over the index the rebuild finished: its extent and version
// come from the record's own header, dropping every record below the loss's
// version inside its extent — the older write Q under the withheld R — and
// the reason decides what the cut leaves: every reason but superseded leaves
// an unsettled loss marker that settling at the loss's version stops, while
// a superseded one cuts the same extent leaving nothing that settling would
// answer for.
func TestRunAppliesASyncedLossRecordAsItsCut(t *testing.T) {
	for _, row := range []struct {
		name   string
		flags  uint64
		yields bool
	}{
		{"a corrupt loss leaves its marker unsettled", 1 | 1<<8, true},
		{"a superseded loss cuts leaving no marker", 1 | 4<<8, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			file := kindsFile(1)
			r.plantSeg(2, buildSeg(2,
				writeRec(10, 3, 0, 64), // Q at v3, below the loss's version
				writeRec(11, 5, 0, 64), // R at v5, withheld by the feed
				lossRec(file, 11, 12, 9, 0, 64, row.flags),
			))

			st := kindsRun(t, r)

			if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
				t.Errorf("Lookup over the loss's extent hit, want the cut to drop every record below the loss's version there")
			}
			changes := st.Rebuilt.Index.Since(file, core.Version{})
			if !row.yields && len(changes) != 0 {
				t.Fatalf("Since yielded %+v after a superseded cut, want nothing: the cut drops every record below its version and leaves no marker for itself", changes)
			}
			if !row.yields {
				return
			}
			for _, ch := range changes {
				if ch.Kind != core.ChangeLoss {
					t.Fatalf("Since got %+v among the loss's markers, want only the loss's own: the cut dropped every held record", ch)
				}
			}
			if len(changes) != 1 || changes[0].Version.Counter != 9 || changes[0].Extent != (core.Extent{Off: 0, Len: 64}) {
				t.Fatalf("the loss markers got %+v, want the single one at v9 over [0,64), unsettled", changes)
			}
			st.Rebuilt.Index.SettleFor(7, file, core.Version{Counter: 9})
			if changes := st.Rebuilt.Index.Since(file, core.Version{}); len(changes) != 0 {
				t.Errorf("Since yielded %+v after settling at the loss's version, want the marker settled and nothing held behind it", changes)
			}
		})
	}
}

// TestRunCutsTwoSyncedLossesIntoAVersionOrderedMarkerSet holds the cuts'
// determinism: two synced loss records read in the order opposite their
// versions — the higher-version one first — cut the same rebuilt file into
// the same marker set, and Since answers with both markers in version order,
// whatever order the decode delivered them in.
func TestRunCutsTwoSyncedLossesIntoAVersionOrderedMarkerSet(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	r.plantSeg(1, buildSeg(1, writeRec(10, 3, 0, 128)))
	r.plantSeg(2, buildSeg(2, lossRec(file, 10, 12, 11, 0, 64, 1|1<<8))) // v11, read first
	r.plantSeg(3, buildSeg(3, lossRec(file, 10, 13, 9, 64, 64, 1|1<<8))) // v9, read second

	st := kindsRun(t, r)

	var markers []index.Change
	for _, ch := range st.Rebuilt.Index.Since(file, core.Version{}) {
		if ch.Kind == core.ChangeLoss {
			markers = append(markers, ch)
		}
	}
	if len(markers) != 2 {
		t.Fatalf("Since yielded %d loss markers, want the two the records name: %+v", len(markers), markers)
	}
	if markers[0].Version.Counter != 9 || markers[0].Extent != (core.Extent{Off: 64, Len: 64}) {
		t.Errorf("the first marker got v%d over %+v, want the lower version 9 over [64,128)", markers[0].Version.Counter, markers[0].Extent)
	}
	if markers[1].Version.Counter != 11 || markers[1].Extent != (core.Extent{Off: 0, Len: 64}) {
		t.Errorf("the second marker got v%d over %+v, want the higher version 11 over [0,64)", markers[1].Version.Counter, markers[1].Extent)
	}
	for _, off := range []int64{0, 64} {
		if _, ok := st.Rebuilt.Index.Lookup(file, off); ok {
			t.Errorf("Lookup at %d hit, want the cuts to drop the whole extent the two losses cover", off)
		}
	}
}

// TestRunDeliversALossForASeqNoSegmentHolds holds the retired-record row: a
// loss record may name a sequence number no segment holds — the record it
// dropped retired with a repack — and the open neither crashes nor refuses:
// the event still places and, when the loss is synced, its cut still applies,
// for the unsettled marker is the thing Since answers for the loss with.
func TestRunDeliversALossForASeqNoSegmentHolds(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(10)                                                          // no record of the loss's own file is planted
	r.plantSeg(1, buildSeg(1, lossRec(file, 424242, 12, 201, 0x4100, 16, 1|1<<8))) // synced, corrupt

	sink := &lossSink{}
	st, err := r.asideRun(sink)
	if err != nil {
		t.Fatalf("Run over a loss naming a seq no segment holds: %v, want the open to succeed", err)
	}
	if len(sink.placed) != 1 {
		t.Fatalf("the sink was handed %d loss events, want the one the record names", len(sink.placed))
	}
	changes := st.Rebuilt.Index.Since(file, core.Version{})
	if len(changes) != 1 || changes[0].Kind != core.ChangeLoss {
		t.Fatalf("Since yielded %+v over the loss's file, want its single unsettled loss marker", changes)
	}
	if c := changes[0]; c.Version.Counter != 201 || c.Extent != (core.Extent{Off: 0x4100, Len: 16}) {
		t.Errorf("the loss marker got v%d over %+v, want 201 over [0x4100,16)", c.Version.Counter, c.Extent)
	}
}

// TestRunServesABadCopyFromItsLiveTwin holds the bad-copy rule over the open:
// a record whose original rots — here its kept superseded run, the one
// payload an open reads — is confined as corrupt on its own segment, and a
// re-appended copy the intact segment holds with the same sequence number
// serves what the drop guarded. The subtraction empties Corrupt of the drop
// — no loss record names a record a live copy contradicts — while Damaged
// stands as the classing judged it, and the piece reads back naming the good
// segment, the withheld rotten original never feeding. The same geometry
// over a torn tail instead of an ended slot answers the same.
func TestRunServesABadCopyFromItsLiveTwin(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail func(id core.SegmentID, b []byte) []byte // the original's segment, after its first record
	}{
		{"an ended segment", func(id core.SegmentID, b []byte) []byte {
			// A zero slot mid-segment, then a verifying record whose
			// synced-through stays at the slot's start: the slot was never
			// proven, so a torn slot ends the segment.
			slot := int64(len(b))
			b = append(b, make([]byte, 16)...)
			after := writeRec(70, 6, 0x10000, 16)
			after.Identity, after.Segment = journalID, uint64(id)
			after.SyncedThrough = uint64(slot) // at the slot's start: the slot lies past it
			after.RecordOffset = uint32(len(b))
			return append(b, after.Bytes()...)
		}},
		{"a torn tail", func(id core.SegmentID, b []byte) []byte {
			return append(b, bytes.Repeat([]byte{0xff}, 16)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			file := kindsFile(1)
			id := core.SegmentID(1)
			seg := buildSeg(id, writeRec(60, 5, 0, 128))
			seg[format.SegmentHeaderSize+format.RecordHeaderSize+8+70] ^= 0xff // the payload rots
			r.plantSeg(id, tc.tail(id, seg))
			// The re-appended copy, healthy, in an intact segment of its own.
			r.plantSeg(2, buildSeg(2, writeRec(60, 5, 0, 128)))
			// The synced superseded loss whose kept run [64,128) the original rots in.
			r.plantSeg(3, buildSeg(3, lossRec(file, 60, 61, 9, 0, 64, 1|4<<8)))

			st := kindsRun(t, r)

			if fate := fateOf(t, st, 1); fate.Kind != fateEnded && fate.Kind != fateTornTail {
				t.Fatalf("segment 1's fate got %v, want ended or torn-tail", fate.Kind)
			}
			if got := st.Tails.Corrupt; len(got) != 0 {
				t.Fatalf("Tails.Corrupt got %+v, want it emptied of the original a live twin contradicts", got)
			}
			if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
				t.Errorf("Tails.Damaged got %v, want [1]: the subtraction leaves the classing's verdict alone", st.Tails.Damaged)
			}
			p, ok := st.Rebuilt.Index.Lookup(file, 64)
			if !ok {
				t.Fatalf("Lookup at 64 missed, want the kept run of the healthy copy to feed")
			}
			if p.Record.Segment != 2 {
				t.Errorf("the kept run reads from segment %d at %d, want segment 2, the good copy's home", p.Record.Segment, p.Record.Offset)
			}
			if p.Seq != 60 || p.Extent != (core.Extent{Off: 64, Len: 64}) {
				t.Errorf("the kept run got seq %d over %+v, want 60 over [64,128), the clipped twin's own numbers", p.Seq, p.Extent)
			}
			if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
				t.Errorf("Lookup at 0 hit, want a miss: the loss's extent covers it and the rotten original never feeds")
			}
		})
	}
}

// TestRunRepointsAPieceOutOfASegmentTheOpenEnds holds the copy preference on
// healthy equal-number copies: the rebuild's collapse keeps the one at the
// greater (segment, offset), and when that copy sits in a segment the open
// ends — torn-tail here — while another fed copy sits in a segment the open
// keeps, the built piece is repointed to the greatest kept copy, so the read
// that serves it names a segment the open holds open.
func TestRunRepointsAPieceOutOfASegmentTheOpenEnds(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	seg := buildSeg(9, writeRec(80, 5, 0, 64))
	seg = append(seg, bytes.Repeat([]byte{0xff}, 16)...) // the torn tail that ends segment 9
	r.plantSeg(9, seg)
	r.plantSeg(2, buildSeg(2, writeRec(80, 5, 0, 64))) // the kept twin, in a segment the open keeps

	st := kindsRun(t, r)

	if fate := fateOf(t, st, 9); fate.Kind != fateTornTail {
		t.Fatalf("segment 9's fate got %v, want torn-tail", fate.Kind)
	}
	if fate := fateOf(t, st, 2); fate.Kind != fateIntact {
		t.Fatalf("segment 2's fate got %v, want intact", fate.Kind)
	}
	p, ok := st.Rebuilt.Index.Lookup(file, 0)
	if !ok {
		t.Fatalf("Lookup over the copied record's extent missed, want the piece held")
	}
	if p.Record != (index.RecordAt{Segment: 2, Offset: uint32(format.SegmentHeaderSize)}) {
		t.Errorf("the piece reads from %+v, want {2 %d}: the greatest kept-segment copy, repointed out of the ended segment 9", p.Record, format.SegmentHeaderSize)
	}
	if p.Seq != 80 {
		t.Errorf("the repointed piece carries seq %d, want 80: a copy keeps its record's numbers", p.Seq)
	}
}

// TestRunWithheldNumbersFoldBothAxesIndependently holds the counters' net:
// the withheld numbers fold by their two axes, the highest sequence number
// and the highest version among the withheld independently of each other —
// no one record need carry both, and the pair the fold answers with is one
// no record ever held, for the counters a later pass raises must open above
// every number on both axes the feed never saw.
func TestRunWithheldNumbersFoldBothAxesIndependently(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	r.plantSeg(1, buildSeg(1,
		writeRec(1000, 10, 0, 16), // withheld: the greatest seq, a low version
		lossRec(file, 1000, 12, 201, 0, 16, 1<<8),
		writeRec(500, 4, 0x4100, 16), // feeding, past every rule
	))
	r.plantSeg(2, buildSeg(2,
		writeRec(100, 900, 0x100, 16), // withheld: a low seq, the greatest version
		lossRec(file, 100, 13, 202, 0x100, 16, 1<<8),
	))

	st := kindsRun(t, r)

	if got, want := st.Kinds.Withheld, (index.Numbers{Version: core.Version{Counter: 900}, Seq: 1000}); got != want {
		t.Errorf("Withheld got %+v, want %+v: the greatest seq of one withheld record with the greatest version of another, folded independently", got, want)
	}
	if p, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok {
		t.Errorf("Lookup at 0x4100 missed, want the record no rule names to feed")
	} else if p.Seq != 500 {
		t.Errorf("the fed record at 0x4100 carries seq %d, want 500", p.Seq)
	}
}

// everyReadFS wraps one FS and counts every ReadAt each of its files served,
// whatever the answer — a zero-byte probe past a file's end, the attach
// pass's header read at 0 — so the count is the same one a fault schedule's
// From keys on, 1:1 over the calls the seam sees. It is a pass-through
// wrapper in the recording lineage of readsOf that records all of them
// rather than the reads that returned bytes: a check that arms a fault at
// an ordinal needs the count, not the payload acceptance's bytes.
type everyReadFS struct {
	storage.FS

	mu    sync.Mutex
	calls []oneRead
}

func newEveryReadFS(fs storage.FS) *everyReadFS { return &everyReadFS{FS: fs} }

// Open returns the seam's own file wrapped to record every read of it.
func (e *everyReadFS) Open(name string) (storage.File, error) {
	f, err := e.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return everyReadFile{File: f, name: name, of: e}, nil
}

// saw returns every recorded read, in the order the calls were made.
func (e *everyReadFS) saw() []oneRead {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

// everyReadFile is one opened file whose reads its FS records whole.
type everyReadFile struct {
	storage.File
	name string
	of   *everyReadFS
}

// ReadAt forwards to the file the seam served and records the call, whatever
// it answers: the count a fault schedule keys on counts the attempt itself.
func (f everyReadFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.of.mu.Lock()
	f.of.calls = append(f.of.calls, oneRead{name: f.name, off: off, n: int64(len(p))})
	f.of.mu.Unlock()
	return n, err
}

// TestRunRefusesAReadTheRemainderVerifyCannotComplete holds the I/O leg of
// the partly-superseded check: the verify reads its named record's header
// and body through the open's bound, and a read no bounded attempt cleared
// refuses the open on the range the reader collected — never a corrupt
// verdict, for the kept runs confine only on bytes the bound did return. The
// fault is armed at the verify read's own ordinal, measured over the same
// directory first: the reads of one segment file are sequential and
// deterministic across the phases (the attach header, the scan walk, the
// aside body, then the verify's own header and body read), so the count the
// un-faulted run made names the call the fault answers, and the verify's
// header read is the last read of the record's whole first header the run
// makes.
func TestRunRefusesAReadTheRemainderVerifyCannotComplete(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	id := core.SegmentID(5)
	name := format.SegmentName(id)
	r.plantSeg(id, buildSeg(id,
		writeRec(60, 5, 0, 128), // the payload is intact: only the fault can fail the verify
		lossRec(file, 60, 61, 9, 0, 64, 1|4<<8),
	))

	// Measure: the same open over the recorder, unfaulted, names the verify
	// header read's ordinal among the segment's reads.
	measured := newEveryReadFS(r.seam.FS())
	d := r.runDeps()
	d.FS = measured
	if _, err := Run(context.Background(), d, core.Version{}, journalID, nil); err != nil {
		t.Fatalf("Run unfaulted: %v", err)
	}
	total, verify := 0, 0
	for _, one := range measured.saw() {
		if one.name != name {
			continue
		}
		total++
		if one.off == format.SegmentHeaderSize && one.n == format.RecordHeaderSize {
			verify = total // the last read of the record's whole first header: the verify's own
		}
	}
	if verify == 0 {
		t.Fatalf("the unfaulted run made no read of the record's whole first header, want the verify's header read among them")
	}
	if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, name, verify, 0, syscall.EIO)); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Run over the read the verify could not complete got err %v, want an UnreadableRangesError naming the range no bound cleared", err)
	}
	if errors.Is(err, core.ErrCorrupt) {
		t.Errorf("the refusal %v matches ErrCorrupt, want ErrUnreadable alone: a read the bound never cleared earns no verdict on bytes it never returned", err)
	}
	want := []core.SegmentRange{{Segment: id, Off: format.SegmentHeaderSize, Len: format.RecordHeaderSize}}
	if !slices.Equal(ure.Ranges, want) {
		t.Errorf("the refusal's ranges got %+v, want exactly the verify's own header read %+v", ure.Ranges, want)
	}
}

// TestRunSkipsTheRemainderVerifyOfARecordAnotherRuleDropsWhole holds the one
// whole-withhold answer over both phases: a record the failed list, a whole
// loss name or a forget of its tag above its seq withholds whole, so no run
// of it ever feeds and the verify reads none of it — a rotten record
// another rule already holds whole confines nowhere, its corrupt drop would
// name a record the feed serves nothing of, and every open would confine it
// fresh again, for its kept runs never feed and so never meet the read that
// would catch them.
func TestRunSkipsTheRemainderVerifyOfARecordAnotherRuleDropsWhole(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	id := core.SegmentID(1)
	seg := buildSeg(id,
		writeRec(60, 5, 0, 128),
		lossRec(file, 60, 61, 9, 0, 64, 1|4<<8), // synced, superseded: the kept run is [64,128)
		lossRec(file, 60, 62, 10, 0, 128, 1<<8), // unsynced, corrupt: the record drops whole
	)
	// One payload byte rotted: the kept run verifies as badly as the rest.
	seg[format.SegmentHeaderSize+format.RecordHeaderSize+8+70] ^= 0xff
	r.plantSeg(id, seg)

	st := kindsRun(t, r)

	if got := st.Tails.Corrupt; len(got) != 0 {
		t.Fatalf("Tails.Corrupt got %+v over a record another rule withholds whole, want none: the verify reads no run of it", got)
	}
	if len(st.Tails.Damaged) != 0 {
		t.Errorf("Tails.Damaged got %v, want none: nothing confined on the segment", st.Tails.Damaged)
	}
	for _, off := range []int64{0, 64} {
		if _, ok := st.Rebuilt.Index.Lookup(file, off); ok {
			t.Errorf("Lookup at %d hit, want a miss: the record withholds whole under the unsynced loss", off)
		}
	}
	if len(st.Kinds.Losses) != 2 {
		t.Errorf("State.Kinds.Losses holds %d losses, want the two: the superseded name and the whole name of the same record", len(st.Kinds.Losses))
	}
}

// TestRunKeepsAConfinedTwinFromSubtractingItsEqualDrop holds the live set a
// subtracted drop must answer to: the copy the open itself found rotten is
// not live — it confined, it fed nothing — so two rotten copies of one
// seq in two segments the open keeps subtract nothing: each drop would
// otherwise subtract the other's, both would vanish with no loss record
// ever written for them, and the sync that cut the kept run away would
// leave bytes the open never re-verifies serving in its place.
func TestRunKeepsAConfinedTwinFromSubtractingItsEqualDrop(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	for _, id := range []core.SegmentID{1, 2} {
		var seg []byte
		if id == 1 {
			seg = buildSeg(id, writeRec(60, 5, 0, 128), lossRec(file, 60, 61, 9, 0, 64, 1|4<<8))
		} else {
			seg = buildSeg(id, writeRec(60, 5, 0, 128))
		}
		// One byte of the payload rotted after its writer built it.
		seg[format.SegmentHeaderSize+format.RecordHeaderSize+8+70] ^= 0xff
		r.plantSeg(id, seg)
	}

	st := kindsRun(t, r)

	if len(st.Tails.Corrupt) != 2 {
		t.Fatalf("Tails.Corrupt got %+v over two rotten twins, want the drop of each: a confined copy is not a live one", st.Tails.Corrupt)
	}
	if got := st.Tails.Corrupt[0].Segment; got != 1 {
		t.Errorf("the first corrupt drop got segment %d, want 1", got)
	}
	if got := st.Tails.Corrupt[1].Segment; got != 2 {
		t.Errorf("the second corrupt drop got segment %d, want 2", got)
	}
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1, 2}) {
		t.Errorf("Tails.Damaged got %v, want [1 2]: the classing judged both", st.Tails.Damaged)
	}
	for _, off := range []int64{0, 64} {
		if _, ok := st.Rebuilt.Index.Lookup(file, off); ok {
			t.Errorf("Lookup at %d hit, want a miss: both rotten copies withhold by location", off)
		}
	}
}

// TestRunDoesNotLetACoveredForgetCoverAnything holds the uniform no-effect
// of rule 6 over the forgets: a forget the failed list covers covers
// nothing itself, so a loss of its tag below its seq keeps its whole
// withhold — the named record withholds by the loss — and a hold of the
// tag below the forget refuses the open again, for no rule covers it. With
// the covered forget covered by a whole loss name the answer is the same
// round: the forget loses its cover of the loss somewhere else, and the
// loss finds its own effect again.
func TestRunDoesNotLetACoveredForgetCoverAnything(t *testing.T) {
	t.Run("the named record withholds by the loss", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		file := kindsFile(1)
		r.plantSeg(1, buildSeg(1,
			writeRec(20, 3, 0, 16),                // the record the loss names
			forgetRec(7, 10),                      // the forget the failed list covers
			lossRec(file, 20, 5, 21, 0, 16, 1<<8), // the loss of the tag below the forget
		))

		sink := &lossSink{failed: []core.Seq{10}}
		st, err := r.asideRun(sink)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}

		if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
			t.Errorf("Lookup over the loss's extent hit, want a miss: the covered forget covers nothing, so the loss withholds the record")
		}
		if len(st.Kinds.Losses) != 1 || st.Kinds.Losses[0].Dropped != 20 {
			t.Errorf("State.Kinds.Losses got %+v, want the one loss naming the record: the covered forget does not cover it", st.Kinds.Losses)
		}
		if got := st.Kinds.Forgets; len(got) != 0 {
			t.Errorf("State.Kinds.Forgets got %+v, want none: a covered forget holds no effect anywhere", got)
		}
	})
	t.Run("a hold of the tag refuses the open again", func(t *testing.T) {
		r := newRig(t)
		r.writeFormat()
		r.plantSeg(1, buildSeg(1,
			rec(11, func(rc *raw.Record) { // the hold below the covered forget
				rc.Seq = 7
				rc.Offset, rc.Length = 0, 0
				rc.Cut = &raw.CutBody{Cut: 3}
			}),
			forgetRec(7, 10),
		))

		sink := &lossSink{failed: []core.Seq{10}}
		if _, err := r.asideRun(sink); !errors.Is(err, core.ErrNotImplemented) {
			t.Fatalf("Run over a hold below a covered forget got err %v, want ErrNotImplemented: the covered forget covers nothing, so no rule covers the hold", err)
		}
	})
}

// TestRunConfinesARottenSupersededRemainderAsCorrupt holds the open's check
// of a partly-superseded record: its kept runs are bytes no serving read
// would ever verify again — the loss stopped naming them — so the open reads
// the record's whole payload and, when it fails, confines the kept runs as
// one corrupt drop on their own scan, judged like any other dropped record:
// corrupt and damaged, the drop's synced bit the loss payload carried, its
// extent the kept run rather than the loss's. The record withholds whole,
// held by nothing.
func TestRunConfinesARottenSupersededRemainderAsCorrupt(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	id := core.SegmentID(1)
	seg := buildSeg(id,
		writeRec(60, 5, 0, 128),
		lossRec(file, 60, 61, 9, 0, 64, 1|4<<8), // the kept run is [64,128)
	)
	// One payload byte of the record rotted after its writer built it.
	seg[format.SegmentHeaderSize+format.RecordHeaderSize+8+70] ^= 0xff
	r.plantSeg(id, seg)

	st := kindsRun(t, r)

	if len(st.Tails.Corrupt) != 1 {
		t.Fatalf("Tails.Corrupt got %+v over a rotten kept run, want the one corrupt drop of it", st.Tails.Corrupt)
	}
	c := st.Tails.Corrupt[0]
	if c.Segment != 1 {
		t.Errorf("the corrupt drop got segment %d, want 1, the record's own", c.Segment)
	}
	if !c.Drop.Synced {
		t.Errorf("the corrupt drop got Synced false, want true: the synced bit the superseded loss payload carried")
	}
	if c.Drop.Header.Extent != (core.Extent{Off: 64, Len: 64}) {
		t.Errorf("the corrupt drop's extent got %+v, want the kept run [64,128): the append a later open makes names the run, not the loss's own extent", c.Drop.Header.Extent)
	}
	if c.at() != format.SegmentHeaderSize {
		t.Errorf("the corrupt drop names offset %d, want %d, where the withheld record sits", c.at(), format.SegmentHeaderSize)
	}
	if !slices.Equal(st.Tails.Damaged, []core.SegmentID{1}) {
		t.Errorf("Tails.Damaged got %v, want [1]: the classing stays as it judged", st.Tails.Damaged)
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup at 0 hit, want a miss: the rotten record withholds whole")
	}
	if _, ok := st.Rebuilt.Index.Lookup(file, 64); ok {
		t.Errorf("Lookup at 64 hit, want a miss: the kept run confined as corrupt holds nothing")
	}
	if got, want := st.Kinds.Withheld, (index.Numbers{Version: core.Version{Counter: 5}, Seq: 60}); got != want {
		t.Errorf("Withheld got %+v, want %+v: the numbers of the confined record", got, want)
	}
}

// TestRunKeepsTheVictimOfACoveredCovererWithItsOwnCover holds the chain
// cover: a rule covered by another covers nothing itself, so the records it
// would have covered keep their own covers. Three whole losses name one
// another in a chain — Z at seq 500 names Y at 400, Y names X at 300, X
// names R at 100: Z, covered by nothing, withholds Y's effect; Y, covered
// by Z, has none of its own over X; X, judged after Y lost its own cover,
// keeps its effect — so R, the victim twice removed of a cover that a cover
// cancelled, withholds whole. A round that locks the chain's first links
// out together would leave X covered too, and R would feed.
func TestRunKeepsTheVictimOfACoveredCovererWithItsOwnCover(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	r.plantSeg(1, buildSeg(1,
		writeRec(100, 3, 0, 16),                   // R, the record X names
		lossRec(file, 100, 300, 300, 0, 16, 1<<8), // X at 300 names R
		lossRec(file, 300, 400, 300, 0, 16, 1<<8), // Y at 400 names X
		lossRec(file, 400, 500, 300, 0, 16, 1<<8), // Z at 500 names Y
	))

	st := kindsRun(t, r)

	if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup over R's extent hit, want a miss: X keeps its effect, so the loss naming R withholds it")
	}
	if len(st.Kinds.Losses) != 2 {
		t.Fatalf("State.Kinds.Losses got %d entries (%+v), want the two: X and the Z that covers Y, which covers nothing itself", len(st.Kinds.Losses), st.Kinds.Losses)
	}
	named := map[core.Seq]bool{}
	for _, lo := range st.Kinds.Losses {
		named[lo.Dropped] = true
	}
	if !named[100] || !named[400] {
		t.Errorf("the delivered losses name the dropped records %v, want X's (100) and Z's (400): Y delivers nothing", named)
	}
	if got, want := st.Kinds.Withheld, (index.Numbers{Version: core.Version{Counter: 3}, Seq: 100}); got != want {
		t.Errorf("Withheld got %+v, want %+v: R's own numbers, withheld by X", got, want)
	}
}

// TestRunWithholdsARecordTwoRulesNameTogether holds the overlap row: a
// record both a forget and an unsynced loss name — whichever drops it, no
// other path re-holds it — withholds whole, while a record of the same tag
// after the forget's seq feeds normally and the loss, covered by no other
// rule, still delivers its event once.
func TestRunWithholdsARecordTwoRulesNameTogether(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	r.plantSeg(1, buildSeg(1, writeRec(10, 3, 0, 16)))
	r.plantSeg(2, buildSeg(2, writeRec(40, 4, 0x4100, 16)))
	r.plantSeg(3, buildSeg(3, forgetRec(7, 30), lossRec(file, 10, 31, 201, 0, 16, 1<<8)))

	sink := &lossSink{}
	st, err := r.asideRun(sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, ok := st.Rebuilt.Index.Lookup(file, 0); ok {
		t.Errorf("Lookup over the twice-named record's extent hit, want a miss: no path re-holds what one rule dropped")
	}
	if p, ok := st.Rebuilt.Index.Lookup(file, 0x4100); !ok {
		t.Fatalf("Lookup at 0x4100 missed, want the write after the forget to feed")
	} else if p.Seq != 40 {
		t.Errorf("the record at 0x4100 holds seq %d, want 40", p.Seq)
	}
	if len(sink.placed) != 1 {
		t.Errorf("the sink was handed %d loss events, want the one unsynced loss, covered by no rule", len(sink.placed))
	}
	if st.Kinds.Withheld != (index.Numbers{Version: core.Version{Counter: 3}, Seq: 10}) {
		t.Errorf("Withheld got %+v, want the greatest numbers a withheld record carried ({3,10})", st.Kinds.Withheld)
	}
}

// TestRunGivesACoveredLossRecordNoEffectAnywhere holds the covered aside's
// no-effect at the map the filter withholds by: a loss record another rule
// drops — a forget of its own tag above its own seq — feeds nothing to the
// filter, so the record it names neither withholds nor clips anywhere, for
// with no delivered event and no cut a name acting alone would leave a hole
// in the feed nothing explains. The named record and the forgetting tag are
// cross-tag on purpose: with both of one tag every writer-reachable geometry
// is forget-dominated — the forget of the tag withholds every record the loss
// would name below it — so the covered loss's own leg shows only where its
// tag differs from the tag of the record it names, a geometry the writer has
// but no single rule dominates.
func TestRunGivesACoveredLossRecordNoEffectAnywhere(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	file := kindsFile(1)
	r.plantSeg(2, buildSeg(2,
		writeRec(10, 3, 0, 64), // of tag 7, another tag than the loss's
		forgetRec(9, 6),        // forgets the loss's own tag, above the loss's own seq
		rec(10, func(rc *raw.Record) { // the covered loss: tag 9, seq 5
			rc.Tag = 9
			rc.FileID = file.Bytes()
			rc.Seq, rc.Version = 5, raw.Version{Counter: 9}
			rc.Offset, rc.Length = 0, 64
			rc.Loss = &raw.LossBody{Dropped: 10, Flags: 1 | 4<<8} // synced, superseded
		}),
	))

	st := kindsRun(t, r)

	p, ok := st.Rebuilt.Index.Lookup(file, 0)
	if !ok {
		t.Fatalf("Lookup over the covered loss's extent missed, want the named record feeding whole: the covered loss clips nothing")
	}
	if p.Extent != (core.Extent{Off: 0, Len: 64}) || p.Seq != 10 || p.Version.Counter != 3 {
		t.Errorf("the named record got seq %d at v%d over %+v, want seq 10 at v3 over [0,64): neither withheld nor clipped by the covered loss", p.Seq, p.Version.Counter, p.Extent)
	}
	for _, ch := range st.Rebuilt.Index.Since(file, core.Version{}) {
		if ch.Kind != core.ChangeWrite {
			t.Errorf("Since yielded %+v, want the held write alone: the covered loss cuts nothing and marks nothing", ch)
		}
	}
	if got := st.Kinds.Withheld; got != (index.Numbers{}) {
		t.Errorf("Withheld got %+v, want the zero pair: the covered loss withholds nothing", got)
	}
	if len(st.Kinds.Losses) != 0 {
		t.Errorf("State.Kinds.Losses holds %d losses, want none: the covered loss is collected nowhere", len(st.Kinds.Losses))
	}
	if len(st.Tails.Corrupt) != 0 {
		t.Errorf("Tails.Corrupt got %+v, want none: the named record verified and confined nothing", st.Tails.Corrupt)
	}
	if len(st.Kinds.Forgets) != 1 || st.Kinds.Forgets[0].Tag != 9 || st.Kinds.Forgets[0].Seq != 6 {
		t.Errorf("State.Kinds.Forgets got %+v, want the one forget of tag 9 at seq 6, itself uncovered", st.Kinds.Forgets)
	}
}

// TestRunDecodesASyncedSupersededLossWithItsExtent holds the superseded
// half's own facts: a synced loss record whose reason is superseded decodes
// over the extent its header names — [0,64K) of a record holding [0,128K) —
// and carries its reason and synced bit as its body wrote them. Whether the
// named record's kept remainder withholds or feeds is the feed filter's, not
// this pass's, and nothing here asserts it.
func TestRunDecodesASyncedSupersededLossWithItsExtent(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	seg := buildSeg(id,
		rec(3, func(rc *raw.Record) { rc.Offset, rc.Length = 0, 128*1024 }), // the record named, over [0,128K)
		rec(10, func(rc *raw.Record) { // superseded, synced, over [0,64K)
			rc.Offset, rc.Length = 0, 64*1024
			rc.Seq = 101
			rc.Version = raw.Version{Counter: 201}
			rc.Loss = &raw.LossBody{Dropped: 100, Flags: 1 | 4<<8}
		}))
	r.plantSeg(id, seg)

	sink := &lossSink{}
	st, err := r.asideRun(sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(st.Kinds.Losses) != 1 {
		t.Fatalf("Run: State.Kinds.Losses holds %d losses, want the one the superseded loss record names", len(st.Kinds.Losses))
	}
	lo := st.Kinds.Losses[0]
	want := core.LossEvent{
		Share:   7,
		File:    core.NewFileID([16]byte{0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x0a, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f}),
		Extent:  core.Extent{Off: 0, Len: 64 * 1024},
		Version: core.Version{Counter: 201},
		Reason:  core.LossSuperseded,
		Synced:  true,
		AtOpen:  true,
	}
	if lo.Ev != want {
		t.Errorf("Run: the collected OpenLoss's event got %+v, want %+v", lo.Ev, want)
	}
	if lo.Dropped != 100 {
		t.Errorf("Run: the collected OpenLoss names the dropped record %d, want 100", lo.Dropped)
	}
}

// TestRunCollectsForgetsByTheirGreatestSeq holds the forget rule's collected
// half: two forgets of one tag keep the greater seq, and a forget names a
// share nothing on disk holds — a tag nothing else names, or a seq no record
// on disk carries — and still collects, with no segments: the feed that
// collects them is not built.
func TestRunCollectsForgetsByTheirGreatestSeq(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(1)
	seg := buildSeg(id,
		forgetRec(5, 11),
		forgetRec(5, 21), // the greater seq of the same tag wins
		forgetRec(0, 31), // a tag nothing on disk holds
		forgetRec(7, 41), // another tag nothing on disk holds
	)
	r.plantSeg(id, seg)

	st, err := r.asideRun(nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []struct {
		tag core.ShareTag
		seq core.Seq
	}{
		{0, 31},
		{5, 21},
		{7, 41},
	}
	if got := st.Kinds.Forgets; len(got) != len(want) {
		t.Fatalf("Run: State.Kinds.Forgets holds %d entries, want %d: one per tag", len(got), len(want))
	} else {
		for i, w := range want {
			if got[i].Tag != w.tag || got[i].Seq != w.seq {
				t.Errorf("Run: State.Kinds.Forgets[%d] got a forget of tag %d at seq %d, want tag %d at seq %d",
					i, got[i].Tag, got[i].Seq, w.tag, w.seq)
			}
			if segs := got[i].Segments; len(segs) != 0 {
				t.Errorf("Run: the forget of tag %d holds segments %v, want none yet: the feed that collects them is not built", w.tag, segs)
			}
		}
	}
}

// TestRunRefusesAVerifiedHoldKindRecord holds the D6 refusal: a hold, unhold
// or stamp record that verifies — its header on the scan and its body read
// whole here — and that no drop covers refuses the open with
// ErrNotImplemented naming its kind and its segment, whatever the loss sink
// (a nil one included) would have answered.
func TestRunRefusesAVerifiedHoldKindRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  raw.Record
	}{
		{"hold", rec(11, func(rc *raw.Record) {
			rc.Offset, rc.Length = 0, 0
			rc.Cut = &raw.CutBody{Cut: 3}
		})},
		{"unhold", rec(12, func(rc *raw.Record) {
			rc.FileID = [16]byte{}
			rc.Version = raw.Version{}
			rc.Offset, rc.Length = 0, 0
			rc.Cut = &raw.CutBody{Cut: 4}
		})},
		{"stamp", rec(13, func(rc *raw.Record) {
			rc.Offset, rc.Length = 0, 0
			rc.Cut = &raw.CutBody{Cut: 5}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, buildSeg(1, rec(1, func(rc *raw.Record) {
				rc.Offset, rc.Length = 0, 16
				rc.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, 16)}
			})))
			r.plantSeg(2, buildSeg(2, tc.rec))

			st, err := r.asideRun(nil)
			if !errors.Is(err, core.ErrNotImplemented) {
				t.Fatalf("Run over a %s record got err %v, want one matching %v", tc.name, err, core.ErrNotImplemented)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("Run: the refusal %q names no %s, the record's own kind", err, tc.name)
			}
			if !strings.Contains(err.Error(), "segment 2") {
				t.Errorf("Run: the refusal %q names no segment 2, the record's own segment", err)
			}
			if len(st.Kinds.Losses) != 0 {
				t.Errorf("Run: a refused open collected %d losses, want none: the open serves nothing", len(st.Kinds.Losses))
			}
		})
	}
}

// TestRunCoveredHoldKindRecordsDoNotRefuse holds the covered half of the
// aside rule: a hold that another rule on the same disk already drops — a
// loss record naming its seq, a forget of its own tag above it, the failed
// list the sink answers — never refuses the open, and a Run with such
// records succeeding is the whole assertion.
func TestRunCoveredHoldKindRecordsDoNotRefuse(t *testing.T) {
	hold := func(seq uint64) raw.Record {
		return rec(11, func(rc *raw.Record) {
			rc.Seq = seq
			rc.Offset, rc.Length = 0, 0
			rc.Cut = &raw.CutBody{Cut: 3}
		})
	}
	for _, tc := range []struct {
		name   string
		failed []core.Seq
		recs   []raw.Record
	}{
		{"its seq named by an unsynced loss", nil, []raw.Record{
			hold(3),
			rec(10, func(rc *raw.Record) { // the loss naming the hold, unsynced
				rc.Offset, rc.Length = 0, 64
				rc.Seq = 4
				rc.Version = raw.Version{Counter: 201}
				rc.Loss = &raw.LossBody{Dropped: 3, Flags: 1 << 8}
			}),
		}},
		{"below a forget of its own tag", nil, []raw.Record{
			hold(3),
			forgetRec(7, 9),
		}},
		{"on the failed list", []core.Seq{3}, []raw.Record{hold(3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(1, buildSeg(1, tc.recs...))

			if _, err := r.asideRun(&lossSink{failed: tc.failed}); err != nil {
				t.Errorf("Run over a covered hold got err %v, want none: another rule already drops the record", err)
			}
		})
	}
}

// TestRunConfinesALossBodyThatDoesNotVerify holds the confinement: a loss
// record whose body fails — a byte rotted after the writer built it, or a
// checksum-valid body this format version does not define, a reason or a
// flag word no writer writes — drops into its own scan as a corrupt drop
// the tail's classing judges like any other, publishes nothing through the
// sink, and never refuses the open.
func TestRunConfinesALossBodyThatDoesNotVerify(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(rc *raw.Record)
		rot  bool // flip a body byte after the segment was built
	}{
		{"a rotted body byte", func(rc *raw.Record) { rc.Loss = &raw.LossBody{Dropped: 100, Flags: 1 | 1<<8} }, true},
		{"an undefined reason", func(rc *raw.Record) { rc.Loss = &raw.LossBody{Dropped: 100, Flags: 6 << 8} }, false},
		{"a reserved flag bit", func(rc *raw.Record) { rc.Loss = &raw.LossBody{Dropped: 100, Flags: 1<<2 | 1<<8} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			id := core.SegmentID(1)
			seg := buildSeg(id, rec(10, func(rc *raw.Record) {
				rc.Offset, rc.Length = 0, 64
				rc.Seq = 101
				rc.Version = raw.Version{Counter: 201}
				// The record's own sync covers its bytes, so the classing
				// has the proof a corrupt drop needs.
				rc.SyncedThrough = format.SegmentHeaderSize + 96 + 16
				tc.edit(rc)
			}))
			if tc.rot {
				off := format.SegmentHeaderSize + format.RecordHeaderSize + 4
				seg[off] ^= 0xff
			}
			r.plantSeg(id, seg)

			sink := &lossSink{}
			st, err := r.asideRun(sink)
			if err != nil {
				t.Fatalf("Run over a loss body that does not verify got err %v, want none: the record confines", err)
			}
			if len(sink.placed) != 0 {
				t.Errorf("Run: the sink was handed %d loss events over a confined record, want none", len(sink.placed))
			}
			want := int64(format.SegmentHeaderSize) // the loss record's own offset
			found := slices.ContainsFunc(st.Tails.Corrupt, func(c corruptRecord) bool {
				return c.Segment == id && c.at() == want
			})
			if !found {
				t.Errorf("Run: the tail's corrupt drops %v name no drop of segment %d at %d, the confined loss record",
					st.Tails.Corrupt, id, want)
			}
		})
	}
}

// TestRunRefusesALossRecordOfZeroLength holds the one malformed row the
// header alone shows: a loss record whose length field is zero names an
// extent no cut could ever be read over, and the open refuses with
// ErrZeroLength naming its segment and its offset — never reading the body
// or interpreting a piece-wide rule from a record that names no piece.
func TestRunRefusesALossRecordOfZeroLength(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(3)
	r.plantSeg(id, buildSeg(id, rec(10, func(rc *raw.Record) {
		rc.Offset, rc.Length = 0, 0
		rc.Loss = &raw.LossBody{Dropped: 100, Flags: 1 | 1<<8}
	})))

	st, err := r.asideRun(nil)
	if !errors.Is(err, core.ErrZeroLength) {
		t.Fatalf("Run over a zero-length loss record got err %v, want one matching %v", err, core.ErrZeroLength)
	}
	if !strings.Contains(err.Error(), "segment 3") {
		t.Errorf("Run: the refusal %q names no segment 3, the record's own segment", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", format.SegmentHeaderSize)) {
		t.Errorf("Run: the refusal %q names no record offset %d, the record's own slot", err, format.SegmentHeaderSize)
	}
	if len(st.Kinds.Losses) != 0 {
		t.Errorf("Run: a refused open collected %d losses, want none: the open serves nothing", len(st.Kinds.Losses))
	}
}

// TestDecodeAsidesRefusesALossRecordThatNamesNoFile holds the other malformed
// row: a loss record whose header names no file must refuse the open, for it
// would drop a record it cannot name. The format's own header rules refuse
// such a record before the scan can set it aside, so the row is fed to the
// pass directly — the backstop keeps the pass whole should that rule lift.
func TestDecodeAsidesRefusesALossRecordThatNamesNoFile(t *testing.T) {
	r := newRig(t)
	rd := newReader(context.Background(), r.seam.FS(), r.clk, nil)
	scs := []segmentScan{{ID: core.SegmentID(1), Aside: []asideRecord{{
		Header: format.RecordHeader{
			Kind:    format.KindLoss,
			Segment: 1,
			Extent:  core.Extent{Off: 0, Len: 64},
			Seq:     8,
			Offset:  format.SegmentHeaderSize,
		},
	}}}}

	_, err := decodeAsides(rd, journalID, nil, scs)
	if !errors.Is(err, core.ErrCorrupt) {
		t.Fatalf("decodeAsides over a loss record naming no file got err %v, want one matching %v", err, core.ErrCorrupt)
	}
	if !strings.Contains(err.Error(), "segment 1") {
		t.Errorf("decodeAsides: the refusal %q names no segment 1, the record's own segment", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", format.SegmentHeaderSize)) {
		t.Errorf("decodeAsides: the refusal %q names no record offset %d, the record's own slot", err, format.SegmentHeaderSize)
	}
}

// TestRunCarriesEveryReasonAndFlagIntoTheEvent holds the reason mapping: each
// reason a loss record can name decodes into the event the sink is handed,
// and the flag word's two bits ride along as the synced and offloaded flags.
func TestRunCarriesEveryReasonAndFlagIntoTheEvent(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(1)
	cases := []struct {
		reason    core.LossReason
		flags     uint64
		synced    bool
		offloaded bool
	}{
		{core.LossCorrupt, 1 << 8, false, false},
		{core.LossStale, 2 << 8, false, false},
		{core.LossFailedWindow, 3<<8 | 1<<0, true, false},
		{core.LossSuperseded, 4<<8 | 1<<1, false, true},
		{core.LossAcknowledged, 5<<8 | 1<<0 | 1<<1, true, true},
	}
	var recs []raw.Record
	for i, c := range cases {
		c := c
		recs = append(recs, rec(10, func(rc *raw.Record) {
			// Each loss names a record no other record in this segment
			// carries, so no loss covers another with its own name.
			rc.Offset, rc.Length = uint64(64*i), 64
			rc.Seq = uint64(101 + i)
			rc.Version = raw.Version{Counter: uint64(201 + i)}
			rc.Loss = &raw.LossBody{Dropped: uint64(200 + i), Flags: c.flags}
		}))
	}
	r.plantSeg(id, buildSeg(id, recs...))

	sink := &lossSink{}
	if _, err := r.asideRun(sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(sink.placed) != len(cases) {
		t.Fatalf("Run: the sink was handed %d loss events, want the %d planted", len(sink.placed), len(cases))
	}
	for i, c := range cases {
		got := sink.placed[i]
		if got.Reason != c.reason {
			t.Errorf("Run: loss %d's event carries reason %d, want %d", i, got.Reason, c.reason)
		}
		if got.Synced != c.synced {
			t.Errorf("Run: loss %d's event carries Synced %v, want %v", i, got.Synced, c.synced)
		}
		if got.Offloaded != c.offloaded {
			t.Errorf("Run: loss %d's event carries Offloaded %v, want %v", i, got.Offloaded, c.offloaded)
		}
		if got := sink.dropped[i]; got != core.Seq(200+i) {
			t.Errorf("Run: loss %d's event names the dropped record %d, want %d", i, got, 200+i)
		}
	}
}

// TestRunDeliversTwoIdenticalLossRecordsOnce holds the carried-forward copy:
// two identical loss records in two segments name the same piece, and one
// open reports it once — the second delivery would double-report one loss
// to a poller. The event each would carry is the same, so which segment's
// record delivered is not observable and is not asserted.
func TestRunDeliversTwoIdenticalLossRecordsOnce(t *testing.T) {
	loss := func() raw.Record {
		return rec(10, func(rc *raw.Record) {
			rc.Offset, rc.Length = 0, 64
			rc.Seq = 101
			rc.Version = raw.Version{Counter: 201}
			rc.Loss = &raw.LossBody{Dropped: 100, Flags: 1 | 1<<8}
		})
	}
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, buildSeg(1, loss()))
	r.plantSeg(2, buildSeg(2, loss())) // the carried-forward copy

	sink := &lossSink{}
	st, err := r.asideRun(sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.placed) != 1 {
		t.Errorf("Run: the sink was handed %d loss events for one piece, want one: %+v", len(sink.placed), sink.placed)
	}
	if len(st.Kinds.Losses) != 1 {
		t.Errorf("Run: State.Kinds.Losses holds %d losses for one piece, want one", len(st.Kinds.Losses))
	}
}
