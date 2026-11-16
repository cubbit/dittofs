package recovery

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// collectF2 is the second tag's file, so one fixture holds records of two
// files under two share tags.
var collectF2 = core.NewFileID([16]byte{15: 0x42})

// collectLoss builds one loss record of tag over the extent its header
// names: the file, the dropped seq and the flag word its body carries.
func collectLoss(tag core.ShareTag, file core.FileID, dropped, seq, counter uint64, off, n int64, flags uint64) raw.Record {
	return rec(10, func(r *raw.Record) {
		r.Tag = uint32(tag)
		r.FileID = file.Bytes()
		r.Seq, r.Version = seq, raw.Version{Counter: counter}
		r.Offset, r.Length = uint64(off), uint64(n)
		r.Loss = &raw.LossBody{Dropped: dropped, Flags: flags}
	})
}

// collectRec builds one header-only record of kind and tag over the extent
// its header names — a release, a truncate, a clone target — with the
// numbers the check chose.
func collectRec(kind uint8, tag core.ShareTag, file core.FileID, seq, counter uint64, off, n int64) raw.Record {
	return rec(kind, func(r *raw.Record) {
		r.Tag = uint32(tag)
		r.FileID = file.Bytes()
		r.Seq, r.Version = seq, raw.Version{Counter: counter}
		r.Offset, r.Length = uint64(off), uint64(n)
	})
}

// collectWrite builds one write record of tag over [off, off+n), carrying
// the payload its length names.
func collectWrite(tag core.ShareTag, file core.FileID, seq, counter uint64, off, n int64) raw.Record {
	return rec(1, func(r *raw.Record) {
		r.Tag = uint32(tag)
		r.FileID = file.Bytes()
		r.Seq, r.Version = seq, raw.Version{Counter: counter}
		r.Offset, r.Length = uint64(off), uint64(n)
		r.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, n)}
	})
}

// slotLen answers the bytes a record of kind whose header names the extent
// length n takes as a whole, spelling the format's own layout rule for the
// expectation the collected Loc and charge must copy.
func slotLen(kind format.Kind, n int64) int64 {
	return format.RecordHeader{Kind: kind, Extent: core.Extent{Len: n}}.Size()
}

// TestRunCollectsEachAttachedRecordsPlacementAndAccounting holds the feed's
// collection: every header-only record a scan attached notes one Rec at the
// placement its own slot spells — its segment, its own offset, the whole
// bytes its kind and length take — withheld records among them, one entry
// per copy, for every copy's bytes keep its own segment held; the charge
// collects per segment and tag the whole size and the size less the payload
// of every attached record and every aside header the scan set aside, for
// the append charged each of them too and a reopen must answer the same
// figures; the forget's snapshot names exactly the segments holding the
// tag's records below its seq; and a loss's holder set names every segment
// holding a copy of the record it names, withheld copies among them, and
// the failed list's note names every home of its record.
func TestRunCollectsEachAttachedRecordsPlacementAndAccounting(t *testing.T) {
	tag7, tag5 := core.ShareTag(7), core.ShareTag(5)
	f1 := kindsFile(1)
	r := newRig(t)
	r.writeFormat()

	// Segment 1 holds tag 7's write and truncate, both below the forget the
	// third segment carries, so both withhold while they still charge.
	r.plantSeg(1, buildSeg(1,
		collectWrite(tag7, f1, 10, 10, 0, 64),
		collectRec(4, tag7, f1, 12, 12, 0x100, 0), // truncate: Off is the new size
	))
	// Segment 2 holds tag 5's records: a write the synced loss names, a clone
	// target — the one header-only kind whose slot a body widens — and one
	// copy of the record the unsynced loss names.
	r.plantSeg(2, buildSeg(2,
		collectWrite(tag5, collectF2, 20, 20, 0, 16),
		rec(7, func(rc *raw.Record) { // the clone target, over [0x100, 0x110)
			rc.Tag = uint32(tag5)
			rc.FileID = collectF2.Bytes()
			rc.Seq, rc.Version = 22, raw.Version{Counter: 22}
			rc.Offset, rc.Length = 0x100, 16
			rc.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
		}),
		collectWrite(tag5, collectF2, 40, 40, 0x200, 64), // the named record, copy one
	))
	// Segment 3 carries the forget of tag 7 above its two records and the
	// write above it, which feeds.
	r.plantSeg(3, buildSeg(3,
		forgetRec(uint32(tag7), 30),
		collectWrite(tag7, f1, 31, 31, 0x400, 16),
	))
	// Segment 4 holds the named record's second copy, the release the failed
	// list withholds, and a write past every rule.
	r.plantSeg(4, buildSeg(4,
		collectWrite(tag5, collectF2, 40, 40, 0x200, 64),  // copy two
		collectRec(3, tag5, collectF2, 50, 50, 0x300, 16), // the failed-list release
		collectWrite(tag5, collectF2, 52, 52, 0x380, 16),
	))
	// Segment 5 holds the two loss records: the synced one naming the write
	// of seq 20, the unsynced one naming the copied record of seq 40.
	r.plantSeg(5, buildSeg(5,
		collectLoss(tag5, collectF2, 20, 21, 921, 0, 16, 1|1<<8),
		collectLoss(tag5, collectF2, 40, 41, 941, 0x200, 64, 1<<8),
	))

	st, err := r.asideRun(nil, []core.Seq{50})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// One Rec per header-only record the scans attached, in the feed's
	// order, each at the placement its own slot spells: the segment, the
	// record's own offset — past the write before it whose own slot takes
	// 96 bytes of header, 8 of time and 64 of payload — and the whole bytes
	// the kind and the header's extent take. The withheld truncate (below
	// the forget) and the withheld release (on the failed list) note like
	// the fed clone target.
	wantRecs := []reclaim.Rec{
		{
			Kind: format.KindTruncate, File: f1, Seg: 1,
			Ext: core.Extent{Off: 0x100}, Pair: index.Numbers{Version: core.Version{Counter: 12}, Seq: 12},
			Seq: 12, Tag: tag7, Loc: segment.Location{Segment: 1, Off: 216, Len: slotLen(format.KindTruncate, 0)},
		},
		{
			Kind: format.KindCloneTarget, File: collectF2, Seg: 2,
			Ext:  core.Extent{Off: 0x100, Len: 16},
			Pair: index.Numbers{Version: core.Version{Counter: 22}, Seq: 22},
			Seq:  22, Tag: tag5,
			// Past the write of 16 payload bytes, whose slot takes 96 of
			// header, 8 of time, 16 of payload and no padding.
			Loc: segment.Location{Segment: 2, Off: 48 + slotLen(format.KindWrite, 16), Len: slotLen(format.KindCloneTarget, 16)},
		},
		{
			Kind: format.KindRelease, File: collectF2, Seg: 4,
			Ext:  core.Extent{Off: 0x300, Len: 16},
			Pair: index.Numbers{Version: core.Version{Counter: 50}, Seq: 50},
			Seq:  50, Tag: tag5,
			// Past the copy of 64 payload bytes before it.
			Loc: segment.Location{Segment: 4, Off: 48 + slotLen(format.KindWrite, 64), Len: slotLen(format.KindRelease, 16)},
		},
	}
	if got := st.Kinds.Records; !slices.Equal(got, wantRecs) {
		t.Errorf("Run: the collected records got %+v, want %d Recs at their own placements, withheld copies among them: %+v", got, len(wantRecs), wantRecs)
	}

	// Per segment and tag, the whole sizes and the sizes less the payloads of
	// everything the scans attached — the two figures the journal's append
	// charges — with the aside records' slots beside them: the forget's and
	// the losses' whole 96-byte headers and 16-byte bodies charge the tag of
	// their own headers like any append charged them. Every payload-carrying
	// record counts its bytes whole and its header-body padding as overhead.
	wantCharge := map[core.SegmentID]map[core.ShareTag]Charge{
		1: {tag7: {Bytes: slotLen(format.KindWrite, 64) + slotLen(format.KindTruncate, 0),
			Overhead: slotLen(format.KindWrite, 64) - 64 + slotLen(format.KindTruncate, 0)}},
		2: {tag5: {Bytes: slotLen(format.KindWrite, 16) + slotLen(format.KindCloneTarget, 16) + slotLen(format.KindWrite, 64),
			Overhead: (slotLen(format.KindWrite, 16) - 16) + slotLen(format.KindCloneTarget, 16) + (slotLen(format.KindWrite, 64) - 64)}},
		3: {tag7: {Bytes: slotLen(format.KindForget, 0) + slotLen(format.KindWrite, 16),
			Overhead: slotLen(format.KindForget, 0) + (slotLen(format.KindWrite, 16) - 16)}},
		4: {tag5: {Bytes: slotLen(format.KindWrite, 64) + slotLen(format.KindRelease, 16) + slotLen(format.KindWrite, 16),
			Overhead: (slotLen(format.KindWrite, 64) - 64) + slotLen(format.KindRelease, 16) + (slotLen(format.KindWrite, 16) - 16)}},
		5: {tag5: {Bytes: 2 * slotLen(format.KindLoss, 16),
			Overhead: 2 * slotLen(format.KindLoss, 16)}},
	}
	if got := st.Kinds.Charging; len(got) != len(wantCharge) {
		t.Fatalf("Run: the charge collected %d segments, want %d: %+v", len(got), len(wantCharge), got)
	}
	for seg, tags := range wantCharge {
		gotSeg, ok := st.Kinds.Charging[seg]
		if !ok {
			t.Errorf("Run: the charge names no segment %d, want its records' figures %+v", seg, tags)
			continue
		}
		if len(gotSeg) != len(tags) {
			t.Errorf("Run: segment %d's charge holds %d tags, want %d: %+v", seg, len(gotSeg), len(tags), gotSeg)
		}
		for tag, want := range tags {
			if got := gotSeg[tag]; got != want {
				t.Errorf("Run: segment %d's tag %d charged %+v, want %+v: the whole sizes and the sizes less the payloads of everything the segment holds, asides included", seg, tag, got, want)
			}
		}
	}

	// The forget's snapshot names exactly the segment holding tag 7's records
	// below the forget's seq: its own later write — above it — and the losses
	// of the other tag count nowhere.
	if len(st.Kinds.Forgets) != 1 || st.Kinds.Forgets[0].Tag != tag7 || st.Kinds.Forgets[0].Seq != 30 {
		t.Fatalf("Run: the collected forgets got %+v, want the one of tag 7 at seq 30", st.Kinds.Forgets)
	}
	if got := st.Kinds.Forgets[0].Segments; !slices.Equal(got, []core.SegmentID{1}) {
		t.Errorf("Run: the forget's snapshot got segments %v, want [1]: the only segment holding tag 7's records below seq 30", got)
	}

	// Each loss names every segment holding a copy of the record it names,
	// withheld copies included; each aside order lists by its own name, so
	// the checks find them by the seq they drop.
	byDropped := map[core.Seq][]core.SegmentID{}
	for _, lo := range st.Kinds.Losses {
		byDropped[lo.Dropped] = lo.Segments
	}
	wantHomes := map[core.Seq][]core.SegmentID{
		20: {2},    // the synced loss's named record sits in segment 2 alone
		40: {2, 4}, // both copies of the unsynced loss's named record, in feed order
	}
	for seq, want := range wantHomes {
		if got := byDropped[seq]; !slices.Equal(got, want) {
			t.Errorf("Run: the loss naming seq %d holds segments %v, want %v: every segment holding a copy of the record it names", seq, got, want)
		}
	}

	// The failed list's note names every segment still holding its record.
	if len(st.Kinds.ReadOnlyDrops) != 1 || st.Kinds.ReadOnlyDrops[0].Dropped != 50 {
		t.Fatalf("Run: the read-only drops got %+v, want the one note of the failed seq 50", st.Kinds.ReadOnlyDrops)
	}
	if got := st.Kinds.ReadOnlyDrops[0].Segments; !slices.Equal(got, []core.SegmentID{4}) {
		t.Errorf("Run: the failed seq's note holds segments %v, want [4]: the one segment holding the withheld release", got)
	}
}

// TestRunCountsAConfinedCopyAmongALossesHolders holds the reach of a loss's
// holder set against the copies the feed fed: the copy the open itself
// confined — a rotten kept run of a partly-superseded record — fed nothing
// and joined no fed-copies registry, but its bytes still hold the record the
// loss names, so its home counts among the loss's holders like the healthy
// twin's, and its slot still charges its segment.
func TestRunCountsAConfinedCopyAmongALossesHolders(t *testing.T) {
	file := kindsFile(1)
	r := newRig(t)
	r.writeFormat()
	seg := buildSeg(1,
		writeRec(60, 5, 0, 128),
		lossRec(file, 60, 61, 9, 0, 64, 1|4<<8), // synced, superseded: the kept run is [64,128)
	)
	// One payload byte of the kept run rotted after its writer built it.
	seg[format.SegmentHeaderSize+format.RecordHeaderSize+8+70] ^= 0xff
	r.plantSeg(1, seg)
	r.plantSeg(2, buildSeg(2, writeRec(60, 5, 0, 128))) // the healthy twin, kept

	st := kindsRun(t, r)

	if len(st.Kinds.Losses) != 1 || st.Kinds.Losses[0].Dropped != 60 {
		t.Fatalf("kindsRun: the collected losses got %+v, want the one superseded loss naming seq 60", st.Kinds.Losses)
	}
	if got := st.Kinds.Losses[0].Segments; !slices.Equal(got, []core.SegmentID{1, 2}) {
		t.Errorf("kindsRun: the loss naming seq 60 holds segments %v, want [1 2]: the confined copy's home counts beside the fed twin's, for its bytes still hold the record", got)
	}
	// The confined copy still charges its whole slot, and the loss record
	// beside it charges its own: the segment's figures answer what its bytes
	// are, whatever the open found rotten in them.
	want := Charge{Bytes: slotLen(format.KindWrite, 128) + slotLen(format.KindLoss, 16),
		Overhead: slotLen(format.KindWrite, 128) - 128 + slotLen(format.KindLoss, 16)}
	if got := st.Kinds.Charging[1][7]; got != want {
		t.Errorf("kindsRun: segment 1's tag 7 charged %+v, want %+v: the confined write's whole slot beside the loss record's own", got, want)
	}
}
