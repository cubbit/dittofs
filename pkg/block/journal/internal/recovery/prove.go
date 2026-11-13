package recovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// tailRecord is one record the tail proof hands to the re-append phase:
// Segment and At name where the record sits, Header the header the walk
// verified, and First the whole record — header and body both — exactly as
// the first read served it, so the re-append writes back the bytes the
// journal wrote.
type tailRecord struct {
	Segment core.SegmentID
	At      int64
	Header  format.RecordHeader
	First   []byte
}

// proof is the tail proof's answer: the records the re-append phase takes
// and the segments whose proof sync failed. The marks phase reads Reappends
// for its lost-tail check.
type proof struct {
	Reappends     []tailRecord
	FailedWindows []core.SegmentID
}

// markerSync is the marker flow's answer for the one sync it made for a
// segment it opened: spent when its sync succeeded — the segment's bytes
// already reached the device, so the tail flow re-reads there without a
// second sync — and failed when its sync failed, when no later success this
// open stands in for it: the tail flow hands the window over without
// asking the device at all. It is none for every segment the marker flow
// never opened, whose one sync — where the flow makes any — is the tail
// flow's own.
type markerSync uint8

const (
	markerSyncNone markerSync = iota
	markerSyncSpent
	markerSyncFailed
)

// runTailProof proves on the device the two things a crash could leave
// unproven: every seal marker no successor names, and the tail of every
// segment no header, seal or sync proves.
//
// The marker half: the classing answered fateSealToProve for each unnamed
// sealed segment, its walk having seen the marker through the reader, in the
// bytes a live process serves; the proof takes the file, syncs it, and reads
// the marker's 32 bytes directly — what reached the device, never what the
// reader collected. A marker that verifies there seals the segment as a
// successor's name would; one that disagrees — short, rotted or written
// somewhere else — is treated as absent, and its segment carries the torn
// tail the classing gives an unsealed one. The sync the marker half made is
// the segment's one sync when it succeeded, and its failure is the segment's
// one failure when it did not.
//
// The tail half: the newest segment of each stream — no own header names it
// as its predecessor, and no seal proves it — has each record past the
// highest synced-through its verifying records named read through the reader
// and checked whole, both checksums, its bytes kept as the first copy; the
// segment is then synced — save where the marker flow's sync already carried
// its bytes, or already failed, when no second sync is made — and each
// record that verified is re-read directly and checked again. A record that
// verifies there stays where it is; one the device does not hold — its
// re-read short, rotted, or refused — is lost by the device, handed to the
// re-append with its first copy, and the segment ended; one whose first
// read does not verify is torn — never written, so it is re-appended
// nowhere, reported nowhere, and the rebuilt state drops its hold entirely,
// the index entry for a content record and the aside entry for an aside
// one — and re-places what its replay displaced, so the index ends as it
// would had the rebuild never been fed the record at all. A sync that
// fails hands every record that verified to the re-append the same way, as
// a failed window, and the open still succeeds: the re-append resolves it.
//
// The records the flow reads are every kind the walk attached — the content
// kinds the index was fed among them, and the aside kinds a later phase
// owns: a kind the proof excepts would leave its bytes unproven and its
// loss unreported, for the re-append's copier copies records whole and
// kind-blind.
//
// A segment the flow ends is ended at the END of the last record that held
// — verified on the device at its re-read, verified at its first read when
// it is handed to the re-append, or lying below the synced-through a sync
// proved whole — never at the first record that did not: a bad record
// before a later holding one is stepped over, exactly as a torn slot is,
// and a segment whose every bad record is its last still cuts past it; a
// lost record, a failed sync or a torn record below a holding one is what
// ends a segment. A segment whose every bad verdict is torn, with nothing
// holding past the first torn record, is cut at that record's own start
// and sealed instead — the torn tail's own rule, that a crash tearing a
// stream's last write costs a seal and never a segment's content. The
// synced-through raises either cut only where it names no record's
// interior — a point a stamping writer may leave inside the very record it
// follows, which is the one place the classing's floor would cut a record
// in half. The classing's own offset is never crossed, so a cut never
// breaks one of its floor rules, and a segment where nothing went wrong
// keeps the fate the classing wrote. Nothing is appended here: the phase's
// only reach into storage is the sync before each direct read, and the cut
// its fates name is a later phase's.
func runTailProof(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version) error {
	// One scan per own segment, by id: every fate the loop proves owns one,
	// the classing having answered a fate per scan it was handed. Each is a
	// pointer into the state's own scans, so a torn record's drop — an aside
	// entry — reaches the state the open keeps.
	scans := make(map[core.SegmentID]*segmentScan, len(st.Rebuilt.Scans))
	for i := range st.Rebuilt.Scans {
		scans[st.Rebuilt.Scans[i].ID] = &st.Rebuilt.Scans[i]
	}

	// The scans whose markers were treated as absent, held for the tail
	// flow below: the fate each was left is that flow's to keep or deepen,
	// never to clear. markerSyncOf holds each one's sync outcome, and every
	// segment the marker flow never opened answers none.
	var unsealed []core.SegmentID
	markerSyncOf := make(map[core.SegmentID]markerSync)

	for i := range st.Tails.Fates {
		fate := &st.Tails.Fates[i]
		if fate.Kind != fateSealToProve {
			continue // sealed by a successor's name, or no seal to prove
		}
		sc, ok := scans[fate.ID]
		if !ok {
			// The classing writes one fate per scan, so a fate with no
			// scan names a segment whose seal nothing may speak for.
			return fmt.Errorf("%w: segment %d holds a seal-to-prove fate no scan answers", core.ErrCorrupt, fate.ID)
		}

		f, err := deps.FS.Open(format.SegmentName(fate.ID))
		if err != nil {
			// The house read rule: the refusal answers on the ranges the
			// reader collected, exactly as the open's other reads answer.
			return refusedRead(rd, err)
		}

		// decision: bytes that read and disagree are judged by the marker's
		// decoder and never reported — the marker is treated as absent and
		// its segment carries the torn tail the classing gives an unsealed
		// one, never damaged. The one thing this phase refuses is a read
		// the bound did not complete, and that refusal keeps read.go's own
		// ceiling: a filesystem that refuses direct reads holds its journal
		// closed at an unnamed marker, withdrawn only when an unread
		// record can be held instead of refusing, as read.go's read
		// refusals are.
		unseal := func() {
			fate.Kind, fate.Offset = fateTornTail, min(max(sc.RecordsEnd, sc.SyncedThrough), sc.Size)
			unsealed = append(unsealed, fate.ID)
		}

		// The sync carries the segment's bytes to the device before the
		// marker is read there; it is the segment's one sync, the tail flow
		// below never making a second. A context error answers the open;
		// any other failure is a failed window — the marker's own answer
		// and the tail flow's both, which joins it — and the marker's
		// absence, since the marker is never re-read after a failed sync:
		// a later one can succeed without writing the pages the failed one
		// dropped, so a later success stands in for nothing.
		if cerr := ctx.Err(); cerr != nil {
			_ = f.Close()
			return cerr
		}
		if serr := f.Sync(); serr != nil {
			_ = f.Close()
			if isContextErr(serr) {
				return serr
			}
			st.Proof.FailedWindows = append(st.Proof.FailedWindows, fate.ID)
			markerSyncOf[fate.ID] = markerSyncFailed
			unseal()
			continue
		}
		markerSyncOf[fate.ID] = markerSyncSpent

		// The marker read, taken straight off the device and under the
		// read bound. It never goes through the reader — a range the
		// reader would collect here names the marker, and the refusal it
		// answers below names it by hand — and its bytes are never classed:
		// a short file is its own length, and the bytes either decode or
		// they do not.
		buf := make([]byte, format.SealMarkerSize)
		var n int
		_, rerr := readretry.Do(ctx, rd.clk, readretry.Attempts, func() error {
			var e error
			n, e = f.ReadAtDirect(buf, sc.End)
			if errors.Is(e, io.EOF) {
				return nil // a read to the file's own end completed
			}
			return e
		})
		_ = f.Close()
		switch {
		case isContextErr(rerr):
			return rerr
		case rerr != nil:
			// A read no bounded attempt completed refuses the open on the
			// marker's own range, as read.go's read refusals refuse on
			// what the reader collected: the marker names a seal no
			// successor proves, so no marker the device never showed may
			// seal one.
			return &core.UnreadableRangesError{Ranges: []core.SegmentRange{{
				Segment: fate.ID, Off: sc.End, Len: format.SealMarkerSize,
			}}}
		}

		if _, derr := format.DecodeSealMarker(buf[:n], expect, fate.ID, sc.End); derr == nil {
			fate.Kind = fateSealed // a marker proven on the device seals as a successor's name does
			continue
		}
		unseal()
	}

	// The tail-proof set: the newest segment of each stream — one no own
	// header names as a predecessor — that no seal proves. The marker flow
	// above queued its own unsealed scans here; every fate the classing left
	// short of fateSealed names the same segments, so the queue joins them
	// without a second pass. A zero-header segment holds no records, so its
	// fate is the classing's alone.
	named := namedPredecessors(st.Attached)
	zeroHeader := make(map[core.SegmentID]bool, len(st.Attached.ZeroHeader))
	for _, id := range st.Attached.ZeroHeader {
		zeroHeader[id] = true
	}
	queued := make(map[core.SegmentID]bool, len(unsealed))
	var pending []core.SegmentID
	for _, id := range unsealed {
		if !queued[id] {
			queued[id] = true
			pending = append(pending, id)
		}
	}
	fateByID := make(map[core.SegmentID]*segmentFate, len(st.Tails.Fates))
	for i := range st.Tails.Fates {
		f := &st.Tails.Fates[i]
		fateByID[f.ID] = f
		if named[f.ID] || f.Kind == fateSealed || queued[f.ID] || zeroHeader[f.ID] {
			continue
		}
		pending = append(pending, f.ID)
	}
	slices.Sort(pending)

	// The numbers of the records this run has judged torn, one added at each
	// drop: a later repair is fed no record an earlier or its own verdict
	// drops, so the set the repairs re-apply is exactly the set a rebuild
	// without the torn records would have been fed.
	tornSoFar := make(map[index.Numbers]bool)

	for _, id := range pending {
		sc, ok := scans[id]
		if !ok {
			// The classing writes one fate per scan, so a fate with no
			// scan names a segment whose tail nothing may speak for.
			return fmt.Errorf("%w: segment %d holds a fate no scan answers", core.ErrCorrupt, id)
		}
		if err := proveTail(ctx, deps.FS, rd, st, expect, sc, fateByID[id], markerSyncOf[id], tornSoFar); err != nil {
			return err
		}
	}

	// The re-append takes the records in order, and the phases that re-prove
	// a failed window its segments in order, whatever order the fates were
	// visited in.
	slices.SortStableFunc(st.Proof.Reappends, func(a, b tailRecord) int {
		return cmp.Or(cmp.Compare(a.Segment, b.Segment), cmp.Compare(a.At, b.At))
	})
	slices.Sort(st.Proof.FailedWindows)
	return nil
}

// proveTail proves on the device every record of one segment the classing
// left unproven, and settles the segment's fate where a record did not
// prove. Nothing is appended: the flow's only reaches into storage are its
// reads and, where the marker flow made none, the one sync between the two
// of them.
func proveTail(ctx context.Context, fs storage.FS, rd *reader, st *State, expect core.JournalID, sc *segmentScan, fate *segmentFate, marker markerSync, tornSoFar map[index.Numbers]bool) error {
	if marker != markerSyncFailed && (sc.SyncedThrough >= sc.RecordsEnd || sc.RecordsEnd <= format.SegmentHeaderSize) {
		// No record can end past the synced-through the segment's own records
		// name when the point reaches the last record's end, and a segment
		// with no verifying record at all holds nothing to prove: the
		// classing's answer covers the segment, and the flow reaches no file.
		// Only a sync that already failed — a marker flow's — ends the
		// segment without records to prove.
		return nil
	}
	cands, heldBelow, err := tailCandidates(rd, expect, *sc)
	if err != nil {
		return err
	}

	// The first read of each proof record, payload included: the bytes the
	// reader serves are the only copy the re-append may keep a record's
	// numbers from, so they are read and verified whole and kept. Every read
	// goes through the reader, so a read the bound did not clear refuses the
	// open as the open's other reads do. Every verified record counts as
	// held wherever the flow lands — the device holds it, or the re-append
	// writes it back from the copy kept here — and a torn one never does:
	// treated as never written, it is dropped from everything the rebuilt
	// state holds of it, its displacement repaired at once.
	name := format.SegmentName(sc.ID)
	var verified []tailRecord
	verifiedAt := make(map[int64]bool, len(cands))
	heldEnd := heldBelow
	torn, tornAt := false, int64(0)
	for _, h := range cands {
		rec, ok, err := tailFirst(rd, expect, sc.ID, name, h.Offset)
		if err != nil {
			return err
		}
		if !ok {
			// A record whose bytes read and do not verify is torn: never
			// re-appended, never reported, never damage, and held nowhere
			// — the index entry for a content record, the aside entry for
			// an aside one, both dropped, so the journal never claims to
			// hold a byte the device never took — and the record's
			// displacement repaired: what it overwrote or cut holds its
			// extent again.
			if derr := dropTornRecord(rd, st, expect, sc, h, tornSoFar); derr != nil {
				return derr
			}
			if !torn || h.Offset < tornAt {
				tornAt = h.Offset
			}
			torn = true
			continue
		}
		verified = append(verified, rec)
		verifiedAt[h.Offset] = true
		heldEnd = max(heldEnd, h.Offset+h.Size())
	}

	lost := false
	windowFailed := false
	switch marker {
	case markerSyncFailed:
		// The window failed — at the marker flow, or at a sync whose failure
		// the marker flow's own already counts as once and once only:
		// nothing verified on the device, and no second sync this open
		// stands in for the one that failed. Each record that verified at
		// its first read joins the re-append from the copy kept there, and
		// the segment ends even when no record needed proving, for a
		// segment whose sync failed is a failed segment, never sealed.
		if !slices.Contains(st.Proof.FailedWindows, sc.ID) {
			// The marker flow may have named the segment already, when its
			// sync failed the marker's proof first: the window is one, not
			// two, and whether it failed at the marker or at the tail is
			// nothing the answer keeps.
			st.Proof.FailedWindows = append(st.Proof.FailedWindows, sc.ID)
		}
		st.Proof.Reappends = append(st.Proof.Reappends, verified...)
		windowFailed = true
	default:
		if len(cands) == 0 {
			// A segment with no proof records takes no second sync: the
			// marker flow's, where the segment had one, is the segment's
			// sync, and a segment with none is untouched.
			return nil
		}

		f, ferr := fs.Open(name)
		if ferr != nil {
			// The house read rule, as the open's other opens take it: the
			// refusal answers on the ranges the reader collected.
			return refusedRead(rd, ferr)
		}
		var serr error
		if marker == markerSyncNone {
			// The tail's own sync carries the records to the device before
			// they are re-read there. A spent marker flow's sync already
			// did, and a second one would stand in for nothing it could
			// name: the flow re-reads on what the first carried, the
			// success of its sync already spent.
			if cerr := ctx.Err(); cerr != nil {
				_ = f.Close()
				return cerr
			}
			serr = f.Sync()
		}
		if serr != nil && isContextErr(serr) {
			_ = f.Close()
			return serr
		}
		if serr != nil {
			_ = f.Close()
			// decision: a sync that fails hands the window over only through
			// the records whose first read verified — a record that failed
			// verification there has no copy the re-append could keep its
			// numbers from, so it is torn wherever the window lands, and only
			// the verified ones re-enter the journal as re-appends. Overturned
			// if the failed-window rule is ever ruled to re-append from an
			// unverifiable copy.
			if !slices.Contains(st.Proof.FailedWindows, sc.ID) {
				st.Proof.FailedWindows = append(st.Proof.FailedWindows, sc.ID)
			}
			st.Proof.Reappends = append(st.Proof.Reappends, verified...)
			windowFailed = true
		} else {
			// The sync carried the tail's bytes to the device: each record
			// that verified at the first read is re-read there and checked
			// again.
			for _, rec := range verified {
				holds, rerr := tailOnDevice(ctx, f, rd, expect, rec)
				if rerr != nil {
					_ = f.Close()
					return rerr
				}
				if !holds {
					// Lost by the device: the re-append writes the record
					// back from the copy its first read verified, and the
					// cut keeps its extent, for the re-append claims it.
					st.Proof.Reappends = append(st.Proof.Reappends, rec)
					lost = true
				}
			}
			_ = f.Close()
		}
	}

	if !torn && !lost && !windowFailed {
		// Everything proved whole, the sync carried: zero changes — the
		// fate the classing wrote stands.
		return nil
	}

	// The fate, once per segment after the flow. A segment with nothing
	// holding past its first torn record — no record lost, no window failed,
	// every bad verdict torn and no record held below a torn one — keeps
	// the torn tail's own rule: the cut is the FIRST torn record's start and
	// the segment is cut there and sealed, never emptied, for a crash that
	// tore a stream's last write costs a seal, not a segment's whole content.
	// Any lost record, any failed sync, and a torn record below one that
	// held — torn-slot rule, what held past it would be discarded by a cut
	// at the torn one's start — end the segment instead: a lost record is
	// re-appended and its extent claimed, and a failed sync leaves bytes the
	// device may never have taken, so a seal would prove nothing on it.
	//
	// The ended cut is the END of the last record that held, floored at the
	// segment's first record slot when none did, and the torn-tail cut keeps
	// the same floors and clamps: the synced-through the segment's records
	// named raises the cut only where it names no record's interior — every
	// record containing the point is one of the candidates, so a point inside
	// a record the flow did not hold would leave half a record below the cut,
	// and the cut stays at the record the straddler's rule names instead.
	// The cut never passes the file's size, and the classing's own non-zero
	// offset is never crossed, so neither of its floor rules is broken.
	kind, off := fateEnded, heldEnd
	if !lost && !windowFailed && torn && heldEnd <= tornAt {
		kind, off = fateTornTail, tornAt
	}
	if sc.SyncedThrough > off && !unheldStraddles(cands, verifiedAt, sc.SyncedThrough) {
		off = sc.SyncedThrough
	}
	off = min(off, sc.Size)
	// decision: the classing's own non-zero offset is minned in although no
	// segment the classing and this flow read from one scan can ever name a
	// cut above it: every candidate ends at or below the VerifiedEnd and
	// RecordsEnd those offsets floor at, the synced-through a raise names is
	// minned the same way at the classing's side, and a torn record's start
	// lies below its own end, so the min holds the value it would hold
	// without it. It is kept for the day a classing rule floors a cut below
	// a candidate's end — a cut this flow answers may cross no floor it does
	// not know the reason for — and it is withdrawn the day such a rule is
	// shown, rather than tested around.
	if fate.Offset != 0 {
		off = min(off, fate.Offset)
	}
	fate.Kind, fate.Offset = kind, off
	return nil
}

// unheldStraddles reports whether the point lies strictly inside a candidate
// the first read did not verify. Every record containing the point ends past
// it, so every record containing it is a candidate; one that held has its
// end in the cut already, so only an unheld one can leave the point's floor
// inside a record.
func unheldStraddles(cands []format.RecordHeader, verifiedAt map[int64]bool, point int64) bool {
	for _, h := range cands {
		if h.Offset < point && point < h.Offset+h.Size() && !verifiedAt[h.Offset] {
			return true
		}
	}
	return false
}

// dropTornRecord drops one record the flow found torn — verified at neither
// of its reads — from everything the rebuilt state holds of it: the index
// entry the walk fed for a content record, the aside entry it set for an
// aside one, and, for a content record, the placement of everything its
// replay displaced, re-placed from the records that remain. Torn is never
// written, so nothing of the record survives the open: no re-append carries
// it, no hold claims its extent, no aside phase ever answers for it — and
// what it overwrote or cut holds its extent again, as it did before the
// record's replay took it. tornSoFar holds the numbers the flow has judged
// torn already, this one joining it, so a repair is fed no record a torn
// verdict later in the run drops of its own.
func dropTornRecord(rd *reader, st *State, expect core.JournalID, sc *segmentScan, h format.RecordHeader, tornSoFar map[index.Numbers]bool) error {
	if h.Kind > format.KindUnmark {
		// decision: an aside record is held only in the scan's aside list,
		// which no built phase reads yet — the kinds' rules are the later
		// marks phase's — so dropping it changes nothing a caller can see
		// today, and the state must still not claim it once one is.
		sc.Aside = slices.DeleteFunc(sc.Aside, func(a asideRecord) bool {
			return a.Header.Offset == h.Offset
		})
		return nil
	}
	// The content kinds the walk fed the index one converted record each.
	// Only a clone target makes contentRecord read — the spec its body is —
	// and a clone target's body was verified at the scan, so a torn
	// candidate never asks for it; a read that refuses here refuses the
	// open, exactly as the flow's own reads do.
	r, cerr := contentRecord(rd, expect, h)
	if cerr != nil {
		return refusedRead(rd, cerr)
	}
	tornSoFar[r.Numbers] = true
	st.Rebuilt.Index.Unplace(r.File, index.RecordAt{Segment: r.Segment, Offset: r.At},
		core.Extent{Off: r.Off, Len: r.Len})
	return repairTornPlacement(rd, st, expect, r, tornSoFar)
}

// repairTornPlacement restores what the torn record's replay displaced: the
// index comes out exactly as the rebuild would have produced it had the
// record never been fed, over the span the record's replay reached — its
// extent, or the removal to the end a truncate's and a delete's reach names.
// The records to re-apply are every record of the file that reaches the span,
// from any attached segment — the one the torn record sits in and the named
// and unverified ones beside it, cross-segment overwrites included — minus
// the records the run has judged torn so far, and the index folds them over
// the span in the precedence the rebuild applies them in, so the answer does
// not depend on the order the segments were fed or walked.
//
// ponytail: the records to re-apply are re-scanned from disk, one walk of
// every attached segment per torn record — the scans the rebuild kept drop
// their converted records so a journal of millions of records does not hold
// them twice, and a torn record is a rare verdict of a damaged open. Keep a
// per-open record index keyed on file and extent, shared by every repair the
// run makes, when an open that finds many torn records shows these re-walks
// in a profile.
func repairTornPlacement(rd *reader, st *State, expect core.JournalID, torn index.Record, tornSoFar map[index.Numbers]bool) error {
	window := tornReach(torn)
	if window.Len <= 0 {
		// A record whose replay reached nothing — a marks kind over no
		// extent, a truncate past every offset — placed and cut nothing the
		// index holds.
		return nil
	}
	var recs []index.Record
	collect := func(id core.SegmentID) error {
		// A read the bound does not clear refuses the open, as the open's
		// other reads do; the scans the rebuild made of these bytes already
		// succeeded, so this only refuses where the device stopped answering.
		sc, err := scanSegment(rd, expect, SegmentInfo{ID: id})
		if err != nil {
			return refusedRead(rd, err)
		}
		for _, r := range sc.Records {
			if r.File != torn.File || tornSoFar[r.Numbers] || !reachesOverlap(tornReach(r), window) {
				continue
			}
			recs = append(recs, r)
		}
		return nil
	}
	for _, info := range st.Attached.Own {
		if err := collect(info.ID); err != nil {
			return err
		}
	}
	for _, id := range slices.Concat(st.Attached.Unverified, st.Attached.ZeroHeader) {
		if err := collect(id); err != nil {
			return err
		}
	}
	st.Rebuilt.Index.ReplayWindow(torn.File, window, torn.Numbers.Version, recs)
	return nil
}

// tornReach is the span of the file a record's replay reached: the extent
// its kind names, or the removal to the end a truncate's and a delete's
// reach amounts to — the same reach the rebuild's drop ranges note the
// record by.
func tornReach(r index.Record) core.Extent {
	switch r.Kind {
	case index.RecTruncate:
		return core.Extent{Off: r.Off, Len: math.MaxInt64 - r.Off}
	case index.RecDelete:
		return core.Extent{Off: 0, Len: math.MaxInt64}
	default:
		return core.Extent{Off: r.Off, Len: r.Len}
	}
}

// reachesOverlap reports whether two reaches cover a byte in common.
func reachesOverlap(a, b core.Extent) bool {
	return a.Off < b.Off+b.Len && b.Off < a.Off+a.Len
}

// tailCandidates walks the segment's record headers again and returns the
// records the proof reads — each attached kind, content and aside both, whose
// end — the header's own size of itself and its body, never the index's — is
// past the highest synced-through the segment's verifying records named, a
// record that straddles the point included — together with heldBelow, the
// end of the last record lying at or below the point: the sync that named
// the point proved such a record whole, so the flow counts it held. A record
// the classing already dropped is out of the candidates — confined, or a
// clone target whose body failed: its torn-or-corrupt verdict is judged, not
// judged twice — while its end still counts toward heldBelow when it lies
// below the point, for the same proving sync names it.
//
// ponytail: the rebuild holds nothing of the records it fed the index — the
// scans it keeps drop them, so a journal of millions of records does not
// carry an index record twice — so the proof walks the segment's headers
// again, one bounded read per record, on the one segment per stream whose
// tail is unproven. Keep the unproven tail's record offsets in the scan, set
// where the scan walks, when an open whose newest segments are long shows
// the re-walk in a profile.
func tailCandidates(rd *reader, expect core.JournalID, sc segmentScan) ([]format.RecordHeader, int64, error) {
	walk, werr := format.ScanSegmentHeaders(
		readerAt{rd: rd, name: format.SegmentName(sc.ID)}, sc.Size, expect, sc.ID)
	if werr != nil {
		// The open's walk of these same bytes already succeeded, so a walk
		// erroring here is a read the bound did not clear, and it refuses
		// the open as the open's own reads do.
		return nil, 0, refusedRead(rd, werr)
	}
	judged := make(map[int64]bool, len(sc.Dropped))
	for _, d := range sc.Dropped {
		judged[d.Header.Offset] = true
		if d.Entry.RecordOffset != 0 {
			judged[d.Entry.RecordOffset] = true
		}
	}
	out := make([]format.RecordHeader, 0, len(walk.Records))
	heldBelow := int64(format.SegmentHeaderSize)
	for _, r := range walk.Records {
		h := r.Header
		end := h.Offset + h.Size()
		if end <= sc.SyncedThrough {
			heldBelow = max(heldBelow, end)
			continue // held whole by the sync that named the point
		}
		if judged[h.Offset] {
			continue
		}
		out = append(out, h)
	}
	return out, heldBelow, nil
}

// tailFirst reads one proof record's whole bytes through the open's reader —
// the header at the record's own offset to learn the body's length, then the
// body, both checksums checked — and keeps them as the copy a later re-append
// writes back from. It reports whether the record verified whole (a record
// whose bytes read and do not verify is torn); a read the bound did not clear
// refuses the open, exactly as tailCandidates' walk answers its own.
func tailFirst(rd *reader, expect core.JournalID, seg core.SegmentID, name string, at int64) (rec tailRecord, verified bool, err error) {
	head := make([]byte, format.RecordHeaderSize)
	n, rerr := rd.ReadAt(name, head, at)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// A short read is a read to the file's own end: its bytes go to the
		// decoders, which judge them; every other error is the open's.
		return tailRecord{}, false, refusedRead(rd, rerr)
	}
	h, derr := format.DecodeRecordHeader(head[:n], expect, seg, at)
	if derr != nil {
		return tailRecord{}, false, nil
	}
	body := make([]byte, h.BodySize())
	nb, rerr := rd.ReadAt(name, body, at+format.RecordHeaderSize)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return tailRecord{}, false, refusedRead(rd, rerr)
	}
	if _, verr := format.VerifyRecordBody(body[:nb], expect, h); verr != nil {
		return tailRecord{}, false, nil
	}
	first := make([]byte, 0, format.RecordHeaderSize+h.BodySize())
	first = append(first, head[:n]...)
	first = append(first, body[:nb]...)
	return tailRecord{Segment: seg, At: at, Header: h, First: first}, true, nil
}

// tailOnDevice re-reads one record the first read verified — whole, in one
// direct read of the bytes the first read measured, for the record's length
// is known and no blind block is opened — through the open file's direct
// read, the device's own bytes and never the page the cache holds, under the
// read bound. It reports whether the device holds the record's verifying
// bytes.
//
// decision: a re-read that fails — a filesystem's refusal included — is not
// a refusal of the open, for the record below it already verified at its
// first read and its copy is kept: the record is lost by the device, and the
// re-append writes it back. This is the one read at open whose refusal does
// not keep the journal closed — the ceiling read.go's refusal decisions
// name, withdrawn with theirs when an unread record can be held instead of
// refused. A short read is the same verdict as disagreeing bytes: the device
// answered with what it holds. Only the context's error comes back as
// itself, answered by the caller, so a cancelled open is never taken for a
// lost record.
func tailOnDevice(ctx context.Context, f storage.File, rd *reader, expect core.JournalID, rec tailRecord) (holds bool, err error) {
	// The whole record, header and body, at the offset its header named: one
	// read answers the device's copy of the record the first read kept.
	whole := make([]byte, len(rec.First))
	var n int
	_, rerr := readretry.Do(ctx, rd.clk, readretry.Attempts, func() error {
		var e error
		n, e = f.ReadAtDirect(whole, rec.At)
		if errors.Is(e, io.EOF) {
			return nil // a read to the file's own end completed, short
		}
		return e
	})
	if isContextErr(rerr) {
		return false, rerr
	}
	if rerr != nil {
		return false, nil // the device refused the read: the record is lost, never a verdict
	}
	if n < format.RecordHeaderSize {
		return false, nil // the device answered short of the record's own header
	}
	h, derr := format.DecodeRecordHeader(whole[:format.RecordHeaderSize], expect, rec.Segment, rec.At)
	if derr != nil {
		return false, nil // bytes that read and do not agree are lost with the record whole
	}
	if _, verr := format.VerifyRecordBody(whole[format.RecordHeaderSize:n], expect, h); verr != nil {
		return false, nil
	}
	return true, nil
}

// refusedRead answers an error the open did not clear, as the open's own
// house read rule takes it: the context's error as itself first, then the
// ranges the reader collected as the refusal that names what could not be
// read, then the error as its own.
func refusedRead(rd *reader, err error) error {
	if isContextErr(err) {
		return err
	}
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return &core.UnreadableRangesError{Ranges: ranges}
	}
	return err
}
