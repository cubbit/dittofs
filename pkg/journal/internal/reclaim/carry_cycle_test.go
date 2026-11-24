package reclaim_test

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/reclaim"
)

// plantTruncateAt plants one truncate of file at size under the version the
// caller names, the way the writer's own removal path plants one: the file's
// append domain held, one sequence number drawn, the header-only record
// appended where the stream stands, the removal applied with its marker over
// everything past the size, the held delta accounted. The record registers
// live with the core's own liveness — the registry its pass and retirer walk,
// which the fixtures' own tracker is not. A version drawn before the older
// record that will outrank the truncate ever plants is the shape the
// two-segment cycle's crossed pairs are made of: the pair must name a
// relationship the counter's draw order could not place if the record took a
// fresh version here, so the caller draws the version it wants the pair to
// carry and hands it in.
func (f *copyFixture) plantTruncateAt(t *testing.T, tag core.ShareTag, file core.FileID, size int64, v core.Version) (reclaim.Rec, core.Seq) {
	t.Helper()
	unlock := f.core.Domains().Lock(file)
	defer unlock()
	seq, err := f.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq of the truncate of file %v at size %d: %v", file, size, err)
	}
	pl, err := f.core.Streams().For(file).Append(format.RecordHeader{
		Kind: format.KindTruncate, File: file, Extent: core.Extent{Off: size}, Seq: seq, Version: v, Tag: tag,
	}, format.RecordBody{})
	if err != nil {
		t.Fatalf("Append of the truncate record: %v", err)
	}
	at := index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)}
	hd, _, err := f.ix.ApplyRemoval(tag, file, core.ChangeTruncate,
		core.Extent{Off: size, Len: math.MaxInt64 - size}, v, seq, at, nil)
	if err != nil {
		t.Fatalf("ApplyRemoval of the truncate: %v", err)
	}
	f.core.Account(hd)
	rec := reclaim.Rec{
		Kind: format.KindTruncate, File: file, Seg: pl.Loc.Segment,
		Ext:  core.Extent{Off: size},
		Pair: index.Numbers{Version: v, Seq: seq}, Seq: seq, Tag: tag, Loc: pl.Loc,
	}
	f.core.Liveness().Register(rec)
	return rec, seq
}

// truncatePlacementsOf answers every placement the seam's own bytes hold of a
// truncate record whose sequence number sits in want, decoded straight off
// the disk the way the fixture's own read-backs are: the ground truth of
// which segment a record's copies stand in, whatever the ledger or the index
// choose to answer about them.
func (f *copyFixture) truncatePlacementsOf(t *testing.T, want ...core.Seq) []index.RecordAt {
	t.Helper()
	present := make(map[core.Seq]struct{}, len(want))
	for _, s := range want {
		present[s] = struct{}{}
	}
	names, err := f.seam.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []index.RecordAt
	for _, name := range names {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		raw := f.readNameBytes(t, name)
		// The walk starts past the segment header the file opens with, and
		// stops where the records end: the past-the-last bytes hold a seal
		// marker or a footer, no record.
		for off := format.SegmentHeaderSize; off+format.RecordHeaderSize <= len(raw); {
			h, derr := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], copyIdentity, id, int64(off))
			if derr != nil {
				break
			}
			if h.Kind == format.KindTruncate {
				if _, ok := present[h.Seq]; ok {
					out = append(out, index.RecordAt{Segment: id, Offset: uint32(off)})
				}
			}
			off += int(h.Size())
		}
	}
	return out
}

// beside returns the one placement in placements that sits in a segment other
// than notThis — the carried copy, where notThis is its original — failing
// the test when the copy has not placed.
func beside(t *testing.T, notThis core.SegmentID, placements ...index.RecordAt) index.RecordAt {
	t.Helper()
	for _, at := range placements {
		if at.Segment != notThis {
			return at
		}
	}
	t.Fatalf("no placement stands in a segment beside %d (got %v), want the carried copy the pass placed", notThis, placements)
	return index.RecordAt{}
}

// TestCarriedRemovalsRetireTheirSourcesAndUnwindTheCycle plants the removal
// churn's deterministic two-segment cycle and unwinds it through the real pass
// machinery: two files on one stream, each truncated at a size whose drop
// ranges — the older write the truncate cuts — land in the OTHER segment, so
// each removal record answers undroppable over the outranking guard while
// the other segment stands, and each segment answers held: the wedge the
// issue's churn wedged the journal with, in one pass's reach. Each unit is
// driven as its own real pass so the state between the two retirements — the
// first source gone while its carried record still answers undroppable over
// the second segment's standing — is observable rather than inferred; the
// passes share one copy target, so both records' copies land in the one
// placement. The assertions pin the fix's whole lane: the carried record's
// second placement frees the source to retire while the record stays
// undroppable (marker settled, outranked), the second segment's durable going
// releases the first record's registration at the drop test the relay
// re-tests, and the second record's register-and-release, through the marker
// its settlement ends, completes the unwind.
func TestCarriedRemovalsRetireTheirSourcesAndUnwindTheCycle(t *testing.T) {
	p := newPassFixture(t)
	f := p.f
	live := f.core.Liveness()
	// The two files share one stream, so their records interleave through the
	// same segments and both carried records land in the one active target
	// the seals roll the stream to.
	xa := copyFile(0x30)
	xb := sameStreamFile(t, f, xa)
	// The crossed pairs: every write's version drawn before every
	// truncate's, whatever order the records append in — the truncate of one
	// file plants before the old write of the other lands in the later
	// segment, and the write's lower pair is what the other segment's ranges
	// note below the truncate that counts on them.
	vWA, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the first old write's version: %v", err)
	}
	vWB, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the second old write's version: %v", err)
	}
	vTA, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the first truncate's version: %v", err)
	}
	vTB, err := f.counters.Draw()
	if err != nil {
		t.Fatalf("Draw of the second truncate's version: %v", err)
	}
	payA := bytes.Repeat([]byte("oldwrite!"), 15)[:128]
	payB := bytes.Repeat([]byte("OLDWRITE!"), 15)[:128]
	// The first segment: the old write whose later half the SECOND segment's
	// truncate cuts, and the truncate of the other file, whose outranker
	// waits in the later segment below.
	f.plantAt(t, copyTag, xb, core.Extent{Len: 128}, payB, 0, vWB)
	ta, seqTA := f.plantTruncateAt(t, copyTag, xa, 64, vTA)
	sa := ta.Seg
	f.sealSource(t, xb)
	f.waitSpares(t)
	// The second segment: the old write of the other file — its lower-pair
	// note keeps the first truncate undroppable while this segment stands —
	// and the truncate cutting the first segment's write past its size.
	f.plantAt(t, copyTag, xa, core.Extent{Len: 128}, payA, 0, vWA)
	tb, seqTB := f.plantTruncateAt(t, copyTag, xb, 64, vTB)
	sb := tb.Seg
	if sb == sa {
		t.Fatalf("the second window's records landed in segment %d, want a segment beside the sealed %d", sb, sa)
	}
	f.sealSource(t, xa)
	f.waitSpares(t)

	// The cycle stands even with the first truncate's marker settled: each
	// record undroppable over the write its drop ranges name in the OTHER
	// segment, and each segment answering held by its own record.
	live.MarkerSettled(xa, vTA)
	if live.Droppable(ta) {
		t.Fatal("Droppable of the settled truncate in the first segment = true, want false: the other segment holds the older write the removal counts on, below its pair")
	}
	if n := live.LivePlacementsOf(ta); n != 1 {
		t.Fatalf("live registrations of the first truncate before the pass = %d, want 1: settled but outranked, the registration stands", n)
	}
	if live.Droppable(tb) {
		t.Fatal("Droppable of the truncate in the second segment = true, want false: the first segment holds the older write the removal counts on")
	}
	if !live.Held(sa) || !live.Held(sb) {
		t.Fatalf("Held of the cycle's segments = %v and %v, want both true: each is kept by its own undroppable removal record", live.Held(sa), live.Held(sb))
	}

	// The first pass: its unit carries the first truncate into the copy
	// target — the record's second placement — and retires the source.
	freed, err := p.repack(context.Background(), copySegCap*8, sa)
	if err != nil {
		t.Fatalf("Repack over the first cycle segment: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("Repack over the first cycle segment freed %d, want the source retired by the unit that carried its truncation", freed)
	}
	if !p.setGone(sa) {
		t.Fatal("the first cycle segment is still in the set, want the carried record's second placement to free its source to retire while the record stays undroppable")
	}
	// The source retired while its removal record stays undroppable: the
	// copy stands registered live in the target, its marker settled, and
	// the still-standing second segment keeps it — the whole point of the
	// fix, observed between the two retirements rather than inferred.
	cp := beside(t, sa, f.truncatePlacementsOf(t, seqTA)...)
	cpRec := ta
	cpRec.Seg = cp.Segment
	if live.Droppable(cpRec) {
		t.Fatal("Droppable of the carried truncate while the second cycle segment stands = true, want false: the older write it counts on is still noted there below its pair, settlement or no")
	}
	if n := live.LivePlacementsOf(cpRec); n != 1 {
		t.Fatalf("live registrations of the carried truncate in the copy target after its source retired = %d, want 1: the copy stands registered live, the class holding the target for it alone", n)
	}
	if live.Droppable(tb) {
		t.Fatal("Droppable of the second truncate after the first pass = true, want false: its marker never settled, and the second unit must reach it before any settlement does")
	}

	// The second pass over the other cycle segment: the mirror unit carries
	// the second truncate into the same copy target and retires it, and the
	// first record's drop test then releases at the durable relay.
	freed, err = p.repack(context.Background(), copySegCap*8, sb)
	if err != nil {
		t.Fatalf("Repack over the second cycle segment: %v", err)
	}
	if freed <= 0 {
		t.Fatalf("Repack over the second cycle segment freed %d, want the source retired by the unit that carried its truncation", freed)
	}
	if !p.setGone(sb) {
		t.Fatal("the second cycle segment is still in the set, want the unit that carried its truncation to retire it")
	}

	// Both still-undroppable removal records were carried into the one copy
	// placement the passes placed: one placement for each sequence number
	// stands on disk, in the same segment — the originals left with their
	// retired sources.
	plA := f.truncatePlacementsOf(t, seqTA)
	plB := f.truncatePlacementsOf(t, seqTB)
	if len(plA) != 1 {
		t.Fatalf("the first truncate holds %d placements after the passes (%v), want 1: the original left with the retired source and the carried copy stands", len(plA), plA)
	}
	if len(plB) != 1 {
		t.Fatalf("the second truncate holds %d placements after the passes (%v), want 1: the original left with the retired source and the carried copy stands", len(plB), plB)
	}
	if plA[0].Segment != plB[0].Segment {
		t.Fatalf("the carried truncates stand in segments %d and %d, want the one copy placement the passes placed them in", plA[0].Segment, plB[0].Segment)
	}
	hA, bA := f.readBack(t, plA[0])
	if hA.Kind != format.KindTruncate || hA.File != xa || hA.Seq != seqTA || hA.Tag != copyTag ||
		bA.Payload != nil || bA.Loss != (format.Loss{}) {
		t.Fatalf("the carried first truncate at %v decodes as header %+v body %+v, want the truncate kind, its file, tag and the sequence number both its numbers are", plA[0], hA, bA)
	}

	// The second segment's drop-test then releases: the carried first
	// truncate — settled and undroppable through the first pass — turned
	// droppable with the durable going of the second, whose relay re-tested
	// its file and spent the registration the copy held its target with.
	if !live.Droppable(cpRec) {
		t.Fatal("Droppable of the carried truncate after the second segment durably retired = false, want true: nothing below its pair stands in any other segment any more")
	}
	if n := live.LivePlacementsOf(cpRec); n != 0 {
		t.Fatalf("live registrations of the carried truncate after the durable relay re-tested its file = %d, want 0: the drop test the second segment's going freed spent the copy's registration", n)
	}
	// The second record's own register-and-release, through the marker its
	// settlement ends: still registered and undroppable beside the settled
	// first, it releases at the settle's re-test — the cycle's last record
	// holding nothing once its file's answer turns droppable.
	cpB := tb
	cpB.Seg = plB[0].Segment
	if n := live.LivePlacementsOf(cpB); n != 1 {
		t.Fatalf("live registrations of the carried second truncate before its settlement = %d, want 1: the copy stands registered live in the target, waiting on its marker", n)
	}
	if live.Droppable(cpB) {
		t.Fatal("Droppable of the carried second truncate before its marker settles = true, want false: the marker keeps it until the settlement covers it")
	}
	live.MarkerSettled(xb, vTB)
	if !live.Droppable(cpB) {
		t.Fatal("Droppable of the carried second truncate after the settlement = false, want true: no marker and nothing it may still outrank")
	}
	if n := live.LivePlacementsOf(cpB); n != 0 {
		t.Fatalf("live registrations of the carried second truncate after the settlement's re-test = %d, want 0: the cycle's last registered record drops with its file's answer", n)
	}
}
