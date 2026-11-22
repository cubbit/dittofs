package vfs

import (
	"container/list"
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// callerKey is everything a cached caller was resolved from but the State:
// Resolve's arguments whole, the identity's by its proof key, and the
// client's address where a client rule decided the call. The State's serial
// is the caller's own Built, which a lookup compares with the current one.
// The key is built beside the one Resolve call it stands for, and that call
// reads its arguments from it, so a field added to SharePolicy is keyed with
// no edit here; a field added to Identity is an edit to Identity.Key.
//
// decision: the cache holds resolution's result alone. Admission's share
// state, minimums and client rules run on every call, so the key holds their
// output, narrowed, and not the netgroups' or the share list's version, which
// a cache of those steps too would need. Key those versions if the cache ever
// holds what the client rules decide.
type callerKey struct {
	proof    identity.ProofKey
	policy   entity.SharePolicy
	narrowed identity.ReadOnlyCause
	client   netip.Addr // the zero Addr where no client rule decided
}

// The caller cache's bounds, and how long one resolution may run on behalf
// of the calls waiting for it.
const (
	maxCallers      = 262144
	maxCallerGroups = 1 << 24
	resolveBound    = 20 * time.Second
)

// callers is admission's cache of resolved callers: the only one, as
// identity keeps none. An entry is stored only while the serial of the State
// its caller was built under is current, and served only while it still is,
// so no caller built under an older State is stored or served. No entry is
// served at or after its caller's Fresh. Concurrent misses of one key share
// one resolution.
//
// ponytail: one serial for every principal, so one change anywhere empties
// the whole cache, and the entries under an older serial no call looks up
// again wait for the least recently used end to evict them. Key entries on
// per-principal versions, pushed rather than polled, when a profile shows
// resolution misses after State changes at the reference box's call rate.
//
// ponytail: the bounds are fixed counts, 262 144 callers and 2²⁴ group
// references. Charge the cache to RFC 24's connection-state pool, past its
// share evicting as now, once that pool exists.
type callers struct {
	resolver   identity.Resolver
	now        func() time.Time
	maxEntries int
	maxGroups  int

	hits, misses, evictions prometheus.Counter

	mu      sync.Mutex
	entries map[callerKey]*list.Element // each holding a *cachedCaller
	lru     list.List                   // most recently used first
	groups  int                         // the group references the entries hold
	flights map[callerKey]*flight       // the resolutions running, by key

	beforeStore func(context.Context) // run with the resolution's context once it answered, before its caller is stored; nil but in tests
	waiting     func()                // run as a missing call starts waiting on another's flight; nil but in tests
}

type cachedCaller struct {
	key    callerKey
	caller *identity.Caller
}

// flight is one resolution the misses of one key wait on, run by the call
// that missed first on its own goroutine. serial is the State's serial that
// call read before its lookup; c and err are set before done is closed.
//
// decision: the resolution runs on the goroutine of the call that started
// it, never on one of its own, so the goroutines resolving are the admitted
// calls' and no more. That call waits out its own cancel, up to
// resolveBound, as the others are served what it resolves; a panic in it
// unwinds through that call's recovery, and the flight answers the others
// ErrUnavailable. Run it elsewhere only on a goroutine pool the process
// already counts.
//
// decision: the flights are a map on callerKey itself, not x/sync's
// singleflight, whose string key would be an encoding of callerKey that no
// compiler check holds whole: a field it dropped would hand one caller's
// resolution to another's call. Use singleflight if a key encoding is ever
// shown whole by a test as Identity.Key is.
type flight struct {
	done   chan struct{}
	serial uint64
	c      *identity.Caller
	err    error
}

func newCallers(r identity.Resolver, now func() time.Time, maxEntries, maxGroups int) *callers {
	counter := func(name, help string) prometheus.Counter {
		return prometheus.NewCounter(prometheus.CounterOpts{Namespace: "dittofs", Name: name, Help: help})
	}
	return &callers{
		resolver: r, now: now, maxEntries: maxEntries, maxGroups: maxGroups,
		hits:      counter("vfs_caller_cache_hits_total", "Admitted calls served a cached caller."),
		misses:    counter("vfs_caller_cache_misses_total", "Admitted calls that found no cached caller, each then resolving or joining a resolution running."),
		evictions: counter("vfs_caller_cache_evictions_total", "Cached callers evicted, least recently used first, past the cache's bounds."),
		entries:   map[callerKey]*list.Element{},
		flights:   map[callerKey]*flight{},
	}
}

func (cs *callers) collectors() []prometheus.Collector {
	return []prometheus.Collector{cs.hits, cs.misses, cs.evictions}
}

// resolve answers the caller k stands for: the cached one built under the
// current State, or what resolve, the one Resolve call k was built beside,
// answers. A miss joins the key's flight, or starts one if none runs and
// resolves on its own goroutine, on a context detached from its own and
// bounded at resolveBound, so a starter that gives up leaves the others
// served. A call that joined waits only until its own ctx ends.
func (cs *callers) resolve(ctx context.Context, k callerKey, resolve func(context.Context) (*identity.Caller, error)) (*identity.Caller, error) {
	serial := cs.resolver.Current()
	cs.mu.Lock()
	if c := cs.lookup(k, serial); c != nil {
		cs.mu.Unlock()
		cs.hits.Inc()
		return c, nil
	}
	f := cs.flights[k]
	if f == nil {
		f = &flight{done: make(chan struct{}), serial: serial}
		cs.flights[k] = f
		cs.mu.Unlock()
		cs.misses.Inc()
		cs.fly(context.WithoutCancel(ctx), k, f, resolve)
		return f.c, f.err
	}
	cs.mu.Unlock()
	cs.misses.Inc()
	if cs.waiting != nil {
		cs.waiting()
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// A call that joined the flight after an Apply the flight's starter had
	// not read resolves on its own, unless the flight answered a caller built
	// under that State or a later one, so no call admitted after a State is
	// served a caller, or a refusal, decided under an older one.
	if f.serial < serial && (f.err != nil || f.c.Built < serial) {
		return resolve(ctx)
	}
	return f.c, f.err
}

// fly runs f's resolution, stores what it answers, and answers f. A panic in
// the resolution answers f ErrUnavailable and goes on unwinding.
func (cs *callers) fly(ctx context.Context, k callerKey, f *flight, resolve func(context.Context) (*identity.Caller, error)) {
	f.err = fmt.Errorf("%w: the caller's resolution panicked", vfs.ErrUnavailable)
	defer func() {
		cs.mu.Lock()
		delete(cs.flights, k)
		if f.err == nil {
			cs.store(k, f.c)
		}
		cs.mu.Unlock()
		close(f.done)
	}()
	ctx, cancel := context.WithTimeout(ctx, resolveBound)
	defer cancel()
	c, err := resolve(ctx)
	if err == nil && cs.beforeStore != nil {
		cs.beforeStore(ctx)
	}
	f.c, f.err = c, err
}

// lookup is the entry under k, built under serial and still fresh, moved to
// the front; nil if none. An entry built under an older serial, or past its
// Fresh, is removed; one built under a newer serial than the call read is
// kept for the calls that read it. It runs under cs.mu.
func (cs *callers) lookup(k callerKey, serial uint64) *identity.Caller {
	e := cs.entries[k]
	if e == nil {
		return nil
	}
	c := e.Value.(*cachedCaller).caller
	if stale := !cs.now().Before(c.Fresh); c.Built != serial || stale {
		if c.Built < serial || stale {
			cs.remove(e)
		}
		return nil
	}
	cs.lru.MoveToFront(e)
	return c
}

// store keeps c under k, in place of any caller held there, only while the
// serial of the State c was built under is current and before c's Fresh, so
// a caller whose Fresh is zero, which every instant is past, is never
// stored. It evicts the least recently used past the bounds, counting each
// eviction. It runs under cs.mu.
//
// ponytail: a caller with a zero Fresh, every local principal's and every
// unproven one's, resolves on every call, the cost admission pays without a
// cache, so a disable reaches its next call even before a new State is
// applied. Store it, bounded by the State alone, once every node applies each
// principals generation and a benchmark of local callers shows per-call
// resolution in admission's cost.
func (cs *callers) store(k callerKey, c *identity.Caller) {
	if c.Built != cs.resolver.Current() || !cs.now().Before(c.Fresh) {
		return
	}
	n := len(c.Groups) + len(c.DenyOnly)
	if n > cs.maxGroups {
		return
	}
	if e := cs.entries[k]; e != nil {
		cs.remove(e)
	}
	cs.entries[k] = cs.lru.PushFront(&cachedCaller{key: k, caller: c})
	cs.groups += n
	for cs.lru.Len() > cs.maxEntries || cs.groups > cs.maxGroups {
		cs.remove(cs.lru.Back())
		cs.evictions.Inc()
	}
}

// remove drops e. It runs under cs.mu.
func (cs *callers) remove(e *list.Element) {
	cc := cs.lru.Remove(e).(*cachedCaller)
	delete(cs.entries, cc.key)
	cs.groups -= len(cc.caller.Groups) + len(cc.caller.DenyOnly)
}
