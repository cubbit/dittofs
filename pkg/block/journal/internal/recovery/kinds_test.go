package recovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
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
	if got := st.Kinds.Losses[0].Segments; len(got) != 0 {
		t.Errorf("Run: the collected loss holds segments %v, want none yet: the feed that collects them is not built", got)
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
