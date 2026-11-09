package durability

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Domains is the file append domain's table, which the datapath owns and
// passes in.
type Domains interface {
	// Lock takes id's file append domain and returns its release.
	Lock(id core.FileID) (unlock func())
}

// Config is what a Durability is built over.
type Config struct {
	Clock   core.Clock   // required
	Domains Domains      // required
	Tracer  core.Tracer  // optional
	Log     *slog.Logger // optional; nil means discard
	// Resolved, when set, is told that a failed segment's window is resolved
	// (R19): no record of it is pending any more. It runs on the stream's
	// worker with no lock held, so it must not wait on a Sync.
	Resolved func(stream uint32, seg core.SegmentID)

	// Index is the placement index the layer lifts retained content out of
	// when a sync completes. It is required.
	Index *index.Index

	// Dir is the journal's directory, the key of its per-process loss state.
	Dir string

	// Counters issue the loss record's sequence number. It is required.
	Counters *index.Counters

	// MarkLoss leaves the index's loss marker for a synced loss, and returns
	// the change to the file's held bytes that the marker made. It is
	// required.
	MarkLoss func(core.FileID, core.Extent, core.Version) (index.HeldDelta, error)

	// Account, when set, is handed every change the layer makes to the index
	// as a held delta, so the byte counters kept above it see it: the delta
	// MarkLoss returned (once per marker left, only when MarkLoss succeeded),
	// and the ones a lift or a failed window made. Nil discards the deltas.
	// Lose calls it on the caller's goroutine holding none of the layer's
	// locks and no file append domain; a lift or a failed window calls it with
	// the changed file's append domain held. It must therefore not wait on a
	// Sync or take a file's append domain.
	Account func(index.HeldDelta)

	// Undo, when set, undoes in memory one header-only record a read-only
	// journal fails: a truncate, deallocate, delete, release or any other kind
	// that is neither content nor a loss record. It reverses what the record's
	// publish changed in the index and returns that change as a held delta,
	// which the layer hands to Account. It runs on the record's stream worker
	// with the record's file append domain held, so it must not wait on a
	// Sync or take a file's append domain. Nil undoes nothing.
	//
	// A file's records are failed newest first, but a window that failed
	// before the journal turned read-only has already dropped its content and
	// kept its header-only records, so a header-only record can be undone
	// after an older write of its file was dropped. Undo must therefore bring
	// back only what the record's own publish removed and is still owed, never
	// a piece of a record the index no longer holds. A record that names no
	// file (a hold, unhold, stamp, offloaded, unmark or forget record) is
	// undone under the zero file's domain, a placeholder that excludes nothing
	// its publish holds, so its undo can race its own publish; see the
	// decision at failFile.
	Undo func(w segment.Written) index.HeldDelta

	// Check runs the checks open makes of the journal's mount and directory.
	// The read-only probe calls it every core.ProbeInterval, from its own
	// goroutine with no lock held. Nil means no check runs before the
	// probe's spare, which is only for a journal whose open makes none; a
	// real journal must always pass one (see the decision at probeOnce).
	Check func() error

	// MakeSpare makes one spare segment and keeps it for the reopen. The probe
	// calls it only while no spare is held and the check passes, from its own
	// goroutine with no lock held. An EROFS it returns reaches Enter only if
	// the maker's storage is wrapped by Observe. Nil means the probe can make
	// no spare.
	//
	// The maker it calls must not consult the read-only check the pool's
	// refill consults: that check refuses while the journal is read-only, and
	// the probe's spare is the one write a read-only journal wants, so a
	// maker gated on it would never make one. A failed MakeSpare must leave
	// nothing on disk: the probe tries again at the next tick after any
	// failure but an EROFS, and a failure that left a file behind would pile
	// up spares where the probe holds at most one.
	MakeSpare func() error
}

// SyncDurationBounds are the upper bounds of the sync duration histogram's
// buckets; the last bucket counts everything above the last bound.
var SyncDurationBounds = []time.Duration{
	time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second,
}

// Histogram is a copy of a duration histogram: Counts has len(Bounds)+1
// buckets, Counts[i] counting the observations at or below Bounds[i].
type Histogram struct {
	Bounds []time.Duration
	Counts []uint64
	Sum    time.Duration
}

// Part is the layer's report, read from counters kept at each sync.
type Part struct {
	Syncs, SyncFailures uint64
	SyncDuration        Histogram
	// LastSyncError is the most recent sync failure across the streams, nil
	// when none has failed.
	LastSyncError *SyncFailure

	// ReadOnly is the read-only state, read from the layer's atomic snapshot.
	ReadOnly core.ReadOnlyState

	// Losses are the events in the ring, oldest first.
	Losses []core.LossEvent

	// LossGeneration is the directory's loss generation, read after the ring
	// is copied so it includes the raise of every event in Losses.
	LossGeneration uint64

	// CorruptDataLoss counts the corrupt-record losses whose data was not
	// offloaded, so nothing remains to repair from.
	CorruptDataLoss uint64
	// CorruptRepairable counts the corrupt-record losses whose data was
	// offloaded.
	CorruptRepairable uint64
	// StaleExtents counts the losses of the stale reason.
	StaleExtents uint64

	// DamagedSegments counts the segments that held a record which did not
	// verify when it was read. A segment stays counted, with its row in
	// Segments, until the journal closes.
	DamagedSegments uint64
	// Segments are the damaged segments' rows, in segment order.
	Segments []DamagedRow
}

// Durability is the ledger and the workers of every stream.
type Durability struct {
	clock   core.Clock
	domains Domains
	tracer  core.Tracer
	log     *slog.Logger

	index *index.Index
	// undo is Config.Undo: the in-memory undo of a header-only record a
	// read-only journal fails.
	undo func(segment.Written) index.HeldDelta

	// windowSeq numbers the windows the workers take, so a window is named
	// across the streams.
	windowSeq atomic.Uint64

	dir      *dirState
	counters *index.Counters
	markLoss func(core.FileID, core.Extent, core.Version) (index.HeldDelta, error)
	account  func(index.HeldDelta)

	// streams are the append streams a loss record is written through, bound
	// by Start. It is nil before Start, which Lose reports as an error.
	streams atomic.Pointer[segment.Streams]

	// workers are the started streams, stored once by Start, so Enter can
	// wake every worker without taking mu.
	workers atomic.Pointer[[]*stream]

	// refuse reports why the journal takes no append, or nil. It is never
	// nil: New installs Admit, which refuses once the journal is read-only.
	refuse func() error

	// ro is the read-only state, nil while the journal takes writes. Each
	// value is immutable and replaced whole, so reads take no lock.
	ro atomic.Pointer[roState]

	// afterAppend, afterPass, beforeWait, beforeSync, beforeFail and afterFail
	// are test hooks, nil in production. beforeFail runs on the worker once per
	// window, before its records are failed, and afterFail once they are
	// failed and reported, before the window is resolved; afterPass runs at
	// the end of each worker pass, once the pass has done everything it will
	// do for the wake that started it; beforeWait runs in a caller's await,
	// on its work, each time it is about to park, after its checks found
	// something still pending. None is called with a lock held.
	afterAppend func()
	afterPass   func()
	beforeWait  func([]work)
	beforeFail  func(*window)
	afterFail   func()
	beforeSync  func()

	// afterCorruptDrop is a test hook, nil in production: Corrupt runs it
	// under the file's domain on what the index dropped, before it reads the
	// dropped pieces.
	afterCorruptDrop func(*index.FailedWindow)

	// beforeTakeWindow is a test hook, nil in production: the worker runs it
	// with no lock held before it takes each queued end event's window.
	beforeTakeWindow func()

	// lmu guards the loss ring, the counters and pieces, and the generation
	// read and raise that go with them.
	//
	// decision: lmu is a leaf mutex over the ring, the counters, the pieces
	// set and the generation read and raise made with them, so a snapshot
	// pairs a generation with exactly the events that raised it. The
	// directory registry's own mutex is taken inside it and calls nothing
	// back. It is never held across an Append, a Sync or a MarkLoss, which
	// take locks of other domains, so it adds no edge to the journal's lock
	// order. A window raise (RaiseWindow) takes the registry alone and raises
	// with no event, so a snapshot may show it ahead of the events.
	lmu               sync.Mutex
	ring              [core.LossRingSize]core.LossEvent
	head              int // index of the oldest event
	count             int
	nextEvent         uint64
	corruptDataLoss   uint64
	corruptRepairable uint64
	stale             uint64

	// dmu guards damaged.
	//
	// decision: dmu is a leaf mutex over the damaged rows, held only for a map
	// access and never across an index call, an Append or any other lock, so
	// Stats reads the damage without a lock a write needs.
	dmu     sync.Mutex
	damaged map[core.SegmentID]*damagedRow

	// copyMu guards copies, the table of other copies of re-appended records.
	// It is a leaf mutex, held only for a map access.
	copyMu sync.Mutex
	copies map[core.Seq]copyEntry

	// readCopyAt, onHoldCopy and onUnholdCopy are test hooks, nil in
	// production. readCopyAt replaces the stream's read of a copy; onHoldCopy
	// and onUnholdCopy run, under the file's domain, before each call of
	// holdCopy and unholdCopy.
	readCopyAt   func(seg core.SegmentID, p []byte, off int64) (int, error)
	onHoldCopy   func(ownedCopy)
	onUnholdCopy func(ownedCopy)

	// pieces holds every (dropped seq, extent) a Lose or a Place has taken,
	// so one piece of a dropped record is reported once, while another piece
	// of the same record, which a reconcile can drop alone, is reported on
	// its own.
	//
	// ponytail: it grows by one entry per piece lost for the life of the
	// Durability and is never pruned, so it follows the number of loss
	// records the journal holds, not the ring. Upgrade to dropping an entry
	// once no copy of its dropped record remains to be reported again, when
	// a journal with a long loss history shows up in a heap profile.
	pieces map[piece]struct{}

	// resolved is Config.Resolved: the announcement of a resolved window.
	resolved func(stream uint32, seg core.SegmentID)

	// readOnly is the journal's read-only entry, handed the failure that
	// stopped a stream. New installs Enter; it is replaceable before Start.
	//
	// decision: it is reached only through an EndEvent carrying Stopped, but
	// the stream also stops without an event, for example on a failed
	// successor header write after a failed sync (or one carrying EROFS on
	// any roll), whose stop travels through later appends' ErrStreamStopped;
	// on End of an already ended segment with EROFS; and on a straggler
	// write's EROFS after the segment's event was handed over. So a stream
	// can stop with readOnly uncalled, as when segment 1's sync fails and
	// then the spare's header write fails on the roll; unless Observe saw an
	// EROFS on the way, Admit then keeps answering nil while the stream's
	// appends fail with ErrStreamStopped. Removed by a stop
	// notification from the stream stack, raised on every stop whatever path
	// it took and routed to readOnly with the stopping error, after which a
	// stopped stream always reaches it.
	readOnly func(cause error)

	// mu guards made, started and stopped. It is not held across a stream
	// call, except that Start registers the layer's end consumer under it:
	// nothing takes mu below a stream's own locks, so that cannot deadlock.
	mu      sync.Mutex
	made    map[uint32]*stream
	started bool
	stopped bool

	// afterSync, when set by a test, runs in the worker after a stream sync
	// returns and before the worker takes the stream's lock.
	afterSync func()

	// cmu guards change, a layer-wide channel closed and replaced on every
	// stream change, so a waiter on several streams wakes on any of them. It
	// is a leaf lock, taken under a stream's mu.
	//
	// ponytail: one channel serves every stream, so a report on any stream
	// wakes every waiter of every stream and each re-scans its ledger targets.
	// The cost grows with waiters times reports, and is paid whether or not
	// the waiter's stream changed. Upgrade to a channel per stream, which a
	// waiter on several streams then selects over, when a profile of many
	// concurrent Sync callers shows the wake-ups.
	cmu    sync.Mutex
	change chan struct{}

	// check and makeSpare are Config.Check and Config.MakeSpare.
	check     func() error
	makeSpare func() error

	// probe is the state of the read-only probe goroutine: probeIdle until
	// Enter starts it, probeRunning while it runs and probeStopped once Stop
	// has run. A single compare-and-swap decides between the first Enter and
	// Stop, so no probe starts after Stop and Stop joins the one that did.
	probe     atomic.Int32
	probeDone chan struct{}

	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// New returns a Durability. A nil Clock, Domains, Index, Counters or MarkLoss is a
// caller bug and panics; the error is that of resolving Dir.
func New(cfg Config) (*Durability, error) {
	if cfg.Clock == nil {
		panic("durability: the layer needs its clock")
	}
	if cfg.Domains == nil {
		panic("durability: the layer needs the file append domains")
	}
	if cfg.Index == nil {
		panic("durability: the layer needs the index it lifts through")
	}
	if cfg.Counters == nil || cfg.MarkLoss == nil {
		panic("durability: the layer needs its counters and loss marker")
	}
	dir, err := stateFor(cfg.Dir)
	if err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &Durability{
		clock:    cfg.Clock,
		domains:  cfg.Domains,
		tracer:   cfg.Tracer,
		log:      log,
		resolved: cfg.Resolved,
		index:    cfg.Index,
		undo:     cfg.Undo,
		made:     map[uint32]*stream{},
		done:     make(chan struct{}),
		change:   make(chan struct{}),

		check:     cfg.Check,
		makeSpare: cfg.MakeSpare,
		probeDone: make(chan struct{}),

		dir:      dir,
		counters: cfg.Counters,
		markLoss: cfg.MarkLoss,
		account:  cfg.Account,
		pieces:   map[piece]struct{}{},
	}
	d.refuse = d.Admit
	d.readOnly = d.Enter
	return d, nil
}

// changes returns the channel the next stream change closes.
func (d *Durability) changes() <-chan struct{} {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	return d.change
}

// notifyAll wakes every waiter of every stream. It is called with a stream's
// mu held, which is safe since cmu is a leaf.
func (d *Durability) notifyAll() {
	d.cmu.Lock()
	close(d.change)
	d.change = make(chan struct{})
	d.cmu.Unlock()
}

// entry is one unsynced record of a stream's ledger.
type entry struct {
	w segment.Written
	// seen is when the worker first observed the record, on the clock.
	seen    time.Duration
	seenSet bool
	// done is set once the worker has completed the entry under the file's
	// append domain.
	done bool
	// tracked is the entry's place in the stream's Track order, which a
	// window keeps when it takes the entry, so a read-only journal undoes a
	// file's records newest first across windows and segments. The worker
	// also finds the entry again by it after Corrupt took an earlier entry
	// out of the ledger.
	tracked uint64
}

// stream is one stream's state.
//
// decision: mu is a leaf mutex, never held across I/O, a sync, the clock, a
// file domain or another lock, because both sinks run under the
// segment's append domain and must not wait on anything.
type stream struct {
	d  *Durability
	no uint32
	s  *segment.Stream

	mu sync.Mutex
	// trackedN counts the records the stream tracked; each entry carries its
	// own count.
	trackedN uint64
	// ledger holds the entries in Track order. Track only appends, the
	// worker compacts, and Corrupt takes out an entry no sync covered, so an
	// entry's index can move down between two steps of the worker, which finds
	// it again by its count.
	//
	// ponytail: one entry per unsynced record, bounded by what a stream
	// accepts within one sync bound. Durable scans it linearly. Upgrade
	// to a per-file index when a profile shows the scan.
	ledger []entry
	// synced is the largest offset a sync reported, per segment.
	synced map[core.SegmentID]int64
	// failed holds the segments whose sync failed.
	failed map[core.SegmentID]error
	// ending holds the segments a sync found already ended whose end event
	// has not been taken yet: no waiter leads a sync for them and the timer
	// skips them.
	ending map[core.SegmentID]struct{}

	// counted holds the segments whose failed sync the worker counted and
	// whose end event has not been taken yet. failed alone cannot say so: the
	// completion pass drops a failure nothing names (a segment's only window
	// comes from its end event, so no resolved window can precede it), and
	// a seal's EndSeal for that segment arriving after would count the one
	// fsync again. takeWindow removes the mark with the event, the one every
	// ended segment gets.
	counted map[core.SegmentID]struct{}
	// ends are the end events the stream's consumer queued for the worker.
	ends []segment.EndEvent
	// windows are the pending windows, in end order.
	windows []*window
	// lost holds the locations of the records a failed window dropped from the
	// index, or a read-only journal failed, so that none reads durable once
	// the window no longer names it, and a Sync that targeted one still
	// answers the failure.
	//
	// ponytail: one entry per failed record for the life of the stream, never
	// pruned, so it follows the number of records a failure took. Upgrade to
	// dropping an entry once the record's segment is retired from the journal,
	// when a stream with a long failure history shows up in a heap profile.
	lost map[segment.Location]lostRecord
	// marks are the failures no Sync has reported yet, per file, oldest
	// first; told holds, per pending window, the files already told of it.
	//
	// ponytail: a file that lost a record and is never named by a Sync again
	// keeps its mark for the life of the stream, so marks follow the number of
	// such files. Upgrade to dropping a file's marks when the file is removed,
	// when a stream with many abandoned files shows up in a heap profile.
	marks map[core.FileID][]mark
	told  map[uint64]map[core.FileID]struct{}
	// undone holds, per file, the read-only refusal of a header-only record
	// of the file that the read-only journal undid. Such a record can never
	// become durable, so every Sync naming the file returns the refusal,
	// bare, for as long as the layer lives, rather than once through a mark.
	//
	// ponytail: one entry per file with an undone header-only record for the
	// life of the Durability, never freed; the journal leaves read-only only
	// by being closed and reopened, which drops the layer. Upgrade to
	// dropping a file's entry once its removal is retried through a reopened
	// journal or the file is forgotten, when a read-only journal that stays
	// open over many removed files shows up in a heap profile.
	undone map[core.FileID]error
	// lastSeg is the newest segment the stream is known to have reached: the
	// segment of a tracked record or of a sync, whichever is later.
	lastSeg  core.SegmentID
	haveLast bool
	closed   bool
	// want is set by the waiter that leads a sync and cleared by the worker
	// as it starts one; syncing is set while the worker is inside the
	// stream's Sync.
	want    bool
	syncing bool

	// corrupt are the corrupt records' losses Corrupt handed to the worker.
	corrupt []corruptJob
	// owed holds, per failed segment whose window is not taken yet, the files
	// Corrupt took a record of out of the ledger: the window takeWindow makes
	// for the segment marks them, so they are told as if it had failed them.
	owed map[core.SegmentID][]core.FileID

	// kick wakes the worker; capacity 1, sent without blocking.
	kick chan struct{}

	// reported is set by the Synced sink and cleared by the worker's
	// completion pass, so a pass with no report since the last has nothing
	// to complete. stamped counts the ledger's leading entries the worker
	// has stamped; entries are stamped in Track order.
	reported bool
	stamped  int

	// The counters have one writer, the worker, and are read by Stats
	// without mu, so a poll never waits on a lock an append takes.
	syncs, failures atomic.Uint64
	// lastFail is the last sync failure, stored by the worker and read by
	// Stats without mu.
	lastFail atomic.Pointer[failStamp]
	buckets  []atomic.Uint64
	sum      atomic.Int64

	// scratch and live are the worker's own, reused by each pass: the Track
	// counts of the entries it completes and the segments that still hold an
	// entry.
	scratch []uint64
	live    map[core.SegmentID]struct{}
}

func newStream(d *Durability, no uint32) *stream {
	return &stream{
		d:       d,
		no:      no,
		live:    map[core.SegmentID]struct{}{},
		synced:  map[core.SegmentID]int64{},
		failed:  map[core.SegmentID]error{},
		ending:  map[core.SegmentID]struct{}{},
		counted: map[core.SegmentID]struct{}{},
		lost:    map[segment.Location]lostRecord{},
		kick:    make(chan struct{}, 1),
		buckets: make([]atomic.Uint64, len(SyncDurationBounds)+1),
	}
}

// wake asks the worker to run a pass, without blocking.
func (st *stream) wake() {
	select {
	case st.kick <- struct{}{}:
	default:
	}
}

// covered reports whether a sync has reached the end of the record. The caller
// holds mu.
func (st *stream) covered(loc segment.Location) bool {
	return st.synced[loc.Segment] >= loc.Off+loc.Len
}

// Sink returns stream no's sink; pass d.Sink to segment.NewStreams. Calling it
// twice for one number panics.
func (d *Durability) Sink(no uint32) segment.Sink {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.made[no]; dup {
		panic("durability: a second sink was asked for one stream")
	}
	st := newStream(d, no)
	d.made[no] = st
	return segment.Sink{
		Track: func(w segment.Written) {
			st.mu.Lock()
			st.trackedN++
			st.ledger = append(st.ledger, entry{w: w, tracked: st.trackedN})
			st.lastSeg, st.haveLast = w.Loc.Segment, true
			// A record a sync already reached, reported before the sink saw
			// it, still needs a completion pass.
			if st.covered(w.Loc) {
				st.reported = true
			}
			st.mu.Unlock()
			st.wake()
		},
		Synced: func(seg core.SegmentID, through int64) {
			// A sync reported once the journal is read-only moves nothing,
			// even one in flight at the transition that succeeded: every
			// record it covered that was not completed is failed, and no
			// Sync of one may return nil. The state is read under mu, the
			// mutex failReadOnly moves the ledger under, so the read and the
			// apply are one step to the failure pass. A report that reads
			// nil applies before any pass of the entry moves a record (a
			// pass runs only once Admit refuses, and takes mu first), so
			// every record it covers is still in the ledger, covered, and
			// is completed rather than failed, which is right for a sync
			// that landed before the entry. One that reads the refusal
			// moves nothing. Admit is one atomic load, so mu stays a leaf.
			st.mu.Lock()
			if d.Admit() != nil {
				st.mu.Unlock()
				return
			}
			if through > st.synced[seg] {
				st.synced[seg] = through
			}
			st.reported = true
			// A waiter is released here without waiting for the worker: a
			// seal's sync covers records no worker sync of its own follows.
			d.notifyAll()
			st.mu.Unlock()
			st.wake()
		},
	}
}

// Start binds the streams, whose count must equal the number of sinks made,
// registers the layer for their end events and launches one worker per stream.
// It is idempotent and a no-op after Stop. It must run before any stream's
// first append: the stream panics on a consumer registered after one.
func (d *Durability) Start(streams *segment.Streams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.stopped {
		return
	}
	if streams.Len() != len(d.made) {
		panic("durability: the streams do not match the sinks made")
	}
	started := make([]*stream, streams.Len())
	for i := range started {
		st, ok := d.made[uint32(i)]
		if !ok {
			panic("durability: no sink was made for a stream")
		}
		st.s = streams.Stream(i)
		started[i] = st
	}
	d.started = true
	d.streams.Store(streams)
	d.workers.Store(&started)
	streams.OnEnd(d.onEnd)
	for _, st := range started {
		d.wg.Add(1)
		go d.run(st)
	}
	// An Enter that ran before the workers were stored woke none of them; the
	// state is read after the store, so one of the two wakes them.
	if d.Admit() != nil {
		for _, st := range started {
			st.wake()
		}
	}
}

// Durable reports whether the record at the location is durable: its segment
// has been synced through the record's end. A location in a pending window
// of a failed segment reports false, however far a sync reached.
//
// decision: a location with no ledger entry and in no window reports true. A
// caller holding a location Append returned has a record that was tracked and,
// if it is gone from the ledger, already completed, so true is right for every
// location the contract lets a caller hold. It fails open for a location
// Append never returned, which reads durable although nothing was written
// there. Overturn this if a caller can ask about an arbitrary location, which
// then needs a record of the completed ones to tell the two apart.
//
// A record a failed window dropped has no entry and no window once the window
// is resolved, so the stream keeps its location apart and reports false for it.
//
// ponytail: it scans every stream's ledger, O(streams x ledger). Upgrade to a
// per-segment index when a profile shows the scan.
func (d *Durability) Durable(at segment.Location) bool {
	made := d.snapshot()
	for _, st := range made {
		st.mu.Lock()
		for i := range st.ledger {
			if st.ledger[i].w.Loc == at {
				ok := st.covered(at)
				st.mu.Unlock()
				return ok
			}
		}
		// A record of a pending window is never durable, whatever the
		// segment's synced offset says.
		_, lost := st.lost[at]
		if lost || st.inWindowLocked(at) {
			st.mu.Unlock()
			return false
		}
		st.mu.Unlock()
	}
	return true
}

// SyncBound returns the longest a record waits for a sync.
func (d *Durability) SyncBound() time.Duration { return core.SyncBound }

// Stats returns the counters kept at each sync, without walking the ledger. A
// sync is counted just after the waiters it covered are released, so a Sync
// that has just returned may not be in the counters yet.
func (d *Durability) Stats() Part {
	made := d.snapshot()

	// The read-only state is one atomic load: no lock a write holds is taken.
	p := Part{ReadOnly: d.ReadOnlyState(), SyncDuration: Histogram{
		Bounds: append([]time.Duration(nil), SyncDurationBounds...),
		Counts: make([]uint64, len(SyncDurationBounds)+1),
	}}
	var last *failStamp
	for _, st := range made {
		if f := st.lastFail.Load(); f != nil && (last == nil || f.mono > last.mono) {
			last = f
		}
		p.Syncs += st.syncs.Load()
		p.SyncFailures += st.failures.Load()
		p.SyncDuration.Sum += time.Duration(st.sum.Load())
		for i := range st.buckets {
			p.SyncDuration.Counts[i] += st.buckets[i].Load()
		}
	}
	if last != nil {
		sf := last.SyncFailure
		p.LastSyncError = &sf
	}

	d.lmu.Lock()
	p.Losses = make([]core.LossEvent, d.count)
	for i := range p.Losses {
		p.Losses[i] = d.ring[(d.head+i)%core.LossRingSize]
	}
	p.CorruptDataLoss = d.corruptDataLoss
	p.CorruptRepairable = d.corruptRepairable
	p.StaleExtents = d.stale
	// The generation is read in the same section as the ring: Lose raises it
	// and publishes in one section, so the snapshot never holds the raise
	// without its event, nor an event without its raise.
	p.LossGeneration = d.dir.load()
	d.lmu.Unlock()
	p.DamagedSegments, p.Segments = d.damagedReport()
	return p
}

// Stop stops the workers and joins them, then records the corrupt losses
// still queued, so a loss handed over before Stop has its record appended
// when Stop returns. A second call is a no-op.
func (d *Durability) Stop() {
	d.stop.Do(func() {
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		made := d.snapshot()
		close(d.done)
		d.stopProbe()
		for _, st := range made {
			st.mu.Lock()
			st.closed = true
			d.notifyAll()
			st.mu.Unlock()
		}
		// Every Corrupt that queued before closed was set has its loss in a
		// queue now, and every later one records its loss itself.
		d.wg.Wait()
		for _, st := range made {
			d.drainCorrupt(st)
		}
	})
	d.wg.Wait()
}

// target is the last unsynced record of one file in one segment of a stream.
type target struct {
	file core.FileID
	loc  segment.Location
}

// work is a caller's targets on one stream.
type work struct {
	st      *stream
	ids     []core.FileID
	targets []target
}

// pending reports whether the record at loc is in a pending window, or still in
// the ledger and not completed: no sync covers it, or one does and the worker
// has not yet finished with it, which lifts what it superseded. A record gone
// from both was covered and completed, so the answer does not depend on the
// segment's synced offset, which the worker forgets once a segment has no
// entries. The caller holds mu.
//
// ponytail: a linear scan, O(ledger) per target. Upgrade to a per-file index
// when a profile shows the scan.
func (st *stream) pending(loc segment.Location) bool {
	for i := len(st.ledger) - 1; i >= 0; i-- {
		if st.ledger[i].w.Loc == loc {
			return !st.covered(loc) || !st.ledger[i].done
		}
	}
	// A record of a pending window, or one a failed window dropped, is pending
	// whatever the segment's synced offset: no sync value satisfies a record of
	// a failed segment.
	_, lost := st.lost[loc]
	return lost || st.inWindowLocked(loc)
}

// inWindowLocked reports whether a pending window holds the record at loc. The
// caller holds mu.
func (st *stream) inWindowLocked(loc segment.Location) bool {
	for _, w := range st.windows {
		for i := range w.entries {
			if w.entries[i].w.Loc == loc {
				return true
			}
		}
	}
	return false
}

// Sync returns once every record of the named files appended before the call
// is durable on its stream, or the first failure, whichever comes first. A
// file with nothing unsynced needs nothing, so a call naming only such files
// returns at once with no sync. The error is a *core.SyncError when a sync
// covering a record failed, core.ErrClosed once the layer stopped, and
// ctx.Err() when ctx is done first; a cancelled caller changes no state and
// the sync it may have led runs on for the other waiters. Once the journal is
// read-only, a failure matches core.ErrJournalReadOnly too: a content record
// it failed is reported as a failed window is, once, a *core.SyncError
// wrapping the refusal. A file with a header-only record it undid is answered
// differently: every Sync naming the file, or waiting on the record, returns
// the refusal bare, never a *core.SyncError, for as long as the layer lives,
// since the record can never become durable and a caller that routed it as a
// failed window would retry what the journal undid. That answer stands in for
// any read-only failure the call is owed; only a failure of a window that
// failed before the entry is kept beside it, joined, and that join alone
// matches both errors.As to *core.SyncError and core.ErrJournalReadOnly.
//
// A failed window is reported once per file: the first Sync naming a file
// that lost a record in it returns the *core.SyncError and clears the file's
// mark, and a Sync that was waiting on the lost record when another took that
// report returns nil, though its record is lost and not durable.
//
// A covered record counts as synced only once the worker has lifted what it
// superseded, which it does under the file's append domain, so the caller
// holds the append domain of none of the files it names: it would wait on the
// worker while the worker waits on it, and every other file of the stream
// would stop completing too.
//
// Syncs are issued by the stream's worker, never by the caller's goroutine,
// so a cancelled caller is not stuck in the syscall.
func (d *Durability) Sync(ctx context.Context, ids ...core.FileID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-d.done:
		return core.ErrClosed
	default:
	}
	// Every stream is consulted, also one with no target of the call: a failed
	// window leaves the report for a file it lost a record of, and the record
	// is no target by then.
	var ws []work
	for _, st := range d.snapshot() {
		ws = append(ws, work{st: st, ids: ids, targets: st.targetsOf(ids)})
	}
	return d.await(ctx, ws, true)
}

// WaitDurable returns nil once the record at the location is durable,
// core.ErrClosed once the layer stopped, or, once the journal is read-only and
// the record is failed, the refusal, which matches core.ErrJournalReadOnly. It
// takes no context and never returns a *core.SyncError: while the journal
// takes writes, a record of a segment whose sync failed keeps waiting, since
// following a re-append is not part of this layer yet, until Stop or until the
// journal turns read-only, which wakes every such waiter. It consults and
// clears no marks, which belong to the Sync that names the file. A record with
// no sync running for it gets one, as a Sync would. The caller holds no file's
// append domain, as for Sync.
func (d *Durability) WaitDurable(at segment.Location) error {
	var ws []work
	for _, st := range d.snapshot() {
		st.mu.Lock()
		ok := st.pending(at)
		st.mu.Unlock()
		if ok {
			ws = append(ws, work{st: st, targets: []target{{loc: at}}})
		}
	}
	return d.await(context.Background(), ws, false)
}

func (d *Durability) snapshot() []*stream {
	d.mu.Lock()
	defer d.mu.Unlock()
	made := make([]*stream, 0, len(d.made))
	for _, st := range d.made {
		made = append(made, st)
	}
	return made
}

// targetsOf returns, for each id, the last record of that file in each
// segment of the ledger that is not completed, and in each pending window. A
// record a sync covers stays a target until the worker has lifted what it
// superseded, so a Sync issued in between waits for the lift too. A sync
// covers a prefix of one segment, so covering a file's last record says
// nothing of its records in an older segment; each segment gets its own target
// and waits for its seal or fails through the failed map. Compaction removes
// only completed entries, so a target stays in the ledger until the worker is
// done with it.
func (st *stream) targetsOf(ids []core.FileID) []target {
	st.mu.Lock()
	defer st.mu.Unlock()
	var ts []target
	consider := func(e *entry) {
		for _, id := range ids {
			if e.w.File != id {
				continue
			}
			dup := false
			for _, t := range ts {
				dup = dup || (t.file == id && t.loc.Segment == e.w.Loc.Segment)
			}
			if !dup {
				ts = append(ts, target{file: id, loc: e.w.Loc})
			}
		}
	}
	for i := len(st.ledger) - 1; i >= 0; i-- {
		if e := &st.ledger[i]; !e.done {
			consider(e)
		}
	}
	// A pending window's records are targets whatever the segment's synced
	// offset. A content record is answered from the mark its failure leaves; a
	// header-only one, which no failure takes, gets the SyncError once its
	// window has been failed.
	for j := len(st.windows) - 1; j >= 0; j-- {
		w := st.windows[j]
		for i := len(w.entries) - 1; i >= 0; i-- {
			consider(&w.entries[i])
		}
	}
	return ts
}

// await waits until no target of any stream is pending. With failFast, the
// failures owed to the call (its files' marks, and the answer of a header-only
// record of a failed window) end the wait once no failure is still in flight
// on any stream, and the call returns them as one error. Once the journal is
// read-only, a target it failed ends the wait the same way, with or without
// failFast: its refusal is the answer, joined with any failure the call is
// owed that does not already carry it.
//
// decision: a failure found on a stream does not end the wait while any stream
// of the call, that one included, still has a target in a failed segment whose
// failure the worker has not finished handling, so one Sync names every file
// the call lost, whatever the layout of the files on streams. A stream whose
// targets are merely waiting for a sync that has not failed does not hold the
// answer back: a failure is returned as soon as no failure is in flight, and
// one the call could not know of is told, mark and all, to the file's next
// Sync. The failures taken so far are never dropped: a call that ends early,
// because the layer stopped or the context is done, returns them in place of
// that error, since their marks are cleared and nothing else would report
// them. Withdraw the wait if a failed window can stay unhandled indefinitely,
// since a Sync would then hang on it rather than return the failure it holds.
func (d *Durability) await(ctx context.Context, ws []work, failFast bool) error {
	var owed *owedFailure
	var refused error
	// answer joins what the call is owed: the failures it took and the
	// read-only refusal of a record the read-only journal undid. The refusal
	// is returned bare, never as a *core.SyncError, so a caller cannot route
	// it as a failed window and retry what the journal undid: a failure the
	// read-only journal reported matches the refusal already and is dropped
	// for it. Only a failure of a window that failed before the journal
	// turned read-only is kept, joined with the refusal; that answer matches
	// both errors.As to *core.SyncError and errors.Is to
	// core.ErrJournalReadOnly.
	answer := func() error {
		switch {
		case owed != nil && refused != nil && !errors.Is(owed.err, core.ErrJournalReadOnly):
			return errors.Join(owed.err, refused)
		case refused != nil:
			return refused
		case owed != nil:
			return owed.err
		default:
			return nil
		}
	}
	// early is the answer for a call that cannot wait any longer.
	early := func(err error) error {
		if owed != nil || refused != nil {
			return answer()
		}
		return err
	}
	for {
		// Taken before the checks, so a change after one is never missed, and
		// shared by every stream so a change on any of them wakes the caller.
		wait := d.changes()
		pending, failing := false, false
		for _, w := range ws {
			p, f, found, err := w.st.check(w.ids, w.targets, failFast)
			if found != nil {
				// Every stream owes the call its marks, and a mark left on one
				// stream would be told to the file's next Sync again: the rest
				// are taken before returning, each under its own stream's
				// mutex, and one error names them.
				owed = mergeOwed(owed, found)
			}
			if errors.Is(err, core.ErrJournalReadOnly) {
				// A record the read-only journal failed ends the wait too, and
				// the other streams still owe their marks.
				refused = err
				failing = failing || f
				continue
			}
			if err != nil {
				return early(err)
			}
			pending, failing = pending || p, failing || f
		}
		if (owed != nil || refused != nil) && !failing {
			return answer()
		}
		if !pending && owed == nil && refused == nil {
			return nil
		}
		if d.beforeWait != nil {
			d.beforeWait(ws)
		}
		select {
		case <-ctx.Done():
			return early(ctx.Err())
		case <-wait:
		case <-d.done:
			return early(core.ErrClosed)
		}
	}
}

// owedFailure is a failure one stream owes a Sync, with the id of the window
// it belongs to.
type owedFailure struct {
	err    *core.SyncError
	window uint64
}

// mergeOwed joins two failures of one Sync: the older window's segment and
// error, the files of both. Window ids are global, so the order of the
// streams the failures came from does not matter.
func mergeOwed(into, from *owedFailure) *owedFailure {
	if into == nil {
		return from
	}
	if from.window < into.window {
		into.err.Segment, into.err.Err, into.window = from.err.Segment, from.err.Err, from.window
	}
	for _, f := range from.err.Files {
		if !slices.Contains(into.err.Files, f) {
			into.err.Files = append(into.err.Files, f)
		}
	}
	return into
}

// answersFailureLocked returns the id of the window, and true, when the record
// at loc is a header-only record of a pending window that failWindow has run
// for: the only record that answers a Sync with its failure. A record still in the ledger, or in a
// window not yet failed, answers nothing, so that the loss generation is
// raised before any Sync the failure fails returns. The caller holds mu.
func (st *stream) answersFailureLocked(loc segment.Location) (uint64, bool) {
	for _, w := range st.windows {
		for i := range w.entries {
			if w.entries[i].w.Loc == loc {
				return w.id, w.failed && !isContent(w.entries[i].w)
			}
		}
	}
	return 0, false
}

// undoingLocked reports whether the stream holds a header-only record of one
// of ids, other than a loss record, that a read-only journal's failure pass
// will undo and has not yet: one in a pending window, or one in the ledger
// that no sync covered and the worker has not completed. The caller holds mu
// and has seen the journal read-only.
//
// ponytail: a linear scan of the ledger and the windows, run on each check of
// a Sync while the journal is read-only. Upgrade to a per-file count of
// header-only records when a profile of a read-only journal shows the scan.
func (st *stream) undoingLocked(ids []core.FileID) bool {
	undoable := func(e *entry) bool {
		return !isContent(e.w) && !isLoss(e.w) && slices.Contains(ids, e.w.File)
	}
	for i := range st.ledger {
		if e := &st.ledger[i]; !e.done && !st.covered(e.w.Loc) && undoable(e) {
			return true
		}
	}
	for _, w := range st.windows {
		for i := range w.entries {
			if undoable(&w.entries[i]) {
				return true
			}
		}
	}
	return false
}

// check looks at one stream's targets. With failFast it first takes the marks
// owed to ids, and still evaluates the targets afterwards. It reports whether
// any target is pending, whether a target sits in a failed segment whose
// failure is not yet answered (failing), and the failure owed to the call: the
// marks it took, joined with the answer of a header-only record whose window
// has been failed. It returns an error for a closed layer, and then only when
// it owes no failure, and the read-only refusal once a target is a record the
// read-only journal failed, or, with failFast, once a file of ids has a
// header-only record it undid, alongside whatever failure it owes. It also leads
// a sync when the targets need one.
//
// decision: one sync per stream runs at a time and covers every waiter (group
// commit). The waiter that finds none in flight and none asked for leads: it sets
// want and the worker issues the sync. A sync per waiter would multiply
// fsyncs by the waiter count. Joining whatever sync is running would be wrong
// as well: a record whose slot completed after that sync began would be told
// it is durable, so every waiter re-checks its own target once a sync ends
// and a target still uncovered leads the next one. The worker, not the
// waiter's goroutine, issues the sync so a cancelled waiter is never stuck in
// the syscall.
//
// decision: a content target in a pending window, or in a failed segment, is
// pending until the worker has failed it, and the Sync is answered from the
// mark the failure leaves for the file, not from the failed segment. The
// answer therefore follows the generation's raise and the record's loss, which
// a Sync of a failed record must not precede. A target the failure took is no
// target any more: a Sync that targeted it and finds no mark left returns, as
// another Sync of the file took the report, and a failure is told once.
//
// decision: a header-only target in a pending window answers its
// *core.SyncError once failWindow has run for that window, and not before:
// such a record is not failed, it stays in the window, and no mark is ever set
// for it, but the generation must be up before a Sync the failure fails
// returns, and the window's content records are failed by then, so a file
// with both is told by its marks first and never twice for one Sync. Until
// then the target waits, and counts as a failure in flight. Once answered it
// no longer does, for it stays in its window until the re-append and the call
// would never end; the call's other targets still decide whether a failure is
// in flight, so the answer is returned with them and not ahead of them.
// Overturned by the re-append of a header-only record, which must wait for the
// new sync instead of failing at once. Once the journal is read-only the
// record is undone and failed (failReadOnly), every wait on it then ends with
// the refusal, and so does every later Sync naming its file (st.undone); until
// the pass has undone it, it counts as a failure in flight for a Sync naming
// its file (undoingLocked), so no other failure is answered ahead of it.
func (st *stream) check(ids []core.FileID, targets []target, failFast bool) (pending, failing bool, found *owedFailure, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var owed *owedFailure
	if failFast {
		owed = st.takeMarksLocked(ids)
	}
	lead := false
	// A report owed for a segment whose window is not taken yet is waited for,
	// as a failure in flight: takeWindow turns it into a mark, which a later
	// check answers.
	if failFast && st.owesLocked(ids) {
		pending, failing = true, true
	}
	var failSeg core.SegmentID
	var failWindow uint64
	var failErr error
	var refused error
	if failFast {
		// A file with a header-only record the read-only journal undid is
		// refused on every Sync naming it: the record can never become
		// durable, and a Sync that returned nil would let its caller commit
		// what the journal undid.
		for _, id := range ids {
			if err, ok := st.undone[id]; ok {
				refused = err
				break
			}
		}
		// One the read-only journal has yet to undo holds the answer back
		// as a failure in flight, so a failure the call already took is not
		// returned ahead of the refusal it is about to owe.
		if refused == nil && st.d.Admit() != nil && st.undoingLocked(ids) {
			failing = true
		}
	}
	for _, t := range targets {
		if l, lost := st.lost[t.loc]; lost {
			if l.undone {
				// A header-only record the read-only journal undid fails every
				// Sync and WaitDurable that waits on it.
				refused = l.err
				continue
			}
			if !failFast {
				// A durability wait on a failed record ends with the
				// read-only refusal; while the journal takes writes it keeps
				// waiting, as the record is never durable.
				if err := st.d.Admit(); err != nil {
					refused = err
					continue
				}
				pending = true
			}
			// Failed and gone from its window: the mark told the file, so a
			// Sync has nothing more to wait for.
			continue
		}
		if !st.pending(t.loc) {
			continue
		}
		pending = true
		if st.covered(t.loc) && !st.inWindowLocked(t.loc) {
			// A sync covers it and the worker is lifting what it superseded:
			// the wait is for that, which another sync does not hasten.
			continue
		}
		if e, bad := st.failed[t.loc.Segment]; bad {
			// The segment failed, so the record's failure is on its way, from
			// a mark the worker has yet to set or an answer it has yet to
			// allow: the caller waits for it rather than for a sync.
			if id, ok := st.answersFailureLocked(t.loc); failFast && ok {
				// Answered: its window has been failed, so it is no failure
				// in flight. Only the first answer is reported.
				if failErr == nil {
					failSeg, failWindow, failErr = t.loc.Segment, id, e
				}
			} else {
				failing = true
			}
			continue
		}
		// A target in an older segment is waited on and never leads: the
		// seal of its segment syncs it and reports through the sink. One in a
		// segment a sync found ended never leads either: the segment takes
		// no sync, and its end event is on its way.
		if _, ended := st.ending[t.loc.Segment]; !ended && st.haveLast && t.loc.Segment == st.lastSeg {
			lead = true
		}
	}
	// answered is the failure owed to the call once the header-only answer
	// found, if any, is joined to the marks taken.
	answered := func() *owedFailure {
		if failErr == nil {
			return owed
		}
		se := &core.SyncError{Segment: failSeg, Err: failErr}
		for _, t := range targets {
			if t.loc.Segment == failSeg && st.pending(t.loc) {
				se.Files = append(se.Files, t.file)
			}
		}
		return mergeOwed(owed, &owedFailure{err: se, window: failWindow})
	}
	if refused != nil {
		// A target the read-only journal failed ends this stream's wait; a
		// failure still in flight on it holds the answer back as any other.
		return false, failing, answered(), refused
	}
	if !pending {
		return false, false, owed, nil
	}
	if st.closed {
		if owed != nil {
			return false, false, owed, nil
		}
		return false, false, nil, core.ErrClosed
	}
	if failErr != nil {
		return false, failing, answered(), nil
	}
	if lead && !st.syncing && !st.want {
		st.want = true
		st.wake()
	}
	return true, failing, owed, nil
}
