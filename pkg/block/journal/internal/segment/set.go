// Package segment holds the ledger of the segments the journal owns: the
// set of segments a stream has taken (active, sealed or ended), the spares
// made for the pool beside it, and every byte they occupy. Each set segment
// is accounted at its cap from the moment it is taken until its unlink,
// unless shortened at seal or by a torn-tail cut; each spare is accounted at
// the cap. The ledger is pure memory: no interface separates it from its
// callers, it performs no I/O, and the retirement executor, the seal and the
// spare pool hand it its facts.
//
// A segment's bytes leave the accounted footprint exactly once: out of it by
// an unlink (Retire), or into the spare pool by a recycle (Recycle, which
// moves it, not lowers it), in the step that removes the segment from the
// set, and only in the call that found it there. A spare's leave by
// DropSpare. A later call of any of these kinds is refused, counted as a
// double retire and logged at Warn.
//
// The package also makes spares (Maker, make.go): a whole segment at the
// cap, zeroed end to end and durable under its final name, its reservation
// taken against the journal's maximum before any storage is touched and —
// on every failure — unlinked before it is given back, or held in the set,
// End-ed at its cap, for a retirement to unlink. Ids are drawn above every
// name the directory holds, and a failed drawing is never reused.
//
// The package also keeps the pool (Pool, pool.go): the spares separated into
// three classes — content, headroom and repack reserve — handed out by a
// take that performs no I/O and kept ready by a refill worker that makes
// them off the write path, one goroutine the pool's Stop joins.
package segment

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// State is where a segment of the set stands. Spares are not in the set:
// they are held beside it, because nothing appends to them and their bytes
// come back to the pool through the pool's own paths.
type State uint8

const (
	// Active is a segment a stream appends to.
	Active State = iota + 1
	// Sealed is a segment closed by a seal; never appended to again.
	Sealed
	// Ended is a segment ended by a failed append or sync, or by recovery;
	// never sealed.
	Ended
)

// The ledger's errors. Every operation validates its whole argument and the
// set's state before changing anything, so a refused call changes no counter
// and no map — with one exception the ledger's own rule writes down: a
// Retire or Recycle that finds its id out of the set counts a double retire
// and logs at Warn, which is the counter and the log the refusal exists to
// raise.
var (
	// ErrNotInSet is the refusal of an operation naming a segment the set
	// does not hold (or a Take of an id that is no spare): the double retire
	// this package counts, a release arrived too late, a claim on a segment
	// already retired.
	ErrNotInSet = errors.New("segment: not in the set")
	// ErrWrongState is the refusal of an operation naming a segment in a
	// state it does not serve: retiring a segment a stream still appends to,
	// sealing one already sealed, accounting a record into a sealed one.
	ErrWrongState = errors.New("segment: wrong state for the operation")
	// ErrDuplicateID is the refusal of an add naming a segment id the ledger
	// already owns, in the set or among the spares.
	ErrDuplicateID = errors.New("segment: id already owned")
	// ErrBadSize is the refusal of a size that accounts no storage or more
	// than the segment holds: a shrink that grows or empties, a record of
	// no bytes, a recycle of a seal that never said full, whatever its size.
	ErrBadSize = errors.New("segment: bad size")
	// ErrBadID is the refusal of the id that names no segment: the ledger
	// never reserves or moves storage under it, because no segment file can
	// carry it and an id that names nothing cannot be returned by a retire.
	ErrBadID = errors.New("segment: bad id")
	// ErrMismatch is what Check reports once the accounted footprint and the
	// sum the set's sizes make of it differ.
	ErrMismatch = errors.New("segment: accounted footprint does not match the set")
	// ErrClaimed is the refusal of a retirement of a segment another
	// retirement or a repack holds the claim of.
	ErrClaimed = errors.New("segment: claimed by another retirement")
)

// Counters are the ledger's counters, read without a walk: the writing side
// places a call on its own path for every counter it needs, and none may
// walk the set to answer. DoubleRetires counts every retire, recycle or spare
// drop that found its id already gone — two reclamation paths selecting one
// segment or one spare, the race the idempotence rule counts.
type Counters struct {
	// UsedBytes is the accounted footprint: every segment of the set at its
	// accounted size, plus every spare at the segment cap. It is raised
	// before a spare is allocated and lowered only after an unlink has
	// happened, so it never runs below the storage actually in use.
	UsedBytes int64
	// Segments is the number of segments in the set.
	Segments int64
	// Sealed is the number of sealed segments in the set.
	Sealed int64
	// NoAppendOverBytes is the part of NoAppendBytes that is not payload
	// — the record headers, padding and whole payload-free records those
	// segments' entries carry, moved by exactly the edges NoAppendBytes
	// moves at, the charges of ended segments included. The figure the
	// ended-unreclaimed identity subtracts beside its live half, so the
	// answer names live payload only, the same basis UnreclaimedBytes rides
	// and never above it.
	NoAppendOverBytes int64
	// NoAppendBytes is the record bytes the segments that take no further
	// append — the sealed and the ended alike — hold, the total the per-tag
	// noAppendTag map keeps per share, moved at every edge that map moves
	// at: raised by a Seal, an End and an adoption of such a segment, joined
	// by a record completing against an ended one, lowered at every removal.
	// The recorded charges are the basis; the accounted size never enters,
	// so the figure rides the same record-byte basis the shares' settled
	// figures do.
	NoAppendBytes int64
	// Spares is the number of spares the ledger holds.
	// decision: it counts every spare the ledger holds, every class the pool
	// separates them into, because Check sums every one of them at the cap —
	// content, headroom and repack reserve alike. RFC 1 §3.7's Spares gauge,
	// which counts the content class alone, is the pool's own
	// PoolCounters.Spares. Withdrawn if the ledger ever learns the classes
	// itself, which no present caller needs.
	Spares int64
	// DoubleRetires counts retires, recycles and spare drops that found the
	// segment or spare out of the set. A retire that instead finds an Active
	// segment is refused without a count: that is a different bug (see
	// Retire).
	DoubleRetires uint64
}

// liveCounters is Counters as the set keeps it: atomics, moved under mu with
// the state they count and read without it.
type liveCounters struct {
	UsedBytes, Segments, Sealed, NoAppendOverBytes, NoAppendBytes, Spares atomic.Int64
	DoubleRetires                                                         atomic.Uint64
}

// Set is the ledger of the segments the journal owns and of every byte they
// occupy.
//
// decision: the set, the footprint and the spare counts sit under the
// capacity lock domain of RFC 1 §8.2 — store-wide counters, no I/O under the
// mutex, nothing acquired while holding it but the leaf mutex a per-tag
// figure takes to add a tag it has not seen (see core.TagTally), and never
// held across anything but map and counter work. Overturning this splits the counting by domain,
// not the ledger itself, and fails only if the lock recorder of record ever
// finds a plain lock held above it.
type Set struct {
	// decision: mu is the capacity domain and is Init'ed as a leaf: nothing
	// is acquired while it is held, in order or out, except the leaf mutex of
	// a core.TagTally when a tag first appears, which holds no I/O and takes
	// nothing (its own decision gives why), and the recorder
	// reports the first acquisition made under it — which the rank rules
	// alone could not, for an in-order take under it names no violation.
	// That is what makes the order the callers imply deadlock-free: the
	// durability worker paths take this lock under a stream's leaf mutex,
	// the sinks take that mutex under the segment append lock, so a
	// capacity-under-append acquisition is in order by rank and turns into
	// the cycle the leaf flag exists to catch only once some Set method
	// takes another lock under mu. What the flag still cannot see is an
	// unranked lock taken under mu, or a path no recorded run exercises:
	// it reports what the recorder ranks, nothing more. Overturned the day
	// mu must hold across anything — a callback, a callback's locks, or a
	// second mutex — and the leaf marker goes with it.
	mu     lockorder.Mutex
	cap    int64
	log    *slog.Logger
	segs   map[core.SegmentID]*entry
	spares map[core.SegmentID]struct{}
	tags   core.TagTally
	// over is each share tag's record bytes that are not payload — headers,
	// padding and the whole of every payload-free record — kept beside tags
	// and folded back by the same retirement.
	over core.TagTally
	// retired is each share tag's cumulative record bytes released by
	// retirements and recycles — the monotone figure of what left, never
	// lowered, read beside tags by the journal's admission seed. The two
	// figures move in one lock only through removeLocked, so a reader of
	// the pair sees each retirement exactly once: either still in tags,
	// or already moved into retired, never both and never neither.
	retired map[core.ShareTag]int64

	// noAppendTag holds, per share tag, the record bytes its records hold
	// in the segments that take no further append — the sealed and the
	// ended alike — moved under mu beside the entries' own per-tag bytes at
	// each state change, charge and removal.
	noAppendTag core.TagTally

	// untaken holds the segments whose end event has not been taken by its
	// consumer yet: the end itself is made synchronously in the step that
	// ends the segment, while the event hands over on its own goroutine, so
	// the untaken flag is the reader's proof of the end before the take. It
	// is cleared by the take and by the segment's own removal, and kept
	// beside the set's state under mu.
	untaken map[core.SegmentID]struct{}

	// c holds the counters of Counters above, maintained at each event that
	// moves one under mu and never rebuilt: every read is a set of atomic loads.
	c liveCounters

	// onSeal and onRetire are the observers registered through OnSeal and
	// OnRetire, guarded by obs: the delivery lock, which every
	// observer-firing call takes for the whole of its state change and
	// holds across the callback. They are registered before the set serves
	// any caller, beside the rest of the ledger's construction.
	onSeal   func(core.SegmentID)
	onRetire func(core.SegmentID)

	// obs serialises observer delivery: taken by each call that fires an
	// observer before its state change begins and held until the callback
	// has returned. A retire of a segment therefore cannot fire before
	// that segment's seal callback has returned — the retire blocks until
	// the seal's delivery is whole, state change and callback alike — and
	// each callback sees the state change it names complete, every
	// mutator's mu held nowhere across a callback. No call takes it but the
	// two fire sites and the two registrations, so it joins no cycle.
	//
	// decision: obs is unranked and not a leaf: it is held across mu and
	// across the observers' callbacks. The lock-order recorder never sees
	// it. Withdraw this if a callback ever calls a set mutator that
	// delivers, which would wait on the delivery its own call holds.
	obs sync.Mutex
}

// entry is one segment of the set: its state, the size it is accounted at,
// its claim flag, and the record bytes per share tag it holds. The claim
// lives here so it leaves with the segment the day it is retired. The record
// bytes and the size are one invariant: what the segment holds never exceeds
// what it is accounted at, so a record that would break the bound is refused
// where it is charged, and a cut that would break it is refused where the
// size is lowered. Its fields are guarded by the mu of the Set that holds it,
// and only that Set reaches an entry — the capacity lock domain its owner
// names covers it.
type entry struct {
	state State
	size  int64
	// stream is the append stream that took the segment, zero until a stream
	// names it: a segment an open adopted is not named.
	stream  uint32
	claimed bool
	// full is what the seal named: true only for a segment closed at its cap
	// by a full seal, never for one the seal named early — whose truncation
	// may have failed and whose size cannot tell the two apart.
	full bool
	// held is the total record bytes the entries below hold for every share
	// together — the running bound against size, not a derived sum.
	held int64
	// tags is nil until the first record byte is accounted to the segment.
	tags map[core.ShareTag]int64
	// over is the part of tags that is not payload, per tag; nil until the
	// first record is accounted.
	over map[core.ShareTag]int64
}

// overSum totals a per-tag overhead map. A nil map is the segment that has
// charged none, summing to zero.
func overSum(m map[core.ShareTag]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// tagSum totals a per-tag record-byte map the way overSum does an overhead
// one. A nil map is the segment that charged none.
func tagSum(m map[core.ShareTag]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// absentMessage is the Warn of a retire or recycle that subtracted nothing.
const absentMessage = "segment: retire found the segment out of the set"

// NewSet returns an empty ledger that accounts every spare at segCap, the
// segment cap. A cap that is not positive names no storage a spare could be
// accounted at, so it is a caller bug and panics. A nil logger keeps the
// ledger silent — each call logs nothing, and every Warn line is suppressed.
func NewSet(segCap int64, log *slog.Logger) *Set {
	if segCap <= 0 {
		panic(fmt.Sprintf("segment: the cap %d is not a size a spare can be accounted at", segCap))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Set{
		cap:     segCap,
		log:     log,
		segs:    make(map[core.SegmentID]*entry),
		spares:  make(map[core.SegmentID]struct{}),
		retired: make(map[core.ShareTag]int64),
		untaken: make(map[core.SegmentID]struct{}),
	}
}

// Cap returns the size every segment of the set is accounted at, which no
// record a stream appends to one of its segments ends past. It is fixed when
// the set is made and takes no lock.
func (s *Set) Cap() int64 { return s.cap }

// OnSeal registers fn as the set's seal observer: called with the id of each
// segment a Seal closed, once the segment turned Sealed and with no lock of
// the ledger's held — the whole state change its call made is visible to
// the callback and to every other call — and never for a refusal. A seal's
// callback has returned before any retire callback of the segment it sealed
// runs, and a callback may read the set but not register an observer, which
// would wait on the delivery its own call holds. A nil fn registers
// nothing; a second call replaces the first. The observer is registered
// beside the rest of the ledger's construction, before the set serves any
// caller.
func (s *Set) OnSeal(fn func(core.SegmentID)) {
	s.obs.Lock()
	defer s.obs.Unlock()
	s.onSeal = fn
}

// OnRetire registers fn as the set's retire observer: called with the id of
// each segment that left the set — a Retire's unlink and a Recycle's old id
// alike — once the removal and every step it carried are complete and with
// no lock of the ledger's held, and never for a refusal or a spare's drop.
// The same delivery rules as OnSeal's hold: a retire callback for a segment
// runs after that segment's seal callback has returned, and a callback may
// read the set but not register an observer. The ledger names the retired
// segment only: which files its records held is written nowhere in it once
// the segment is gone, so a consumer that needs them keeps that registry
// itself.
func (s *Set) OnRetire(fn func(core.SegmentID)) {
	s.obs.Lock()
	defer s.obs.Unlock()
	s.onRetire = fn
}

// sealSeen fires the seal observer for id, which a Seal has just closed.
// It is called with mu released and obs held by the caller, which held obs
// across its whole state change — so a retire of the segment cannot fire
// before this callback has returned.
func (s *Set) sealSeen(id core.SegmentID) {
	if s.onSeal == nil {
		return
	}
	s.onSeal(id)
}

// retireSeen fires the retire observer for id, which a Retire or a Recycle
// has just removed from the set. It is called with mu released and obs held
// by the caller, which held obs across its whole state change.
func (s *Set) retireSeen(id core.SegmentID) {
	if s.onRetire == nil {
		return
	}
	s.onRetire(id)
}

// SetRecorder installs the lock-order recorder the ledger's mutex reports to,
// with its capacity rank and its leaf promise: the pool's mutex and the
// capacity admission's are of the same domain, so no two of them are ever
// held at once, and the ledger's callers come at it from every direction —
// so its own marker, not a rank, is what keeps nothing under it. It runs
// before the set is shared — before any call the mutex serialises races
// another — and a nil recorder leaves the mutex a plain one.
func (s *Set) SetRecorder(rec *lockorder.Recorder) {
	s.mu.InitLeaf(lockorder.Capacity, rec)
}

// ownsLocked reports whether the id is already in the set or among the
// spares. Only ids the ledger does not own may be added.
func (s *Set) ownsLocked(id core.SegmentID) bool {
	if _, ok := s.segs[id]; ok {
		return true
	}
	_, ok := s.spares[id]
	return ok
}

// AddSpare carries the step that reserves cap for a fresh spare against the
// journal's maximum, max, raising UsedBytes before the allocation it accounts
// for happens. The reservation and the check of the maximum are one critical
// section under the ledger's mutex, so two makes near the maximum never both
// pass — the answer is not a read of Counters followed by a decision outside
// the lock. A refusal returns the NoSpaceError the maximum owes — Limit
// LimitJournal, Missing what the reservation lacked, Unit bytes — and changes
// nothing: no counter, no spare, and it is not counted a double retire.
//
// AddSpare is called before the spare's creation; the allocation itself is
// the caller's, and a spare whose making fails is returned by DropSpare once
// its unlink has happened, so the raise always comes first. A leftover the
// unlink cannot remove is carried into the set with Take and ended instead —
// a spare cannot go back to being one — and a later retire takes it.
//
// decision: the maximum is checked against the footprint the ledger holds,
// and a reservation held against the maximum outside the ledger counts only
// once it moves into this critical section, never by being read beside it.
// A max <= 0 therefore refuses every spare, with no special case. Overturn
// this only if a caller must reserve against the maximum without a spare,
// which would need a call of its own rather than a read of UsedBytes.
func (s *Set) AddSpare(id core.SegmentID, max int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == 0 {
		return ErrBadID
	}
	if s.ownsLocked(id) {
		return ErrDuplicateID
	}
	// The overflow-safe form of UsedBytes+cap > max over every max the
	// callers reach: NewMaker refuses a max <= 0, so max-s.cap and the
	// Missing below cannot wrap on this side. A max below the cap refuses
	// here without any wraparound of its own.
	if s.cap > max || s.c.UsedBytes.Load() > max-s.cap {
		return &core.NoSpaceError{
			Limit:   core.LimitJournal,
			Missing: s.c.UsedBytes.Load() + s.cap - max,
			Unit:    core.UnitBytes,
		}
	}
	s.spares[id] = struct{}{}
	s.c.UsedBytes.Add(s.cap)
	s.c.Spares.Add(1)
	return nil
}

// RenameSpare moves spare old to the id fresh, the name its file was renamed
// to: the footprint and the spare count do not change, for the file is the
// same cap under a new name. It refuses an old that is no spare and a fresh
// the ledger already owns.
func (s *Set) RenameSpare(old, fresh core.SegmentID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if fresh == 0 {
		return ErrBadID
	}
	if _, ok := s.spares[old]; !ok {
		return ErrNotInSet
	}
	if s.ownsLocked(fresh) {
		return ErrDuplicateID
	}
	delete(s.spares, old)
	s.spares[fresh] = struct{}{}
	return nil
}

// RefusesSpare answers under the ledger's own mutex whether a spare whose
// making AddSpare has not yet attempted would be refused by the journal
// maximum max, and the NoSpaceError that refusal owes when it would: the
// same critical section and the same figures as AddSpare's own, read with no
// id drawn, no counter moved and no spare added. A roll the pool cannot
// serve reads it to answer the journal's maximum rather than the pool's own
// `spare`, which names a shortage the pool clears on its own.
func (s *Set) RefusesSpare(max int64) (*core.NoSpaceError, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The overflow-safe form of AddSpare's own check, over every max its
	// callers reach: a max below the cap refuses here without a wraparound.
	if s.cap > max || s.c.UsedBytes.Load() > max-s.cap {
		return &core.NoSpaceError{
			Limit:   core.LimitJournal,
			Missing: s.c.UsedBytes.Load() + s.cap - max,
			Unit:    core.UnitBytes,
		}, true
	}
	return nil, false
}

// AdoptSpare accounts a spare the journal finds at open: a segment file the
// directory already holds, allocated at its cap, which the caller has decided
// is a spare — the scan that finds it and the decision are the caller's, and
// belong to open. It raises the footprint by the cap and the spare count,
// changing nothing else, so the spare is accounted beside the set exactly as
// one the Maker made: no limit asks which way a spare entered.
//
// decision: no maximum is checked, because the file already exists and every
// file the journal holds is accounted, so refusing it would leave allocated
// storage unaccounted and no limit bounding it. The footprint may then stand
// above a lowered maximum, which only blocks new spares (AddSpare refuses)
// until retirements bring it down. Withdrawn if open is ever required to
// refuse a directory over its maximum.
func (s *Set) AdoptSpare(id core.SegmentID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == 0 {
		return ErrBadID
	}
	if s.ownsLocked(id) {
		return ErrDuplicateID
	}
	s.spares[id] = struct{}{}
	s.c.UsedBytes.Add(s.cap)
	s.c.Spares.Add(1)
	return nil
}

// AdoptRebuilt enters a segment the open's rebuild found, with its record
// bytes already charged per share tag: the state it was classed at, the
// seal's own full word, the accounted size and each share's record and
// overhead bytes are the scan's facts, handed whole, and the ledger accounts
// them as the event paths would have — the footprint rises by the accounted
// size, the segment count and, for a sealed one, the sealed count, each
// tag's bytes join the shares' totals and the entry's running bound is set to
// their sum, so the counters and Check read the same had the journal itself
// taken, charged, sealed and cut the segment.
//
// The accounted size is the caller's, not the file's: a segment is accounted
// at its cap from its creation until its unlink, and only one shortened by a
// torn-tail cut or at seal below it, so the caller hands the cap or the
// post-seal size. The entry refuses what the format cannot spell — a size
// above the cap, one the charged bytes do not fit, and a full word naming
// anything but a sealed segment closed at the cap — and, like an adopted
// spare, checks no maximum: the file the scan read already exists, and every
// file the journal holds is accounted.
//
// decision: a segment is never refused because the set already holds
// segments, only for an id the ledger owns. The rebuild seeds a fresh set
// with one adoption per segment, so a set that already holds segments is a
// seed half through, and an emptiness gate would refuse every sibling after
// the first. Withdrawn if an adoption ever runs into a ledger already in
// service, which would have to carry a fact the ledger can gate on rather
// than an emptiness it cannot tell from a seed half through.
func (s *Set) AdoptRebuilt(id core.SegmentID, size int64, st State, full bool, tags, over map[core.ShareTag]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == 0 {
		return ErrBadID
	}
	if s.ownsLocked(id) {
		return ErrDuplicateID
	}
	if st != Active && st != Sealed && st != Ended {
		return ErrWrongState
	}
	// A full word is the seal's own; an unsealed segment cannot carry one.
	if full && st != Sealed {
		return ErrWrongState
	}
	if size <= 0 || size > s.cap {
		return ErrBadSize
	}
	// A full seal closed the segment at the cap it is accounted at, so a full
	// word below the cap names a shape no seal makes.
	if full && size != s.cap {
		return ErrBadSize
	}
	var held int64
	for _, n := range tags {
		// Mirrors AddRecord's own bounds: a charge of no bytes is refused, so
		// a zero key never sits in the shares' totals, and each tag's bytes
		// sit inside the storage the size accounts for, as AddRecord's bound
		// keeps an appended record inside the segment's accounted size. The
		// sum is held under the size as it accumulates — each tag's charge
		// must fit the size less what the tags before it took — so the sum
		// never passes the size and can never wrap past the comparison.
		if n <= 0 || n > size-held {
			return ErrBadSize
		}
		held += n
	}
	// An overhead of zero is charged nowhere, as AddRecord charges none: the
	// key is kept out of the entry's map and the totals' alike, so no figure
	// the ledger reads holds a share with nothing in it. A refused call
	// leaves holds a scratch map no ledger state ever sees.
	var holds map[core.ShareTag]int64
	for tag, n := range over {
		// Mirrors AddRecord's own bounds: an overhead is never negative and
		// sits inside the bytes it is part of, or the tag has no bytes to sit
		// in at all.
		if n < 0 || n > tags[tag] {
			return ErrBadSize
		}
		if n > 0 {
			if holds == nil {
				holds = make(map[core.ShareTag]int64, len(over))
			}
			holds[tag] = n
		}
	}
	s.segs[id] = &entry{
		state: st,
		size:  size,
		full:  full,
		held:  held,
		tags:  maps.Clone(tags),
		over:  holds,
	}
	s.c.UsedBytes.Add(size)
	s.c.Segments.Add(1)
	if st == Sealed {
		s.c.Sealed.Add(1)
	}
	if st != Active {
		// An adopted segment joins the no-append measures its state names —
		// a sealed or ended one takes no further append — with the bytes it
		// arrives accounting for, so its later removal lowers them by what
		// it entered with. An entry raised here is the same one Seal and End
		// raise on the running journal.
		s.c.NoAppendBytes.Add(tagSum(tags))
		s.c.NoAppendOverBytes.Add(overSum(holds))
		for tag, n := range tags {
			s.noAppendTag.Add(tag, n)
		}
	}
	for tag, n := range tags {
		s.tags.Add(tag, n)
	}
	for tag, n := range holds {
		s.over.Add(tag, n)
	}
	return nil
}

// SetStream records that stream no took segment id, for its row to report. A
// segment not in the set is left alone.
func (s *Set) SetStream(id core.SegmentID, no uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.segs[id]; ok {
		e.stream = no
	}
}

// Take carries a spare into the set, as the segment a stream appends to. The
// storage is already accounted, so the footprint does not move: it moves only
// when a segment is created, unlinked or shortened.
func (s *Set) Take(id core.SegmentID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.spares[id]; !ok {
		return ErrNotInSet
	}
	delete(s.spares, id)
	s.c.Spares.Add(-1)
	s.c.Segments.Add(1)
	s.segs[id] = &entry{state: Active, size: s.cap}
	return nil
}

// State returns the state of segment id in the set, or ErrNotInSet: what
// the sealed, ended and charged paths need to know of one segment, read
// without a walk of the set.
func (s *Set) State(id core.SegmentID) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return 0, ErrNotInSet
	}
	return e.state, nil
}

// Seal closes a segment to further appends, at the size it is accounted at:
// a full seal keeps the cap, an early one is followed by Shrink to the
// truncated size. The full flag is the seal's own word for itself, and the
// only fact a later recycle can trust: an early seal whose truncation sync
// failed stays Sealed and accounted at its cap (the truncation never became
// durable), so a size at the cap does not name a full seal.
func (s *Set) Seal(id core.SegmentID, full bool) error {
	// obs is held across the state change and the callback: a retire of the
	// segment waits here until the seal's delivery is whole, so the retire
	// callback never runs before the seal callback has returned.
	s.obs.Lock()
	defer s.obs.Unlock()
	s.mu.Lock()
	e, ok := s.segs[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotInSet
	}
	if e.state != Active {
		s.mu.Unlock()
		return ErrWrongState
	}
	e.state = Sealed
	e.full = full
	s.c.Sealed.Add(1)
	s.c.NoAppendBytes.Add(tagSum(e.tags))
	s.c.NoAppendOverBytes.Add(overSum(e.over))
	for tag, n := range e.tags {
		s.noAppendTag.Add(tag, n)
	}
	s.mu.Unlock()
	// No lock of the ledger's is held while the observer runs, and the seal
	// it names is whole: every read answers with state past the change.
	s.sealSeen(id)
	return nil
}

// SealedFull reports whether segment id is in the set, Sealed, and sealed full
// (at its cap): the one shape a retirement recycles. It is a read under s.mu,
// no walk.
func (s *Set) SealedFull(id core.SegmentID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	return ok && e.state == Sealed && e.full
}

// End closes a segment a failed append or sync left, keeping the cap it is
// accounted at until it is unlinked: an ended segment is never sealed, and
// its records leave with the segment when it is retired.
func (s *Set) End(id core.SegmentID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state != Active {
		return ErrWrongState
	}
	e.state = Ended
	s.c.NoAppendBytes.Add(tagSum(e.tags))
	s.c.NoAppendOverBytes.Add(overSum(e.over))
	for tag, n := range e.tags {
		s.noAppendTag.Add(tag, n)
	}
	return nil
}

// OweEnd records that id, already ended, is owed an end event its consumer
// has not taken yet: the segment's failure opened a window the consumer —
// not the end's own maker — is the only word for, so from the moment the end
// is made a reader that asks about the segment before the take must be
// answered as though the window were open already. The step that ends the
// segment and starts its event is the caller; TakeEnd is the take's.
func (s *Set) OweEnd(id core.SegmentID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.untaken[id] = struct{}{}
}

// TakeEnd clears the owed end of id: its event reached the consumer, whose
// own state — the window it made, the failure it recorded — answers from
// here. One taken is not owed; a second take changes nothing.
func (s *Set) TakeEnd(id core.SegmentID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.untaken, id)
}

// EndOwed reports whether id ended and its end event has not been taken yet.
func (s *Set) EndOwed(id core.SegmentID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.untaken[id]
	return ok
}

// Shrink lowers the accounted size of a segment of the set that no stream
// appends to anymore: what a seal's truncation or a torn-tail cut of an
// ended segment takes from the file comes off the footprint here. A segment a
// stream still appends to is refused: its bytes sit at the cap from its
// takeover to its seal or its end, and a cut before either would sit the
// footprint below the storage the file keeps allocating, a break no check of
// the set's own fields can see. The deducted difference is the caller's to
// name; only the accounting is the ledger's.
//
// decision: Active is refused rather than served, because both cuts the
// format knows — the seal's truncation and the torn-tail cut of an ended
// segment — run after the stream left the segment, so a shrink naming an
// Active one is a caller bug, and serving it would let the footprint fall
// below the allocated storage with the ledger none the wiser. Withdrawn if a
// cut of an appended-to segment ever exists, which a marker from the stream
// side would have to keep above the append's bytes.
//
// decision: the Ended case is served, and recovery's torn-tail cut (RFC 28
// §3.3) is the only caller meant to reach it — an ended segment keeps its cap
// until its unlink, and a runtime failed-append ended segment has no cut of
// it coming. The ledger cannot tell the two callers apart, so the caller
// contract holds the ceiling: a Shrink of an ended segment that never came
// from recovery keeps the accounted size below the file, silent to every
// check. Withdrawn if a runtime caller ever needs its own cut, which must
// then carry a fact the ledger can gate on.
//
// decision: the Sealed case is served any number of times, because the
// ledger cannot see the file. The seal writer makes one cut by contract — it
// shrinks once, after the truncation is durable — but a caller that shrank an
// early-sealed segment again would keep the footprint below the file with
// Check still nil, and the ledger would not know; a cut flag cannot hold this
// either, because a legitimate path (recovery's torn-tail cut of an ended
// segment, later truncated past its trailer) can cut twice. Withdrawn if a
// caller ever needs its second cut: it must carry a fact the ledger can gate
// on, or the sequences both callers make become known and a flag can count
// cuts.
func (s *Set) Shrink(id core.SegmentID, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state == Active {
		return ErrWrongState
	}
	// A full seal has no cut of the file behind it: nothing was truncated,
	// so there is no cut to account, and only the seal the caller named early
	// ever serves its truncation here.
	if e.full && e.state == Sealed {
		return ErrWrongState
	}
	if size <= 0 || size > e.size {
		return ErrBadSize
	}
	// The cut may lower the size only as far as the record bytes the segment
	// still holds: below them the set would charge shares for storage the
	// segment no longer accounts for. Both cuts the format knows — a seal's
	// truncation and the torn-tail cut of an ended segment — run after the
	// segment took its last record, and each leaves the size at or above
	// those bytes, so a lower cut is a caller bug.
	if size < e.held {
		return ErrBadSize
	}
	s.c.UsedBytes.Add(-(e.size - size))
	e.size = size
	return nil
}

// AddRecord accounts n record bytes — a record's header, payload and padding
// together — to the share whose tag the record carries, in an Active segment
// of the set. The footprint does not move: the bytes are inside storage the
// segment already accounts for, so they count against the share's record
// bytes, not against the journal's allocation.
//
// decision: an Ended segment is served exactly as an Active one, because an
// append failure ends its segment with appends still in flight, and the
// record that completes after the end wrote bytes inside storage the segment
// already covers — uncharging them would hand the share bytes the journal
// still holds (R55). A Sealed segment is refused still, for a seal drains
// every appender before it seals, so no completion can reach a charge past
// one; a refusal that would name an arrived-late charge against a sealed
// segment is a caller bug this refusal reports rather than a state it
// absorbs. Withdrawn the day a charge can ever arrive for a segment after
// its seal — a seal that stops draining its appender, or a completion that
// outlives the drain — which would have to carry the segment's own id
// through to the charge for the gate to hold.
//
// ponytail: one mutex serves every append's tag accounting, so a mesh of
// streams on many files funnels into one lock the hotter it gets. Ceiling:
// contention on it, visible as a flat profile with every append's owner
// waiting here. Upgrade to per-segment counters per tag once a profile shows
// it, and let the retire and recycle paths fold them back into each share's
// total before releasing its per-tag bytes.
func (s *Set) AddRecord(id core.SegmentID, tag core.ShareTag, n, overhead int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok {
		return ErrNotInSet
	}
	if e.state != Active && e.state != Ended {
		return ErrWrongState
	}
	if n <= 0 || overhead < 0 || overhead > n {
		return ErrBadSize
	}
	// The record's bytes sit inside storage the segment already accounts for,
	// so the running total of its record bytes cannot pass its size: a
	// record that would take it past charges a share for storage the segment
	// does not have, and is refused before anything moves.
	if e.held+n > e.size {
		return ErrBadSize
	}
	if e.tags == nil {
		e.tags = make(map[core.ShareTag]int64)
	}
	if e.over == nil {
		e.over = make(map[core.ShareTag]int64)
	}
	e.held += n
	e.tags[tag] += n
	s.tags.Add(tag, n)
	if e.state == Ended {
		// A record completing against an ended segment joins the measures
		// its end took; a sealed one is refused above, and an active one
		// has not entered them yet.
		s.c.NoAppendBytes.Add(n)
		if overhead > 0 {
			s.c.NoAppendOverBytes.Add(overhead)
		}
		s.noAppendTag.Add(tag, n)
	}
	if overhead > 0 {
		e.over[tag] += overhead
		s.over.Add(tag, overhead)
	}
	return nil
}

// TagFigures returns copies of the record bytes and the non-payload bytes held
// per share tag. The two are read one after the other without the set's lock,
// so a move between the reads shows in one map and not the other.
func (s *Set) TagFigures() (bytes, overhead map[core.ShareTag]int64) {
	// The overhead is read first: a record's bytes are added before its
	// overhead, so a record landing between the two reads shows its bytes
	// without its overhead, which reads the unreclaimed figure high for one
	// operation in flight. That holds for additions only: a retirement or a
	// forget lowers the bytes before the overhead, so a read between the two
	// steps can show the figure low, which the caller's clamp at zero catches.
	overhead = s.over.Snapshot()
	return s.tags.Snapshot(), overhead
}

// ForgetTag releases tag's record bytes from the whole set's accounting, as
// the durable forget record of the tag does: the bytes stay in their segments
// until those retire, but they are the tag's no longer — a Stats listing of
// tags with records on disk no longer names it, a fresh seed of its capacity
// figure opens at zero, and the retirement of a segment that held its records
// releases nothing more for it. The per-segment entries and the running total
// leave together, and the bytes they held move to the tag's cumulative
// retired figure — the release the retirements would have made, made at the
// forget, once — so the capacity reconciliation that reads the pair settles
// the tag's figure to what is left of it: nothing.
//
// The call runs after the forget's liveness snapshot was taken, which reads
// the per-segment figures this clears.
//
// decision: the released figures are what a share-scope repack reads to name
// its segments, so one aimed at a re-attached tag can no longer seal or
// select the segments that hold only its forgotten, dead records — matching
// the reopen, whose rebuilt set never counts the withheld records at all.
// Those segments are still reclaimed: the worker's tick takes them once
// nothing holds them, and a zero-scope pass selects them by their own bytes.
// The ceiling: in a journal with no idle sealer and no zero-scope pass, an
// active segment holding only forgotten records is sealed only by a later
// roll. Overturned by a ruled rule that dead records still bind a share's
// scope, which would need the figures kept and a reopen that counts them.
func (s *Set) ForgetTag(tag core.ShareTag) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.segs {
		n := e.tags[tag]
		if n == 0 {
			continue
		}
		// The segment's records are still there: only the tag's share of the
		// accounting moves, so the totals the Check walk reconciles — the
		// segment's own held bytes against its per-tag entries — stay whole.
		// A segment that takes no further append — sealed or ended — held
		// the tag's bytes in the no-append measures, and the release takes
		// them out of those measures here: the retirement that later
		// subtracts the emptied maps cannot be their second door.
		if e.state != Active {
			s.c.NoAppendBytes.Add(-(n))
			s.c.NoAppendOverBytes.Add(-(e.over[tag]))
			s.noAppendTag.Add(tag, -n)
		}
		e.held -= n
		delete(e.tags, tag)
		delete(e.over, tag)
	}
	if n := s.tags.Get(tag); n != 0 {
		s.retired[tag] += n
		s.tags.Set(tag, 0)
	}
	s.over.Set(tag, 0)
}

// ShareBytes returns the record bytes of the share's tag still held in
// segments of the set — one map read, no walk of the set.
func (s *Set) ShareBytes(tag core.ShareTag) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags.Get(tag)
}

// ShareBytesAndRetired reads, under the set's one lock, the tag's settled
// record bytes — the bytes its records still hold in the set's segments —
// beside its cumulative retired record bytes: what retirements and recycles
// have released of the tag, monotone, never lowered. The pair is one read so
// a seed of the tag's capacity figure sees every retirement exactly once: a
// retirement that has run lowers tags and raises retired in the same lock,
// one that has not leaves both where they were — whichever side of the read
// a retirement lands on, it shows in exactly one of the two figures.
func (s *Set) ShareBytesAndRetired(tag core.ShareTag) (settled, retired int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags.Get(tag), s.retired[tag]
}

// Row is one segment of the set as Snapshot reads it. Tags is the row's own
// copy of the record bytes per share tag, so a caller may keep or change it;
// it is nil for a segment with no records.
type Row struct {
	ID    core.SegmentID
	State State
	Size  int64
	// Stream is the append stream that took the segment; zero for one an
	// open adopted, which no stream has named.
	//
	// ponytail: an adopted segment reports stream zero, indistinguishable from
	// a real stream-0 segment, for the rebuild keeps
	// no header's stream beside its scan. Upgrade to carrying the scanned
	// header's stream into the adoption when a reader needs the stream of a
	// segment that predates this process.
	Stream uint32
	Tags   map[core.ShareTag]int64
}

// Snapshot lists every segment of the set, spares excluded, in ascending id
// order. It is one lock and a copy, with no I/O: the rows describe the set at
// one instant, and a caller that acts on one must still re-check it, as every
// claim does.
func (s *Set) Snapshot() []Row {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows := make([]Row, 0, len(s.segs))
	for id, e := range s.segs {
		rows = append(rows, Row{ID: id, State: e.state, Size: e.size, Stream: e.stream, Tags: maps.Clone(e.tags)})
	}
	slices.SortFunc(rows, func(a, b Row) int { return cmp.Compare(a.ID, b.ID) })
	return rows
}

// Claim takes a retirement's claim on a segment of the set that no stream
// can still append to. It is held until Release clears it, or until the
// segment is retired, in which case the claim is gone: a release or a new
// claim on a retired segment finds nothing and takes nothing.
func (s *Set) Claim(id core.SegmentID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.segs[id]
	if !ok || e.state == Active || e.claimed {
		return false
	}
	e.claimed = true
	return true
}

// Release clears a claim. Releasing what is not claimed, or a segment already
// retired — whose claim left with it — changes nothing.
//
// decision: releasing a retired id reaches nothing and clears nothing, so the
// claim of a retired segment cannot be erased by a late release — and the
// same holds for the fresh id a recycle minted, because ids are never reused
// under a live set: a segment's id starts at 1 and ascends, and the id of a
// new spare or a recycle exceeds every id in the directory (RFC 28 §2.2). A
// reused id could make this release land on the new segment that owns it, so
// the holder's own claim checks — which read the flag and the state together —
// are the ceiling that holds until then. Withdrawn if ids ever become
// reusable, in which case the claim must carry the id of the retired segment
// rather than reaching by it.
func (s *Set) Release(id core.SegmentID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.segs[id]; ok {
		e.claimed = false
	}
}

// ClaimRetirement checks a segment can be retired and takes its claim as one
// step, under the ledger's own mutex with no I/O, so two retirements of one
// segment serialise here rather than racing each other's steps. It answers
// the segment's state and the seal's own full word — the only fact a later
// recycle can trust — once the claim is held.
//
// The refusals follow the ledger's own table: an id out of the set counts a
// double retire and logs at Warn like a Retire that subtracted nothing; a
// spare and an Active segment are refused ErrWrongState uncounted, for the
// same reason Retire refuses both; a segment whose claim another path holds
// is refused ErrClaimed, uncounted.
//
// decision: the claim is refused while it is held at all, whichever path
// holds it — the plan's word is "refused … a segment with a live claim": a
// second retirement, or a repack's mid-move claim — rather than only when
// it is another retirement's, because the ledger cannot tell the two apart
// and a retirement that ran beside a repack would unname storage the repack
// was still moving. The claim dies with the segment a Retire or a Recycle
// removes, so nothing releases it and R57 holds: a release after the
// retirement finds no entry. A refused retirement that does not retire the
// segment releases the claim itself, so a retry can take it again — allowed
// by the same rule, because the segment was not retired. Overturned if epic
// 9 ever needs to retire a segment while holding its own claim, which would
// need a claim token passed in rather than a flag flipped here.
func (s *Set) ClaimRetirement(id core.SegmentID) (st State, full bool, err error) {
	s.mu.Lock()
	e, ok := s.segs[id]
	if !ok {
		if _, spare := s.spares[id]; spare {
			s.mu.Unlock()
			return 0, false, ErrWrongState
		}
		s.c.DoubleRetires.Add(1)
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(id), "op", "retire")
		return 0, false, ErrNotInSet
	}
	defer s.mu.Unlock()

	if e.state == Active {
		return 0, false, ErrWrongState
	}
	if e.claimed {
		return 0, false, ErrClaimed
	}
	e.claimed = true
	return e.state, e.full, nil
}

// DropSpare returns a spare's cap after the spare's unlink has happened: like
// Retire, it is the ledger's half of a reclamation, called once the storage
// side is done, and only the call that finds the id among the spares
// subtracts. A spare is not a segment — no stream ever appended to it — so
// its removal touches the spare count and the footprint and nothing else.
//
// The id that names no spare counts itself a double retire and logs at Warn,
// exactly as Retire's does: the pool and a failed make's cleanup can both
// select one spare, and the second drop finds it gone. A segment of the set
// is refused uncounted, for the same reason Retire refuses a spare: the caller
// named live storage by the wrong door, and counting that would drown the
// race the count exists to watch.
func (s *Set) DropSpare(id core.SegmentID) (freed int64, err error) {
	s.mu.Lock()
	if _, ok := s.spares[id]; !ok {
		if _, seg := s.segs[id]; seg {
			s.mu.Unlock()
			return 0, ErrWrongState
		}
		s.c.DoubleRetires.Add(1)
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(id), "op", "drop")
		return 0, ErrNotInSet
	}
	delete(s.spares, id)
	s.c.Spares.Add(-1)
	s.c.UsedBytes.Add(-(s.cap))
	s.mu.Unlock()
	return s.cap, nil
}

// Retire carries the unlink step of a retirement: it removes a segment of
// the set — Sealed or Ended — subtracts its accounted size from the footprint
// and releases each of its record bytes from the share it was accounted to.
// Only the call that finds the id in the set subtracts: the reclamation
// executor calls on the one hand and the retire queue on the other can both
// select one segment, and the second retire finds its bytes already gone,
// subtracts nothing, counts itself as a double retire and logs at Warn.
//
// decision: a segment still Active, and a spare of the pool, are refused
// rather than retired, and neither refusal is counted as a double retire.
// A retire of one means the caller's selection named a segment a stream
// still appends to, or arrived at a spare by the wrong door — a spare goes
// out through DropSpare — and both are a bug in the caller, not the race
// the count watches for, and counting either would drown that signal. The
// ledger is the only side that knows which ids are spares, so the refusal
// belongs here rather than in a caller's pre-check. Withdrawn if a caller
// ever needs to see either as a count.
func (s *Set) Retire(id core.SegmentID) (freed int64, err error) {
	// obs is held across the state change and the callback, as Seal's is,
	// so the retire callback runs after any seal callback of the segment
	// has returned.
	s.obs.Lock()
	defer s.obs.Unlock()
	s.mu.Lock()
	e, ok := s.segs[id]
	if !ok {
		if _, spare := s.spares[id]; spare {
			s.mu.Unlock()
			return 0, ErrWrongState
		}
		s.c.DoubleRetires.Add(1)
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(id), "op", "retire")
		return 0, ErrNotInSet
	}
	if e.state == Active {
		s.mu.Unlock()
		return 0, ErrWrongState
	}
	freed = e.size
	s.removeLocked(id, e)
	s.mu.Unlock()
	// The segment is gone from the set and the counters are whole; no lock
	// of the ledger's is held while the observer runs.
	s.retireSeen(id)
	return freed, nil
}

// Recycle carries the recycle step of a retirement: it removes a Sealed
// segment accounted at its full cap from the set and moves its storage to
// the spare pool under a fresh id, so the footprint does not move at all —
// the segment's bytes stay in use, now a spare's; a lock-free reader may see
// it one cap higher in the middle, never lower — while each of its record
// bytes is released to the share it was accounted to, exactly once. Every
// refusal is checked before anything changes: a recycle that fails has taken
// nothing and given nothing.
//
// decision: a recycle is a retire of the old id plus a new spare in one
// step, so the accounted footprint does not move and only an unlink of
// either the segment or the spare ever lowers it, while the spare pool grows
// by one. Overturning this only when a spare can be created smaller than a
// cap, which the segment format today forbids. A spare of the pool is
// refused uncounted, for the same reason as Retire's.
func (s *Set) Recycle(old, fresh core.SegmentID) error {
	// obs is held across the state change and the callback, as Retire's
	// is: the old id's retire callback runs once the fresh spare and the
	// counters the recycle restores are in place.
	s.obs.Lock()
	defer s.obs.Unlock()
	s.mu.Lock()
	e, ok := s.segs[old]
	if !ok {
		if _, spare := s.spares[old]; spare {
			s.mu.Unlock()
			return ErrWrongState
		}
		s.c.DoubleRetires.Add(1)
		s.mu.Unlock()
		s.log.Warn(absentMessage, "segment", uint64(old), "op", "recycle")
		return ErrNotInSet
	}
	if e.state != Sealed {
		s.mu.Unlock()
		return ErrWrongState
	}
	// Only a seal that said full names a segment whose file still holds the
	// whole cap — an early seal whose truncation sync failed stays Sealed at
	// the cap too, and recycling either would hand a truncated file to the
	// pool as a full spare (RFC 28 §2.5). A full seal is also the only seal
	// Shrink leaves at the cap: it refuses a full-sealed segment outright, so
	// the seal's own word is the whole test and the size is read nowhere.
	if !e.full {
		s.mu.Unlock()
		return ErrBadSize
	}
	if fresh == 0 {
		s.mu.Unlock()
		return ErrBadID
	}
	if s.ownsLocked(fresh) { // ownsLocked(fresh) covers fresh == old: old was found in segs
		s.mu.Unlock()
		return ErrDuplicateID
	}
	// The segment leaves and the spare enters under the one lock, before
	// the observer runs, so a callback reading the set sees the recycle
	// whole: the footprint unmoved and the fresh id owned.
	//
	// decision: the spare's cap is added to the footprint before the old
	// segment's is taken off, so a reader that takes no lock (Counters)
	// can see the footprint one cap too high in the middle of the recycle,
	// never one cap too low. High is the safe direction for the two gates
	// that read it at the journal maximum, the headroom's at-limit test and
	// the admission's footprint snapshot: a read that overstates the
	// footprint keeps the gate closed, while one that understated it would
	// admit an append the maximum does not have room for. Overturned if a
	// reader ever needs the footprint exact at every instant, which would
	// need a lock or a sequence it retries on.
	s.c.UsedBytes.Add(s.cap)
	s.c.Spares.Add(1)
	s.removeLocked(old, e)
	s.spares[fresh] = struct{}{}
	s.mu.Unlock()
	s.retireSeen(old)
	return nil
}

// removeLocked takes a segment out of the set and releases what it held:
// its accounted size from the footprint, its claim (which dies with the
// entry), and each of its per-tag record bytes from the share it was
// accounted to; a share's figure that reaches zero keeps its entry in the
// per-tag tally (see the ponytail on core.TagTally). The caller holds mu and
// has already found the entry in the set.
func (s *Set) removeLocked(id core.SegmentID, e *entry) {
	delete(s.segs, id)
	// The removed segment is owed no end event any more: whatever the take
	// did, the segment's leaving is the reader's later word.
	delete(s.untaken, id)
	if e.state == Sealed {
		s.c.Sealed.Add(-1)
	}
	if e.state != Active {
		// The segment's leaving takes the no-append measures with it: an
		// active one never entered them. A per-tag figure that reaches
		// nothing keeps its entry in the tally (see the ponytail on
		// core.TagTally); a negative leftover stays for Check.
		s.c.NoAppendBytes.Add(-(tagSum(e.tags)))
		s.c.NoAppendOverBytes.Add(-(overSum(e.over)))
		for tag, n := range e.tags {
			s.noAppendTag.Add(tag, -n)
		}
	}
	s.c.Segments.Add(-1)
	s.c.UsedBytes.Add(-(e.size))
	for tag, n := range e.tags {
		s.retired[tag] += n
		// A negative leftover can only be a release the totals never held:
		// left in place for Check to report rather than cleared.
		s.tags.Add(tag, -n)
	}
	for tag, n := range e.over {
		s.over.Add(tag, -n)
	}
}

// NoAppendBytesOf returns the record bytes tag's records hold in the
// segments that take no further append — the sealed and the ended alike —
// one atomic read of the figure the set maintains at each state change, charge
// and removal, taking no lock and allocating nothing.
func (s *Set) NoAppendBytesOf(tag core.ShareTag) int64 {
	return s.noAppendTag.Get(tag)
}

// Counters returns the ledger's maintained counters: atomic loads, taking no
// lock, no walk of the set and no allocation. The fields are read one after
// another, so a move between two reads shows in one and not the other.
func (s *Set) Counters() Counters {
	return Counters{
		UsedBytes:         s.c.UsedBytes.Load(),
		Segments:          s.c.Segments.Load(),
		Sealed:            s.c.Sealed.Load(),
		NoAppendOverBytes: s.c.NoAppendOverBytes.Load(),
		NoAppendBytes:     s.c.NoAppendBytes.Load(),
		Spares:            s.c.Spares.Load(),
		DoubleRetires:     s.c.DoubleRetires.Load(),
	}
}

// Check walks the set and reports whether the accounted footprint is the sum
// of every segment's accounted size plus every spare at the segment cap,
// whether each segment's held record bytes match the bytes its own entries
// name and fit its accounted size, and whether each maintained count matches
// what it is counted from. It is the verification of last resort, called by
// recovery's checks and by tests, not on any write path, and reports a
// mismatch as an error naming both figures wrapped around ErrMismatch.
func (s *Set) Check() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		sum      int64
		segments int64
		sealed   int64
		noApp    int64
		noAppOv  int64
	)
	// The per-tag no-append figures beside the two totals, so the counters
	// answer a walk the way the shares' figures do — a leak a release leaves
	// behind shows here rather than in a caller's Stats.
	noAppTag := make(map[core.ShareTag]int64)
	for id, e := range s.segs {
		// The total and its own per-tag map are two statements of one fact: a
		// drifted total admits a record that does not fit, so the check sums
		// the tags and takes neither statement's word. Above the size is the
		// same break from the other side.
		var tot int64
		for _, n := range e.tags {
			tot += n
		}
		if tot != e.held || e.held > e.size {
			return fmt.Errorf("%w: segment %d holds %d record bytes, its entries make %d and it is accounted at %d",
				ErrMismatch, uint64(id), e.held, tot, e.size)
		}
		// The no-append measures are the same two statements over the
		// segments that take no further append — the sealed and the ended
		// alike — so an active segment's bytes never enter them.
		if e.state != Active {
			for tag, n := range e.tags {
				noApp += n
				noAppTag[tag] += n
			}
			for _, n := range e.over {
				noAppOv += n
			}
		}
		sum += e.size
		segments++
		if e.state == Sealed {
			sealed++
		}
	}
	if s.c.NoAppendBytes.Load() != noApp {
		return fmt.Errorf("%w: the no-append record bytes are %d, the sealed and ended segments' entries make %d",
			ErrMismatch, s.c.NoAppendBytes.Load(), noApp)
	}
	if s.c.NoAppendOverBytes.Load() != noAppOv {
		return fmt.Errorf("%w: the no-append overhead bytes are %d, the sealed and ended segments' entries make %d",
			ErrMismatch, s.c.NoAppendOverBytes.Load(), noAppOv)
	}
	// Zero-valued keys count as absent on either side: a figure that
	// reached nothing is no share the ledger knows, wherever an edge left
	// the key behind.
	live := func(m map[core.ShareTag]int64) map[core.ShareTag]int64 {
		out := make(map[core.ShareTag]int64, len(m))
		for tag, n := range m {
			if n != 0 {
				out[tag] = n
			}
		}
		return out
	}
	if !maps.Equal(live(s.noAppendTag.Snapshot()), live(noAppTag)) {
		return fmt.Errorf("%w: the per-tag no-append bytes do not match the sealed and ended segments' entries: %v vs %v",
			ErrMismatch, s.noAppendTag.Snapshot(), noAppTag)
	}
	for range s.spares {
		sum += s.cap
	}

	// Every share's total is the sum of the bytes its segments' entries hold
	// for it: the walk sums each entry's own tags into a fresh map and
	// compares, so a release that dropped or doubled one — or a charge that
	// reached a share's total without its segment's entry — is a broken
	// ledger the check reports here. A share that reaches zero leaves the
	// ledger's map, so it also holds no entry to sum: the two maps agree in
	// length first, then byte for byte.
	held := make(map[core.ShareTag]int64)
	for _, e := range s.segs {
		for tag, n := range e.tags {
			held[tag] += n
		}
	}

	overs := make(map[core.ShareTag]int64)
	for _, e := range s.segs {
		for tag, n := range e.over {
			overs[tag] += n
		}
	}
	spares := int64(len(s.spares))
	tagsMatch, overMatch := maps.Equal(held, s.tags.Snapshot()), maps.Equal(overs, s.over.Snapshot())
	if s.c.UsedBytes.Load() == sum &&
		s.c.Segments.Load() == segments &&
		s.c.Sealed.Load() == sealed &&
		s.c.Spares.Load() == spares &&
		tagsMatch && overMatch {
		return nil
	}
	if s.c.UsedBytes.Load() != sum {
		return fmt.Errorf("%w: accounted %d, the set's sizes make %d",
			ErrMismatch, s.c.UsedBytes.Load(), sum)
	}
	if !tagsMatch {
		return fmt.Errorf("%w: the shares' record bytes do not match the segments' entries: %v vs %v",
			ErrMismatch, s.tags.Snapshot(), held)
	}
	if !overMatch {
		return fmt.Errorf("%w: the shares' overhead bytes do not match the segments' entries: %v vs %v",
			ErrMismatch, s.over.Snapshot(), overs)
	}
	return fmt.Errorf("%w: the counts do not match the set: %d segments vs %d, %d sealed vs %d, %d spares vs %d",
		ErrMismatch, s.c.Segments.Load(), segments, s.c.Sealed.Load(), sealed, s.c.Spares.Load(), spares)
}
