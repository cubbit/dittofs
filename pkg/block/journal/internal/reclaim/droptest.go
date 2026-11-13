package reclaim

import (
	"maps"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// covered returns the extent rec's record covers, spelled the way the drop
// query keeps its ranges: a truncate covers everything past the size its
// header names, a delete the whole file, and every other kind the header's
// own extent. A header-only rec for a truncate or delete carries the length 0
// the format spells those removals with, which alone would cover no byte and
// every drop test on one would trivially pass.
func covered(rec Rec) core.Extent {
	switch rec.Kind {
	case format.KindTruncate:
		return core.Extent{Off: rec.Ext.Off, Len: math.MaxInt64 - rec.Ext.Off}
	case format.KindDelete:
		return core.Extent{Off: 0, Len: math.MaxInt64}
	default:
		return rec.Ext
	}
}

// Droppable answers whether rec's record can be dropped: dropped as dead
// weight by a repack pass, its count released by the re-test; false means
// the record still stands for something on disk and its segment is held by
// counting it. The answer reads the index and the tracker's registries only,
// never a segment, and compares by the pair the drop query keeps — version
// then sequence, per extent and per file — never by sequence alone, never
// against the whole file, never against the journal-wide lowest pair:
//
//   - a truncate, deallocate, delete or clone target record is droppable
//     once its removal marker is settled and nothing it may still outrank
//     remains in another segment;
//   - a release record is droppable once nothing it may still outrank
//     remains, and so is a synced loss whose reason is a superseding record,
//     which left no marker;
//   - another synced loss is droppable once Settle has covered its marker
//     — the marker its own version left — beside outranking nothing;
//   - an unsynced loss reaches only the record it names, so it is droppable
//     exactly when no live registration of the file carries the sequence
//     number it names and nothing it can outrank remains either;
//   - every other kind is never droppable.
//
// An unknown kind is never droppable either: what the tracker cannot name it
// keeps, the safe direction of a removal kept too long.
//
// It takes the registry lock for every kind, the forget and unsynced-loss
// rules read registries it guards; the re-test, which holds that lock across
// its walk, reaches the same answer through droppableLocked.
func (t *Tracker) Droppable(rec Rec) bool {
	t.rm.Lock()
	defer t.rm.Unlock()
	return t.droppableLocked(rec)
}

// droppableLocked is Droppable with the registry lock held by the caller, so
// the re-test's walk can ask after each record without taking the lock a
// second time.
func (t *Tracker) droppableLocked(rec Rec) bool {
	e := covered(rec)
	switch rec.Kind {
	case format.KindTruncate, format.KindDeallocate, format.KindDelete, format.KindCloneTarget:
		return t.ix.MarkerSettled(rec.File, rec.Pair.Version) &&
			!t.ix.MayOutrank(rec.File, rec.Seg, e, rec.Pair)
	case format.KindRelease:
		return !t.ix.MayOutrank(rec.File, rec.Seg, e, rec.Pair)
	case format.KindLoss:
		if !rec.Synced {
			// An unsynced loss reaches only the record it names, by its
			// sequence number, wherever that record sits, so it is
			// droppable once nothing holds that record: no live
			// registration of the file carries the named number — the
			// records the segments hold of the file are the tracker's own
			// registrations, and the record the loss named was one of the
			// file's — and no holder of the loss's own presence marks
			// remains — the open-time answer to the same question, a
			// registry entry that holds nothing by itself — and once
			// nothing else it can outrank remains either.
			//
			// decision: the registry is trusted only beside an outranking
			// guard, and the answer fails closed on both. The registry
			// knows every record to answer the reach of the loss only if
			// every path that appends a record registers it — the open-time
			// registration of a rebuild's records included — and nothing
			// proves that yet, so a named record the registry never heard
			// of would turn the loss droppable while its bytes are still on
			// disk, and a crash would resurrect a record the loss says is
			// gone. The MayOutrank guard bounds that failure: a lower-pair
			// record of the file overlapping the loss's extent in another
			// segment keeps the loss whatever the registry says, and an
			// unregistered record can only be one more record below the
			// loss's pair inside its extent that the guard keeps the loss
			// for. Overturned by the registration being proven complete —
			// every append path registering, with a check that fails when
			// one does not — after which the guard only costs headroom and
			// may go.
			for r := range t.recs[rec.File] {
				if r.Seq == rec.Named {
					return false
				}
			}
			if len(t.marks[rec]) > 0 {
				return false
			}
			return !t.ix.MayOutrank(rec.File, rec.Seg, e, rec.Pair)
		}
		if rec.Superseded {
			return !t.ix.MayOutrank(rec.File, rec.Seg, e, rec.Pair)
		}
		return t.ix.MarkerSettled(rec.File, rec.Pair.Version) &&
			!t.ix.MayOutrank(rec.File, rec.Seg, e, rec.Pair)
	case format.KindForget:
		// A forget record is droppable exactly when its snapshot is gone:
		// no segment holding records of its tag below it remains in the
		// set. A record never registered with a snapshot — an empty one,
		// or a plain Register of one — answers droppable at once, and
		// drops at the next event of its file like any record registered
		// already droppable.
		return len(t.forgets[rec]) == 0
	default:
		// ponytail: the kinds that decide bits and holds — offloaded,
		// unmark, hold, unhold, stamp, reconcile and reconciled — are all
		// answered never droppable by this one rule, which is the safe
		// direction: a segment carrying them is held until its records'
		// rules land. The narrower rule per kind — an offloaded or unmark
		// record is live while it decides the bit of a held or retained
		// extent, a hold, unhold or stamp while the hold it records is, a
		// reconcile until a settling record names it — is applied here the
		// day the calls that write those records land, because carrying
		// them all now would drain a segment's headroom under an offload
		// workload before any of them can leave.
		return false
	}
}

// markTaken drops an unsynced loss record's presence marks once the
// registry has given up its registration, so a mark set exists only while
// its loss record counts live, exactly as forgetTaken treats a snapshot.
// The caller holds rm.
func (t *Tracker) markTaken(rec Rec) {
	if rec.Kind == format.KindLoss {
		delete(t.marks, rec)
	}
}

// retest walks the records registered live of files and, for each one the
// drop test now answers droppable, unregisters it — every registration the
// registry hands over is taken under the same rm the walk holds, one
// release per registration actually given up (a file named twice by files
// collects its records twice but the registry hands each registration to
// exactly one taker, so no count is lowered a second time), and two re-tests
// of one file racing each other can never collect a record twice. When the
// releases leave the segment holding nothing, the candidate fires as any
// release's does. It is the one place a registered record drops; every
// event below reaches it.
//
// ponytail: a file with many removal records re-tests them all at each
// event, O(records of the file) a walk that also holds rm across its index
// queries. Upgrade to an index of removal records by extent when a re-test
// of a heavy file shows in a profile, which also lifts rm off the walk's
// reads.
func (t *Tracker) retest(files ...core.FileID) {
	t.rm.Lock()
	rel := make([]Rec, 0, 8)
	seen := make(map[core.FileID]struct{}, len(files))
	for _, f := range files {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		for rec, n := range t.recs[f] {
			for range n {
				if t.droppableLocked(rec) && t.take(rec) {
					t.forgetTaken(rec)
					t.markTaken(rec)
					t.releasedSpent(rec)
					rel = append(rel, rec)
				}
			}
		}
	}
	// Every taken registration leaves the registry here, under the walk's
	// lock, before any count moves: the registry is the release's authority,
	// so a caller's paired Unregister after the re-test takes nothing from
	// it, and only a take that succeeded carries a count away.
	t.rm.Unlock()
	for _, rec := range rel {
		t.releaseCount(rec)
	}
}

// releasedSpent counts one registration of rec a re-test's release spent
// into the per-segment released set, the record the carry's prologue re-asks
// once the pass's copies are noted. The caller holds rm.
func (t *Tracker) releasedSpent(rec Rec) {
	if t.released == nil {
		t.released = make(map[core.SegmentID]map[Rec]int)
	}
	if t.released[rec.Seg] == nil {
		t.released[rec.Seg] = make(map[Rec]int)
	}
	t.released[rec.Seg][rec]++
}

// Retest re-tests the records registered live of each of files: each one the
// drop test now answers droppable stops counting live, exactly as Unregister
// releasing it would. It is the event a change of a file's state carries that
// the tracker has none of its own: a settle the caller ran straight through
// the index, a range retired the file's answers counted on, a pass's copies
// appended on disk below the removals that could outrank them — the test runs
// only once the pass's own copies are appended, its target segment's included.
func (t *Tracker) Retest(files ...core.FileID) { t.retest(files...) }

// MarkerSettled states that metadata has recorded every removal of file at
// or below v — the marker-covered state the index serves is settled with it
// — and re-tests the records registered live of the file: the markers at or
// below v leave the index, so a removal, a release or a loss record of the
// file whose only remaining reason to live was its marker may now be
// droppable.
func (t *Tracker) MarkerSettled(file core.FileID, v core.Version) {
	t.ix.Settle(file, v)
	t.retest(file)
}

// pruneMarks takes the retired segment out of every unsynced loss record's
// presence marks and returns the files of the losses whose mark sets
// emptied here: the marks hold nothing and lower no count, but a loss
// whose last holder left is one the re-test must now judge — through the
// same drop test every release reaches, with the registry walk and the
// outranking guard, never taken here — and the files returned are the
// entries the retirement's re-test needs to reach those losses even when
// the caller's files list names none of them. The caller holds rm.
func (t *Tracker) pruneMarks(seg core.SegmentID) []core.FileID {
	var files []core.FileID
	for rec, marks := range t.marks {
		if rec.Seg == seg {
			// The loss record counted in the retired segment; its marks
			// leave with its registration.
			delete(t.marks, rec)
			continue
		}
		if _, ok := marks[seg]; !ok {
			continue
		}
		delete(marks, seg)
		if len(marks) == 0 {
			delete(t.marks, rec)
			files = append(files, rec.File)
		}
	}
	return files
}

// SegmentRetired ends a segment's liveness: every record still registered
// live in seg stops counting — the registrations are taken from their
// files' registries under rm, then their counts lowered one by one through
// the same release path a record's own Unregister takes, so the candidate
// fires for the held-to-not-held transition the retirement crosses exactly
// as a release does — the segment's byte counters leave the index too
// (RetireSegment below, idempotent in the index, so a journal whose own
// retirement relays the same call meets it retired and retires nothing
// twice), and the files its records named are re-tested, their answers no
// longer kept by anything in seg. The drop ranges the segment holds of
// each named file do NOT leave with the retirement: the file list waits
// at pendingGone for RemovalDurable, the retirer's durable signal, so a
// removal record whose retention counted the segment answers not droppable
// until the unlink that retired it is durable — a drop that preceded the
// unlink's durability would have a crash bring the retired segment back
// without the removal record that covered it. The retirer runs the retired
// hook — this call — after the segment has left the set but before the
// directory sync; a recycle's own sync has already returned by then, so
// its pending entry lives only the microseconds between the two hooks.
// files, when empty, is filled from the tracker's own registry of
// the segment's files; a caller carrying its own list must name every file
// the segment held, or a file its list misses keeps its registry entries
// for a segment no re-test will drop again. Every record registered live of
// the retired segment is taken whatever the files list names — a forget
// record's zero file is one such, and a caller's list never names it. The
// holds citing seg as their copies are released with the retirement — the
// segment is gone, and a still-held record of the segment it held ended
// was carried to a copy durable before the hand-over — through the same
// once-per-hold lowering a mark or a seal takes, and the holds the retired
// segment itself was the keeping segment of leave the registry as its
// state leaves the tracker, so neither registration outlives the segment.
// The segment's state leaves the tracker with the segment, so a scan of
// the states never visits a retired one and Held of a retired segment
// answers from the index's counters alone, which the relay has emptied.
// The journal's own wiring hands the retirer's retired hook the call with
// the index's files of the segment — the list that covers content-only
// files the tracker's registry never held, which a nil files list fills
// from the registry and so misses — and hands the durable signal the
// RemovalDurable call that consumes what the retirement parked:
//
// decision: the retired hook — like the set's retire observer before it —
// fires once the segment has left the set but before the directory sync
// that would make the unlink or recycle durable, so the drop ranges the
// file list names wait behind the park for RemovalDurable rather than
// leaving with the retirement itself: a drop that preceded the unlink's
// durability would have a crash bring the retired segment back without
// the removal record that covered it, and a failed directory sync never
// reaches the parked list's consumer at all. A caller wiring the set's
// retire observer instead hands the same shape a durability order it
// neither observes nor controls — safe only where that caller has made
// removal durable before the Retire the observer rides.
//
// ponytail: the retirement walks the whole registry of forget records to
// prune the ones naming the segment, O(forgets outstanding) beside a
// records walk that visits every registered record of every file,
// O(all registered records) — all of it under rm, and the call runs in
// the retirer's retired-hook delivery, so a stream roll can wait behind
// one retirement's whole walk. Upgrade to a walk scoped
// to the retiring segment's own files, from the state's files table, and
// an index of forget records by snapshot segment, when a retirement of a
// busy journal shows in a profile.
func (t *Tracker) SegmentRetired(seg core.SegmentID, files []core.FileID) {
	t.rm.Lock()
	// The released set left with the segment's bytes: a record the re-test
	// spent is no longer recoverable once nothing holds the bytes its
	// release covered.
	delete(t.released, seg)
	st := t.stateOf(seg)
	if st != nil {
		st.mu.Lock()
		if len(files) == 0 {
			for f := range st.files {
				files = append(files, f)
			}
		}
		st.mu.Unlock()
	}
	rel := make([]Rec, 0, 8)
	for f, rs := range t.recs {
		for rec := range rs {
			if rec.Seg != seg {
				continue
			}
			// take() until the registry holds no more of this record — one
			// collected entry per registration it actually handed over —
			// so the counts below lower for exactly the registrations that
			// were live: a take that refused (nothing left of the record)
			// releases nothing. The zero file a forget record registers
			// under is walked here like any other, whatever the files list
			// named.
			for t.take(rec) {
				t.forgetTaken(rec)
				t.markTaken(rec)
				rel = append(rel, rec)
			}
		}
		if len(rs) == 0 {
			delete(t.recs, f)
		}
	}
	// The retired segment leaves every forget snapshot that names it, and
	// a forget record whose snapshot empties here — its last tag-record
	// bearing segment gone — is released exactly as the re-test would
	// release it: the take gates the count on the registry as any release
	// does, once per registration a doubled record holds.
	for rec, snap := range t.forgets {
		if rec.Seg == seg {
			// The record counted in the retired segment; its snapshot
			// leaves with its registration.
			delete(t.forgets, rec)
			continue
		}
		if _, ok := snap[seg]; !ok {
			continue
		}
		delete(snap, seg)
		if len(snap) == 0 {
			delete(t.forgets, rec)
			// take() until the registry holds no more of the record, like
			// the records walk above: a caller may have registered the
			// forget twice, and each live registration lowers one count.
			for t.take(rec) {
				rel = append(rel, rec)
			}
		}
	}
	// The retired segment leaves every mark set that names it: a mark
	// holds nothing, so its leaving lowers no count — but a loss record
	// whose marks empty here, its last holder gone, is NOT taken. It is
	// instead re-tested: its file joins files, and the drop test the
	// retirement's re-test runs judges it through the registry walk and
	// the outranking guard like any other record, so nothing releases
	// while the named record is still registered somewhere or something
	// lower-pair overlaps what the loss covers. The appended file also
	// covers a caller's list that names none of it. The loss records the
	// marks name sit in other segments — a segment holding a copy of what
	// a loss in it names is a reach the record the loss names does not
	// have — so the only marks of the retired segment's own records would
	// be a loss and its holder in one, whose registration the take above
	// has already released.
	files = append(files, t.pruneMarks(seg)...)
	// The retire's half of the durable-unlink gate: the file list waits
	// here for RemovalDurable, and the drop ranges it names stay in the
	// index until then, keeping every removal that counted on the segment
	// answering not droppable.
	if len(files) > 0 {
		if t.pendingGone == nil {
			t.pendingGone = make(map[core.SegmentID][]core.FileID)
		}
		// Accumulated per relay, never replaced: the journal's own retirement
		// relays SegmentRetired beside the retirer's hook, so a second list
		// must not strand the first's files. A file named twice is harmless —
		// RemovalDurable's SegmentGone is idempotent and its re-test keeps
		// one entry per file.
		t.pendingGone[seg] = append(t.pendingGone[seg], files...)
	}
	t.rm.Unlock()
	t.retireHolds(seg)
	for _, rec := range rel {
		t.releaseCount(rec)
	}
	t.forget(seg)
	t.ix.RetireSegment(seg)
	t.retest(files...)
}

// RemovalDurable consumes the pending file list SegmentRetired stored for
// seg — the retirer's durable signal runs it once the directory sync after
// the unlink or recycle returned nil — relaying each named file's
// ix.SegmentGone(f, seg) and re-testing those files, so the index serves
// no drop query against the retired segment's records any more and a
// removal record whose retention counted the segment may drop at the next
// event of its file. The ranges' leaving lands here, never at the
// retirement itself: a retirement whose directory sync failed never reaches
// this call, and its entry stays where pendingGone's own decision names.
// A segment with no pending entry — one never retired, or one whose
// durability this call already consumed — releases nothing. The entry is
// consumed under the registry lock it was stored with, so a second call
// finds no list to relay a second time.
func (t *Tracker) RemovalDurable(seg core.SegmentID) {
	t.rm.Lock()
	files := t.pendingGone[seg]
	delete(t.pendingGone, seg)
	t.rm.Unlock()
	for _, f := range files {
		t.ix.SegmentGone(f, seg)
	}
	t.retest(files...)
}

// retireHolds clears the ended-hold registry along a retirement: the holds
// citing seg as their copies are released — a retired segment's proofs are
// closed, and the records a hold kept the citing segments holding were
// carried to a durable copy before the hand-over — and the holds seg itself
// was the keeping segment of leave the registry as its state leaves the
// tracker, their keeping segment gone: neither needs a lowering, and the
// registry entry is the leak this clears. Each other hold is lowered once,
// on the segment that keeps it.
func (t *Tracker) retireHolds(seg core.SegmentID) {
	t.hm.Lock()
	rel := t.holds[seg]
	delete(t.holds, seg)
	for copies, hs := range t.holds {
		kept := hs[:0]
		for _, h := range hs {
			if h.seg == seg {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			delete(t.holds, copies)
			continue
		}
		t.holds[copies] = kept
	}
	t.hm.Unlock()
	for _, h := range rel {
		t.releaseHold(h)
	}
}

// forget removes the segment's state from the tracker's table, so the table
// keeps only the segments the journal still holds and the copy-per-creation
// table stays bounded by the live segments. A segment retired before any
// record of it was registered has no state and nothing to forget.
func (t *Tracker) forget(seg core.SegmentID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.m.Load()
	if m == nil {
		return
	}
	if _, ok := (*m)[seg]; !ok {
		return
	}
	copied := make(map[core.SegmentID]*segState, len(*m)-1)
	maps.Copy(copied, *m)
	delete(copied, seg)
	t.m.Store(&copied)
}
