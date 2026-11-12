// Package reclaim tracks which journal segments still hold something, so
// reclamation asks Held and never reads a segment to learn it: beside the
// index's per-segment byte counters, the tracker counts the live header-only
// records a segment carries, the deferred losses and ended holds that keep
// a segment with no content, and answers from those counters alone.
package reclaim

import (
	"maps"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
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
//
// Tag is the share the record's header names: part of the key of every
// registry keyed by Rec — the registrations, the forget snapshots and the
// loss marks alike — so a registration and its release must carry the same
// one, or the registry entry the pair disagrees on sits undropped. A content
// record's tag reads from its file; a forget record names no file, so its
// tag is the one field its caller must spell out, and a forget registered
// without its tag registers and releases like any other record but keys no
// tag's snapshot (RegisterForget names the rule).
type Rec struct {
	Kind       format.Kind
	File       core.FileID
	Seg        core.SegmentID
	Ext        core.Extent
	Pair       index.Numbers
	Seq        core.Seq
	Named      core.Seq
	Tag        core.ShareTag
	Synced     bool
	Superseded bool
	// Loc is where the record's own bytes sit, the placement its append
	// reported. Zero is tolerated — a registration without one keys,
	// releases and drops exactly like any other — and a carry reaching a
	// record with no place fails its unit, since it cannot read or move
	// the record. Loc is part of the key of every registry this type keys
	// by the whole Rec, Tag's own rule: a registration and its release
	// must spell the same one, or the entry twice-keyed sits undropped.
	Loc segment.Location
}

// Tracker is one journal's segment liveness: per segment, beside the bytes
// the index counts, the records registered live, a deferred-loss flag and a
// count of ended holds — the things that keep a segment held with no
// content. It is safe for concurrent use: Held reads atomics alone and takes
// no lock; registrations, releases and re-tests take the registry lock rm, the
// ended-hold events take hm, and whatever moves a segment's counters also
// locks the one segment it names.
type Tracker struct {
	ix        *index.Index
	candidate func(core.SegmentID)

	// member answers whether id is still a segment of the set, the one
	// check a snapshot's segments are re-checked against: a retirement
	// that ran between the caller's snapshot and the registration lost its
	// SegmentRetired — before the tracker held the snapshot — so the
	// re-check catches it instead. It is set once, beside the tracker's
	// construction, before events run; nil treats every snapshot segment
	// as present and leaves the snapshot the caller's word alone.
	member func(core.SegmentID) bool

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
	// forgets is each live forget record's snapshot, keyed by the whole Rec
	// like recs: the segments that held records of the record's share tag
	// when the forget was appended — the only ones that can hold a record
	// of the tag below it. Guarded by rm, so the drop test asks for it
	// with the lock the re-test already holds; an entry leaves when the
	// record's registration does, and a retired segment leaves every
	// snapshot that names it.
	forgets map[Rec]map[core.SegmentID]struct{}
	// marks is each live unsynced loss record's presence marks, keyed by
	// the whole Rec of the loss itself: the segments still holding copies
	// of the record it names, the only thing that keeps the loss from
	// answering droppable. A mark lives in the registry alone — it raises
	// no segment count and holds nothing — so a holder segment is held or
	// not by its own bytes and header-only records, never by the mark.
	// Two loss records naming one sequence number with different extents
	// keep their own mark sets, whatever holder they share, and each ends
	// with its own record's release. Guarded by rm, like forgets; an entry
	// leaves when the loss's registration does, and a retired holder
	// segment leaves every mark set that names it.
	marks map[Rec]map[core.SegmentID]struct{}
	// pendingGone is each retired segment's file list waiting for the
	// unlink that retired it to become durable: SegmentRetired fills it
	// with the files whose drop ranges name the segment, and RemovalDurable
	// — the retirer's durable signal — consumes it, relaying each file's
	// index drop of the segment's ranges. Between the two, no removal the
	// segment's records kept answers droppable, so a removal record never
	// leaves the journal before the unlink it counted on is durable.
	//
	// decision: an entry whose retirement's directory sync failed is never
	// consumed in this process — a failed sync is never re-reported durable
	// by a later one — so every removal the segment kept answers not
	// droppable until the process ends, the safe direction: a drop before
	// the unlink is durable would have a crash bring the retired segment
	// back without the removal record that covered it. The next open's
	// scan settles what the failed sync left on disk: a segment it finds
	// gone rebuilds no ranges of, and one it still finds named is retired
	// and dropped again there.
	pendingGone map[core.SegmentID][]core.FileID

	// mu serialises creating states: a leaf lock, held only while the state
	// table is copied, never across the index or a candidate call.
	mu sync.Mutex
	// m is the table of the states, one per segment a record was registered
	// in. A segment's retirement removes its entry, so the table tracks the
	// live segments, but the removal is not this table's own: it is the
	// retired-segment step that takes the entry out.
	// ponytail: a copy-on-write map: the first record of a segment copies
	// the whole table, O(S) for the S live segments, once per segment
	// created — a table copy per new segment, fine while a journal holds a
	// few thousand segments at once; in exchange every later register and
	// every Held reads the table with no lock and no allocation. A journal
	// with unbounded live segments pays the copy on each creation, so
	// upgrade to a sharded map or a persistent structure once segment
	// creation shows in a profile.
	m atomic.Pointer[map[core.SegmentID]*segState]

	// hm guards the ended holds below. It is taken by HoldEnded,
	// MarksRecorded, SealObserved and retireHolds only — nothing that holds it calls
	// back into the tracker — so it joins no cycle and sits beside the
	// processing of the states, whose locks the releases take only after
	// hm is back down.
	hm sync.Mutex
	// holds is the ended holds registered on each copies' segment, one
	// entry per HoldEnded that names it: a hold ends at the proof its
	// registration named — the seal of its copies' segment, seen through
	// SealObserved, or a recorded mark past the last copy, handed through
	// MarksRecorded — and the segment a hold keeps waits for its own
	// proof, untouched by the releases of another hold's copies.
	holds map[core.SegmentID][]endedHold
}

// endedHold is one hold registered on an ended segment: the segment the
// hold keeps, and the position of its last copy, the point a recorded mark
// must pass to prove the copies.
type endedHold struct {
	seg  core.SegmentID
	last int64
}

// ForgetSet is one forget record of an open's rebuild with its snapshot:
// the segments that held records of the record's tag below it when the
// journal appended it, collected per tag and segment by the rebuild's
// feed — the set the forget's registration re-checks against the set's own
// member answer.
type ForgetSet struct {
	Rec  Rec
	Segs []core.SegmentID
}

// UnsyncedLoss is one unsynced loss record of an open's rebuild with the
// segments still holding copies of the record it names: the reach of an
// unsynced loss is the record it names alone, so its liveness ends when the
// last of these segments retires, and the registration below reads the
// list exactly the forget's snapshot reads its own.
type UnsyncedLoss struct {
	Rec    Rec
	HeldBy []core.SegmentID
}

// New returns a tracker that answers Held from ix's per-segment bytes,
// firing candidate with the id of each segment whose last thing left through
// the tracker's own calls. A nil candidate fires nothing; a nil index is a
// caller bug and panics.
//
// The candidate is a prompt, never a fact, and it must not mutate the
// segment set: the tracker fires it from inside its own release paths, and
// wired to the set as a retire observer the set holds its delivery lock
// across the whole callback — a candidate that itself sealed, retired or
// otherwise changed the set would self-deadlock on that lock. A consumer
// claims the segment its own way and re-checks Held under the claim before
// retiring anything, because a candidacy is only the tracker's word that
// the crossing happened, and the crossing can always race back.
func New(ix *index.Index, candidate func(core.SegmentID)) *Tracker {
	if ix == nil {
		panic("reclaim: a tracker needs its index")
	}
	return &Tracker{ix: ix, candidate: candidate}
}

// SetMember names the tracker's answer for whether a segment is still in
// the set, consulted by the forget registration to re-check a caller's
// snapshot against the registry the retirements keep: the production value
// answers from the segment ledger, a segment being a member exactly while
// it is. It must be set beside the tracker's construction, before events
// run; a tracker left without one treats every snapshot segment as
// present.
func (t *Tracker) SetMember(member func(core.SegmentID) bool) { t.member = member }

// inSet answers the member check, or true when none was set: without one
// nothing may be assumed gone, so a snapshot is taken as it was given.
func (t *Tracker) inSet(seg core.SegmentID) bool {
	if t.member == nil {
		return true
	}
	return t.member(seg)
}

// Held reports whether seg still holds anything: bytes of held or retained
// extents, a live header-only record, a deferred loss, or an ended hold. No
// byte the index counts and no registration the tracker keeps, and the two
// flags a segment carries beside them, are ever released by anything but the
// events below. Held reads the index's counters and the tracker's atomics,
// taking no lock, allocating nothing and never reading the segment.
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
//
// The caller's contract: every append path calls Register exactly once per
// header-only record the path appends, before the segment carrying the
// record can seal — the re-test events answer from the registry alone, so
// a record nothing registered is one no re-test can release — and pairs it
// with Unregister only for records the path unregisters by its own rules,
// letting the registry own every re-test release instead: a caller's
// Unregister of what a re-test already took answers nothing, by design.
// The registration paths still missing: the forget record's append path,
// and the registration of a rebuilt journal's records at open, which must
// have run before the retirement worker starts — a rebuild that registered
// nothing leaves its segments answering not held and hands them straight
// to retirement. And no production path settles and re-tests yet, so a
// registered removal or loss record stays counted for the process's life
// unless its segment retires.
func (t *Tracker) Register(rec Rec) {
	t.count(rec)
	t.rm.Lock()
	t.addLocked(rec)
	t.rm.Unlock()
}

// count raises rec's segment count and files table: the half of a
// registration that precedes its registry entry.
func (t *Tracker) count(rec Rec) {
	st := t.entry(rec.Seg)
	st.mu.Lock()
	st.files[rec.File] = struct{}{}
	st.live.Add(1)
	st.mu.Unlock()
}

// addLocked inserts one registration of rec into the file's registry. The
// caller holds rm.
func (t *Tracker) addLocked(rec Rec) {
	if t.recs == nil {
		t.recs = make(map[core.FileID]map[Rec]int)
	}
	if t.recs[rec.File] == nil {
		t.recs[rec.File] = make(map[Rec]int)
	}
	t.recs[rec.File][rec]++
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
	if held {
		t.forgetTaken(rec)
		t.markTaken(rec)
	}
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
	t.lowerCount(rec.Seg, st, &st.live)
}

// lowerCount takes one unit from the counter n of seg's state — a released
// registration, a released ended hold — with the transition logic every
// release shares: the candidate fires exactly once for the held-to-not-held
// crossing, whichever concurrent release is the one that crosses it. The
// two isHeld reads span the change under the state's lock, so two releases
// of one segment's last registrations still answer one candidacy. The caller
// has found the state.
//
// The clamp at zero is a defensive floor, unreachable while every release is
// take-gated: the registry hands over one registration per count, so a
// release with no count left is a pairing the registries did not make. It
// stays because a negative count would offset the next live records and make
// Held answer false over them, so changing nothing is the safe direction.
func (t *Tracker) lowerCount(seg core.SegmentID, st *segState, n *atomic.Int64) {
	st.mu.Lock()
	if n.Load() <= 0 {
		st.mu.Unlock()
		return
	}
	wasHeld := t.isHeld(seg, st)
	n.Add(-1)
	held := t.isHeld(seg, st)
	st.mu.Unlock()
	if wasHeld && !held && t.candidate != nil {
		t.candidate(seg)
	}
}

// SetDeferred counts on the segment one record loss whose dropped record it
// holds: raised when a deferral starts — the record is owed to the copy
// that retires this segment, so the segment stays held while one stands —
// and lowered when a deferral ends, once the copy that writes the loss
// record has it durable. The registry of deferred losses lives with the
// durability layer, which counts per segment the losses it deferred and
// the segments holding their dropped records: it must call SetDeferred
// once per deferral that starts, and once per deferral that ends, on the
// segment holding the dropped record. No such registry exists yet, so
// nothing moves the count; it and Held's read of it are in place for the
// day one does. The count clamps at zero: an end without a start, or a
// repeated end, lowers nothing. The last end on a segment holding nothing
// else crosses to not held and fires the candidate, exactly as any release
// does.
//
// A segment named here is not re-created for the set's word alone: with a
// member check set, a segment the set no longer holds answers nothing, for
// the same reason a retirement releases before it forgets — without one,
// the caller must not name a segment already retired.
func (t *Tracker) SetDeferred(seg core.SegmentID, on bool) {
	st := t.entryIfLive(seg)
	if st == nil {
		return
	}
	st.mu.Lock()
	wasHeld := t.isHeld(seg, st)
	if on {
		st.deferred.Add(1)
	} else if st.deferred.Load() > 0 {
		st.deferred.Add(-1)
	}
	held := t.isHeld(seg, st)
	st.mu.Unlock()
	if wasHeld && !held && t.candidate != nil {
		t.candidate(seg)
	}
}

// entryIfLive returns the segment's state, making one only when the
// segment can still hold one: with a member check set, a segment the set
// no longer names has no state to make, so a call late on a retired
// segment resurrects nothing and lowers no count. A tracker without a
// membercheck has only the caller's word, and serves it.
func (t *Tracker) entryIfLive(seg core.SegmentID) *segState {
	if !t.inSet(seg) {
		return nil
	}
	return t.entry(seg)
}

// RegisterForget counts rec's record live in its segment beside what every
// registration keeps: a forget record ends what its share tag held below it,
// so it stays live while any of segs — the segments holding records of the
// tag when the forget was appended, the snapshot its caller takes once the
// forget's barrier has placed every record in flight — remains in the set.
// A segment of the snapshot already retired before this call — between the
// caller's snapshot and here — is pruned the same way a later retirement
// prunes it, so one hold of rm closes both races without a second registry:
// the registration and the snapshot enter the tracker's tables under it, so
// no re-test of the zero file can see the record without its snapshot and
// drop it, then each segment is re-checked against the member check and one
// already gone leaves now; a retirement that runs after the hold finds the
// snapshot there and prunes it itself. An empty remaining snapshot names a barrier that
// released everything below the forget: the record is droppable the moment
// it is registered, and the registration itself carries the re-test that
// drops it — the zero file a forget record registers under is one no later
// event of the journal reaches, so waiting for one would pin the record's
// own segment for the segment's life. A segment
// pruned by the re-check or by a retirement is gone from the set for good —
// a retired id is never drawn again — so a snapshot never names one twice.
//
// decision: the snapshot is the whole rule and no per-record scan runs: at
// the forget, only the segments whose record bytes of the tag are non-zero
// can hold a record of the tag below the forget's sequence number, because
// every later record of the tag is appended above it, and the barrier has
// already placed every record the tag had in flight — a carry under the old
// sequence number counts in its new segment, in the snapshot too. The
// snapshot is exact only while nothing appends a record of the tag below
// the forget's sequence number once the barrier has passed; a carry outside
// the file append domain, or a re-append of a failed window naming the tag,
// overturns it. The upgrade is a scan of the snapshot's segments for
// records of the tag at each retirement.
//
// decision: the Rec handed here must name its tag: Tag is part of the key of
// every registry this registration enters, so the registration and the
// Unregister that releases it must carry the same one, or the entry sits.
// The registration is the forget's liveness snapshot and nothing more — what
// keeps the record's segment held until every segment that held the tag's
// records below it leaves the set — since the forgetting mark the copy pass
// consults is the share registry the Forget path writes before its barrier,
// not this one. The Forget that appends the record and the open's rebuild
// that reads it back build the Rec from the record's own header, which names
// the tag; nothing else carries it, so the tag is the one field the caller
// must not invent its own value for. The rule holds while a forget record
// names exactly one tag; one that ever carries a second tag identity the
// journal must distinguish overturns it, for the registry key would then
// have to name both.
func (t *Tracker) RegisterForget(rec Rec, segs []core.SegmentID) {
	if len(segs) == 0 {
		t.Register(rec)
		t.retest(rec.File)
		return
	}
	t.count(rec)
	t.rm.Lock()
	t.addLocked(rec)
	if t.forgets == nil {
		t.forgets = make(map[Rec]map[core.SegmentID]struct{})
	}
	snap := make(map[core.SegmentID]struct{}, len(segs))
	for _, s := range segs {
		snap[s] = struct{}{}
	}
	t.forgets[rec] = snap
	// The re-check runs under the registry's own lock, so a SegmentRetired
	// pruning this snapshot waits it out and finds what is left; one that
	// already ran is answered here by the member check.
	for s := range snap {
		if !t.inSet(s) {
			delete(snap, s)
		}
	}
	if len(snap) == 0 {
		delete(t.forgets, rec)
		t.rm.Unlock()
		t.retest(rec.File)
		return
	}
	t.rm.Unlock()
}

// forgetTaken drops a forget record's snapshot once the registry has given
// up its registration, so the snapshot exists only while the record counts
// live. The caller holds rm.
func (t *Tracker) forgetTaken(rec Rec) {
	if rec.Kind == format.KindForget {
		delete(t.forgets, rec)
	}
}

// RegisterAtOpen is the registration the rebuild at an open hands its
// records to, so the tracker the journal opens with answers as one fed the
// same records as they were appended would: every header-only record
// counts live exactly as Register counts it, whatever its kind — one the
// drop test would already answer droppable included, which the open's own
// OpenDone drops — each forget record registers with the snapshot
// its caller collected (RegisterForget's own rule), and each unsynced
// loss record beside a presence mark of the record it names in every
// segment holding one (registerMark), so the loss's drop query finds the
// named record through the marks until the last holder retires. An
// unknown or unsupported kind registers live: what the tracker cannot
// answer about it keeps, the safe direction.
//
// RegisterAtOpen must be called with SetMember wired to the segment
// ledger. A forget's snapshot was collected while the open scanned, and
// its registration re-checks each of its segments against the member
// answer; without one — nil treats every snapshot segment as present — a
// segment the open retired between the collection and this call stays in
// the snapshot, keeping the forget record live past the last segment that
// held the tag's records.
//
// The lists carry no order between them and concurrent callers register
// concurrently — a rebuild whose segment scans run in parallel feeds
// shards of its records — so the result depends on neither: plain
// registrations commute by their own locks, a forget's snapshot and a
// loss's marks are keyed by the whole record, and a loss whose named
// copies the same feed also registers stays undroppable until their
// holder leaves either way. Registering a record twice counts it twice,
// as Register itself does: an open hands each attached record exactly
// once, whatever its copies. The open ends its registration with one
// OpenDone call, after every RegisterAtOpen and after SetMember.
//
// The plain records carry no location. The tracker keys a record by the
// fields the drop test reads, so a registration without its place works —
// but a carry reaching one fails its unit, since it can no longer read the
// record: the open's feed must hand each record its placement once the pass
// that runs the carry lands.
func (t *Tracker) RegisterAtOpen(recs []Rec, forgets []ForgetSet, unsynced []UnsyncedLoss) {
	for _, r := range recs {
		t.Register(r)
	}
	for _, f := range forgets {
		t.RegisterForget(f.Rec, f.Segs)
	}
	for _, u := range unsynced {
		t.Register(u.Rec)
		for _, seg := range u.HeldBy {
			t.registerMark(u.Rec, seg)
		}
	}
}

// OpenDone re-tests every file with registered records, once each: it is
// the close of the open's registration window, called once after every
// RegisterAtOpen call — concurrent feeders included — and after SetMember,
// so the re-test reads a registry no feeder is still filling. Every record
// the open left in the registry is reached, a forget record's zero file
// included, because the zero file holds the record's registration.
//
// The re-test is the open's own event, and it exists because a record the
// drop test already answers droppable at registration — a marker settled
// before the open read it, a release over nothing — would otherwise wait
// for the next event of its file, and a file whose records all resolved at
// the open may see no event for the journal's whole life: the track would
// pin the record's segment until the segment retired, and retirement takes
// the registration anyway — but the segment's bytes stay gone and a
// candidate never fires for the crossing. RegisterAtOpen itself runs no
// re-test, deliberately: a feeder's shard is not the whole attached set, so
// a re-test inside it could drop an unsynced loss — which stays live while
// any record of its file is registered — while the loss's own feed, or the
// record it names, was still in another feeder's shard. The one re-test,
// over the whole registry, is safe here.
//
// Calling it more than once is harmless: a re-test over a settled registry
// drops what an earlier one left and nothing else.
func (t *Tracker) OpenDone() {
	t.rm.Lock()
	files := make([]core.FileID, 0, len(t.recs))
	for f := range t.recs {
		files = append(files, f)
	}
	t.rm.Unlock()
	if len(files) == 0 {
		return
	}
	t.retest(files...)
}

// registerMark keeps one presence mark of the unsynced loss record rec: a
// registry-only entry naming a segment still holding a copy of the record
// rec.Named names. The mark is what the loss's drop query reads — no live
// registration of the file carries the named sequence number while one
// stands — and it holds nothing: the holder segment answers Held by its
// own bytes and counts alone, is a candidacy exactly when it would be
// without the mark, and its retirement takes the mark from the registry
// alone, lowering no count, because the mark raised none.
//
// decision: the mark makes the loss live without pinning its holder,
// because the holder's retirement — the one path that ends a segment's
// holding of a record — is what ends the loss, and a mark counted as a
// live record would hold the holder itself, so the retirement the loss
// waits for would wait on a segment the tracker pins: a wait with no end.
// The rule holds while a holder segment leaves the set only through
// SegmentRetired, the single retirement path, whose take below reaches the
// marks; a path that emptied or unlinked a holder without it would leave
// the loss undroppable for ever — a record kept too long, the safer
// direction — until that path reached the marks too.
func (t *Tracker) registerMark(rec Rec, seg core.SegmentID) {
	t.rm.Lock()
	defer t.rm.Unlock()
	if t.marks == nil {
		t.marks = make(map[Rec]map[core.SegmentID]struct{})
	}
	if t.marks[rec] == nil {
		t.marks[rec] = make(map[core.SegmentID]struct{})
	}
	t.marks[rec][seg] = struct{}{}
}

// HoldEnded registers one hold on an ended segment: the copies of its
// still-held records were re-appended into copies, and the ended segment
// stays held until something proves them — a recorded mark of the copies'
// segment past last, handed through MarksRecorded, that segment's seal, seen
// through SealObserved, or its retirement, whose release of the holds
// citing it SegmentRetired carries. The hold is not the window
// resolution's, the caller's releases or the copies' sync to release, and
// it ends without touching the segment's records. The registration belongs
// before the copies sync — the path's own ordering, the condition that
// overturns this rule — so the seal it waits for is a seal to come: a
// registration late on a copies segment already sealed missed its seal
// event and is released only by a mark, or by the copies' retirement. A
// segment named here is not re-created for the set's word alone, as
// SetDeferred's ruling names.
//
// decision: the hold ends only at the proof named here, and a mark reaching
// exactly the last copy proves the copies but not that they are recorded —
// through must pass last. An ended segment whose records no path
// re-appended holds nothing on this account, which is safe only while no
// path re-appends the records of an ended segment without registering the
// hold: the re-append path must call HoldEnded before its copies sync, and
// it owns the seal bound — the copies' segment is sealed within thirty
// seconds of the copies syncing, so the hold cannot outlast its proof. That
// path's arrival overturns this reasoning. SegmentRetired's release of the
// holds citing a copies segment is safe only while a retired segment's
// still-held records were carried durably first — a segment is retired
// once nothing holds it, and a hold ends with the seg its copies proved;
// overturned if a retirement can ever run without that durable carry.
func (t *Tracker) HoldEnded(seg, copies core.SegmentID, last int64) {
	st := t.entryIfLive(seg)
	if st == nil {
		return
	}
	// The count rises before the registry entry is inserted, so a
	// concurrent release in that window finds the registration absent and
	// lowers nothing, and a Held read in it errs on the held side — the
	// safe direction, a segment kept past its proof's arrival.
	st.holds.Add(1)
	t.hm.Lock()
	defer t.hm.Unlock()
	if t.holds == nil {
		t.holds = make(map[core.SegmentID][]endedHold)
	}
	t.holds[copies] = append(t.holds[copies], endedHold{seg: seg, last: last})
}

// MarksRecorded hands the tracker a recorded mark of the copies' segment,
// covering the segment's recorded marks through the position through: a
// mark past its hold's last copy proves that hold's copies, so each such
// hold is released, and the holds whose last copy sits above through stand.
// A mark short of a hold's last copy, or reaching exactly it, releases
// nothing — the copies' sync alone proves none of them.
func (t *Tracker) MarksRecorded(copies core.SegmentID, through int64) {
	t.hm.Lock()
	hs := t.holds[copies]
	if len(hs) == 0 {
		t.hm.Unlock()
		return
	}
	var rel []endedHold
	n := 0
	for _, h := range hs {
		if through > h.last {
			rel = append(rel, h)
			continue
		}
		hs[n] = h
		n++
	}
	if n == 0 {
		delete(t.holds, copies)
	} else {
		t.holds[copies] = hs[:n]
	}
	t.hm.Unlock()
	for _, h := range rel {
		t.releaseHold(h)
	}
}

// SealObserved reports that a segment of the set was sealed, and releases
// every hold naming it as its copies: a seal closes the segment to further
// appends and proves its records, so the ended segments that copied their
// records into it hold nothing on this account any more. It is the call
// the wiring hands the set's seal observer — set.OnSeal(tracker.SealObserved)
// — and it runs wherever its caller does: the observer's delivery holds
// its lock across the whole callback, so the candidate it fires must claim
// without mutating the set, which a candidate is asked to do anyway: it is
// a prompt, and the retirement that answers it is the caller's own.
// A seal of a segment no hold names releases nothing.
func (t *Tracker) SealObserved(seg core.SegmentID) {
	t.hm.Lock()
	hs := t.holds[seg]
	delete(t.holds, seg)
	t.hm.Unlock()
	for _, h := range hs {
		t.releaseHold(h)
	}
}

// releaseHold takes one ended hold from the segment it kept — the caller
// has taken the registration out of the holds table — through the same
// count-lowering transition every release shares.
func (t *Tracker) releaseHold(h endedHold) {
	st := t.stateOf(h.seg)
	if st == nil {
		return
	}
	t.lowerCount(h.seg, st, &st.holds)
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
	return st != nil && (st.live.Load() > 0 || st.deferred.Load() > 0 || st.holds.Load() > 0)
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
	// deferred counts the record losses whose record a segment holds and
	// the durability layer still defers — one per loss that started, taken
	// back per loss that ended. SetDeferred raises and lowers it, and the
	// registry of deferred losses reaches the tracker through that call
	// alone. No such registry exists yet, so nothing moves it and it reads
	// zero; Held consults it for the day one does.
	deferred atomic.Int64
	// Holds is the count of ended holds registered on the segment — one per
	// HoldEnded — each released at the proof its registration named, a mark
	// past the last copy or the copies' segment's seal. Nothing sets it
	// while no path re-appends an ended segment's records, so it reads
	// zero; Held consults it for the day one does.
	holds atomic.Int64
	// files are the files the segment's registered records named. Entries
	// are never removed; the state leaves the tracker with the segment, and
	// SegmentRetired reads them when its caller carried no list.
	files map[core.FileID]struct{}
}
