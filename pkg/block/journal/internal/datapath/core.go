package datapath

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

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
	// Descriptors bounds the open segment descriptors; nil means a budget of
	// 64.
	Descriptors *core.DescriptorBudget
	Targets     segment.Targets
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
	// noLimit keeps the test helper's generous version limit off the core, so
	// a test can hold a fresh core that issues nothing until it sets one.
	noLimit bool
}

// Core is the journal's assembled write path: the placement index, the
// segment streams, the durability layer over them, and the counters that
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
	if state.Index == nil {
		state.Index = index.New(deps.ExtentLimit)
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

	c := &Core{index: state.Index, counters: state.Counters, set: state.Set, accountSpy: o.accountSpy, afterAppend: o.afterAppend, beforeResolve: o.beforeResolve}
	var domains durability.Domains = newFileLocks()
	if o.wrapDomains != nil {
		domains = o.wrapDomains(domains)
	}
	c.domains = domains

	maker := segment.NewMaker(deps.FS, state.Set, unboundedBytes, 0, deps.Log)
	c.pool = segment.NewPool(maker, deps.Targets, nil, deps.Log)
	c.pool.Start()
	c.cache = segment.NewDescriptors(deps.FS, format.SegmentName, budget)

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
		_ = c.cache.Shutdown()
		c.pool.Stop()
		return nil, err
	}
	c.dur = d
	c.streams = segment.NewStreams(deps.Streams, deps.Identity, state.Set, c.pool, c.cache, d.Sink)
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

// Close stops the durability layer, then closes the streams, then shuts the
// descriptor cache and stops the pool. It is safe to call twice.
func (c *Core) Close() error {
	c.closeOnce.Do(func() {
		c.dur.Stop()
		serr := c.streams.Close()
		cerr := c.cache.Shutdown()
		c.pool.Stop()
		c.closeErr = errors.Join(serr, cerr)
	})
	return c.closeErr
}
