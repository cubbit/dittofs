// Package sidecar is the runner that drives the node's sidecars: it wraps
// each adapter's lifecycle calls with the Syncs of the sidecars serving it,
// in the order RFC 20 section 3.5 states, and retries a sidecar whose Sync
// failed until one succeeds. The concrete sidecars, one package each under
// this one, are later epics'; nothing here imports an adapter.
package sidecar

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// The retry's backoff: 1 s doubling to 30 s, the times RFC 20 section 3.5
// names. The waits run on the runner's clock, which a test replaces through
// export_test.go.
const (
	retryBase = 1 * time.Second
	retryMax  = 30 * time.Second
)

// clock is what the retry's waits run on: the runtime's, until a test
// replaces it through export_test.go.
type clock interface {
	AfterFunc(d time.Duration, f func()) timer
}

type timer interface{ Stop() bool }

type runtimeClock struct{}

func (runtimeClock) AfterFunc(d time.Duration, f func()) timer { return time.AfterFunc(d, f) }

// Runner drives the sidecars it was built with around the adapters they
// serve (RFC 20 section 3.5). Each of its calls carries one adapter's
// lifecycle call, wrapped: the sidecars serving the adapter are synced after
// it is serving, before a Reconfigure that removes an address (that address
// left out), again after every Reconfigure returns, whatever it returned,
// and before the adapter starts draining. A disabled sidecar is synced with
// the empty set and skipped until it is enabled again. A failing Sync fails
// only its own sidecar: Failed names it, the adapters keep serving, and the
// Sync is called again with the sidecar's last set after the backoff until
// one succeeds. Close ends the retries.
//
// decision: the runner wraps the adapter calls it orders, and each Sync runs
// under the caller's context, a retry's under a context Close ends.
type Runner struct {
	ctx    context.Context
	cancel context.CancelFunc
	clock  clock
	wg     sync.WaitGroup // the retry goroutines, joined at Close

	// retryRead runs once a retry has read the failed answer and the set it
	// carries, before it syncs them: a test's hook, set from export_test.go,
	// nil in production.
	retryRead func()

	mu       sync.Mutex
	serving  map[string]adapter.Adapter // by the name Start was called with
	sidecars []*sidecar                 // New's order
}

// sidecar is one of the Runner's with the state its syncs and retries keep.
// failed, retrying, enabled and last sit under r's mutex; mu is held across
// every Sync, so a retry never overlaps a sync the runner drives.
type sidecar struct {
	adapter.Sidecar
	mu       sync.Mutex
	enabled  bool
	failed   bool
	retrying bool
	last     []adapter.Serving // the last set it was to be synced with
}

// New builds the Runner that drives ss. Each sidecar is enabled until
// SetEnabled disables it; the Runner ends with Close.
func New(ss ...adapter.Sidecar) *Runner {
	r := &Runner{serving: map[string]adapter.Adapter{}, clock: runtimeClock{}}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for _, sc := range ss {
		r.sidecars = append(r.sidecars, &sidecar{Sidecar: sc, enabled: true})
	}
	return r
}

// Start carries a's start, wrapped: once it succeeds, every enabled sidecar
// serving name is synced with the set of its serving adapters (RFC 20
// section 3.5).
func (r *Runner) Start(ctx context.Context, name string, a adapter.Adapter) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.serving[name] = a
	r.mu.Unlock()
	for _, sc := range r.enabledFor(name) {
		_ = r.sync(ctx, sc, func() []adapter.Serving { return r.servs(sc.Adapters()) })
	}
	return nil
}

// Reconfigure carries a's reconfigure, wrapped (RFC 20 section 3.5): the
// enabled sidecars serving name are synced first with the addresses next
// drops left out of the adapter's own Serving, so nothing is advertised the
// adapter is about to unservice, and again after the call returns, whatever
// it returned, with the set the adapter's Addrs reports then. The call's own
// error is the return; a failing Sync never fails it.
func (r *Runner) Reconfigure(ctx context.Context, name string, a adapter.Adapter, next settings.Settings) error {
	drop := dropped(a, next)
	syncs := r.enabledFor(name)
	if len(drop) > 0 {
		for _, sc := range syncs {
			_ = r.sync(ctx, sc, func() []adapter.Serving { return without(r.servs(sc.Adapters()), name, drop) })
		}
	}
	err := a.Reconfigure(ctx, next)
	for _, sc := range syncs {
		_ = r.sync(ctx, sc, func() []adapter.Serving { return r.servs(sc.Adapters()) })
	}
	return err
}

// Stop carries a's stop, wrapped (RFC 20 section 3.5): the enabled sidecars
// serving name are synced with the adapter's Serving left out before the
// adapter starts draining, so nothing is advertised that the adapter has
// stopped answering.
func (r *Runner) Stop(ctx context.Context, name string, a adapter.Adapter) error {
	r.mu.Lock()
	delete(r.serving, name)
	r.mu.Unlock()
	for _, sc := range r.enabledFor(name) {
		_ = r.sync(ctx, sc, func() []adapter.Serving { return r.servs(sc.Adapters()) })
	}
	return a.Stop(ctx)
}

// SetEnabled applies a sidecar's live enabled setting: a disabled one is
// synced with the empty set and skipped by the Runner's own syncs until it
// is enabled again, which syncs it with the set of its serving adapters
// (RFC 20 section 3.5). A name no sidecar carries is refused.
func (r *Runner) SetEnabled(ctx context.Context, name string, on bool) error {
	r.mu.Lock()
	var sc *sidecar
	for _, s := range r.sidecars {
		if s.Name() == name {
			sc = s
			break
		}
	}
	if sc == nil {
		r.mu.Unlock()
		return fmt.Errorf("sidecar runner: no sidecar %q", name)
	}
	changed := sc.enabled != on
	sc.enabled = on
	r.mu.Unlock()
	if !changed {
		return nil
	}
	// The set is built under sc's mutex, where sync re-reads the flag: a
	// disable whose sync lands past a concurrent enable re-reads enabled and
	// advertises, instead of landing a nil built ahead of the flag's last
	// answer, and a disable that is still the last answer lands its nil (RFC
	// 20 section 3.5).
	_ = r.sync(ctx, sc, func() []adapter.Serving { return r.servs(sc.Adapters()) })
	return nil
}

// Failed names the sidecars whose last Sync failed, in New's order: the node
// raises each as sidecar_failed until one of its Syncs succeeds (RFC 20
// sections 3.5 and 3.7).
func (r *Runner) Failed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, sc := range r.sidecars {
		if sc.failed {
			names = append(names, sc.Name())
		}
	}
	return names
}

// Close ends the Runner: the retries stop, and a failing sidecar's Sync is
// called no more. It waits any retry still inside its Sync out, so none is
// left running once it returns.
func (r *Runner) Close() {
	r.cancel()
	r.wg.Wait()
}

// sync builds the set it syncs under sc's own mutex, so the sidecar's
// enabled answer and the runner's serving set are read at the mutex's
// front: the last Sync to land cannot carry a set built before one that
// landed before it, and a disabled sidecar is synced with none (RFC 20
// section 3.5).
func (r *Runner) sync(ctx context.Context, sc *sidecar, build func() []adapter.Serving) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	r.mu.Lock()
	on := sc.enabled
	r.mu.Unlock()
	var set []adapter.Serving
	if on {
		set = build()
	}
	return r.syncLocked(ctx, sc, set)
}

// syncLocked is sync's body with sc already held, so a re-sync the retry
// makes and one the Runner drives share sc's mutex and cannot pass one
// another: which set a retry carries and whether the sidecar is still
// failed read behind the same lock that records both.
func (r *Runner) syncLocked(ctx context.Context, sc *sidecar, set []adapter.Serving) error {
	r.mu.Lock()
	sc.last = set
	r.mu.Unlock()
	err := sc.Sync(ctx, set)
	r.mu.Lock()
	sc.failed = err != nil
	if err != nil && !sc.retrying {
		sc.retrying = true
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.retry(sc)
		}()
	}
	r.mu.Unlock()
	return err
}

// retry calls a failed sidecar's Sync again with its last set, after a
// backoff from 1 s doubling to 30 s, until one succeeds or Close ends the
// Runner (RFC 20 section 3.5). A sync the Runner drives while this one waits
// ends it: the sidecar is no longer failed, or the wait carries a newer
// set.
func (r *Runner) retry(sc *sidecar) {
	wait := retryBase
	for {
		wake := make(chan struct{})
		t := r.clock.AfterFunc(wait, func() { close(wake) })
		select {
		case <-wake:
		case <-r.ctx.Done():
			t.Stop()
		}
		if r.ctx.Err() != nil {
			break // Close ended the retries
		}
		if r.retryOnce(sc) {
			break // the retry succeeded, or a driven sync owns the health
		}
		wait = min(2*wait, retryMax)
	}
}

// retryOnce re-syncs sc with its last set, if it is still failed, behind
// sc's mutex: a sync the Runner drives while this one waited cannot be
// passed, so the retry never carries an older set over a newer one, and
// whether this loop leaves is decided where retrying is cleared, so a
// sync failing in that instant still finds its own retry.
func (r *Runner) retryOnce(sc *sidecar) (done bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	r.mu.Lock()
	if !sc.failed {
		sc.retrying = false
		r.mu.Unlock()
		return true // a sync the Runner drove succeeded while this one waited
	}
	set := sc.last
	r.mu.Unlock()
	if r.retryRead != nil {
		r.retryRead()
	}
	if err := r.syncLocked(r.ctx, sc, set); err == nil {
		r.mu.Lock()
		sc.retrying = false
		r.mu.Unlock()
		return true
	}
	return false
}

// enabledFor is the enabled sidecars serving name, in New's order.
func (r *Runner) enabledFor(name string) []*sidecar {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*sidecar
	for _, sc := range r.sidecars {
		if sc.enabled && slices.Contains(sc.Adapters(), name) {
			out = append(out, sc)
		}
	}
	return out
}

// servs is the set a sidecar is to be synced with: one Serving per of its
// adapters now serving, each built from that adapter's own Addrs (RFC 20
// section 3.5).
func (r *Runner) servs(names []string) []adapter.Serving {
	r.mu.Lock()
	type serving struct {
		name string
		a    adapter.Adapter
	}
	var out []serving
	for _, n := range names {
		if a, ok := r.serving[n]; ok {
			out = append(out, serving{n, a})
		}
	}
	r.mu.Unlock()
	set := make([]adapter.Serving, len(out))
	for i, n := range out {
		set[i] = adapter.Serving{Adapter: n.name, Listeners: n.a.Addrs()}
	}
	return set
}

// dropped is which of the addresses a holds next does not name: what a
// Reconfigure that reaches the adapter would remove.
func dropped(a adapter.Adapter, next settings.Settings) []netip.Addr {
	bound := map[netip.Addr]struct{}{}
	for _, ats := range a.Addrs() {
		for _, at := range ats {
			bound[at.Addr()] = struct{}{}
		}
	}
	var drop []netip.Addr
	for at := range bound {
		if !slices.Contains(next.Common().Addrs, at) {
			drop = append(drop, at)
		}
	}
	return drop
}

// without is the serving set with each of drop left out of the Serving of
// the adapter named name.
func without(set []adapter.Serving, name string, drop []netip.Addr) []adapter.Serving {
	out := make([]adapter.Serving, len(set))
	for i, s := range set {
		if s.Adapter != name {
			out[i] = s
			continue
		}
		ls := make(map[adapter.Socket][]netip.AddrPort, len(s.Listeners))
		for k, addrs := range s.Listeners {
			keep := addrs[:0:0]
			for _, at := range addrs {
				if !slices.Contains(drop, at.Addr()) {
					keep = append(keep, at)
				}
			}
			ls[k] = keep
		}
		out[i] = adapter.Serving{Adapter: s.Adapter, Listeners: ls}
	}
	return out
}
