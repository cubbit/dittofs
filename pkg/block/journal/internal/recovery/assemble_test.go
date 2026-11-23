package recovery

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// assembleRun runs the open's recovery over the rig's planted directory with
// the failed list the assembly's checks chose, refusing a run whose
// assembly seeded no ledger.
func assembleRun(t *testing.T, r *rig, failed []core.Seq) State {
	t.Helper()
	d := r.runDeps()
	d.Failed = failedWords(failed)
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Assembled.Set == nil {
		t.Fatalf("Run: Assembled.Set is nil, want the ledger seeded from the scans: %+v", st.Assembled)
	}
	return st
}

// assembleRunForCounters is the same run over the same directory, reading
// the counters straight from the rebuilt state: the raise the assembly owns
// shows red on its own numbers, not on the set it has not seeded yet.
func assembleRunForCounters(t *testing.T, r *rig, failed []core.Seq) State {
	t.Helper()
	d := r.runDeps()
	d.Failed = failedWords(failed)
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Rebuilt.Counters == nil {
		t.Fatalf("Run: the rebuilt counters are nil, want the counters the assembly raises")
	}
	return st
}

// seededWorkload plants the fixture the assembly's checks read: two share
// tags' records across five active segments — a forget of tag 7 above two
// of its records, a synced loss naming one of tag 5's writes, an unsynced
// loss naming a record copied in two segments, a release on the failed
// list — and one sealed segment of tag 7. It returns the sealed segment's
// whole length, the post-seal size the ledger must account it at.
func seededWorkload(t *testing.T, r *rig) int64 {
	t.Helper()
	tag7, tag5 := core.ShareTag(7), core.ShareTag(5)
	f1 := kindsFile(1)
	// Segment 1 holds tag 7's write and truncate, both below the forget the
	// third segment carries, so both withhold while they still charge.
	r.plantSeg(1, buildSeg(1,
		collectWrite(tag7, f1, 10, 10, 0, 64),
		collectRec(4, tag7, f1, 12, 12, 0x100, 0), // truncate: Off is the new size
	))
	// Segment 2 holds tag 5's records: a write the synced loss names, a
	// clone target and one copy of the record the unsynced loss names.
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
	// Segment 4 holds the named record's second copy, the release the
	// failed list withholds, and a write past every rule.
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
	// Segment 6 is sealed, holding one write of tag 7 that feeds.
	sealed, _ := buildSealed(6, collectWrite(tag7, f1, 60, 60, 0x600, 32))
	r.plantSeg(6, sealed)
	return int64(len(sealed))
}

// wantAssembledCharges spells the (segment, tag) figures the workload's
// planted headers charge: the whole sizes and the sizes less the payloads.
func wantAssembledCharges(tag7, tag5 core.ShareTag) map[core.SegmentID]map[core.ShareTag]Charge {
	return map[core.SegmentID]map[core.ShareTag]Charge{
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
		6: {tag7: {Bytes: slotLen(format.KindWrite, 32),
			Overhead: slotLen(format.KindWrite, 32) - 32}},
	}
}

// TestRunAssemblesTheSetTheJournalOpensWith holds the seed: one adopted entry
// per scanned segment, the active ones accounted at the cap and the sealed
// one at its post-seal size, each with the record bytes the feed charged its
// tags — so the counters and the per-tag figures equal what the running
// journal's own event paths would have left — and the ledger's own Check
// agreeing over the whole seed.
func TestRunAssemblesTheSetTheJournalOpensWith(t *testing.T) {
	tag7, tag5 := core.ShareTag(7), core.ShareTag(5)
	r := newRig(t)
	r.writeFormat()
	sealedLen := seededWorkload(t, r)

	st := assembleRun(t, r, []core.Seq{50, 9999})

	c := st.Assembled.Set.Counters()
	wantUsed := 5*testSegCap + sealedLen
	if c.UsedBytes != wantUsed {
		t.Errorf("the seeded ledger's UsedBytes got %d, want %d: five active segments accounted at the %d cap and the sealed one at its %d post-seal size", c.UsedBytes, wantUsed, testSegCap, sealedLen)
	}
	if c.Segments != 6 {
		t.Errorf("the seeded ledger's Segments got %d, want 6: one entry per scanned segment", c.Segments)
	}
	if c.Sealed != 1 {
		t.Errorf("the seeded ledger's Sealed got %d, want 1: the one sealed segment of the workload", c.Sealed)
	}
	if err := st.Assembled.Set.Check(); err != nil {
		t.Errorf("the seeded ledger's Check got %v, want none: the seed moved every counter the event paths do", err)
	}

	// Every row at its classed state and its accounted size.
	rows := st.Assembled.Set.Snapshot()
	if len(rows) != 6 {
		t.Fatalf("the seeded ledger's Snapshot holds %d rows, want 6: %+v", len(rows), rows)
	}
	for i, id := range []core.SegmentID{1, 2, 3, 4, 5, 6} {
		if rows[i].ID != id {
			t.Fatalf("the seed's row %d names segment %d, want %d: the rows are ascending by id", i, rows[i].ID, id)
		}
	}
	for _, id := range []core.SegmentID{1, 2, 3, 4, 5} {
		got, err := st.Assembled.Set.State(id)
		if err != nil || got != segment.Active {
			t.Errorf("segment %d's seeded state got %v (%v), want Active: a segment no fate ends and no marker seals", id, got, err)
		}
	}
	if got, err := st.Assembled.Set.State(6); err != nil || got != segment.Sealed {
		t.Errorf("the sealed segment's seeded state got %v (%v), want Sealed: its scan found the marker", got, err)
	}
	if rows[5].Size != sealedLen {
		t.Errorf("the sealed segment's accounted size got %d, want %d: its post-seal size, the whole file its seal left", rows[5].Size, sealedLen)
	}
	if st.Assembled.Set.SealedFull(6) {
		t.Errorf("the sealed segment below the cap answers SealedFull true, want false: only a seal at the cap is a full one")
	}
	wantSeg1Tags := map[core.ShareTag]int64{tag7: slotLen(format.KindWrite, 64) + slotLen(format.KindTruncate, 0)}
	if got := rows[0].Tags; !mapsEqualK(got, wantSeg1Tags) {
		t.Errorf("segment 1's seeded tags got %v, want %v: the bytes the feed charged the segment's one tag", got, wantSeg1Tags)
	}

	// The per-tag figures over every segment that holds the tag: the whole
	// record bytes and the same less the payloads, the pair the journal's
	// own append charges tag by tag.
	want := wantAssembledCharges(tag7, tag5)
	wantBytes, wantOver := map[core.ShareTag]int64{}, map[core.ShareTag]int64{}
	for _, tags := range want {
		for tag, ch := range tags {
			wantBytes[tag] += ch.Bytes
			wantOver[tag] += ch.Overhead
		}
	}
	gotBytes, gotOver := st.Assembled.Set.TagFigures()
	if !mapsEqualK(gotBytes, wantBytes) || !mapsEqualK(gotOver, wantOver) {
		t.Errorf("the seeded ledger's per-tag figures got bytes %v overhead %v, want bytes %v overhead %v: the charged figures of the six planted segments", gotBytes, gotOver, wantBytes, wantOver)
	}
}

// TestRunAssemblesTheCountersAboveEveryNumber holds the counters' raise: the
// sequence counter opens above every number on the disk — the fed records,
// the withheld ones the feed never saw, and the aside records' own numbers —
// and above every number on the failed list, one the disk holds and one it
// does not; the version counter above every version on disk, the aside
// records' own versions included, whatever the feed fed.
func TestRunAssemblesTheCountersAboveEveryNumber(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	seededWorkload(t, r)

	st := assembleRunForCounters(t, r, []core.Seq{50, 9999})

	// The greatest numbers the disk holds are the fed write's seq 60 on the
	// sealed segment and the unsynced loss record's own version 941; the
	// withheld fold ({50, 50}) and the failed raises (50, and the 9999 the
	// disk does not hold) all land below or between them.
	got, err := st.Rebuilt.Counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if want := core.Seq(10000); got != want {
		t.Errorf("the next sequence number got %d, want %d: above every disk number, the withheld 50 and the failed 9999 the disk does not hold", got, want)
	}
	if got, want := st.Rebuilt.Counters.NextVersion(), (core.Version{Counter: 942}); got != want {
		t.Errorf("the next version got %+v, want %+v: above every version on disk, the loss record's own 941 included", got, want)
	}
}

// TestRunAssemblesTheOpenRegistrations holds the registration lists: every
// attached header-only record registers at the placement and with the tag,
// file and numbers its own header spells, withheld copies included, each
// synced loss record beside them plain, each forget with the snapshot of
// the segments holding its tag's records below it, and each unsynced loss
// paired with every segment still holding a copy of the record it names.
func TestRunAssemblesTheOpenRegistrations(t *testing.T) {
	tag7, tag5 := core.ShareTag(7), core.ShareTag(5)
	f1 := kindsFile(1)
	r := newRig(t)
	r.writeFormat()
	seededWorkload(t, r)

	st := assembleRun(t, r, []core.Seq{50, 9999})

	wantLive := []reclaim.Rec{
		{
			Kind: format.KindTruncate, File: f1, Seg: 1,
			Ext: core.Extent{Off: 0x100}, Pair: index.Numbers{Version: core.Version{Counter: 12}, Seq: 12},
			Seq: 12, Tag: tag7,
			Loc: segment.Location{Segment: 1, Off: 48 + slotLen(format.KindWrite, 64), Len: slotLen(format.KindTruncate, 0)},
		},
		{
			Kind: format.KindCloneTarget, File: collectF2, Seg: 2,
			Ext:  core.Extent{Off: 0x100, Len: 16},
			Pair: index.Numbers{Version: core.Version{Counter: 22}, Seq: 22},
			Seq:  22, Tag: tag5,
			Loc: segment.Location{Segment: 2, Off: 48 + slotLen(format.KindWrite, 16), Len: slotLen(format.KindCloneTarget, 16)},
		},
		{
			Kind: format.KindRelease, File: collectF2, Seg: 4,
			Ext:  core.Extent{Off: 0x300, Len: 16},
			Pair: index.Numbers{Version: core.Version{Counter: 50}, Seq: 50},
			Seq:  50, Tag: tag5,
			Loc: segment.Location{Segment: 4, Off: 48 + slotLen(format.KindWrite, 64), Len: slotLen(format.KindRelease, 16)},
		},
		{
			Kind: format.KindLoss, File: collectF2, Seg: 5,
			Ext:  core.Extent{Off: 0, Len: 16},
			Pair: index.Numbers{Version: core.Version{Counter: 921}, Seq: 21},
			Seq:  21, Named: 20, Tag: tag5, Synced: true,
			Loc: segment.Location{Segment: 5, Off: 48, Len: slotLen(format.KindLoss, 16)},
		},
	}
	if got := st.Assembled.Live; !slices.Equal(got, wantLive) {
		t.Errorf("Run: the live registrations got %d Recs %+v, want the %d the attached headers spell, the synced loss plain beside them: %+v", len(got), got, len(wantLive), wantLive)
	}

	wantForgets := []reclaim.ForgetSet{{
		Rec: reclaim.Rec{
			Kind: format.KindForget, Seg: 3,
			Pair: index.Numbers{Seq: 30}, Seq: 30, Tag: tag7,
			Loc: segment.Location{Segment: 3, Off: 48, Len: slotLen(format.KindForget, 0)},
		},
		Segs: []core.SegmentID{1},
	}}
	if got := st.Assembled.Forgets; len(got) != 1 || got[0].Rec != wantForgets[0].Rec || !slices.Equal(got[0].Segs, wantForgets[0].Segs) {
		t.Errorf("Run: the forget registrations got %+v, want the one forget record of tag 7 with its holder snapshot [1]: %+v", got, wantForgets)
	}

	wantUnsynced := []reclaim.UnsyncedLoss{{
		Rec: reclaim.Rec{
			Kind: format.KindLoss, File: collectF2, Seg: 5,
			Ext:  core.Extent{Off: 0x200, Len: 64},
			Pair: index.Numbers{Version: core.Version{Counter: 941}, Seq: 41},
			Seq:  41, Named: 40, Tag: tag5, Synced: false,
			Loc: segment.Location{Segment: 5, Off: 48 + slotLen(format.KindLoss, 16), Len: slotLen(format.KindLoss, 16)},
		},
		HeldBy: []core.SegmentID{2, 4},
	}}
	if got := st.Assembled.Unsynced; len(got) != 1 || got[0].Rec != wantUnsynced[0].Rec || !slices.Equal(got[0].HeldBy, wantUnsynced[0].HeldBy) {
		t.Errorf("Run: the unsynced losses got %+v, want the one naming seq 40, held by its two copies's segments [2 4]: %+v", got, wantUnsynced)
	}
}

// TestRunAssemblesAnEmptySetOverADirectoryWithNoSegments holds the empty
// row: a directory holding only its format file seeds an empty, valid
// ledger, and no registration names anything, for the open found nothing.
func TestRunAssemblesAnEmptySetOverADirectoryWithNoSegments(t *testing.T) {
	r := newRig(t)
	r.writeFormat()

	st := assembleRun(t, r, nil)

	if rows := st.Assembled.Set.Snapshot(); len(rows) != 0 {
		t.Errorf("the seeded ledger's Snapshot over an empty directory got %+v, want no rows", rows)
	}
	if c := st.Assembled.Set.Counters(); c.UsedBytes != 0 || c.Segments != 0 || c.Sealed != 0 {
		t.Errorf("the seeded ledger's counters over an empty directory got %+v, want the zero counters", c)
	}
	if err := st.Assembled.Set.Check(); err != nil {
		t.Errorf("the empty seed's Check got %v, want none: nothing was adopted", err)
	}
	if st.Assembled.Live != nil || st.Assembled.Forgets != nil || st.Assembled.Unsynced != nil {
		t.Errorf("the registrations over an empty directory got live %+v, forgets %+v, unsynced %+v, want none", st.Assembled.Live, st.Assembled.Forgets, st.Assembled.Unsynced)
	}
}

// TestRunAssemblesAFullSealAtItsCap holds the full word: a named, sealed
// segment whose catalogued footer puts its trailer in the cap's last 32
// bytes — a full seal's own shape — is seeded sealed and full, the one
// shape a retirement recycles. The successor naming the sealed segment is
// planted beside it, for only a successor's claim sends the rebuild to the
// footer that carries the word.
func TestRunAssemblesAFullSealAtItsCap(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	sealed, _ := buildSealed(1, collectWrite(core.ShareTag(7), kindsFile(1), 60, 60, 0x600, 32))
	r.plantSeg(1, sealed)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())

	d := r.runDeps()
	d.SegmentCap = int64(len(sealed))
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Assembled.Set == nil {
		t.Fatalf("Run: Assembled.Set is nil, want the ledger seeded over the sealed segment")
	}

	if got, err := st.Assembled.Set.State(1); err != nil || got != segment.Sealed {
		t.Errorf("the sealed segment's seeded state got %v (%v), want Sealed", got, err)
	}
	if !st.Assembled.Set.SealedFull(1) {
		t.Errorf("the sealed segment at its cap answers SealedFull false, want true: the footer its successor sent the rebuild to verified over the cap's last 32 bytes, the trailer a full seal alone puts there")
	}
	if c := st.Assembled.Set.Counters(); c.Sealed != 1 || c.Segments != 2 || c.UsedBytes != 2*int64(len(sealed)) {
		t.Errorf("the seeded ledger's counters got %+v, want the sealed segment at its %d cap beside the empty successor at the same cap", c, len(sealed))
	}
	if err := st.Assembled.Set.Check(); err != nil {
		t.Errorf("the seed's Check got %v, want none", err)
	}
}

// TestRunAssemblesACapSizedEarlySealAsNotFull holds the refused full word: a
// named segment whose seal marker sits below the cap with the file still at
// the cap — the shape an early seal's failed truncation leaves, the seal
// writer's own trailer below the cap and the free tail the truncate never
// cut on top — answers SealedFull false, for the bytes the cap's last 32
// hold are the uncut tail, a footer no catalog read can verify there, so
// the segment is seeded sealed but never a recycle candidate on its size
// alone.
func TestRunAssemblesACapSizedEarlySealAsNotFull(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	sealed, _ := buildSealed(1, collectWrite(core.ShareTag(7), kindsFile(1), 60, 60, 0x600, 32))
	r.plantSeg(1, append(bytes.Clone(sealed), make([]byte, 256)...))
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())

	d := r.runDeps()
	d.SegmentCap = int64(len(sealed)) + 256 // the cap the truncate would have left the file at
	st, err := Run(context.Background(), d, core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.Assembled.Set == nil {
		t.Fatalf("Run: Assembled.Set is nil, want the ledger seeded over the sealed segment")
	}

	if got, err := st.Assembled.Set.State(1); err != nil || got != segment.Sealed {
		t.Errorf("the sealed segment's seeded state got %v (%v), want Sealed: its scan found the marker whatever the tail says", got, err)
	}
	if st.Assembled.Set.SealedFull(1) {
		t.Errorf("the cap-sized segment whose seal marker sits below the cap answers SealedFull true, want false: the file's size is the truncate that never became durable, and the free tail it left holds no trailer the cap's own last bytes could name")
	}
	if rows := st.Assembled.Set.Snapshot(); len(rows) != 2 || rows[0].Size != d.SegmentCap {
		t.Errorf("the sealed segment's accounted size got rows %+v, want it among the two segments at its %d cap: whatever the truncate failed to take off, the ledger accounts the whole file until the seal phase", rows, d.SegmentCap)
	}
}

// TestRunAssemblesATornTailActiveAtItsCap holds the torn tail's seam: a
// segment the classing gave a torn-tail fate seeds Active — the one state a
// seal takes, the state the cut-and-seal phase that cuts its torn end and
// seals it needs, for a seed that classed it Ended would leave the later seal
// a state it refuses to move and the emptying a segment to unlink instead —
// and the phase then takes that state in the same set: the one row the open
// continues from is the seal's, at the early seal's size.
func TestRunAssemblesATornTailActiveAtItsCap(t *testing.T) {
	endA := endAfter(tailRec(200, 0x4100))
	torn := buildSeg(2, tailRec(200, 0x4100), tailRec(201, 0x8100))
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, torn[:len(torn)-5])
	st := assembleRun(t, r, nil)

	wantFate(t, st, 2, fateTornTail, endA)
	if st.Tails.TornTails != 1 {
		t.Errorf("the torn tail counted %d cuts, want 1", st.Tails.TornTails)
	}
	if rows := st.Assembled.Set.Snapshot(); len(rows) != 1 || rows[0].State != segment.Sealed ||
		rows[0].Size != earlySealSize(endA, 1) {
		t.Errorf("the torn-tailed segment's row got %+v, want the one segment Sealed by the cut at its early-seal size %d",
			rows, earlySealSize(endA, 1))
	}
}

// TestRunAssemblesAnEndedSegmentAsEndedAndCapped holds the ended fate's
// seed: a segment whose fate the classing ended — a torn slot no proof
// reaches, its tail whole — seeds Ended, still accounted at its cap until
// it is unlinked.
func TestRunAssemblesAnEndedSegmentAsEndedAndCapped(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(2, slotSeg(2, func(slot int64) int64 { return slot - 1 }))
	st := assembleRun(t, r, nil)

	wantFate(t, st, 2, fateEnded, 0)
	if rows := st.Assembled.Set.Snapshot(); len(rows) != 1 || rows[0].State != segment.Ended || rows[0].Size != testSegCap {
		t.Errorf("the ended segment's seeded row got %+v, want the one segment Ended at its %d cap until its unlink", rows, testSegCap)
	}
}

// mapsEqualK reports whether two int64 maps agree exactly; the seeded
// figure checks need no tolerance and no missing-key pass.
func mapsEqualK[K comparable](got, want map[K]int64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
