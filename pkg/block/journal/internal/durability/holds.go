package durability

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// segHold is the hold on one failed segment while copies in it are in use:
// the records served from a copy in the segment whose re-append has not yet
// completed at a new place, keyed by sequence number, and the segment's claim.
// A segment with a hold is unresolved: no window of it is announced resolved
// while the hold lives, and the hold's end announces it.
//
// decision: the holds sit in st.holds under the stream's leaf mutex st.mu,
// not under any file's append domain: a segment holds the records of many
// files, so no one file's domain can guard whether it is resolved. The
// segment ledger's own mutex is taken under st.mu, by Set.Claim, Set.Release
// and Set.State, so that a hold's first record and its claim, and its last
// record and the release, are each one step: a second read on the segment
// never sees the record of the first without the claim behind it. The order
// is st.mu, then the Set's mutex, and it cannot invert: the Set's mutex is the
// capacity domain's leaf, which takes nothing while held, and the retirer
// calls ClaimRetirement and Release with no lock of its own held and never
// reaches st.mu. Overturned if the Set ever calls out under its mutex, or if a
// retirer must read the hold under a lock it holds over the Set.
//
// ponytail: one map per held segment, bounded by the records with copies in
// use from failed segments, and the worker visits every held record of every
// held segment on each pass, taking that record's file domain and asking the
// index twice. Upgrade to waking the worker only on a change that can move a
// hold (a claim let go, a write over a held record, a new place completed) when a
// journal with many held records shows the visits in a profile. A claimed
// hold whose re-append keeps failing is retried on every pass and at every
// tick of the sync bound with no backoff; upgrade to a per-hold retry backoff
// when a persistently failing re-append shows up as worker load or log volume.
type segHold struct {
	records map[core.Seq]ownedCopy
	// claimed says the segment's claim in the set is the hold's; owed says the
	// set refused it when the first record joined and the worker retries it.
	// One of the two is set while the hold lives.
	claimed bool
	owed    bool
	// withheld says a window of the segment resolved while it was held, so its
	// announcement waits for the hold to end.
	withheld bool
	// landed says a held record left the segment by a re-append: its new
	// place completed, or was lost after the index had moved off the copy.
	landed bool
	// placed are the held records already re-appended, keyed by sequence
	// number: the place each was appended at, which the record leaves the
	// hold for once that place has completed.
	placed map[core.Seq]segment.Location
	// pinned are the held records the index may still name at their corrupt
	// place, keyed by sequence number: each counts once toward its corrupt
	// record's segment in d.unproduced.
	pinned map[core.Seq]struct{}
}

// pinLocked records whether the index may still name the corrupt place of h's
// held record seq, and keeps the count of such records per corrupt segment
// that the damaged report reads. A record not in h changes nothing. The caller
// holds mu.
//
// decision: the count is maintained at the events that change it, not read
// from the holds or the index, so Stats reads it under dmu alone, with no lock
// a write needs and no walk (R62). A record is counted from when a read adds
// it to a hold, under its file's domain, and uncounted once a repoint has
// moved it off the corrupt place, the index is found naming it there no more
// (an overwrite or another read's repoint), or it leaves the hold. An
// overwrite is seen only on the worker's next pass over the hold, so the
// count can stay up a pass longer than the index names the record: a block
// that lasts too long, never one that ends early. Overturned if a reclamation
// pass must select such a segment within that pass.
func (st *stream) pinLocked(h *segHold, seq core.Seq, on bool) {
	c, ok := h.records[seq]
	if !ok {
		return
	}
	if _, was := h.pinned[seq]; was == on {
		return
	}
	if on {
		if h.pinned == nil {
			h.pinned = map[core.Seq]struct{}{}
		}
		h.pinned[seq] = struct{}{}
		st.d.countUnproduced(c.Record.Segment, 1)
		return
	}
	delete(h.pinned, seq)
	st.d.countUnproduced(c.Record.Segment, -1)
}

// unpin records that the index no longer names the corrupt place of the
// record seq held on copy segment seg. It takes mu.
func (d *Durability) unpin(st *stream, seg core.SegmentID, seq core.Seq) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if h := st.holds[seg]; h != nil {
		st.pinLocked(h, seq, false)
	}
}

// dropHeldLocked takes seq out of h, uncounting it first. The caller holds mu.
func (st *stream) dropHeldLocked(h *segHold, seq core.Seq) {
	st.pinLocked(h, seq, false)
	delete(h.records, seq)
	delete(h.placed, seq)
}

// holdVerdict is what holdCopy decided for a record served from a copy.
type holdVerdict uint8

const (
	// holdRepoint is a record held under the segment's claim: the index may
	// be repointed to the copy.
	holdRepoint holdVerdict = iota + 1
	// holdOwed is a record held while the segment's claim is owed: nothing
	// is repointed into the segment, and the read is served from the copy.
	holdOwed
	// holdGone is a copy whose segment the set no longer holds: the copy is
	// a loss, and nothing was held.
	holdGone
)

// holdCopy adds the record c to the hold on its copy's segment, and reports
// what the read may do with the copy and whether this call added the record.
// It is called under the record's file append domain, before the repoint, and
// only outside a read-only journal. A record already held adds nothing, so a
// read that finds the same record again holds it once. The record that starts
// a hold takes the segment's claim; when the set refuses it the hold stays,
// with the claim owed, and the worker retries it.
//
// decision: the hold is per record, kept here, because the set's claim is one
// flag per segment with no owner: a second read on a held segment only joins
// the set and takes no claim, and the claim is released only by the step that
// empties the set. Overturned if the set's claim ever carries an owner and a
// count.
//
// decision: a refused claim is not read as a retirement. Set.Claim answers
// only whether it was granted, so a repack's claim mid-move and a retirement
// under way look the same; the set is asked whether the segment still exists,
// and only a segment it no longer holds is gone. A copy read from a segment
// already gone is a loss, as a copy read after its unlink would be.
// Overturned if the set's refusal ever says why.
func (d *Durability) holdCopy(st *stream, c ownedCopy) (v holdVerdict, added bool) {
	set, seg := st.s.Set(), c.Copy.Segment
	st.mu.Lock()
	h := st.holds[seg]
	if h == nil {
		h = &segHold{records: map[core.Seq]ownedCopy{}}
		switch {
		case set.Claim(seg):
			h.claimed = true
		case segmentGone(set, seg):
			st.mu.Unlock()
			return holdGone, false
		default:
			h.owed = true
		}
		if st.holds == nil {
			st.holds = map[core.SegmentID]*segHold{}
		}
		st.holds[seg] = h
	}
	if _, in := h.records[c.Seq]; !in {
		h.records[c.Seq] = c
		added = true
		// The index names the corrupt place until the repoint moves it.
		st.pinLocked(h, c.Seq, true)
	}
	v = holdRepoint
	if h.owed {
		v = holdOwed
	}
	st.mu.Unlock()
	// The worker re-appends the record, or retries the owed claim, and arms
	// its retry tick.
	st.wake()
	return v, added
}

// unholdCopy takes back the hold holdCopy gave c when the repoint then moved
// nothing. It is called under the record's file append domain.
//
// decision: a hold taken back re-appended nothing, so its end returns the
// segment to where it stood before the hold: the claim the hold took is
// released, and the segment is announced resolved only if a window of it
// resolved while it was held, whose announcement the hold withheld. A segment
// already announced is not announced again, and one with a window still
// pending is announced by that window. Overturned if a resolved announcement
// must follow every hold, which would need its consumer to bear repeats.
func (d *Durability) unholdCopy(st *stream, c ownedCopy) {
	set, seg := st.s.Set(), c.Copy.Segment
	st.mu.Lock()
	h := st.holds[seg]
	announce := false
	if h != nil {
		if cur, ok := h.records[c.Seq]; ok && cur == c {
			st.dropHeldLocked(h, c.Seq)
			announce = st.releaseLocked(set, seg, h)
		}
	}
	st.mu.Unlock()
	if announce && d.resolved != nil {
		d.resolved(st.no, seg)
	}
}

// releaseLocked ends h, the hold on seg, once it holds no record: it leaves
// st.holds and its claim is released. It reports whether the segment is to be
// announced resolved: a held record left it by a re-append or a resolution was
// withheld, and no window of the segment is pending, whose own resolution then
// announces it. The caller holds mu.
func (st *stream) releaseLocked(set *segment.Set, seg core.SegmentID, h *segHold) bool {
	if len(h.records) != 0 || st.holds[seg] != h {
		return false
	}
	delete(st.holds, seg)
	if h.claimed {
		set.Release(seg)
	}
	return (h.landed || h.withheld) && !st.pendingLocked(seg)
}

// pendingLocked reports whether seg has failed and its window is not
// resolved: the failure is recorded with its window still to be taken, or the
// window is pending. The caller holds mu.
func (st *stream) pendingLocked(seg core.SegmentID) bool {
	return st.segmentFailedLocked(seg) || slices.ContainsFunc(st.windows, func(w *window) bool { return w.seg == seg })
}

// withholdLocked reports whether seg is held, and if so marks that a window
// of it resolved, so the hold's end announces it. The caller holds mu.
func (st *stream) withholdLocked(seg core.SegmentID) bool {
	h := st.holds[seg]
	if h == nil {
		return false
	}
	h.withheld = true
	return true
}

// segmentGone reports whether the set no longer holds seg.
func segmentGone(set *segment.Set, seg core.SegmentID) bool {
	_, err := set.State(seg)
	return err != nil
}

// holdsDueLocked reports whether a hold waits on a retry the worker's tick
// makes: an owed claim, or a claimed record not yet re-appended or whose new
// place has not yet completed. The caller holds mu.
func (st *stream) holdsDueLocked() bool {
	for _, h := range st.holds {
		if h.owed || len(h.records) != 0 {
			return true
		}
	}
	return false
}

// placingLocked reports whether a held record waits on its new place to
// complete. The caller holds mu.
func (st *stream) placingLocked() bool {
	for _, h := range st.holds {
		if len(h.placed) != 0 {
			return true
		}
	}
	return false
}

// reappendStep is the worker's step over the held segments, on each pass and
// at each tick of the sync bound. For each it retries an owed claim; when the
// set no longer holds the segment, it loses every held record; otherwise it
// removes each record the index no longer names, re-appends each the claim
// covers, removes each whose new place has completed, and ends the hold once
// it holds nothing. In a read-only journal it does nothing: nothing is
// claimed, re-appended or released there.
func (d *Durability) reappendStep(st *stream) {
	st.mu.Lock()
	segs := make([]core.SegmentID, 0, len(st.holds))
	for seg := range st.holds {
		segs = append(segs, seg)
	}
	st.mu.Unlock()
	if len(segs) == 0 || d.isReadOnly() {
		return
	}
	slices.Sort(segs)
	set := st.s.Set()
	for _, seg := range segs {
		d.retryHold(st, set, seg)
	}
}

// retryHold is reappendStep for one held segment.
//
// decision: when the set no longer holds a segment whose claim was owed, the
// verified copy went with it: a repack never carried the record, since the
// index named the corrupt record and not the copy, so each held record goes
// to the drop a read with no copy takes, a corrupt loss with its extents
// dropped and its loss record written by the worker. A record a later write
// overwrote is dropped there with nothing lost. Overturned if a repack ever
// carries a held copy, which would then be registered at its new place.
func (d *Durability) retryHold(st *stream, set *segment.Set, seg core.SegmentID) {
	st.mu.Lock()
	h := st.holds[seg]
	if h == nil {
		st.mu.Unlock()
		return
	}
	gone := false
	if h.owed {
		switch {
		case set.Claim(seg):
			h.claimed, h.owed = true, false
		case segmentGone(set, seg):
			gone = true
			// Every record goes to the drop below, which takes its extents
			// out of the index.
			for seq := range h.pinned {
				st.pinLocked(h, seq, false)
			}
			delete(st.holds, seg)
		}
	}
	recs := make([]ownedCopy, 0, len(h.records))
	for _, c := range h.records {
		recs = append(recs, c)
	}
	claimed := h.claimed
	st.mu.Unlock()
	slices.SortFunc(recs, func(a, b ownedCopy) int { return cmp.Compare(a.Seq, b.Seq) })

	if gone {
		for _, c := range recs {
			unlock := d.domains.Lock(c.File)
			d.dropCorrupt(st, Corruption{
				Share: c.Share, File: c.File, Record: c.Record, Seq: c.Seq,
				Version: c.Version, Extent: c.Extent, AtOpen: c.AtOpen, Synced: c.Synced,
			}, unlock)
		}
		return
	}
	for _, c := range recs {
		d.moveHeld(st, seg, h, c, claimed)
	}
	st.mu.Lock()
	announce := st.releaseLocked(set, seg, h)
	st.mu.Unlock()
	if announce && d.resolved != nil {
		d.resolved(st.no, seg)
	}
}

// moveHeld handles one held record. Under its file's append domain, a record
// not yet re-appended that the index names neither at the corrupt place nor at
// the copy was overwritten, and leaves the hold with no re-append; one the
// claim covers is repointed to the copy if the index still names the corrupt
// record. The domain is then let go, and the record is re-appended with no
// lock held, and stays held with the place it was appended at until that
// place completes. A re-append that fails, or one not made because the
// journal turned read-only meanwhile, leaves the record held for the next
// pass. A record already re-appended
// leaves the hold once its new place has completed (followed through any
// later re-append of a window of its own), and is re-appended again if a read
// has repointed the index back to the copy since.
//
// The hold lasts until the new place completes, not until its append, because
// a held record was acknowledged at the place found corrupt: the copy is the
// only verified place of it until another syncs, and a released copy's segment
// could be repacked away before then, losing the record to a crash.
func (d *Durability) moveHeld(st *stream, seg core.SegmentID, h *segHold, c ownedCopy, claimed bool) {
	unlock := d.domains.Lock(c.File)
	at := index.RecordAt{Segment: c.Copy.Segment, Offset: uint32(c.Copy.Off)}
	atRecord := d.index.Names(c.File, c.Record)
	if !atRecord {
		d.unpin(st, seg, c.Seq)
	}
	atCopy := d.index.Names(c.File, at)
	if placed, ok := d.placedOf(st, seg, h, c); ok {
		if !atCopy {
			// Decided under the domain, so a read that repoints the index back
			// to the copy is wholly before this, and seen above, or wholly
			// after the record left the hold, and holds it again.
			d.settlePlaced(st, seg, h, c, placed)
			unlock()
			return
		}
		// A read found the new place corrupt and repointed the index back to
		// the copy, which it verified: the record is re-appended again.
		d.unplace(st, seg, h, c)
	}
	switch {
	case !atRecord && !atCopy:
		// Overwritten: nothing of the record is served from the copy.
		unlock()
		d.letGo(st, seg, h, c, false)
		return
	case !claimed:
		unlock()
		return
	case atRecord:
		_, hd, err := d.index.RepointRecord(c.File, c.Record, at)
		d.accountDelta(hd)
		if err != nil {
			unlock()
			d.log.Error("journal could not repoint a held record to its copy", "file", c.File, "seq", c.Seq, "err", err)
			return
		}
		d.unpin(st, seg, c.Seq)
	}
	unlock()
	if d.isReadOnly() {
		return
	}
	to, err := d.reappend(st, c)
	if err != nil {
		d.log.Error("journal could not re-append a held record", "file", c.File, "seq", c.Seq, "err", err)
		return
	}
	st.mu.Lock()
	recorded := false
	if cur, ok := h.records[c.Seq]; ok && cur == c && st.holds[seg] == h {
		if h.placed == nil {
			h.placed = map[core.Seq]segment.Location{}
		}
		h.placed[c.Seq] = to
		recorded = true
	}
	st.mu.Unlock()
	if !recorded {
		return
	}
	// The stream tracks a record inside its append, so a new place is pending
	// here unless it is already complete, which only a re-append that tracked
	// nothing leaves. A read that repointed the index back to the copy since
	// leaves the record for the next pass, which re-appends it again.
	unlock = d.domains.Lock(c.File)
	if !d.index.Names(c.File, at) {
		d.settlePlaced(st, seg, h, c, to)
	}
	unlock()
}

// placedOf returns where the held record c was re-appended, if it was. It
// takes mu.
func (d *Durability) placedOf(st *stream, seg core.SegmentID, h *segHold, c ownedCopy) (segment.Location, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.holds[seg] != h {
		return segment.Location{}, false
	}
	to, ok := h.placed[c.Seq]
	return to, ok
}

// unplace forgets where the held record c was re-appended, so the next step
// re-appends it again. It takes mu.
func (d *Durability) unplace(st *stream, seg core.SegmentID, h *segHold, c ownedCopy) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.holds[seg] == h {
		delete(h.placed, c.Seq)
	}
}

// settlePlaced decides the held record c, re-appended at placed, whose copy
// the index no longer names: it leaves the hold once the record's last place,
// placed or a place a window's re-append moved it to, has completed, and stays
// while that place is pending. The caller holds c.File's append domain.
//
// decision: a record whose last place was lost also leaves the hold. That
// place is lost only when its window's re-read did not verify, the read-only
// pass failed it, or a read found it corrupt with no copy verifying, and each
// of those has already dropped the record's extents from the index and
// reported it lost, its loss record excluding every copy of its sequence
// number at recovery, so the copy keeps nothing a release could cost. The ceiling is that the drop itself is the
// loss of an acknowledged record whose held copy may still verify: the
// failure path fails the new place without asking whether a hold backs it.
// A read-only journal never reaches here, as its step does nothing, so there
// the copy's segment stays claimed for a record already dropped until the
// journal closes. Overturned by a ruling that such a record must survive,
// which needs the failure path to repoint the index back to the held copy
// instead of dropping and reporting it.
func (d *Durability) settlePlaced(st *stream, seg core.SegmentID, h *segHold, c ownedCopy, placed segment.Location) {
	st.mu.Lock()
	last := st.followLocked(placed)
	_, lost := st.lost[last]
	pending := !lost && st.pending(last)
	st.mu.Unlock()
	if pending {
		return
	}
	d.letGo(st, seg, h, c, true)
}

// letGo removes c from h, the hold on seg, unless the hold has ended since;
// landed says c left the segment by a re-append.
func (d *Durability) letGo(st *stream, seg core.SegmentID, h *segHold, c ownedCopy, landed bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.holds[seg] != h {
		return
	}
	if cur, ok := h.records[c.Seq]; ok && cur == c {
		st.dropHeldLocked(h, c.Seq)
	}
	h.landed = h.landed || landed
}
