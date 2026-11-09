package durability

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// segHold is the hold on one failed segment while copies in it are in use:
// the records served from a copy in the segment that no re-append has moved
// out yet, keyed by sequence number, and the segment's claim. A segment with
// a hold is unresolved: no window of it is announced resolved while the hold
// lives, and the hold's end announces it.
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
// hold (a claim let go, a write over a held record, a landed re-append) when a
// journal with many held records shows the visits in a profile.
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
	// landed says a re-append of a held record landed.
	landed bool
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
			delete(h.records, c.Seq)
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
// announced resolved: a re-append landed or a resolution was withheld, and no
// window of the segment is pending, whose own resolution then announces it.
// The caller holds mu.
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
// makes: an owed claim, or, once the re-append is installed, a claimed
// record not yet re-appended. The caller holds mu.
func (st *stream) holdsDueLocked(reappend bool) bool {
	for _, h := range st.holds {
		if h.owed || (reappend && len(h.records) != 0) {
			return true
		}
	}
	return false
}

// reappendStep is the worker's step over the held segments, on each pass and
// at each tick of the sync bound. For each it retries an owed claim; when the
// set no longer holds the segment, it loses every held record; otherwise it
// removes each record the index no longer names, re-appends each the claim
// covers, and ends the hold once it holds nothing. In a read-only journal it
// does nothing: nothing is claimed or re-appended there.
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

// moveHeld handles one held record under its file's append domain: a record
// the index names neither at the corrupt place nor at the copy was
// overwritten, and leaves the hold with no re-append; one the claim covers is
// repointed to the copy if the index still names the corrupt record,
// re-appended, and leaves the hold once the re-append lands. A re-append that
// fails leaves the record held for the next pass.
func (d *Durability) moveHeld(st *stream, seg core.SegmentID, h *segHold, c ownedCopy, claimed bool) {
	unlock := d.domains.Lock(c.File)
	defer unlock()
	at := index.RecordAt{Segment: c.Copy.Segment, Offset: uint32(c.Copy.Off)}
	atRecord := d.index.Names(c.File, c.Record)
	landed := false
	switch {
	case !atRecord && !d.index.Names(c.File, at):
		// Overwritten: nothing of the record is served from the copy.
	case !claimed || d.reappend == nil:
		return
	default:
		if atRecord {
			_, hd, err := d.index.RepointRecord(c.File, c.Record, at)
			d.accountDelta(hd)
			if err != nil {
				d.log.Error("journal could not repoint a held record to its copy", "file", c.File, "seq", c.Seq, "err", err)
				return
			}
		}
		if err := d.reappend(st, c); err != nil {
			d.log.Error("journal could not re-append a held record", "file", c.File, "seq", c.Seq, "err", err)
			return
		}
		landed = true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.holds[seg] != h {
		return
	}
	if cur, ok := h.records[c.Seq]; ok && cur == c {
		delete(h.records, c.Seq)
	}
	h.landed = h.landed || landed
}
