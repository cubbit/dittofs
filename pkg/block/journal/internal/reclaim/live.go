// Package reclaim tracks which journal segments still hold something, so
// reclamation asks Held and never reads a segment to learn it: beside the
// index's per-segment byte counters, the tracker counts the live header-only
// records a segment carries and the flags that hold a segment with no
// content, and answers from those counters alone.
package reclaim

import (
	"maps"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// Rec is one record as reclamation sees it: what the record is, the file it
// names, the segment carrying it, the extent it covers, the (version,
// sequence) pair the drop test compares records by, the record's own
// sequence number, and — for a loss record — the sequence number of the
// record it names, zero for every other kind, whether the record the loss
// dropped was synced when it was dropped, and whether it was dropped because
// metadata holds newer content under it (superseded): the flags that decide
// which rule the drop test applies to a loss — a synced loss leaves a
// removal marker, a superseded one does not, and an unsynced loss reaches
// only the record it names.
//
// A Rec names one record wherever its copies sit: the file, the extent and
// the pair identify it, so two Rec values of one kind with the same three
// are one record in two places. The pair alone would too — the journal
// issues one pair once — but the file and the extent make the re-test
// registries answer by key without reading the record back.
type Rec struct {
	Kind       format.Kind
	File       core.FileID
	Seg        core.SegmentID
	Ext        core.Extent
	Pair       index.Numbers
	Seq        core.Seq
	Named      core.Seq
	Synced     bool
	Superseded bool
}

// Tracker is one journal's segment liveness: per segment, beside the bytes
// the index counts, the records registered live and two flags — a deferred
// loss and an ended hold — that keep a segment held with no content. It is
// safe for concurrent use: Held reads atomics alone and takes no lock, and
// each update locks only the one segment it names.
type Tracker struct {
	ix        *index.Index
	candidate func(core.SegmentID)

	// rm guards the re-test registry below. It is taken by the events, by
	// Droppable and by the registrations and releases that touch the
	// registry, never by a Held.
	rm sync.Mutex
	// recs is one entry per file that holds registered records, every record
	// registered live under the file, keyed by the whole Rec and counted: a
	// record's entry leaves only when its last registration is released, so
	// the re-test walks exactly the records of the file nothing has released
	// yet. It is the registry the unsynced-loss rule reads too — a loss
	// reaches only the record it names, whose registrations sit here under
	// the sequence number the loss carries.
	recs map[core.FileID]map[Rec]int

	// mu serialises creating states: a leaf lock, held only while the state
	// table is copied, never across the index or a candidate call.
	mu sync.Mutex
	// m is the table of the states, one per segment a record was registered
	// in. A segment's retirement removes its entry, so the table tracks the
	// live segments, but the removal is not this table's own: it is the
	// retired-segment step that takes the entry out.
	//
	// ponytail: a copy-on-write map: the first record of a segment copies
	// the whole table, O(S) for the S live segments, once per segment
	// created — a table copy per new segment, fine while a journal holds a
	// few thousand segments at once; in exchange every later register and
	// every Held reads the table with no lock and no allocation. A journal
	// with unbounded live segments pays the copy on each creation, so
	// upgrade to a sharded map or a persistent structure once segment
	// creation shows in a profile.
	m atomic.Pointer[map[core.SegmentID]*segState]
}

// New returns a tracker that answers Held from ix's per-segment bytes,
// firing candidate with the id of each segment whose last thing left through
// the tracker's own calls. A nil candidate fires nothing; a nil index is a
// caller bug and panics.
func New(ix *index.Index, candidate func(core.SegmentID)) *Tracker {
	if ix == nil {
		panic("reclaim: a tracker needs its index")
	}
	return &Tracker{ix: ix, candidate: candidate}
}

// Held reports whether seg still holds anything: bytes of held or retained
// extents, a live header-only record, a deferred loss, or an ended hold. It
// reads the index's counters and the tracker's atomics, taking no lock,
// allocating nothing and never reading the segment.
func (t *Tracker) Held(seg core.SegmentID) bool {
	return t.isHeld(seg, t.stateOf(seg))
}

// Register counts rec's record live in its segment, whatever its kind: what
// keeps it is decided where its liveness can change, not here. The record's
// file is kept under the segment whatever the record goes on to hold, so a
// segment that leaves the set can still name the files its records touched.
// Register never makes a segment less held, so it never fires the candidate.
//
// decision: the record is counted live unconditionally, even where the drop
// test would already answer droppable. The drops belong to the re-test
// events, the one place liveness changes: a record registered already
// droppable — a marker settled before its registration reaches the tracker,
// a caller registering straight from an index a settle already covered —
// drops at the next event of its file or at its segment's retirement,
// exactly like one that turned droppable later. Unregister stays paired
// one-to-one with whatever registered, without asking the drop test, so a
// release never races a re-test over one count.
//
// Every registration also enters the file's re-test registry, keyed by the
// whole record with a count of its live registrations — the one table the
// events and Droppable consult. The segment's count is raised before the
// registry entry is inserted: a release or re-test racing the register in
// that window takes only what the registry already holds, so it cannot
// spend this registration's count, and a Held read in it errs on the held
// side — the safe direction, a segment kept a moment past its last record.
func (t *Tracker) Register(rec Rec) {
	st := t.entry(rec.Seg)
	st.mu.Lock()
	st.files[rec.File] = struct{}{}
	st.live.Add(1)
	st.mu.Unlock()
	t.rm.Lock()
	if t.recs == nil {
		t.recs = make(map[core.FileID]map[Rec]int)
	}
	if t.recs[rec.File] == nil {
		t.recs[rec.File] = make(map[Rec]int)
	}
	t.recs[rec.File][rec]++
	t.rm.Unlock()
}

// Unregister stops counting rec's record live in its segment — one
// registration taken from the file's registry, and the count lowered only
// for one the registry held: a record already dropped by a re-test or by
// its segment's retirement releases nothing a second time, so a caller
// whose paired release arrives after a re-test cannot take another
// record's count. A release of what nothing counted changes nothing, and
// one of a record carried in two places releases that one registration
// only. When the release leaves the segment holding nothing, the candidate
// fires once with the segment's id, after every lock of the tracker's is
// released.
//
// The tracker's own releases are the only thing that fires the candidate;
// nothing here watches the index. A segment that goes not held by its bytes
// leaving alone — a release, a loss, a retirement of its ranges — is covered
// where those counters move: the index delta path re-asks the segments its
// update named, and a candidacy scan over the sealed segments catches what
// no event did. A consumer of the candidate treats it as a prompt, never as
// a fact: it claims the segment and re-checks Held under the claim before
// retiring anything.
func (t *Tracker) Unregister(rec Rec) {
	t.rm.Lock()
	held := t.take(rec)
	t.rm.Unlock()
	if !held {
		return
	}
	t.releaseCount(rec)
}

// take removes one registration of rec from its file's registry, reporting
// whether one was there: the registry is the authority a release answers
// by, so only a record the registry holds may lower a segment's count. The
// caller holds rm.
func (t *Tracker) take(rec Rec) bool {
	rs, ok := t.recs[rec.File]
	if !ok {
		return false
	}
	n, ok := rs[rec]
	if !ok || n <= 0 {
		return false
	}
	if n > 1 {
		rs[rec] = n - 1
		return true
	}
	delete(rs, rec)
	if len(rs) == 0 {
		delete(t.recs, rec.File)
	}
	return true
}

// releaseCount lowers the segment's live count for one registration the
// registry has given up — the caller has taken it — with the transition
// logic every release shares: the candidate fires exactly once for the
// held-to-not-held crossing, whichever concurrent release is the one that
// crosses it. The count never goes below zero: a release with no count left
// is a pairing the registries did not make, and changing nothing is safer
// than taking a count that is not this record's.
func (t *Tracker) releaseCount(rec Rec) {
	st := t.stateOf(rec.Seg)
	if st == nil {
		return
	}
	st.mu.Lock()
	if st.live.Load() <= 0 {
		st.mu.Unlock()
		return
	}
	// The two reads together span this release, under the state's lock, so
	// two releases of one segment's last records still answer one
	// candidacy: whichever release crosses the line is the one that sees
	// the pair straddle it.
	wasHeld := t.isHeld(rec.Seg, st)
	st.live.Add(-1)
	held := t.isHeld(rec.Seg, st)
	st.mu.Unlock()
	if wasHeld && !held && t.candidate != nil {
		t.candidate(rec.Seg)
	}
}

// isHeld reports whether seg holds anything once its state is known: the
// index's bytes beside the state's records and flags. A segment with no
// state holds only what the index counts.
//
// decision: the answer is exact only while nothing adds extents or records
// to a segment that is already counting down. The bytes and the record
// count are read without a guard between them, so a write adding extents
// concurrent with the release of the last record can be missed by both
// reads and the segment answers not held while it holds something. The rule
// holds because a segment is a candidacy only once sealed or ended, and no
// path appends to a sealed or ended segment: its records and extents only
// leave. Overturned by a path that adds extents to a segment retirement is
// already judging — a copy landing there, a re-append outside the file
// append domain — which needs a guarded or versioned read in its place. A
// consumer of the candidate must also re-check Held under the claim its own
// retirement takes, never act on the callback alone.
func (t *Tracker) isHeld(seg core.SegmentID, st *segState) bool {
	b := t.ix.SegmentBytes(seg)
	if b.Held > 0 || b.Retained > 0 {
		return true
	}
	return st != nil && (st.live.Load() > 0 || st.deferred.Load() || st.ended.Load())
}

// stateOf returns the segment's state, or nil when none was made yet.
func (t *Tracker) stateOf(seg core.SegmentID) *segState {
	if m := t.m.Load(); m != nil {
		return (*m)[seg]
	}
	return nil
}

// entry returns the segment's state, making one when the segment has none.
// Only the making takes the table lock; an existing state is found without
// it.
func (t *Tracker) entry(seg core.SegmentID) *segState {
	if st := t.stateOf(seg); st != nil {
		return st
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.stateOf(seg); st != nil {
		return st
	}
	st := &segState{files: make(map[core.FileID]struct{})}
	copied := map[core.SegmentID]*segState{seg: st}
	if m := t.m.Load(); m != nil {
		maps.Copy(copied, *m)
	}
	t.m.Store(&copied)
	return st
}

// segState is one segment's liveness beside the index's byte counters: the
// count of its live header-only records, the flags that hold a segment
// with no content, and the files its records named.
type segState struct {
	// mu serialises the record count, the files table and the release
	// decision. Held reads none of it, only the atomics.
	mu sync.Mutex
	// live counts the records registered and not yet released.
	live atomic.Int64
	// deferred says the segment holds a record whose loss an open still
	// defers. Nothing sets it while no deferred-loss registry exists, so
	// it reads false; Held consults it for the day one does.
	deferred atomic.Bool
	// ended says the segment was ended by a failure whose re-appended
	// copies nothing on disk proves yet. Nothing sets it while ended holds
	// are untracked, so it reads false; Held consults it for the day they
	// are.
	ended atomic.Bool
	// files are the files the segment's registered records named. Entries
	// are never removed until the segment's retirement empties the table:
	// a retired segment's file names answer from them, which a
	// SegmentRetired whose caller carried no list asks for.
	files map[core.FileID]struct{}
}
