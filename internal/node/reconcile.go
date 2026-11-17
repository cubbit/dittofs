package node

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

const (
	// reconcileInterval is how often reconcile reads the counters.
	reconcileInterval = time.Second
	// storeBound is how long reconcile waits on one store call, on the
	// node's clock.
	storeBound = 20 * time.Second
	// stepReconcile names the first full reconcile and the loop after it.
	stepReconcile = "reconcile"
)

// reconciler is reconcile's state: the running set and the counters the
// node has acted on, and the wake a write on this node sends.
type reconciler struct {
	// wake has one slot, so a wake sent while a pass runs is kept and runs
	// one more pass after it.
	wake chan struct{}
	// paused, when set, is called with the counters each pass read, before
	// the pass acts on them.
	paused func(kvstore.Counters)

	// Read and written by Start's steps and then by the loop alone.
	seen  kvstore.Counters // the counters the last pass acted on
	full  bool             // a read failed: the next pass acts on every kind
	retry bool             // a refusal asked to be retried: the next pass acts on every kind
	wait  time.Duration    // the backoff's last wait, zero once a pass needs none

	mu sync.Mutex
	// pos is the counters the node has reconciled through: acted on every
	// change up to each, applied or refused.
	pos   kvstore.Counters
	moved chan struct{} // closed when pos moves, then replaced
	// bound is the running set: each share as the engine binds it.
	bound map[entity.ShareID]entity.Share
	// shares and applied are the records the last share pass and the last
	// apply refused.
	shares, applied []refusal
}

func newReconciler() reconciler {
	return reconciler{wake: make(chan struct{}, 1), moved: make(chan struct{}), bound: map[entity.ShareID]entity.Share{}}
}

// refusal is a record the node did not apply, and whether to try it again on
// the backoff rather than wait for the record to change.
type refusal struct {
	rec   entity.RefusedRecord
	retry bool
}

// refused is a refusal of share s, its generation the share list version
// its last change raised.
func refused(s entity.Share, retry bool, err error) refusal {
	p := apiv1.Problem{Status: http.StatusUnprocessableEntity, Title: "the share was refused", Detail: err.Error()}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal
	return refusal{retry: retry, rec: entity.RefusedRecord{
		Record:     entity.ResourceRef{Kind: entity.ResourceShare, ID: s.ID[:]},
		Generation: s.Position,
		Error:      b,
	}}
}

// apply hands each component the settings it reads, at counters c, and
// returns the records it refused.
//
// ponytail: apply hands the engine RFC 13 Appendix B's defaults whatever c
// says, as no settings record carries the engine's values yet, and refuses
// nothing. Read the settings and principals generations' records here once
// those carry what a component reads.
func (n *Node) apply(_ context.Context, _ kvstore.Counters) ([]refusal, error) {
	return nil, n.engine.Apply(engineDefaults)
}

// due is what a pass acts on, given the counters it read and those the
// last pass acted on: the shares when the share list or the settings moved,
// as a share's remote store is configured by settings, and apply when the
// settings or the principals moved. After a read that failed, or a refusal
// to retry, it acts on both. A refusal not to retry waits for its record's
// counter to move.
func due(c, seen kvstore.Counters, again bool) (shares, apply bool) {
	if again {
		return true, true
	}
	settings := c.Settings != seen.Settings
	return settings || c.ShareList != seen.ShareList, settings || c.Principals != seen.Principals
}

// planShares is what a pass does with the listed shares over the running
// set bound: the shares to bind, each not bound or bound in another state,
// in the order of the share list versions their changes raised, and the
// shares refused, each whose namespace names no configured remote store. A
// refused share keeps the binding it had, so one serving goes on serving.
// A share no longer listed keeps its binding: its removal unbinds it.
func planShares(bound map[entity.ShareID]entity.Share, listed []entity.Share, remote func(entity.Share) bool) (bind []entity.Share, refusals []refusal) {
	for _, s := range listed {
		if !remote(s) {
			refusals = append(refusals, refused(s, false, fmt.Errorf("share %s: its namespace names no remote store", s.Name)))
			continue
		}
		if b, ok := bound[s.ID]; !ok || b.State != s.State {
			bind = append(bind, s)
		}
	}
	slices.SortFunc(bind, func(a, b entity.Share) int { return cmp.Compare(a.Position, b.Position) })
	return bind, refusals
}

// readShares lists every share, as the node binds them, and reports which
// name a configured remote store.
//
// ponytail: a remote store counts as configured once a setting exists at
// its scope, as no record of a remote store exists; the namespaces and each
// store's settings are read again at every share pass. Read the remote
// store's record once one is written, and keep the namespaces' stores, which
// never change, once a profile shows the reads.
func (n *Node) readShares(ctx context.Context) ([]entity.Share, func(entity.Share) bool, error) {
	type read struct {
		listed     []entity.Share
		stores     map[entity.NamespaceID][16]byte
		configured map[[16]byte]bool
	}
	var got *read
	err := n.bounded(ctx, func(ctx context.Context) error {
		r := read{stores: map[entity.NamespaceID][16]byte{}, configured: map[[16]byte]bool{}}
		for rec, err := range n.meta.Nodes().List(ctx, entity.ResourceShare, nil) {
			if err != nil {
				return err
			}
			r.listed = append(r.listed, rec.Share)
		}
		cp := n.meta.ControlPlane()
		for ns, err := range cp.Namespaces(ctx, entity.NamespaceID{}) {
			if err != nil {
				return err
			}
			r.stores[ns.ID] = ns.Store
			if _, seen := r.configured[ns.Store]; seen {
				continue
			}
			r.configured[ns.Store] = false
			for _, err := range cp.Settings(ctx, entity.SettingScope{Kind: entity.ScopeRemote, ID: ns.Store}) {
				if err != nil {
					return err
				}
				r.configured[ns.Store] = true
				break
			}
		}
		got = &r
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return got.listed, func(s entity.Share) bool {
		st, ok := got.stores[enginewire.Binding(s, 0).Namespace]
		return ok && got.configured[st]
	}, nil
}

// bounded runs f, a store call, with a context that ends once storeBound
// passes on the node's clock, and answers f's error, or a timeout once the
// bound passed without f answering. f's answer is then dropped, and so is
// what it writes after, so a store that ignores its context holds no pass
// past the bound: f writes its results only into what the caller reads once
// bounded answered nil.
func (n *Node) bounded(ctx context.Context, f func(context.Context) error) error {
	bound := n.cfg.Clock.After(storeBound)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f(ctx) }()
	select {
	case err := <-done:
		return err
	case <-bound:
		return fmt.Errorf("the store did not answer within %v", storeBound)
	}
}

// counters reads the counters, bounded.
func (n *Node) counters(ctx context.Context) (kvstore.Counters, error) {
	var got *kvstore.Counters
	err := n.bounded(ctx, func(ctx context.Context) error {
		c, err := n.meta.Nodes().Counters(ctx)
		got = &c
		return err
	})
	if err != nil {
		return kvstore.Counters{}, err
	}
	return *got, nil
}

// pass reads the counters and acts on each kind due, then publishes the
// counters it read as the positions reconciled through. A read that fails
// leaves the running set and the positions as they were, and has the next
// pass act on every kind.
//
// decision: reconcile never binds a share whose removal is marked, as the
// listing leaves it out, and never unbinds one: the removal's own driver
// does, at the step its record names.
//
// ponytail: a share pass lists every share, the full pass RFC 23 §2.3 asks
// for after a failed read, even when only one share changed. List the shares
// whose position is above the last one acted on, once a share index by
// position exists and a profile shows the listing.
func (n *Node) pass(ctx context.Context) error {
	r := &n.rec
	c, err := n.counters(ctx)
	if err != nil {
		r.full = true
		return err
	}
	if r.paused != nil {
		r.paused(c)
	}
	shares, apply := due(c, r.seen, r.full || r.retry)
	if shares {
		if err := n.reconcileShares(ctx); err != nil {
			r.full = true
			return err
		}
	}
	if apply {
		refusals, err := n.apply(ctx, c)
		if err != nil {
			r.full = true
			return err
		}
		r.mu.Lock()
		r.applied = refusals
		r.mu.Unlock()
	}
	r.full, r.seen = false, c
	r.mu.Lock()
	again := func(f refusal) bool { return f.retry }
	r.retry = slices.ContainsFunc(r.shares, again) || slices.ContainsFunc(r.applied, again)
	r.pos = c
	close(r.moved)
	r.moved = make(chan struct{})
	r.mu.Unlock()
	return nil
}

// reconcileShares binds each share planShares names, a new one or one whose
// state changed, and records the refusals: a share with no remote store,
// and one the engine refused, which is retried.
//
// ponytail: a share bound here is bound in the engine only; the filesystem
// serves the shards it served at its start. Serve the new share's shard
// here once the filesystem has a call that serves one while serving.
//
// ponytail: every share's namespace is taken as held, so no namespace is
// claimed and no binding is fenced. Claim the namespace here, before the
// bind, once namespaces are claimed.
func (n *Node) reconcileShares(ctx context.Context) error {
	r := &n.rec
	listed, remote, err := n.readShares(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	bind, refusals := planShares(r.bound, listed, remote)
	r.mu.Unlock()
	for _, s := range bind {
		if err := n.engine.Bind(ctx, enginewire.Binding(s, 0)); err != nil {
			refusals = append(refusals, refused(s, true, err))
			continue
		}
		r.mu.Lock()
		r.bound[s.ID] = s
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.shares = refusals
	r.mu.Unlock()
	return nil
}

// reconcileLoop runs a pass every reconcileInterval on the node's clock, or
// when a write wakes it, until ctx ends, reporting each to loop. After a
// failed read or a refusal to retry it waits on one backoff instead, from
// firstRetry doubling to lastRetry.
func (n *Node) reconcileLoop(ctx context.Context, loop *health.Loop) {
	r := &n.rec
	for {
		wait := reconcileInterval
		if r.full || r.retry {
			r.wait = min(max(2*r.wait, firstRetry), lastRetry)
			wait = r.wait
		} else {
			r.wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-n.cfg.Clock.After(wait):
		case <-r.wake:
		}
		if err := n.pass(ctx); err != nil && ctx.Err() == nil {
			n.cfg.Logger.Warn("reconcile: read the records", "error", err)
		}
		loop.Turn()
	}
}

// applied reads the counters, wakes reconcile, and waits until the
// settings and principals positions cover the two it read, then returns the
// positions it returned on. A pass that read the counters before this read
// cannot raise the positions that far, so a pass running when the wake is
// sent is not waited on, only the one after it. It returns ctx's error once
// ctx ends.
func (n *Node) applied(ctx context.Context) (kvstore.Counters, error) {
	r := &n.rec
	c, err := n.meta.Nodes().Counters(ctx)
	if err != nil {
		return kvstore.Counters{}, err
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	for {
		r.mu.Lock()
		pos, moved := r.pos, r.moved
		r.mu.Unlock()
		if pos.Settings >= c.Settings && pos.Principals >= c.Principals {
			return pos, nil
		}
		select {
		case <-moved:
		case <-ctx.Done():
			return pos, ctx.Err()
		}
	}
}

// startReconcile runs the first full reconcile, then the loop, watched by
// the watchdog, until Stop.
func (n *Node) startReconcile(ctx context.Context) error {
	n.rec.full = true
	if err := n.pass(ctx); err != nil {
		return err
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	loop := n.watchdog.Loop(stepReconcile)
	go func() {
		defer close(done)
		defer loop.Close()
		n.reconcileLoop(loopCtx, loop)
	}()
	n.started(stepReconcile, func(context.Context) error {
		cancel()
		<-done
		return nil
	})
	return nil
}
