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
// record it names, zero for every other kind.
type Rec struct {
	Kind  format.Kind
	File  core.FileID
	Seg   core.SegmentID
	Ext   core.Extent
	Pair  index.Numbers
	Seq   core.Seq
	Named core.Seq
}

// Tracker is one journal's segment liveness: per segment, beside the bytes
// the index counts, the records registered live and two flags — a deferred
// loss and an ended hold — that keep a segment held with no content. It is
// safe for concurrent use: Held reads atomics alone and takes no lock, and
// each update locks only the one segment it names.
type Tracker struct {
	ix        *index.Index
	candidate func(core.SegmentID)

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
func (t *Tracker) Register(rec Rec) {
	st := t.entry(rec.Seg)
	st.mu.Lock()
	st.files[rec.File] = struct{}{}
	st.live.Add(1)
	st.mu.Unlock()
}

// Unregister stops counting rec's record live in its segment — by count, so
// each registered record must be released once; a release of what nothing
// counted changes nothing. When the release leaves the segment holding
// nothing, the candidate fires once with the segment's id, after every lock
// of the tracker's is released.
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
	// are never removed: a record going not live leaves the file's records
	// of this segment nameable, which a later re-test asks for.
	files map[core.FileID]struct{}
}
