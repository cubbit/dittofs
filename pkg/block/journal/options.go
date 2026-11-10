package journal

import (
	"log/slog"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// The journal's clock, settings and descriptor budget, declared in
// internal/core because the internal packages read them. Each one's contract
// is documented at its declaration.
type (
	Clock            = core.Clock
	Settings         = core.Settings
	DescriptorBudget = core.DescriptorBudget
)

// SyncBound is the longest a record waits to be made durable by a sync of its
// own accord, when no Sync names its file.
const SyncBound = core.SyncBound

// NewDescriptorBudget returns a budget of limit open descriptors, shared by
// every journal opened with it. A limit of zero means a quarter of the
// process's open-file limit.
func NewDescriptorBudget(limit int) *DescriptorBudget {
	return core.NewDescriptorBudget(limit)
}

// Option configures Init, Open or Restamp. A zero Option is ignored.
//
// decision: options are appended to the calls' signatures as a variadic
// parameter, so a call written with only the parameters the journal's
// contract names still compiles. Every option but the clock has a default;
// Open refuses to run without a clock. The function an option applies is
// unexported, so only code inside the journal's tree can write an option.
type Option struct {
	apply func(*core.Config)
}

// WithClock makes the journal read time from c. Open requires it: a journal
// never falls back to the system clock on its own, so a test that forgets its
// clock fails at once instead of running on real time.
func WithClock(c Clock) Option {
	return Option{func(cfg *core.Config) { cfg.Clock = c }}
}

// WithStorage makes the journal reach its directory and files through fs
// instead of the operating system's. Its parameter type is internal, so only
// code inside the journal's tree, such as the crash-simulating seam its tests
// use, can build a value to pass. Without it the journal uses the operating
// system's files. The journal does no file I/O yet, so it does not read fs
// today.
func WithStorage(fs storage.FS) Option {
	return Option{func(cfg *core.Config) { cfg.Storage = fs }}
}

// WithSettings sets the journal's settings. Without it every setting takes its
// default.
func WithSettings(s Settings) Option {
	return Option{func(cfg *core.Config) { cfg.Settings = s }}
}

// WithDescriptorBudget makes the journal hold its segment descriptors under b,
// a budget shared with the other journals of the process. Without it the
// journal gets a budget of its own, at the default limit.
func WithDescriptorBudget(b *DescriptorBudget) Option {
	return Option{func(cfg *core.Config) { cfg.Budget = b }}
}

// WithTracer makes the journal trace its operations through t. Without it the
// journal traces nothing.
func WithTracer(t Tracer) Option {
	return Option{func(cfg *core.Config) { cfg.Tracer = t }}
}

// WithLogger makes the journal log through l. Without it, or with a nil l,
// the journal logs nothing.
func WithLogger(l *slog.Logger) Option {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	return Option{func(cfg *core.Config) { cfg.Logger = l }}
}

// WithSparesReady makes Open call f with the opened journal's spare-pool wait
// before it returns, so the caller — a test fixture, whose journal is real
// after Open hands it over while the pool's first spares are still being made
// in the background — can block until the pool settles at the targets Open
// itself sized. The wait reaches Open's own core, whose targets are the ones
// its build sized, so the sizing has one home.
//
// decision: the parameter is a callback over an internal type, as WithStorage's
// is, so only code inside the journal's tree can write one and no public
// method joins Journal or its value types. Withdrawn when the wait moves to a
// surface a caller can reach without a hook — a Stats that reports the ready
// spares, or a WriteAt that waits rather than answering retry — by whoever
// lands one; the other end then carries the fixture's wait.
func WithSparesReady(f func(core.SparesWait)) Option {
	return Option{func(cfg *core.Config) { cfg.SparesReady = f }}
}
