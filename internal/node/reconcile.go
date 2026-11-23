package node

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const (
	// reconcileInterval is how often reconcile reads the counters.
	reconcileInterval = time.Second
	// storeBound is how long reconcile waits on one store call, on the
	// node's clock.
	storeBound = 20 * time.Second
	// stepFirstReconcile names the first full reconcile, start step 8.
	stepFirstReconcile = "first reconcile"
	// stepReconcile names reconcile's loop, start step 10, and its part.
	stepReconcile = "reconcile"
)

// reconciler is reconcile's state: the running set and the counters the
// node has acted on, and the wake a write on this node sends.
type reconciler struct {
	// table is the node's settings table, which the settings walk reads by.
	table settings.Table
	// wake has one slot, so a wake sent while a pass runs is kept and runs
	// one more pass after it.
	wake chan struct{}
	// paused, when set, is called with the counters each pass read, before
	// the pass acts on them; binding, when set, with each share before
	// reconcile binds it, and bound once the engine bound it; applying, when
	// set, with the counters each apply acts on; waiting, when set, once each
	// wait of the loop is set on the node's clock; awaited, when set, as each
	// write's wait for the node to apply it starts; handing, when set, with
	// each State identity is handed, before its Apply.
	awaited  func()
	paused   func(kvstore.Counters)
	binding  func(entity.Share)
	bound    func(entity.Share)
	applying func(kvstore.Counters)
	waiting  func()
	handing  func(identity.State)

	// Read and written by Start's steps and then by the loop alone.
	seen  kvstore.Counters // the counters the last pass acted on
	again bool             // a read failed, or a refusal asked to be retried: the next pass acts on every kind

	mu sync.Mutex
	// pos is the counters the node has reconciled through: acted on every
	// change up to each, applied or refused.
	pos   kvstore.Counters
	moved chan struct{} // closed when pos moves, then replaced
	// running is the running set: each share as the engine binds it.
	running map[entity.ShareID]entity.Share
	// gone is every share a removal has unbound, which a share pass that
	// listed it before its mark neither runs nor refuses.
	//
	// ponytail: one entry per share removed since the node started. Forget
	// a share once no pass that listed it before its mark can still run, if
	// the count shows in a profile.
	gone map[entity.ShareID]bool
	// shares and applied are the records the last share pass and the last
	// apply refused.
	shares, applied []refusal
	// above is each counter read above pos and not yet reconciled through,
	// and aboveSince when it was first read so, on the clock's Elapsed.
	above      [len(counterKinds)]bool
	aboveSince [len(counterKinds)]time.Duration

	// errors counts the reconcile steps that failed, by the kind they apply.
	errors [len(counterKinds)]atomic.Uint64
}

// report is the counters the node has reconciled through and the records
// it refused, the shares' first, read together.
func (r *reconciler) report() (kvstore.Counters, []entity.RefusedRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []entity.RefusedRecord
	for _, f := range append(slices.Clone(r.shares), r.applied...) {
		out = append(out, f.rec)
	}
	return r.pos, out
}

// read notes the counters c a pass read at now: each above the position
// reconciled through is marked so from now, unless it already was, so a
// pass that fails leaves the lag counting from the first read.
func (r *reconciler) read(c kvstore.Counters, now time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	read, pos := countersOf(c), countersOf(r.pos)
	for k := range read {
		if read[k] > pos[k] && !r.above[k] {
			r.above[k], r.aboveSince[k] = true, now
		}
	}
}

// lags is how long each counter has been read above the position
// reconciled through at now, zero for one that is not. r.mu is held.
//
// decision: the lag counts only from a read above the position, so a pass
// that read the counters at its position and then hangs inside Bind or
// Apply raises no lag, however long it hangs, and the counters may move
// meanwhile unread. The watchdog backstops it: the pass reports no turn, so
// liveness fails after 60 s. Have the status turn read the counters too,
// bounded, if a hung pass must show as a lag before liveness fails.
func (r *reconciler) lags(now time.Duration) (lag [len(counterKinds)]time.Duration) {
	for k, above := range r.above {
		if above {
			lag[k] = now - r.aboveSince[k]
		}
	}
	return lag
}

// serves reports whether share id is in the running set: the filesystem
// admits calls into it only then.
func (r *reconciler) serves(id entity.ShareID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.running[id]
	return ok
}

// unbound takes share id, which its removal has unbound, out of the running
// set and out of the refusals, and keeps a pass that listed it before its
// mark from putting it back, so neither the filesystem nor the node status
// keeps a share that is gone.
func (r *reconciler) unbound(id entity.ShareID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gone[id] = true
	delete(r.running, id)
	r.shares = r.notGone(r.shares)
}

// notGone is refusals without those of a share gone. r.mu is held.
func (r *reconciler) notGone(refusals []refusal) []refusal {
	return slices.DeleteFunc(slices.Clone(refusals), func(f refusal) bool {
		return f.rec.Record.Kind == entity.ResourceShare && r.gone[entity.ShareID(f.rec.Record.ID)]
	})
}

func newReconciler(table settings.Table) reconciler {
	return reconciler{table: table, wake: make(chan struct{}, 1), moved: make(chan struct{}),
		running: map[entity.ShareID]entity.Share{}, gone: map[entity.ShareID]bool{}}
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
// returns the records it refused: the engine, then identity, handed the
// principals generation and one walk of the settings (walkSettings). Once
// start step 9 has run it calls each adapter's slot too, handing it loop to
// report idle on while it waits on a drain, and returns the slots' refusals
// with identity's.
//
// ponytail: apply hands the engine RFC 13 Appendix B's defaults whatever c
// says, as no settings record carries the engine's values yet, and refuses
// nothing. Read the engine's section from the pass's one walk, as identity
// does, once a settings record carries what the engine reads.
//
// ponytail: each slot is handed what the node's tests set, and nothing else
// enables an adapter, as no settings record carries an adapter's section
// yet. Read each adapter's section from the pass's one walk with the
// snapshot's Get once it has a body, so every component of a pass runs one
// view of the settings.
func (n *Node) apply(ctx context.Context, c kvstore.Counters, loop *health.Loop) ([]refusal, error) {
	if n.rec.applying != nil {
		n.rec.applying(c)
	}
	if err := n.engine.Apply(engineDefaults); err != nil {
		return nil, err
	}
	at, snap, err := n.walkSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: the settings walk: %w", err)
	}
	refusals, err := n.applyIdentity(ctx, at, snap)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	if !n.adapting {
		return refusals, nil
	}
	return append(refusals, n.applyAdapters(ctx, loop, c.Settings)...), nil
}

// walkSettings is one walk of the settings, and the counters read before it,
// during which the settings generation did not move. The counters are read
// right before each walk and again after it; a walk the generation moved
// under is discarded and walked again, since the walk reads each page in its
// own transaction and could mix two states of the store. While writes keep
// moving the generation the call waits, and ends only with ctx, as a pass's
// position stalls (RFC 23 §2.3).
func (n *Node) walkSettings(ctx context.Context) (kvstore.Counters, *settings.Snapshot, error) {
	for {
		if err := ctx.Err(); err != nil {
			return kvstore.Counters{}, nil, err
		}
		c, err := n.counters(ctx)
		if err != nil {
			return kvstore.Counters{}, nil, err
		}
		snap, _, err := settings.Read(ctx, n.meta.Nodes(), n.rec.table, n.cfg.Bootstrap.Node.ID)
		if err != nil {
			return kvstore.Counters{}, nil, err
		}
		after, err := n.counters(ctx)
		if err != nil {
			return kvstore.Counters{}, nil, err
		}
		if after.Settings == c.Settings {
			return c, snap, nil
		}
	}
}

// backoff is the wait before the next pass on the backoff, given its last
// wait: firstRetry, doubling to lastRetry; and zero, the backoff ended, when
// the next pass needs none.
func backoff(last time.Duration, again bool) time.Duration {
	if !again {
		return 0
	}
	return min(max(2*last, firstRetry), lastRetry)
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
// refused share is bound only if it was already, so one serving goes on
// serving, and its state changes still reach its binding. A share no longer
// listed keeps its binding: its removal unbinds it.
func planShares(bound map[entity.ShareID]entity.Share, listed []entity.Share, remote func(entity.Share) bool) (bind []entity.Share, refusals []refusal) {
	for _, s := range listed {
		b, ok := bound[s.ID]
		if !remote(s) {
			refusals = append(refusals, refused(s, false, fmt.Errorf("share %s: its namespace names no remote store", s.Name)))
			if !ok {
				continue
			}
		}
		if !ok || b.State != s.State {
			bind = append(bind, s)
		}
	}
	slices.SortFunc(bind, func(a, b entity.Share) int { return cmp.Compare(a.Position, b.Position) })
	return bind, refusals
}

// readShares lists every share, as the node binds them, and reports which
// name a configured remote store.
//
// ponytail: a remote store counts as configured once any setting exists at
// its scope, as no record of a remote store exists; the namespaces and each
// store's settings are read again at every share pass. The store's record is
// RFC 13 Appendix B's required remote-store settings, `endpoint`, `bucket`
// and the credential reference; read those once they are written, keeping
// the share create's check in agreement, which today asks only that the
// namespace names a store. Keep the namespaces' stores, which never change,
// once a profile shows the reads.
func (n *Node) readShares(ctx context.Context) ([]entity.Share, func(entity.Share) bool, error) {
	type read struct {
		listed     []entity.Share
		stores     map[entity.NamespaceID][16]byte
		configured map[[16]byte]bool
	}
	var got *read
	_, err := n.bounded(ctx, func(ctx context.Context) error {
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
// bounded answered nil. With the timeout it also answers the channel f
// answers on once it returns, for a caller whose call may still commit.
func (n *Node) bounded(ctx context.Context, f func(context.Context) error) (running <-chan error, err error) {
	bound := n.cfg.Clock.After(storeBound)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f(ctx) }()
	select {
	case err := <-done:
		return nil, err
	case <-bound:
		return done, fmt.Errorf("the store did not answer within %v", storeBound)
	}
}

// counters reads the counters, bounded.
func (n *Node) counters(ctx context.Context) (kvstore.Counters, error) {
	var got *kvstore.Counters
	_, err := n.bounded(ctx, func(ctx context.Context) error {
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
// pass act on every kind. A share pass that fails counts an error of the
// shares, and an apply that fails one of the settings. A share pass that
// fails still has apply run, so a principals or settings change does not
// wait for the share store, and publishes the principals position it
// applied through.
//
// decision: after a failed share pass only the principals position moves.
// The settings position stays, because a settings change also configures a
// share's remote store, which only the share pass acts on.
//
// ponytail: apply's error counts against the settings alone, though apply
// reads the principals generation too, as one call applies both. Count
// each kind's own failure once apply applies them apart.
//
// decision: reconcile never unbinds a share: the removal's own driver does,
// at the step its record names. A share whose removal is marked after the
// share pass listed it still reaches Bind; the engine records a share that
// Unbind or Drop found unbound as removed, so that Bind answers ErrStale and
// attaches no binding the removal would never release.
//
// ponytail: a share pass lists every share, the full pass RFC 23 §2.3 asks
// for after a failed read, even when only one share changed. List the shares
// whose position is above the last one acted on, once a share index by
// position exists and a profile shows the listing.
func (n *Node) pass(ctx context.Context, loop *health.Loop) error {
	r := &n.rec
	c, err := n.counters(ctx)
	if err != nil {
		r.again = true
		return err
	}
	r.read(c, n.cfg.Clock.Elapsed())
	if r.paused != nil {
		r.paused(c)
	}
	shares, apply := due(c, r.seen, r.again)
	var sharesErr error
	if shares {
		sharesErr = n.reconcileShares(ctx)
		if sharesErr != nil {
			r.errors[kindShares].Add(1)
		}
	}
	if apply {
		refusals, err := n.apply(ctx, c, loop)
		if err != nil {
			r.errors[kindSettings].Add(1)
			r.again = true
			return errors.Join(sharesErr, err)
		}
		r.mu.Lock()
		r.applied = refusals
		if sharesErr != nil {
			r.pos.Principals = c.Principals
			close(r.moved)
			r.moved = make(chan struct{})
		}
		r.mu.Unlock()
	}
	if sharesErr != nil {
		r.again = true
		return sharesErr
	}
	r.seen = c
	r.mu.Lock()
	retry := func(f refusal) bool { return f.retry }
	r.again = slices.ContainsFunc(r.shares, retry) || slices.ContainsFunc(r.applied, retry)
	r.pos = c
	r.above = [len(counterKinds)]bool{}
	close(r.moved)
	r.moved = make(chan struct{})
	r.mu.Unlock()
	return nil
}

// reconcileShares binds each share planShares names, a new one or one whose
// state changed, and records the refusals: a share with no remote store,
// and one the engine refused, which is retried. A share whose removal
// unbound it after the listing is neither run nor refused: the engine refuses
// its late bind stale, and one bound just before the unbind is gone. The
// filesystem admits calls only into the shares run here.
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
	bind, refusals := planShares(r.running, listed, remote)
	r.mu.Unlock()
	for _, s := range bind {
		if r.binding != nil {
			r.binding(s)
		}
		err := n.engine.Bind(ctx, enginewire.Binding(s, 0))
		if errors.Is(err, vfserr.ErrStale) {
			continue
		}
		if err != nil {
			r.errors[kindShares].Add(1)
			refusals = append(refusals, refused(s, true, err))
			continue
		}
		if r.bound != nil {
			r.bound(s)
		}
		r.mu.Lock()
		if !r.gone[s.ID] {
			r.running[s.ID] = s
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.shares = r.notGone(refusals)
	r.mu.Unlock()
	return nil
}

// reconcileLoop runs a pass every reconcileInterval on the node's clock, or
// when a write wakes it, until ctx ends, reporting each to loop. After a
// failed read or a refusal to retry it waits on one backoff instead, from
// firstRetry doubling to lastRetry.
//
// The loop reports idle as its wait ends, so its stall is counted from the
// pass's start: a pass's store calls, each bounded, start the count afresh
// rather than adding to the backoff before them.
func (n *Node) reconcileLoop(ctx context.Context, loop *health.Loop) {
	r := &n.rec
	var fault storeFault
	var wait time.Duration // the backoff's last wait, zero while no pass needs one
	for {
		wait = backoff(wait, r.again)
		next := n.cfg.Clock.After(cmp.Or(wait, reconcileInterval))
		if r.waiting != nil {
			r.waiting()
		}
		select {
		case <-ctx.Done():
			return
		case <-next:
		case <-r.wake:
		}
		loop.Idle()
		fault.observed(ctx, n, stepReconcile, n.pass(ctx, loop), slog.Attr{})
		loop.Turn()
	}
}

// applied reads the counters, wakes reconcile, and waits until the
// settings and principals positions cover the two it read. A pass that read the counters before this read
// cannot raise the positions that far, so a pass running when the wake is
// sent is not waited on, only the one after it. It returns ctx's error once
// ctx ends.
func (n *Node) applied(ctx context.Context) error {
	r := &n.rec
	if r.awaited != nil {
		r.awaited()
	}
	c, err := n.meta.Nodes().Counters(ctx)
	if err != nil {
		return err
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
			return nil
		}
		select {
		case <-moved:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// firstReconcile runs the first full reconcile, start step 8. Apply calls no
// slot yet, so the pass is handed no loop.
func (n *Node) firstReconcile(ctx context.Context) error {
	n.rec.again = true
	return n.pass(ctx, nil)
}

// startReconcile runs reconcile's loop, the status writer and the expiry
// sweep, each watched by the watchdog, until Stop. Its part uses the
// adapters' slots, which a pass calls, so a pass held past its bound leaves
// the adapters open rather than stopped under it.
func (n *Node) startReconcile(context.Context) error {
	loopCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for name, run := range map[string]func(context.Context, *health.Loop){stepReconcile: n.reconcileLoop, stepStatus: n.statusLoop, stepSweep: n.sweepLoop} {
		loop := n.watchdog.Loop(name)
		wg.Go(func() {
			defer loop.Close()
			run(loopCtx, loop)
		})
	}
	n.started(stepReconcile, func(context.Context) error {
		cancel()
		wg.Wait()
		return nil
	}, stepAdapters)
	return nil
}
