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

// Share is a share's registration with the core.
//
// decision: a Share holds no lock domain; it only carries the tag its writes
// publish under. Revisit when a share gains state that writers must serialize.
type Share struct{ Tag core.ShareTag }

// coreOptions are the test-only adjustments newCore applies before the layers
// above the domain table are built.
type coreOptions struct {
	// wrapDomains replaces the file append domain table the core and the
	// durability layer both use.
	wrapDomains func(durability.Domains) durability.Domains
	// accountSpy is told every non-zero delta Account applied.
	accountSpy func(index.HeldDelta)
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
	accountSpy            func(index.HeldDelta)

	closeOnce sync.Once
	closeErr  error
}

// New assembles a Core over deps, continuing from state, and starts it.
func New(deps Deps, state State) (*Core, error) { return newCore(deps, state, coreOptions{}) }

func newCore(deps Deps, state State, o coreOptions) (*Core, error) {
	if state.Set == nil {
		state.Set = segment.NewSet(deps.SegmentCap, deps.Log)
	}
	if state.Index == nil {
		state.Index = index.New(deps.ExtentLimit)
	}
	if state.Counters == nil {
		state.Counters = &index.Counters{}
		if err := state.Counters.SetVersionLimit(core.Version{Counter: 1 << 62}); err != nil {
			return nil, err
		}
	}
	budget := deps.Descriptors
	if budget == nil {
		budget = core.NewDescriptorBudget(64)
	}

	c := &Core{index: state.Index, counters: state.Counters, set: state.Set, accountSpy: o.accountSpy}
	var domains durability.Domains = newFileLocks()
	if o.wrapDomains != nil {
		domains = o.wrapDomains(domains)
	}
	c.domains = domains

	maker := segment.NewMaker(deps.FS, state.Set, 1<<62, 0, deps.Log)
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

// AddShare registers a share and returns its handle.
func (c *Core) AddShare(tag core.ShareTag) *Share { return &Share{Tag: tag} }

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
