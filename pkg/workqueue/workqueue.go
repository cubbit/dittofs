// Package workqueue is the shared work scheduler (RFC 8 §6.1.1): the library
// that dispatches a component's background work, the engine's offload, removals
// and prunes, and GC's deleter, compactor, audit and collection. It is a
// library, not a component: each component runs its own instance, so a stalled
// GC never holds up offload, and the package imports only the standard library,
// so no component reaches another through it (RFC 0 §1.2).
//
// A source supplies the work and owns its durability; the scheduler holds no
// durable state. A tenant is the unit within a source that fairness,
// reservations and cancellation apply to: a shard for the engine, a namespace
// for GC.
//
// Only the surface exists yet: the calls that dispatch answer that they
// cannot, and nothing runs.
package workqueue

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Key names one entry; posts for one key merge.
type Key struct {
	Tenant TenantID // the engine's: a shard; GC's: a namespace
	Kind   uint8    // the engine's: offload, removal or prune
	ID     [16]byte // the engine's: a file ID (RFC 16's 16-byte FileID), as it is; GC's: its prefix, in at most 16 bytes
}

// Item is one unit of work a source posts and a Run receives.
type Item struct {
	Key       Key
	NotBefore time.Time // zero: now
	Cost      int64     // in Limits.Rate's unit
	Expired   bool      // set by the scheduler: Deadline passed, and this is the last dispatch
}

// Outcome is what a Run reports for one item. The outcome alone decides what
// happens to the item's key next.
type Outcome uint8

const (
	// Done: the key leaves, or is pending again at once if a post marked it
	// while it ran. Done resets the key's backoff.
	Done Outcome = iota
	// Retry: the key backs off, with jitter, doubling from its tenant's
	// BackoffMin to BackoffMax.
	Retry
	// Throttled backs the key off as Retry does. Once Rate pacing lands, its
	// tenant's pace also drops until the tenant's next Done.
	Throttled
	// StopTenant acts as CancelTenant for the item's tenant.
	StopTenant
)

// Result is one item's report from a Run.
type Result struct {
	Outcome Outcome
	Err     error // reported; the outcome alone decides what follows
}

// Source is one kind of background work. The scheduler holds no durable state;
// the source rebuilds a tenant's pending work from its own durable records, and
// re-derives from them any post the scheduler dropped.
type Source interface {
	// Rebuild posts t's pending work.
	Rebuild(ctx context.Context, t TenantID, post func(Item)) error
	// Run works one batch; done is called once per item, as each finishes. An
	// item Run returns without done is a Retry.
	Run(ctx context.Context, batch []Item, done func(i int, r Result))
}

// Limits sizes a source's slots and reservation. It is fixed for the source's
// life: Register takes it once.
type Limits struct {
	Batch       int           // items per Run
	Concurrency int           // Runs in flight for the source
	Rate        float64       // Cost per second; 0 is unlimited
	Weight      int           // the source's share of dispatch under contention
	Total       int           // the source's reservation of the instance's fixed total
	RunTimeout  time.Duration // the deadline on every Run's ctx
	Deadline    time.Duration // longest one item is retried; 0: for as long as the source asks
}

// Tenant is one tenant's settings within its source. It is live: SetTenant
// changes it, so a change to the backoff reaches every tenant without a
// restart.
type Tenant struct {
	Weight                 int           // its share of its source
	BackoffMin, BackoffMax time.Duration // jittered exponential backoff per key
	Paused                 bool          // dispatch none of its entries; keep them, and grow no backoff
}

// Stats is one tenant's figures within its source.
type Stats struct {
	Pending, Backoff, Running, Overdue int
	OldestPending                      time.Duration
	Dropped, Expired                   uint64
	LastDone                           time.Time
}

// TenantID names a tenant: a namespace as it is, or a shard big-endian in the
// last four bytes.
type TenantID [16]byte

// SourceID is a registration's index.
type SourceID int

// Clock is the subset of the node's clock (RFC 8 §2.1) the scheduler reads.
type Clock interface {
	Now() time.Time
	Elapsed() time.Duration
	After(d time.Duration) <-chan struct{}
}

// Loop is the watchdog turn the scheduler reports (RFC 25 §3.5), declared here
// so the package imports no implementation; RFC 25's *health.Loop satisfies it
// as it is. Whoever took the loop from the watchdog closes it once Stop has
// returned.
type Loop interface {
	Turn() // a dispatch or a run's return completed
	Idle() // waiting, with no entry due
}

// Scheduler is one instance of the work scheduler: one per journal for the
// engine's sources, one per process for GC's.
type Scheduler struct {
	sources int
}

// New returns a scheduler reading clk, the subset of the node's clock it
// needs, and reporting its turns on loop.
func New(clk Clock, loop Loop) *Scheduler {
	return &Scheduler{}
}

// Register adds src with its limits, which are fixed for the source's life, and
// returns its ID. It is called before Start.
//
// decision: Register panics on limits it cannot honour rather than returning an
// error, because every caller passes constants of its own code, so a bad value
// is a programming error. A value is never replaced by a default. Withdraw the
// panic for an error if a Limits value ever comes from a setting.
func (s *Scheduler) Register(src Source, l Limits) SourceID {
	if err := l.check(); err != nil {
		panic(err)
	}

	id := SourceID(s.sources)
	s.sources++

	return id
}

// check names the first field of l the scheduler cannot honour.
func (l Limits) check() error {
	for _, f := range []struct {
		name string
		bad  bool
		want string
	}{
		{"Batch", l.Batch < 1, "at least 1"},
		{"Concurrency", l.Concurrency < 1, "at least 1"},
		// Pacing a positive Rate is not built, and NaN is not 0.
		{"Rate", l.Rate != 0, "0: positive-rate pacing is not supported"},
		{"Weight", l.Weight < 1, "at least 1"},
		{"Total", l.Total < 1, "at least 1"},
		// RunTimeout is every Run's deadline, and 0 would name none.
		{"RunTimeout", l.RunTimeout <= 0, "positive"},
		{"Deadline", l.Deadline < 0, "0 or positive"},
	} {
		if f.bad {
			return fmt.Errorf("workqueue: Limits.%s must be %s", f.name, f.want)
		}
	}

	return nil
}

// Start begins dispatch.
func (s *Scheduler) Start() {}

// SetTenant adds t to src, running src's Rebuild for it, or changes its
// settings.
func (s *Scheduler) SetTenant(ctx context.Context, src SourceID, t TenantID, c Tenant) error {
	return errors.ErrUnsupported
}

// Post adds or merges it. It never blocks. False means refused, or dropped and
// counted: the source re-derives dropped work from its durable records.
func (s *Scheduler) Post(src SourceID, it Item) bool {
	return false
}

// CancelTenant drops t's entries in src and cancels its Runs; it refuses t's
// posts until SetTenant.
func (s *Scheduler) CancelTenant(src SourceID, t TenantID) {}

// WaitTenant returns once every Run of t in src that was in flight at the call
// has returned.
func (s *Scheduler) WaitTenant(ctx context.Context, src SourceID, t TenantID) error {
	return errors.ErrUnsupported
}

// Stats returns src's figures per tenant.
func (s *Scheduler) Stats(src SourceID) map[TenantID]Stats {
	return nil
}

// Stop refuses dispatch, cancels every Run and joins them.
func (s *Scheduler) Stop(ctx context.Context) error {
	return errors.ErrUnsupported
}
