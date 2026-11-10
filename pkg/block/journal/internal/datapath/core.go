package datapath

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
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
	// MaxBytes is the journal's configured maximum local footprint, in
	// bytes: the whole segments WriteAt's admission reserves against and
	// the spare maker raises UsedBytes under. Zero keeps the maker
	// unbounded and admits by every other limit alone, which is the
	// behaviour of a core opened before the maximum was wired.
	MaxBytes int64
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
// passes the rebuilt ones.
//
// decision: State is plain data handed to New and holds no lock domain of its
// own; the index and counters it carries take their own short, I/O-free locks.
type State struct {
	Index    *index.Index
	Counters *index.Counters
	Set      *segment.Set
}

// coreOptions are the test-only adjustments newCore applies before the layers
// above the domain table are built.
type coreOptions struct {
	// wrapDomains replaces the file append domain table the core and the
	// durability layer both use.
	wrapDomains func(durability.Domains) durability.Domains
	// accountSpy is told every non-zero delta Account applied.
	accountSpy func(index.HeldDelta)
	// state is the State newCore continues from; the zero State builds fresh.
	state State
	// afterAppend runs in WriteAt right after a record is appended and before
	// it is published, with the file's append domain held.
	afterAppend func()
	// beforeResolve runs in ReadAt after the as-of is read and the file's
	// append domain released, before the span is resolved.
	beforeResolve func()
	// beforeRead runs in ReadAt after the span is resolved and the index
	// released, before the first record is read from storage.
	beforeRead func()
	// afterCorrupt runs in ReadAt after Corrupt answered a record that did not
	// verify and before the read goes on from that answer.
	afterCorrupt func()
	// extentLimit overrides the index's entry bound a test core builds with,
	// keeping Deps at the figure the production wiring passes.
	extentLimit int64
	// maxBytes is the journal's maximum the core's admission consults, zero
	// meaning unbounded — the same reading Deps.MaxBytes itself carries.
	maxBytes int64
	// segmentCap replaces the test helper's segment size, so a test can write
	// a record larger than the default segment holds.
	segmentCap int64
	// noLimit keeps the test helper's generous version limit off the core, so
	// a test can hold a fresh core that issues nothing until it sets one.
	noLimit bool
}

// Core is the journal's assembled write and read paths: the placement index,
// the segment streams, the durability layer over them, and the counters that
// follow the index's held-state deltas.
//
// decision: a Core owns no lock of its own. The one domain it uses is the file
// append domain, taken through its domain table and held across a file's
// append and publish, which includes I/O; its counters are atomics. Revisit if
// a core-wide invariant ever needs a mutex of its own.
type Core struct {
	index    *index.Index
	counters *index.Counters
	set      *segment.Set
	pool     *segment.Pool
	cache    *segment.Descriptors
	streams  *segment.Streams
	dur      *durability.Durability
	domains  durability.Domains
	cap      *capacity.Capacity
	segCap   int64               // the set's segment cap, what the streams roll at and the admission sizes by
	retirer  *segment.Retirer    // the retirement executor
	sealer   *segment.IdleSealer // the idle sealer; nil when Deps.IdleSealBytes is zero

	held, dirty, retained atomic.Int64
	writtenBytes          atomic.Uint64
	accountSpy            func(index.HeldDelta)

	// afterAppend is a test seam that holds a write in the gap between its
	// append and its publish.
	//
	// decision: it is nil outside tests, where newCore is only reached through
	// New with no options. It is a field of the core and not of Deps so that no
	// production wiring can set it.
	afterAppend func()

	// beforeResolve is a test seam that holds a read between reading its
	// as-of and resolving its span. It is nil outside tests, for the same
	// reason afterAppend is.
	beforeResolve func()

	// beforeRead is a test seam that holds a read between resolving its span
	// and reading its records. It is nil outside tests, for the same reason
	// afterAppend is.
	beforeRead func()

	// afterCorrupt is a test seam that holds a read between Corrupt's answer
	// and the read of a copy it named. It is nil outside tests, for the same
	// reason afterAppend is.
	afterCorrupt func()

	// registry is the open handles, replaced whole under registryMu and read
	// through the pointer alone. See shares.go.
	registry   atomic.Pointer[[]*Share]
	registryMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
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
func New(deps Deps, state State) (*Core, error) { return newCore(deps, state, coreOptions{}) }

func newCore(deps Deps, state State, o coreOptions) (*Core, error) {
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

	c := &Core{index: state.Index, counters: state.Counters, set: state.Set, segCap: state.Set.Cap(), accountSpy: o.accountSpy, afterAppend: o.afterAppend, beforeResolve: o.beforeResolve, beforeRead: o.beforeRead, afterCorrupt: o.afterCorrupt}
	var domains durability.Domains = newFileLocks()
	if o.wrapDomains != nil {
		domains = o.wrapDomains(domains)
	}
	c.domains = domains

	// The durability layer is built first of the storage-touching pieces,
	// because its Observe wrap is the storage every later part must reach
	// the directory through: the first EROFS from any call the maker, the
	// descriptors or the retirer make turns the journal read-only, and a
	// call the wrap does not see cannot. Its Config needs no maker, pool or
	// cache — the index, the counters, the domains and Account are all of
	// the core's own — so nothing downstream constrains this order.
	d, err := durability.New(durability.Config{
		Clock:    deps.Clock,
		Domains:  domains,
		Tracer:   deps.Tracer,
		Log:      deps.Log,
		Index:    c.index,
		Account:  c.Account,
		Dir:      deps.Dir,
		Counters: c.counters,
		// The durability layer hands the delta Lose returns to Account itself.
		MarkLoss: c.index.Lose,
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
	makerBytes := unboundedBytes
	if maxBytes > 0 {
		makerBytes = maxBytes
	}
	maker := segment.NewMaker(fs, state.Set, makerBytes, 0, deps.Log)
	// The pool asks the read-only check before each spare it makes, so a
	// read-only journal makes none (RFC 1 §5.4).
	c.pool = segment.NewPool(maker, deps.Targets, d.Admit, deps.Log)
	c.cache = segment.NewDescriptors(fs, format.SegmentName, budget)

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
	// The segment cap the admission sizes its pieces and spares by is the
	// set's own, not Deps': the streams roll and the reader bounds a record
	// by the set the State carries, so a State that rebuilds a set of
	// another size must have its admission agree with what its streams do.
	c.cap = capacity.New(capacity.Config{
		Admit:        d.Admit,
		CheckIndex:   state.Index.CheckWrite,
		VersionsLeft: state.Counters.VersionsLeft,
		Footprint:    func() (used int64) { return state.Set.Counters().UsedBytes },
		ShareFigures: state.Set.ShareBytesAndRetired,
		Room:         func(stream int) int64 { return c.streams.Stream(stream).Room() },
		ReadySpares:  c.pool.ReadyContent,
		Hold:         c.pool.Hold,
		Tracer:       deps.Tracer,
		MaxBytes:     maxBytes,
		SegmentCap:   state.Set.Cap(),
	})
	c.streams = segment.NewStreams(deps.Streams, deps.Identity, state.Set, c.pool, c.cache, d.Sink)

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
		Log:      deps.Log,
	})
	c.retirer.Start()

	if deps.IdleSealBytes > 0 {
		c.sealer = segment.NewIdleSealer(c.streams, deps.Clock, deps.IdleSealBytes, d.Admit, deps.Log)
		c.sealer.Start()
	}
	c.pool.Start()
	d.Start(c.streams)
	return c, nil
}

// Index returns the placement index.
func (c *Core) Index() *index.Index { return c.index }

// Durability returns the durability layer.
func (c *Core) Durability() *durability.Durability { return c.dur }

// Streams returns the append streams.
func (c *Core) Streams() *segment.Streams { return c.streams }

// WrittenBytes returns the payload bytes WriteAt has published since the core
// was built: the written_bytes counter.
func (c *Core) WrittenBytes() uint64 { return c.writtenBytes.Load() }

// ReservedBytes returns what open reservations hold and their records have
// not yet filled: the gauge of the writes a limit admitted and whose pieces
// are still landing.
func (c *Core) ReservedBytes() int64 { return c.cap.ReservedBytes() }

// ReservationRefused returns the reservation_refused counter: how many
// reservations the journal's admission has refused, by every limit it names,
// since the core was built.
func (c *Core) ReservationRefused() uint64 { return c.cap.RefusedCount() }

// Retired lowers the share's figure by the retirements its segments have
// freed since the figure last took them into account, and is what a write
// through the retired segment's share waits on: a retry after the
// retirement finds the room it freed. The retirement executor calls it once
// per retirement, after the set's own counters have moved, with the tag
// whose segment was freed; the admission reads the tag's figures off the
// set itself. It is safe from any goroutine.
func (c *Core) Retired(tag core.ShareTag) { c.cap.Retired(tag) }

// Retirer returns the retirement executor the core runs. Nothing above the
// core reaches it through this accessor today — the core's own Close uses
// the field — so it is a test seam: the one a test retires through, driving
// the executor's gates and retired-segment hook exactly as the assembled
// journal's own retirement path does. A later reclamation caller above the
// core, which none exists yet, would take it from here.
func (c *Core) Retirer() *segment.Retirer { return c.retirer }

// retiredSegment is the retirement executor's retired-segment hook: it runs
// once per completed retirement, with the retired segment's id alone, after
// the segment has left the set and holding no lock of the executor's, so it
// must be safe for concurrent calls.
//
// decision: one retirement calls both Durability.Retired and Core.Retired for
// every currently registered share's tag, because the executor's hook fires
// with the segment's id only — after the segment left the set, which share's
// records it held is no longer written anywhere the hook can read — so the
// share whose limit the retirement freed is not known there. Durability.Retired
// is not tag-dependent: it drops the copy table's entries in the segment and
// marks its damaged row retired. Core.Retired is applied to every tag because
// it subtracts only the positive delta of the tag's cumulative retired bytes
// over the baseline the capacity already holds: a tag whose segment was not
// this one computes a delta of zero and is told nothing, so calling it for
// every registered tag is exact and costs one figure read per tag. The tags
// are read off the registry's atomic pointer, and a share attached after the
// hook read it seeds its figure from the set's pair anyway — the baseline the
// seed takes in already carries the retirement — so no tag is missed. A share
// closed concurrently is unregistered but its Retired call is exact the same
// way. Overturned by an executor whose hook carries the tag, which would
// name the one share directly.
func (c *Core) retiredSegment(id core.SegmentID) {
	c.dur.Retired(id)
	for _, tag := range c.Shares() {
		c.Retired(tag)
	}
}

// Close stops the durability layer, then closes the streams, then stops the
// idle sealer, the pool and the retirer — every background worker joined
// before the segments it touches are closed, the pool before the retirer, as
// the segment package's group close documents — and last shuts the descriptor
// cache, whose Shutdown waits for every reader's reference and then closes
// every descriptor. It is safe to call twice.
func (c *Core) Close() error {
	c.closeOnce.Do(func() {
		c.dur.Stop()
		serr := c.streams.Close()
		if c.sealer != nil {
			c.sealer.Stop()
		}
		c.pool.Stop()
		if c.retirer != nil {
			c.retirer.Stop()
		}
		cerr := c.cache.Shutdown()
		c.closeErr = errors.Join(serr, cerr)
	})
	return c.closeErr
}
