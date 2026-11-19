package datapath

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/unread"
)

// Deps is what a Core is built over: the production wiring only.
type Deps struct {
	FS          storage.FS
	Clock       core.Clock
	Tracer      core.Tracer
	Log         *slog.Logger
	Dir         string // the journal's directory, the key of its loss state
	Identity    core.JournalID
	Streams     int   // number of append streams, at least 1
	SegmentCap  int64 // size a segment is accounted at
	ExtentLimit int64 // the placement index's entry bound
	// LastSegment is the highest segment id the directory holds, the figure
	// the spare maker draws its first id above so a fresh spare never meets a
	// rebuilt segment's id. Zero keeps the drawing fresh — the answer of a
	// caller opening a directory with no segments, which the production open
	// is today; a caller that rebuilds a directory's state passes the highest
	// id its attach pass found, or the pool's spares collide with the rebuilt
	// ids and no spare readies.
	LastSegment core.SegmentID
	// MaxBytes is the journal's configured maximum local footprint, in
	// bytes: the whole segments WriteAt's admission reserves against and
	// the spare maker raises UsedBytes under. Zero keeps the maker
	// unbounded and admits by every other limit alone, which is the
	// behaviour of a core opened before the maximum was wired.
	MaxBytes int64
	// HeadroomRecords is the headroom reserved for records without bytes, in
	// records: the spares the pool readies for their rolls and the figure the
	// admission table draws on. Zero reserves no headroom spare and leaves no
	// headroom figure refusing a write — the behaviour of a core whose
	// wiring named none.
	HeadroomRecords int
	// Descriptors bounds the open segment descriptors; nil means a budget of
	// 64.
	Descriptors *core.DescriptorBudget
	Targets     segment.Targets
	// IdleSealBytes is the payload threshold beyond its last catalogued point
	// a stream's active segment must hold before the idle sealer seals it
	// while it idles. Zero builds no idle sealer, which is the behaviour of a
	// core whose wiring never named one; a negative threshold is a setting
	// bug the sealer's constructor panics on.
	IdleSealBytes int64
}

// State is the in-memory state a Core continues from. A zero State builds a
// fresh index, counters and segment set; a caller that rebuilt them from disk
// passes the rebuilt ones, with the Open payload the rebuild collected beside
// them.
//
// decision: State is plain data handed to New and holds no lock domain of its
// own; the index and counters it carries take their own short, I/O-free locks.
//
// decision: the dirty-file list, the journal-wide axes and the per-share
// figures are all seeded by the Open payload alone. The rebuild's own index
// deltas are discarded, so the open derives one delta per rebuilt file that
// holds or retains an extent — its whole held, dirty and retained byte
// figures, the became-dirty transition when the file holds content whose
// offloaded bit is unset, and the zero write time restored content carries —
// and the wiring folds each through Account, the path every running delta
// takes. A State that rebuilds the index while skipping the Open payload
// leaves its figures and dirty files at zero and unlisted; no
// journal-internal caller builds that shape, so the gap is the caller's own
// to close.
type State struct {
	Index    *index.Index
	Counters *index.Counters
	Set      *segment.Set
	// Open is the rebuild's collected payload: nil keeps a continued core
	// off the registration, the dirty seed and the worker start below — the
	// behaviour of a caller seeding the index and set by hand.
	Open *OpenState
}

// OpenState is what a rebuild hands the open beside the rebuilt index,
// counters and segment set: the liveness registrations of every record still
// live on disk — one Rec per attached header-only record and copy, forget
// records with the snapshot of the segments their tag's records held below
// them, and unsynced losses with the segments still holding the record each
// names — and the dirty-file seed, one became-dirty delta per rebuilt file
// that holds content whose offloaded bit is unset, with the zero write time
// restored content carries.
type OpenState struct {
	Live     []reclaim.Rec
	Forgets  []reclaim.ForgetSet
	Unsynced []reclaim.UnsyncedLoss
	// Dirty holds the rebuilt dirty files' seed: one became-dirty delta per
	// file, the whole content of the list the open starts the dirty list
	// from, so a file nothing seeded answers unlisted until a write of its
	// own lands.
	Dirty []index.HeldDelta
	// Spares holds the spares the open re-adopted, each with the class the
	// pool draws it from: the pool adopts them before it starts, so its
	// refill counts them and makes no spare in their place.
	Spares []AdoptedSpare
}

// AdoptedSpare is one spare the open re-adopted and the class it fills.
type AdoptedSpare struct {
	ID    core.SegmentID
	Class segment.Class
}

// coreOptions are the test-only adjustments newCore applies before the layers
// above the domain table are built.
type coreOptions struct {
	// wrapDomains replaces the file append domain table the core and the
	// durability layer both use.
	wrapDomains func(durability.Domains) durability.Domains
	// accountSpy is told every non-zero delta Account applied.
	accountSpy func(index.HeldDelta)
	// stepSpy runs in Account between its three counter updates, told how many
	// have been applied.
	stepSpy func(step int)
	// betweenHeldReads runs in Stats between its dirty and held reads.
	betweenHeldReads func()
	// tracer is the Tracer the core's Deps carry; nil means no tracing.
	tracer core.Tracer
	// state is the State newCore continues from; the zero State builds fresh.
	state State
	// afterAppend runs in WriteAt right after a record is appended and before
	// it is published, with the file's append domain held.
	afterAppend func()
	// beforeAppend runs in appendAndPublish right after the write's grant is
	// taken and before its first record is appended, with the file's append
	// domain held. It is the window a take's room grant and the append that
	// spends it are apart in.
	beforeAppend func()
	// beforeWrote runs in WriteAt after a record is appended and before the
	// share counts it, while the record is still reserved.
	beforeWrote func()
	// beforeResolve runs in ReadAt after the as-of is read and the file's
	// append domain released, before the span is resolved.
	beforeResolve func()
	// beforeRead runs in ReadAt after the span is resolved and the index
	// released, before the first record is read from storage.
	beforeRead func()
	// afterCorrupt runs in ReadAt and Since after Corrupt answered a record that did not
	// verify and before the read goes on from that answer.
	afterCorrupt func()
	// afterRetarget runs in Since after a record whose segment was retired was
	// sent to the record the index now names, before that record is read.
	afterRetarget func()
	// takeHook is installed on the admission's decision section before any
	// goroutine of the core starts; nil in production.
	takeHook func()
	// stopProbe is registered between the retirer's stop and the reclaim
	// worker's, so reverse order runs it once the pool, the idle sealer and
	// the reclaim worker have stopped and before the retirer stops; the stop
	// it returns is what Close runs and is given all three, so it can check
	// each is in the state its Stop doc names. Nil in production.
	stopProbe func(*segment.Pool, *segment.Retirer, *reclaim.Worker) func()
	// shareHook runs in Share between the first closed check and the
	// registry's lock, so a test can fence the core in the window the
	// re-check under registryMu refuses. Nil in production.
	shareHook func()
	// afterSwap runs in BeginClose inside the registry swap's critical
	// section, between the swap and the unlock, so a test can pin the closed
	// flag already up before the fence releases the mutex — the order every
	// attach racing the fence depends on. The hook must take no lock of its
	// own: it runs under registryMu. Nil in production.
	afterSwap func()
	// extentLimit overrides the index's entry bound a test core builds with,
	// keeping Deps at the figure the production wiring passes.
	extentLimit int64
	// maxBytes is the journal's maximum the core's admission consults, zero
	// meaning unbounded — the same reading Deps.MaxBytes itself carries.
	maxBytes int64
	// headroomRecords overrides the headroom the core's admission table
	// builds, keeping Deps at the figure it carries — zero meaning none, the
	// same reading Deps.HeadroomRecords itself carries.
	headroomRecords int
	// contentSpares overrides the pool's content target, so a test can hold a
	// reservation wider than the target's few spares; zero keeps the test
	// helper's own target.
	contentSpares int
	// segmentCap replaces the test helper's segment size, so a test can write
	// a record larger than the default segment holds.
	segmentCap int64
	// dir replaces the test helper's own temp directory, so a test can build
	// one core, close it and build the next over the same directory.
	dir string
	// noLimit keeps the test helper's generous version limit off the core, so
	// a test can hold a fresh core that issues nothing until it sets one.
	noLimit bool
	// clock replaces the helper's stopwatch clock, so a test can advance the
	// sync bound's timer.
	clock core.Clock
	// recorder is the lock-order recorder newCore installs on every lock the
	// core wires: the index's, the set's, the pool's, the streams', the
	// descriptor cache's, the admission's, the headroom's and the file
	// append table's. Nil, the production value, installs none and changes
	// no behaviour.
	recorder *lockorder.Recorder
	// wrapFS replaces the filesystem a test core is built over, wrapping
	// the real one: the wrapper must pass every call through and never
	// answer in the real filesystem's place.
	wrapFS func(storage.FS) storage.FS
}

// Core is the journal's assembled write and read paths: the placement index,
// the segment streams, the durability layer over them, and the counters that
// follow the index's held-state deltas.
//
// decision: a Core owns no lock that spans I/O. The one domain it uses is the
// file append domain, taken through its domain table and held across a file's
// append and publish, which includes I/O; its counters are atomics. Its two
// mutexes, the dirty-file list's and the per-share ledger's, are leaves, each
// held only for a map operation. Revisit
// if a core-wide invariant ever needs a mutex that outlives one.
type Core struct {
	index         *index.Index
	dirtyFiles    dirtyList
	shares        shareLedger
	clock         core.Clock     // the injected clock, read for the write time a dirty write carries
	identity      core.JournalID // the journal identity the records' headers verify against
	counters      *index.Counters
	set           *segment.Set
	pool          *segment.Pool
	cache         *segment.Descriptors
	streams       *segment.Streams
	dur           *durability.Durability
	domains       durability.Domains
	cap           *capacity.Capacity
	headroom      *capacity.Headroom
	maxBytes      int64               // the configured maximum local footprint, zero when unbounded
	segCap        int64               // the set's segment cap, what the streams roll at and the admission sizes by
	contentTarget int                 // the content spare target the pool was built with, for the test helper's wait
	retirer       *segment.Retirer    // the retirement executor
	liveness      *reclaim.Tracker    // the segment liveness the retirement worker asks
	retiring      *reclaim.Worker     // the retirement worker that turns liveness into retirements
	repack        *reclaim.Pass       // the repack pass, over the same pieces the worker serves
	openPayload   bool                // the set carried the open registration Continue asks the credential of
	unread        *unread.Set         // the records no read returned, kept for the per-segment rows
	sealer        *segment.IdleSealer // the idle sealer; nil when Deps.IdleSealBytes is zero

	held, dirty, retained atomic.Int64
	writtenBytes          atomic.Uint64
	filledBytes           atomic.Uint64
	fillRefused           atomic.Uint64
	repackedBytes         atomic.Uint64
	tracer                core.Tracer
	accountSpy            func(index.HeldDelta)

	// stepSpy is a test seam that stands between Account's counter updates, so
	// a test can read the counters with one applied and the next not.
	//
	// decision: it is nil outside tests, for the same reason afterAppend is.
	stepSpy func(step int)

	// betweenHeldReads is a test seam that stands between Stats' reads of the
	// dirty and held counters, so a test can land an update there.
	//
	// decision: it is nil outside tests, for the same reason stepSpy is.
	betweenHeldReads func()

	// rec is the recorder the core's locks report to, the one removals'
	// wait assertion consults. Nil in production, where every consult
	// returns at once.
	rec *lockorder.Recorder

	// afterAppend is a test seam that holds a write in the gap between its
	// append and its publish.
	//
	// decision: it is nil outside tests, where newCore is only reached through
	// New with no options. It is a field of the core and not of Deps so that no
	// production wiring can set it.
	afterAppend func()

	// beforeAppend is a test seam that holds a write in the gap between its
	// grant and its first append, the window a take's room grant is spent in.
	// It is nil outside tests, for the same reason afterAppend is.
	beforeAppend func()
	// beforeWrote is a test seam that runs in WriteAt after a record is in its
	// segment and before the share counts it, the window in which the record
	// is still reserved. It is nil outside tests, for the same reason
	// afterAppend is.
	beforeWrote func()

	// beforeResolve is a test seam that holds a read between reading its
	// as-of and resolving its span. It is nil outside tests, for the same
	// reason afterAppend is.
	beforeResolve func()

	// beforeRead is a test seam that holds a read between resolving its span
	// and reading its records. It is nil outside tests, for the same reason
	// afterAppend is.
	beforeRead func()

	// afterCorrupt is a test seam that holds a read or a Since between Corrupt's answer
	// and the read of a copy it named. It is nil outside tests, for the same
	// reason afterAppend is.
	afterCorrupt func()

	// afterRetarget is a test seam that holds a Since between sending a record
	// to the one the index now names and reading it. It is nil outside tests,
	// for the same reason afterAppend is.
	afterRetarget func()

	// shareHook is a test seam that runs in Share between the first closed
	// check and the registry's lock, so a test can fence the core in the
	// window the re-check under registryMu refuses. It is nil outside tests,
	// for the same reason afterAppend is.
	shareHook func()

	// afterSwap is a test seam that runs in BeginClose inside the registry
	// swap's critical section, between the swap and the unlock, so a test
	// can pin the closed flag up before the fence releases the mutex. It is
	// nil outside tests, for the same reason shareHook is.
	afterSwap func()

	// registry is the open handles, replaced whole under registryMu and read
	// through the pointer alone. See shares.go.
	registry atomic.Pointer[[]*Share]
	// decision: registryMu is a leaf. It covers only swapping and reading the
	// registry pointer, and holds nothing else across it but afterSwap, a
	// test seam that observes the fence from inside the critical section and
	// takes no lock of its own, and, inside the Forget path's critical
	// section, the forgetting mark's own mutex — the mark set under the
	// registry's lock so no attach lands between the two. Nil afterSwap in
	// production, where the critical section stays the swap alone. It stays a
	// plain mutex: a recorded order it cannot break is not its to police.
	registryMu sync.Mutex

	// stranded is the set of stranded tags, replaced whole under strandMu
	// and read through the pointer alone. See strand.go.
	stranded atomic.Pointer[[]core.ShareTag]
	// decision: strandMu is a leaf. It guards stranded's replacement alone,
	// holding nothing across it and taking nothing under it; registryMu's
	// own contract names Share, Close and the Forget path's mark as its
	// takers, and the strand set is still a mutex of its own: the Forget
	// path takes it with every lock of the registry's down.
	strandMu sync.Mutex

	// forgetting is the set of tags whose Forget path is running — the mark
	// that path sets on the registry before its barrier and clears when it
	// returns. Its reader, isForgetting, is what the reclamation pass's
	// copies consult under a file's append domain; the setter belongs to the
	// Forget path, which does not exist yet, so the set lands empty and
	// reads that way. See shares.go.
	forgetting map[core.ShareTag]struct{}
	// decision: forgettingMu is a leaf. It guards the mark set's lookups and
	// replacements alone, holding nothing across them and taking nothing
	// under them: nothing that consults the mark — a copy reading it under
	// the file's append domain it already holds, the Forget path's setter —
	// is waited on by it, and no path of the core takes a ranked lock while
	// it is held. Withdraw it if a creator of the mark ever calls into the
	// core with the mutex taken.
	forgettingMu sync.Mutex

	// closedFlag is set the moment Close begins, so a call that takes no
	// context can refuse at once; the work itself runs under closeOnce.
	closedFlag atomic.Bool
	closeOnce  sync.Once
	// drain counts one goroutine per share the close fenced, each waiting on
	// its share's idle channel; Close waits the group before it stops the
	// core's workers. See close.go.
	drain sync.WaitGroup
	// decision: stopsMu is a leaf. It guards the stops slice alone, and no
	// stop and no close step runs with it held, so a stop that takes a
	// worker's own locks never nests it. See close.go.
	stopsMu sync.Mutex
	// stops are the stop functions of the workers the core started, in the
	// order they registered — in reverse of which Close stops them. See
	// close.go.
	stops []func()
	// group is the segment set the core runs, closed by Close once every
	// registered worker has stopped. Nil on a Core no constructor returned.
	group    *segment.Group
	closeErr error
}

// unboundedBytes is the Maker's byte ceiling: none that a journal could
// reach, so segment creation is limited only by the pool's targets.
const unboundedBytes = int64(1) << 62

// New assembles a Core over deps, continuing from state, and starts it.
//
// decision: New returns without waiting for the pool's first spare segments.
// A write that arrives before a spare is ready is refused with the named,
// retryable spare refusal, and the pool's refill makes one, so the retry
// succeeds. The test helper waits for the spares only so tests are
// deterministic. Overturned by a caller that needs its first write to always
// succeed, such as the open path, which would then wait on the pool's spare
// count.
//
// decision: New trusts its Deps: Streams, SegmentCap and ExtentLimit are not
// validated here, because the open path validates the settings before it
// calls New. Overturned by a caller that builds Deps from user input without
// that validation.
//
// New builds over a set of its own only: a State carrying a Set or an open
// payload is refused, naming Continue, the one door that registers a
// caller's set with the liveness tracker and starts its retirement worker.
// A core New returned over a caller's set would serve appends and reads but
// retire no segment, filling the journal to its maximum with no condition
// raised.
func New(deps Deps, state State) (*Core, error) {
	if state.Set != nil || state.Open != nil {
		return nil, errors.New("datapath: New builds its own set; a caller-supplied set or open payload is continued through Continue, which starts its retirement worker")
	}
	return newCore(deps, state, coreOptions{})
}

// Set returns the segment set the core runs. It is a test seam: a fixture
// that plants records through the core's streams reads the set the core
// built rather than handing it one, since New builds its own.
func (c *Core) Set() *segment.Set { return c.set }

func newCore(deps Deps, state State, o coreOptions) (*Core, error) {
	// A caller's State can carry a rebuilt set; the set the core did not
	// build gains a retirement worker only through the open payload below,
	// which registers its records before the worker's first pass.
	freshSet := state.Set == nil
	if state.Set == nil {
		state.Set = segment.NewSet(deps.SegmentCap, deps.Log)
	}
	extentLimit := deps.ExtentLimit
	if o.extentLimit != 0 {
		extentLimit = o.extentLimit
	}
	if state.Index == nil {
		state.Index = index.New(extentLimit)
	}
	// A fresh Counters issues no version until SetVersionLimit first sets a
	// limit, so a caller that continues from a zero State must set one before
	// its first draw or write.
	if state.Counters == nil {
		state.Counters = &index.Counters{}
	}
	budget := deps.Descriptors
	if budget == nil {
		budget = core.NewDescriptorBudget(64)
	}

	c := &Core{clock: deps.Clock, index: state.Index, counters: state.Counters, set: state.Set, segCap: state.Set.Cap(), identity: deps.Identity, tracer: deps.Tracer, accountSpy: o.accountSpy, stepSpy: o.stepSpy, betweenHeldReads: o.betweenHeldReads, afterAppend: o.afterAppend, beforeAppend: o.beforeAppend, beforeWrote: o.beforeWrote, beforeResolve: o.beforeResolve, beforeRead: o.beforeRead, afterCorrupt: o.afterCorrupt, afterRetarget: o.afterRetarget, shareHook: o.shareHook, afterSwap: o.afterSwap, rec: o.recorder, forgetting: make(map[core.ShareTag]struct{})}
	tbl := newFileLocks()
	if c.rec != nil {
		tbl.SetRecorder(c.rec)
	}
	var domains durability.Domains = tbl
	if o.wrapDomains != nil {
		domains = o.wrapDomains(domains)
	}
	c.domains = domains

	// The liveness tracker is built after the retirer below, but the events
	// that reach it through the closures named here — the loss records the
	// durability layer appends, the segments its moves empty — first run
	// once a data path does, which is never before d.Start; assigning tr
	// early keeps the wiring's order readable while the closures stay safe.
	var tr *reclaim.Tracker
	// The recorder is installed before anything is started: the pool's
	// refill worker, the retirer, the sealer and the durability layer all
	// reach these locks in the background, and a lock Init'ed after its
	// first use keeps no account of it. The set is told here rather than
	// left to the later blocks because a State that carries it was shared
	// by the caller, so this is the last place the wiring can reach it
	// before its own use.
	if c.rec != nil {
		c.index.SetRecorder(c.rec)
		state.Set.SetRecorder(c.rec)
	}

	// The durability layer is built first of the storage-touching pieces,
	// because its Observe wrap is the storage every later part must reach
	// the directory through: the first EROFS from any call the maker, the
	// descriptors or the retirer make turns the journal read-only, and a
	// call the wrap does not see cannot. Its Config needs no maker, pool or
	// cache — the index, the counters, the domains and Account are all of
	// the core's own — so nothing downstream constrains this order.
	d, err := durability.New(durability.Config{
		// Read at call time: the capacity is built after this layer.
		Landed: func(tag core.ShareTag, n int64) {
			if c.cap != nil {
				c.cap.Landed(tag, n)
			}
		},
		Clock:    deps.Clock,
		Domains:  domains,
		Tracer:   deps.Tracer,
		Log:      deps.Log,
		Index:    c.index,
		Account:  c.Account,
		Dir:      deps.Dir,
		Identity: deps.Identity,
		Counters: c.counters,
		// The durability layer hands the delta Lose returns to Account itself.
		MarkLoss: c.index.Lose,
		// The owed ends: a segment that ended is answered window-open by the
		// set until this layer takes its event, so a pass that reaches a
		// newly failed segment between its end and the take declines it.
		EndOwed: state.Set.EndOwed,
		TakeEnd: state.Set.TakeEnd,
		// Each loss record counts live where it landed: a segment holding
		// one stays held until the record's own liveness ends.
		LossRegistered: func(l durability.Loss, seq core.Seq, at segment.Location) {
			tr.Register(reclaim.Rec{
				Kind:  format.KindLoss,
				File:  l.File,
				Seg:   at.Segment,
				Ext:   l.Extent,
				Pair:  index.Numbers{Version: l.Version, Seq: seq},
				Seq:   seq,
				Named: l.Seq,
				// The share whose content the loss names, part of the key of
				// every registry the registration enters, and the place its
				// append reported: a carry reaches the record through the
				// registry, and one registered without either fails the unit
				// over it, for it can neither read nor move the record.
				Tag: l.Share,
				Loc: at,
				// The flags that pick the loss's own drop rule.
				Synced:     l.Synced,
				Superseded: l.Reason == core.LossSuperseded,
			})
		},
		// A move the durability layer made emptied a segment: the segment is
		// offered to the retirement worker with the causality record whose
		// durability its retirement waits on — but only when nothing else
		// holds it, checked here rather than in the worker, so the offer the
		// worker queues names a segment the tracker already answers not
		// held. Like the move that runs it, the closure takes only the
		// tracker's lock-free read and the worker's queue lock, and waits on
		// no sync and no append domain.
		Emptied: func(segs []core.SegmentID, at segment.Location) {
			for _, seg := range segs {
				if !tr.Held(seg) {
					c.retiring.Emptied(seg, at)
				}
			}
		},
		// A segment gone from the registry was sealed or ended and emptied
		// before it retired: no mark can prove a record there, and a
		// markless Sync is the answer — the caller keeps each stream's
		// latest mark, so a proofless mark naming a dead segment would
		// replace the stream's real proof.
		Sealed: func(id core.SegmentID) bool {
			st, err := state.Set.State(id)
			return err != nil || st == segment.Sealed
		},
		// A Sync naming a file with nothing unsynced still gets its mark,
		// from the file's newest record the index names.
		Latest: c.latestRecord,
	})
	if err != nil {
		return nil, err
	}
	c.dur = d
	fs := d.Observe(deps.FS)

	// The spare maker reserves every segment it allocates against the
	// maximum: zero keeps the maker unbounded, as the core was before the
	// maximum was wired, so its tests keep passing unchanged. The admission
	// reads the same figure the maker bounds by, so both agree on what the
	// maximum holds.
	maxBytes := deps.MaxBytes
	if maxBytes == 0 {
		maxBytes = o.maxBytes
	}
	c.maxBytes = maxBytes
	headroomRecords := deps.HeadroomRecords
	if o.headroomRecords != 0 {
		headroomRecords = o.headroomRecords
	}
	makerBytes := unboundedBytes
	if maxBytes > 0 {
		makerBytes = maxBytes
	}
	maker := segment.NewMaker(fs, state.Set, makerBytes, deps.LastSegment, deps.Log)
	// The pool asks the read-only check before each spare it makes, so a
	// read-only journal makes none (RFC 1 §5.4). The headroom's spare class
	// is sized by the records it holds, whole spares at the charge the
	// admission table draws by.
	//
	// decision: zero headroom records readies no headroom spare and so
	// throttles no content write — the journal fails open, holding no
	// headroom to measure — because zero is the wire value only while the
	// open path that resolves every setting's default does not exist; the
	// journals it wires resolve the default and pass the figure here.
	// Withdraw once a zero HeadroomRecords can reach this from a caller
	// that names a setting, rather than only a wiring that names none.
	targets := deps.Targets
	if o.contentSpares != 0 {
		targets.Content = o.contentSpares
	}
	if headroomRecords > 0 {
		// Each open stream also needs its seal marker, one footer entry and the
		// trailer, which a segment cannot seal without.
		sealReserve := int64(deps.Streams) * (format.SealMarkerSize + format.CatalogEntrySize + format.TrailerSize)
		targets.Headroom = int((int64(headroomRecords)*capacity.RecordCharge + sealReserve + state.Set.Cap() - 1) / state.Set.Cap())
	}
	c.contentTarget = targets.Content
	c.pool = segment.NewPool(maker, targets, d.Admit, deps.Log)
	c.cache = segment.NewDescriptors(fs, format.SegmentName, budget)
	if c.rec != nil {
		c.pool.SetRecorder(c.rec)
		c.cache.SetRecorder(c.rec)
	}
	// The re-adopted spares reach the pool before Start, the order Adopt
	// holds every spare to: the open's directory sync has already proven
	// their names. A refusal is an id the set already owns, which no open
	// hands over; nothing has started yet, so the open fails with no worker
	// to stop.
	if state.Open != nil {
		for _, a := range state.Open.Spares {
			if err := c.pool.Adopt(a.ID, a.Class); err != nil {
				return nil, err
			}
		}
	}

	// The admission every appending call asks once: its injected facts are
	// the plain reads and takes of the layers it consults, and its counters
	// move under its own mutex — the capacity domain — which none of the
	// funcs below holds across a call. The share's limit a call reads off
	// its Share handle, the only place it lives. The extent bound a test
	// sets through the options replaces Deps' figure before the index is
	// built, so a core built for a tight bound sees it everywhere. The
	// read-only check is the durability layer's own Admit, so the journal the
	// seam turned read-only refuses at once, before any limit is read.
	//
	// The headroom's admission table runs over the same facts the content
	// admission reads — the set's footprint and the shared maximum — and
	// measures itself by its own ledger, not the pool, and lends Take its half rule below, so both derive
	// from one wiring of each fact.
	c.headroom = capacity.NewHeadroom(capacity.HeadroomConfig{
		Records:    headroomRecords,
		Footprint:  func() int64 { return state.Set.Counters().UsedBytes },
		MaxBytes:   maxBytes,
		SegmentCap: state.Set.Cap(),
		Admit:      d.Admit,
	})
	// The segment cap the admission sizes its pieces and spares by is the
	// set's own, not Deps': the streams roll and the reader bounds a record
	// by the set the State carries, so a State that rebuilds a set of
	// another size must have its admission agree with what its streams do.
	c.cap = capacity.New(capacity.Config{
		Admit:          d.Admit,
		CheckIndex:     state.Index.CheckWrite,
		CheckFillIndex: state.Index.CheckFillIndexBound,
		VersionsLeft:   state.Counters.VersionsLeft,
		DrawVersion:    state.Counters.Draw,
		EntriesLeft:    state.Index.EntriesLeft,
		Footprint:      func() (used int64) { return state.Set.Counters().UsedBytes },
		ShareFigures:   state.Set.ShareBytesAndRetired,
		Room:           func(stream int) int64 { return c.streams.Stream(stream).Room() },
		ReadySpares:    c.pool.ReadyContent,
		Hold:           c.pool.Hold,
		BelowHalf:      c.headroom.BelowHalf,
		Tracer:         deps.Tracer,
		MaxBytes:       maxBytes,
		SegmentCap:     state.Set.Cap(),
	})
	c.streams = segment.NewStreams(deps.Streams, deps.Identity, state.Set, c.pool, c.cache, d.Sink)
	if c.rec != nil {
		// With the streams built and nothing started, the admission and its
		// headroom table can be told now too: every injected fact they read
		// is a snapshot that takes its lock before the section runs, and a
		// same-domain nesting between them would be the recorded finding it
		// is, not one to paper over by init order.
		c.streams.SetRecorder(c.rec)
		c.headroom.SetRecorder(c.rec)
		c.cap.SetRecorder(c.rec)
	}
	if o.takeHook != nil {
		c.cap.SetTakeHook(o.takeHook)
	}

	// The retirement executor over the pieces the core owns, gated on Admit
	// like every other writer, and told of each retirement once the segment
	// has left the set.
	c.retirer = segment.NewRetirer(segment.RetirerConfig{
		Set:      state.Set,
		Cache:    c.cache,
		Pool:     c.pool,
		Maker:    maker,
		FS:       fs,
		ReadOnly: d.Admit,
		Retired:  func(id core.SegmentID) { c.retiredSegment(id) },
		// decision: the retired segment reaches the liveness tracker
		// through this durable-retirement signal alone, not through the
		// retirer's retired hook or the set's retire observer: on the
		// unlink's own path those fire after the unlink's directory sync
		// has returned, whatever it answered — only this signal carries
		// the sync's success (a recycle has made its removal durable
		// before its hooks run) — and a crash that undoes the retirement
		// must find the tracker's registrations intact — the surviving
		// file rebuilds its records, and what a rebuild is owed cannot
		// have been released on an unlink nothing made durable.
		// Both halves of the tracker's own gate ride the signal: the
		// files the call carries are the index's own, not the tracker's
		// registry fill — the registry names only files with registered
		// records, so a segment whose files were all content would leave
		// their drop ranges parked behind RemovalDurable for a list that
		// never named them — and RemovalDurable consumes the park the call
		// itself made, so the ranges reach the index's SegmentGone, whose
		// contract demands the unlink be durable, in the same delivery.
		// The ceiling: a failed directory sync delivers neither, so the
		// tracker keeps a retired segment's registrations — holds it
		// answered not held, for the segment is gone from the set — and
		// its files' drop ranges stay, until the next open's rebuild
		// recomputes both.
		Durable: func(ret segment.Retirement) {
			tr.SegmentRetired(ret.ID, state.Index.FilesOfSegment(ret.ID))
			tr.RemovalDurable(ret.ID)
		},
		Log: deps.Log,
	})

	// The liveness tracker and the retirement worker close the loop the
	// executor alone left open: the tracker counts what keeps a segment
	// held beside its bytes, fires a candidate when the last thing leaves,
	// and the worker claims each candidate, re-checks it against the
	// tracker, the durability layer's gate and the ledger's state, and hands
	// it to the retirer — the same executor a repack or a liveness-driven
	// retirement reaches, whose queue and claim rules both stay its own.
	//
	// The tracker is built before the worker because the worker's
	// constructor demands one, and the candidate it carries defers to the
	// worker through the field because the worker cannot exist first: the
	// candidate never fires before the assignment, the tracker's events
	// running only on the data paths, which start below.
	tr = reclaim.New(state.Index, func(seg core.SegmentID) { c.retiring.Prompt(seg) })
	c.liveness = tr
	c.retiring = reclaim.NewWorker(state.Set, tr, d, c.retirer)
	// A segment is a member exactly while it is still a segment of the set,
	// so a snapshot segment a retirement already took is answered gone.
	tr.SetMember(func(id core.SegmentID) bool {
		_, err := state.Set.State(id)
		return err == nil
	})
	state.Set.OnSeal(tr.SealObserved)

	// The repack pass over the core's own pieces: the selector claims the
	// candidates a pass runs over, the copier copies and carries under the
	// same liveness tracker and the same streams the retirement worker
	// serves, and the retirer is the same executor a worker's hand-over
	// reaches — a pass's retirements run synchronously in its own
	// goroutines, one claim at a time. A deferred loss the durability layer
	// holds against a segment reaches the carry as the registry entry
	// mapped field by field, and once the pass has the record durable the
	// deferral ends through the same registry, matched back by its mapped
	// entry — the registry entry is its own identity, and the map keeps
	// package reclaim off the layer that owns it. Every repoint's held
	// delta runs through Account as any other index change does.
	// The pins keep the protocol that pins segments out of selection — the
	// running offers' — wired to the selector alone: a pass excludes its own
	// stayed sources by holding their claims, never by touching a pin, so
	// nothing a pass does can delete an offer's.
	pun := unread.New(deps.Clock)
	c.unread = pun
	pins := reclaim.NewPins()
	c.repack = reclaim.NewPass(reclaim.PassConfig{
		Selector: reclaim.NewSelector(state.Set, state.Index, pun, d, pins),
		Copier: reclaim.NewCopier(state.Index, state.Counters, c.streams, pun, tr, c.domains, c.isForgetting,
			func(ctx context.Context, bad reclaim.BadSource) (reclaim.CorruptOutcome, error) {
				out, err := d.Corrupt(ctx, durability.Corruption{
					Share: bad.Tag, File: bad.File, Record: bad.Record,
					Seq: bad.Seq, Version: bad.Version, Extent: bad.Extent, DomainHeld: true,
				})
				if err != nil {
					return 0, err
				}
				switch out.Kind {
				case durability.CorruptDropped:
					return reclaim.CorruptDropped, nil
				case durability.CorruptUseCopy:
					return reclaim.CorruptUseCopy, nil
				}
				return 0, fmt.Errorf("datapath: the durability layer answered corrupt with an outcome %d no mapping names", out.Kind)
			},
			deps.Clock, deps.Log,
			func(seg core.SegmentID) []reclaim.DeferredLoss {
				var out []reclaim.DeferredLoss
				for _, l := range d.DeferredAgainst(seg) {
					out = append(out, deferredLossOf(l))
				}
				return out
			},
			c.headroom.Admit, c.headroom.Drawn,
			// A copy's roll refused for want of a reserve spare waits the
			// pool's own settle once — the refill racing the unit — before
			// the refusal is the caller's: the model's repack-and-forget
			// mix drew the flake this closes.
			c.pool.WaitReserveSpare),
		Tracker:    tr,
		Index:      state.Index,
		Set:        state.Set,
		Durability: d,
		Durable:    d,
		Retirer:    c.retirer,
		EndDeferral: func(seg core.SegmentID, e reclaim.DeferredLoss) bool {
			for _, l := range d.DeferredAgainst(seg) {
				if deferredLossOf(l) == e {
					return d.EndDeferral(seg, l)
				}
			}
			return false
		},
		Account: c.Account,
		Reserve: reclaim.Reserve{
			Held:     func() int64 { return c.pool.Counters().RepackReserveBytes },
			Count:    c.cap.CopyLanded,
			Transfer: c.cap.CopiesTransfer,
		},
		Sealer: reclaim.NewSealer(c.streams, state.Set, c.headroom.Admit, c.headroom.Drawn, c.headroom.Refund),
	})

	// The open's registration: a core continued from a rebuilt state
	// registers every record the rebuild found still live on disk through
	// the same call the append paths take, with the set's member answer and
	// seal observer wired above; the one OpenDone that closes the window
	// then spends every registration the drop test already answers
	// droppable — a marker settled before the open read its record, a
	// removal nothing on disk outranks — so its segment's candidate crosses
	// now rather than at its file's first event, which a file whose records
	// all resolved at the open may never see. The seed's deltas each run
	// through Account, the consumer every delta of a running journal takes,
	// so the journal-wide axes and the per-share figures open at the
	// figures the rebuilt index holds rather than at zero; the zero write
	// time the deltas carry is the time restored content opens with.
	// openPayload is Continue's credential: the set this core runs was
	// continued from a recovery whose open registration the tracker holds.
	c.openPayload = state.Open != nil
	if o := state.Open; o != nil {
		tr.RegisterAtOpen(o.Live, o.Forgets, o.Unsynced)
		tr.OpenDone()
		for _, d := range o.Dirty {
			c.Account(d)
		}
	}

	// The workers register their stops as they start, in the order their
	// pieces were built: the retirer, then the reclaim worker, then the idle
	// sealer, then the pool, then the durability layer. Close stops them in
	// the reverse of this order — the durability layer, whose Stop wakes
	// every durability waiter, first, then the pool, then the idle sealer,
	// then the reclaim worker, then the retirer — so the pool is stopped
	// before the retirer, the order the segment package's group close
	// documents for the same parts it owns: the wake the retirer's queue drop
	// sends must reach a pool that makes nothing, and a retirement still in
	// flight must unlink its segment rather than reserve a slot in a live
	// pool. The reclaim worker's stop registers right after the retirer's
	// so reverse order stops the worker before the retirer it hands to —
	// no candidate reaches a stopped executor — and it registers whether
	// the freshSet guard started the worker, since Stop is safe to call
	// on a worker that never started. Stopping the pool before the idle sealer is
	// safe not because of the drain — the sealer is a background worker,
	// not a data-path call — but because an idle seal never takes from the
	// pool: it never rolls, and a roll in the seal's window belongs to the
	// stream's appends, not the sealer, so a stopped pool cannot starve or
	// deadlock a sealer in flight.
	c.retirer.Start()
	// decision: the retirement worker runs only once its set's records are
	// registered with the liveness tracker, whatever built the set, and —
	// over a set continued from an open payload — only once the open that
	// continues it has landed the loss records it owes, through
	// Continue: the worker's candidate scan answers a segment holding
	// only corrupt drops, and its unlink would strip the mask an in-flight
	// loss record has not yet made durable, serving the older copies the
	// crash superseded. New starts the worker itself only over a set it
	// built fresh, which owns no seen records and owes no loss record; the
	// offers a continued set's registration fires while the worker waits
	// are drained by the first pass. A caller's set without an Open payload
	// carries no registration, and Continue refuses it: the worker's
	// tick would hand its records' not-held segments to the retirer while
	// the records the disk still holds ride in them. Overturned by the
	// rebuild hand-off always carrying its open payload, which would close
	// this to every set.
	if freshSet {
		c.retiring.Start()
	}
	c.addStop(c.retirer.Stop)
	// The repack pass's stop registers right after the retirer's, so reverse
	// order stops it before the retirer: a pass interrupted between its
	// units hands the source its in-flight unit is retiring to a retirer
	// still running, and the stop joins the pass before the executor is
	// stopped — the durability layer, whose own stop runs first and wakes
	// every durability waiter, is what frees a pass stuck in a wait.
	c.addStop(c.repack.Stop)
	// The probe registers between the retirer's stop and the reclaim
	// worker's, so reverse order runs it once the reclaim worker has stopped
	// and before the retirer stops — the window the stop-order test reads
	// all three workers' states in.
	if o.stopProbe != nil {
		c.addStop(o.stopProbe(c.pool, c.retirer, c.retiring))
	}
	c.addStop(c.retiring.Stop)

	if deps.IdleSealBytes > 0 {
		c.sealer = segment.NewIdleSealer(c.streams, deps.Clock, deps.IdleSealBytes, d.Admit, deps.Log)
		c.sealer.Start()
		c.addStop(c.sealer.Stop)
	}
	c.pool.Start()
	c.addStop(c.pool.Stop)
	d.Start(c.streams)
	c.addStop(d.Stop)

	// The group closes the segment set once every registered worker has
	// stopped: the streams, the idle sealer, the pool, the retirer and the
	// descriptor cache, in the order the segment package documents.
	c.group = segment.NewGroup(segment.GroupConfig{
		Streams: c.streams,
		Idle:    c.sealer,
		Pool:    c.pool,
		Retirer: c.retirer,
		Cache:   c.cache,
	})
	return c, nil
}

// Index returns the placement index.
func (c *Core) Index() *index.Index { return c.index }

// Liveness returns the segment liveness tracker the open's registrations
// filled: the one answer of an attached segment the open's own emptying asks
// before it retires anything.
func (c *Core) Liveness() *reclaim.Tracker { return c.liveness }

// Retire runs one retirement through the executor the core runs — the same
// recycle-or-unlink path, refill included, a reclaim worker's hand-over
// takes — so an open-time retiree and a running journal's own answer one
// path.
func (c *Core) Retire(id core.SegmentID, reason segment.Reason) (segment.Retirement, error) {
	return c.retirer.Retire(id, reason)
}

// Repack runs one repack pass over the core: units of one source segment
// each, bounded by a budget of copied bytes, aimed at the scope the caller
// names. A done context is answered before anything changes; a pass the
// core's close interrupts between its units returns core.ErrClosed having
// completed the unit in flight. The bytes the pass's copies moved are added
// to the repacked_bytes counter, and a tracer sees one repack_pass span
// carrying the pass's segments, bytes and freed figures. It returns freed,
// the storage the pass's retirements returned through the executor.
//
// decision: the span is opened with the pass's own outcome rather than
// around its work, because a tracer takes its attributes at the span's start
// and the pass's segment, byte and freed figures exist only at its end — a
// span opened beforehand could carry the budget alone. Overturned by a
// tracer whose ending carries its attributes, which would wrap the work
// instead.
func (c *Core) Repack(ctx context.Context, budget int64, scope reclaim.Scope) (int64, error) {
	if c.closed() {
		return 0, core.ErrClosed
	}
	out, err := c.repack.Run(ctx, budget, scope)
	if out.Bytes > 0 {
		c.repackedBytes.Add(uint64(out.Bytes))
	}
	if c.tracer != nil {
		_, end := c.tracer(ctx, core.SpanRepackPass,
			slog.Int("segments", out.Segments),
			slog.Uint64("bytes", uint64(out.Bytes)),
			slog.Int64("freed", out.Freed))
		end(err)
	}
	return out.Freed, err
}

// deferredLossOf maps one registry entry of the durability layer's deferred
// losses onto the copy's own shape, field by field. The mapping is the
// pass's whole contract with the registry: the carry builds its record from
// it, and the deferral-end wiring matches the entry back by it — one entry
// per dropped piece, so the mapped fields name one entry exactly.
func deferredLossOf(l durability.Loss) reclaim.DeferredLoss {
	return reclaim.DeferredLoss{
		Share:     l.Share,
		File:      l.File,
		Extent:    l.Extent,
		Version:   l.Version,
		Seq:       l.Seq,
		Reason:    l.Reason,
		Offloaded: l.Offloaded,
		Synced:    l.Synced,
	}
}

// Durability returns the durability layer.
func (c *Core) Durability() *durability.Durability { return c.dur }

// Unread returns the set of records no read returned, which the repack
// pass's reads mark and the per-segment rows report.
func (c *Core) Unread() *unread.Set { return c.unread }

// Streams returns the append streams.
func (c *Core) Streams() *segment.Streams { return c.streams }

// Domains returns the file append domains, which the reclamation pass
// takes per file — one at a time — around its copy's liveness check and
// append (RFC 1 §8.3).
func (c *Core) Domains() durability.Domains { return c.domains }

// WrittenBytes returns the payload bytes WriteAt has published since the core
// was built: the written_bytes counter.
func (c *Core) WrittenBytes() uint64 { return c.writtenBytes.Load() }

// FilledBytes returns the payload bytes Fill has published since the core was
// built: the filled_bytes counter.
func (c *Core) FilledBytes() uint64 { return c.filledBytes.Load() }

// FillRefused returns how many fills were refused as stale since the core was
// built: the fill_refused counter.
func (c *Core) FillRefused() uint64 { return c.fillRefused.Load() }

// ReservedBytes returns what open reservations hold and their records have
// not yet filled: the gauge of the writes a limit admitted and whose pieces
// are still landing.
func (c *Core) ReservedBytes() int64 { return c.cap.ReservedBytes() }

// HeldEntries returns the placement index entries open reservations hold and no
// publish has claimed yet. It is a read of the admission's own counter, for the
// tests that check a reservation's entries are handed over as the index claims
// them.
func (c *Core) HeldEntries() int64 { return c.cap.HeldEntries() }

// ReservationRefused returns the reservation_refused counter: how many
// reservations the journal's admission has refused, by every limit it names,
// since the core was built.
func (c *Core) ReservationRefused() uint64 { return c.cap.RefusedCount() }

// PendingEntries returns the index entries admitted plain grants hold and have
// not yet published, the counter a test checks returns to rest.
func (c *Core) PendingEntries() int64 { return c.cap.PendingEntries() }

// PoolCounters returns the spare pool's counts, read from the pool's own
// maintained figures: the tests that check a reservation's spares are held
// and returned read them here.
func (c *Core) PoolCounters() segment.PoolCounters { return c.pool.Counters() }

// Retired lowers the share's figure by the retirements its segments have
// freed since the figure last took them into account, and is what a write
// through the retired segment's share waits on: a retry after the
// retirement finds the room it freed. It is a test seam: the retirement
// executor reaches the same reconciliation through retiredSegment, which
// runs it for every seeded tag once the set's own counters have moved. It
// is safe from any goroutine.
func (c *Core) Retired(tag core.ShareTag) { c.cap.Retired(tag) }

// Retirer returns the retirement executor the core runs. Nothing above the
// core reaches it through this accessor today — the core's own close stops
// it through the registry registered at its Start, not through the field —
// so it is a test seam: the one a test retires through, driving
// the executor's gates and retired-segment hook exactly as the assembled
// journal's own retirement path does. A later reclamation caller above the
// core, which none exists yet, would take it from here.
func (c *Core) Retirer() *segment.Retirer { return c.retirer }

// retiredSegment is the retirement executor's retired-segment hook: it runs
// once per completed retirement, with the retired segment's id alone, after
// the segment has left the set and holding no lock of the executor's, so it
// must be safe for concurrent calls.
//
// decision: one retirement calls both Durability.Retired and the capacity's
// Retired for every tag it holds a figure for, because the executor's hook
// fires with the segment's id only — after the segment left the set, which
// share's records it held is no longer written anywhere the hook can read —
// so the share whose limit the retirement freed is not known there.
// Durability.Retired is not tag-dependent: it drops the copy table's entries
// in the segment and marks its damaged row retired. The capacity's Retired is
// applied to every seeded tag — not the registry's — because the seed runs
// once per tag: a share closed while its segment was waiting on the retirer
// is unregistered at the hook, and a figure the hook left stale-high would
// stay that way when the share reattached, refusing its writes naming the
// share's limit until some unrelated retirement fired while a share of the
// tag was registered again. A tag whose segment was not this one computes a
// delta of zero over the baseline the capacity already holds, so calling it
// for every seeded tag is exact and costs one figure read per tag. A tag
// seeded for the first time after this hook read the set reads the pair the
// retirement already moved, so it is missed by nothing. Overturned by an
// executor whose hook carries the tag, which would name the one share
// directly.
//
// The copies a pass counted against the segment move to their shares' figures
// first, in the same step, and before the figures drop by the segment's own
// bytes: a tally between the two stands above the truth, never below it.
func (c *Core) retiredSegment(id core.SegmentID) {
	c.dur.Retired(id)
	c.cap.CopiesTransfer(id)
	c.cap.RetiredEverySeeded()
}

// AfterRetirementTick makes the retirement worker call fn after each tick's
// scan and pass. It has no production caller: a test waits on the worker's
// own events through it, where the worker's real timer allows no clock.
func (c *Core) AfterRetirementTick(fn func()) { c.retiring.AfterTick(fn) }

// CopyHeldBytes returns the record bytes of repack copies held out of the
// shares' tallies, whose sources are not yet retired. It reports the
// capacity's own copy ledger, the transfer of which the pass's abandon and
// the retirement hand over.
//
// decision: no production caller; a test of the copy ledger's transfer holds
// the signature up — the capacity sits inside the core, unreachable any other
// way. Drop the method, or give it a production caller, once the figures are
// exported through Stats.
func (c *Core) CopyHeldBytes() int64 { return c.cap.CopyHeldBytes() }

// ShareFigure returns the record-byte figure the share's limit is admitted
// against, zero for a tag no write has seeded. It reports the capacity's own
// tally, which a test compares with the set's settled bytes.
//
// decision: no production caller; the landed-record tests hold the signature
// up — the capacity sits inside the core, unreachable any other way. Drop the
// method, or give it a production caller, once the figures are exported
// through Stats.
func (c *Core) ShareFigure(tag core.ShareTag) int64 { return c.cap.ShareFigure(tag) }

// WaitSpares blocks until the pool has readied every spare the open's own
// targets name, or reports the pool's word for why it stopped filling. It is
// the core half of the hook Open hands its caller's SparesReady callback: the
// wait the fixture blocks on is the pool's own settle event, woken by the
// refill and never by time.
func (c *Core) WaitSpares(ctx context.Context) error { return c.pool.WaitSpares(ctx) }

// WaitRecordSpares blocks until the pool holds n spares a header-only
// record's roll can take, or reports the pool's word for why it stopped
// filling: the wait the open's loss pass takes before the first record it
// appends, n being the streams its records append on.
func (c *Core) WaitRecordSpares(ctx context.Context, n int) error {
	return c.pool.WaitRecordSpares(ctx, n)
}

// TakeReserve draws one spare of the pool's repack reserve whose id is above
// floor for the open's own copy (Pool.TakeReserveAbove), waiting for the pool's settle when the class is short — a wait for
// the reserve class alone, as the open's loss records wait for their own
// spares and not the pool's whole goal, so an open whose emptying draws first
// past a refill that has not landed blocks on the pool rather than failing,
// and a maximum that leaves the content class short does not refuse a copy
// whose reserve spare is ready — and returning the pool's
// error when none can be made and the refill stopped. It is the one shape the
// open hands the emptying step's copies: the drawn spare enters the set as
// the segment the copy makes of it, sealed by the step that took it.
func (c *Core) TakeReserve(ctx context.Context, floor core.SegmentID) (core.SegmentID, error) {
	id, err := c.pool.TakeReserveAbove(floor)
	if err == nil || !errors.Is(err, core.ErrNoSpace) {
		return id, err
	}
	if werr := c.pool.WaitReserveSpare(ctx); werr != nil {
		return 0, werr
	}
	return c.pool.TakeReserveAbove(floor)
}

// Continue builds the core over a set continued from a recovery's open
// payload, runs beforeRetire — the open's loss pass and the emptying that
// gates on it, the steps that must land between the core and its retirement
// worker — and only then starts the worker: the tick's scan answers a
// segment holding only corrupt drops, and starting it ahead of the pass
// would unlink such a segment while the mask the pass has not yet made
// durable is its only gate. A core built over an
// open payload is never returned with its worker unstarted. A failure of
// beforeRetire closes the core and answers the failure; a set that carried no
// open payload is refused and closed, for its records hold no registration
// with the tracker and the worker's tick would hand their not-held segments
// to the retirer while the records the disk still holds ride in them — the
// same refusal New's own start decision makes. New refuses every
// caller-supplied set, so Continue is the only door to a core over one.
func Continue(deps Deps, state State, beforeRetire func(*Core) error) (*Core, error) {
	c, err := newCore(deps, state, coreOptions{})
	if err != nil {
		return nil, err
	}
	if !c.openPayload {
		_ = c.Close()
		return nil, errors.New("datapath: the core's set carried no open payload, so nothing registered its records for a retirement worker")
	}
	if err := beforeRetire(c); err != nil {
		_ = c.Close()
		return nil, err
	}
	c.retiring.Start()
	return c, nil
}
