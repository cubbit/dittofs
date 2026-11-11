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
// It takes the registry lock only for an unsynced loss; the re-test, which
// holds that lock across its walk, reaches the same answer through
// droppableLocked.
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
			// droppable once no live registration of the file carries the
			// named number — the records the segments hold of the file are
			// the tracker's own registrations, and the record the loss
			// named was one of the file's — and once nothing else it can
			// outrank remains either.
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

// Retest re-tests the records registered live of file: each one the drop
// test now answers droppable stops counting live, exactly as Unregister
// releasing it would. It is the event a change of the file's state carries
// that the tracker has none of its own: a settle the caller ran straight
// through the index, a range retired the file's answers counted on.
func (t *Tracker) Retest(file core.FileID) { t.retest(file) }

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

// CopiesDone re-tests the records registered live of each of files: a
// pass's copies are on disk below the removals that could outrank them
// like any other record, so the test runs only once the pass's own copies
// are appended, its target segment's included.
func (t *Tracker) CopiesDone(files []core.FileID) { t.retest(files...) }

// SegmentRetired ends a segment's liveness: every record still registered
// live in seg stops counting — the registrations are taken from their
// files' registries under rm, then their counts lowered one by one through
// the same release path a record's own Unregister takes, so the candidate
// fires for the held-to-not-held transition the retirement crosses exactly
// as a release does — the relations the segment held in the index leave
// too (its byte counters, and each named file's drop ranges of it, both
// idempotent in the index, so a journal whose own retirement relays the
// same calls meets them retired and retires nothing twice), and the files
// its records named are re-tested, their answers no longer kept by anything
// in seg. files, when empty, is filled from the tracker's own registry of
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
//
// ponytail: the retirement walks the whole registry of forget records to
// prune the ones naming the segment, O(forgets outstanding) beside the
// records walk it already runs. Upgrade to an index of forget records by
// snapshot segment when a retirement with many outstanding forgets shows
// in a profile.
//
// The segment's state leaves the
// tracker with the segment, so a scan of the states never visits a retired
// one and Held of a retired segment answers from the index's counters
// alone, which the relay has emptied. Call it once the unlink or recycle
// is durable.
func (t *Tracker) SegmentRetired(seg core.SegmentID, files []core.FileID) {
	t.rm.Lock()
	st := t.stateOf(seg)
	if st != nil {
		st.mu.Lock()
		if len(files) == 0 {
			for f := range st.files {
				files = append(files, f)
			}
			st.files = nil
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
	// does.
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
			if t.take(rec) {
				rel = append(rel, rec)
			}
		}
	}
	t.rm.Unlock()
	t.retireHolds(seg)
	for _, rec := range rel {
		t.releaseCount(rec)
	}
	t.forget(seg)
	t.ix.RetireSegment(seg)
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
