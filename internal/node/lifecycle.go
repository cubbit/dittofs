package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The readiness words /health/ready answers.
const (
	readyStarting = "starting"
	readyReady    = "ready"
	readyDraining = "draining"
	readyNot      = "not_ready"
)

// The names of the start steps that start a part, as a failed Start's error
// and a stop that missed its bound name them.
const (
	stepListeners  = "listeners"
	stepEngine     = "engine"
	stepFilesystem = "filesystem"
	stepRunner     = "runner"
)

// stopBound is how long Stop waits for one part to stop, on the node's clock,
// for every part but the adapters', which RFC 23 §2.2 bounds by its drain
// deadline plus 30 s.
//
// ponytail: one fixed bound for every other part, as RFC 23 sets no other
// per-step figure. Make it a setting once an operator needs another.
const stopBound = 30 * time.Second

// engineDefaults are the engine's settings until the settings records carry
// them: RFC 13 Appendix B's proposed values.
var engineDefaults = engine.Settings{ExistenceAge: time.Second, GroupCommitMaxFiles: 256, CloneMaxLen: 1 << 30}

// lifecycle is the node's start and stop state.
type lifecycle struct {
	mu       sync.Mutex // serialises Start and Stop
	started  bool       // a Start ran: the node does not start again
	parts    []*part    // what Start started and Stop has not stopped, in start order
	shares   []entity.Share
	removing []entity.Share // the shares being removed, as Start read them
	api, met net.Addr
	failed   atomic.Pointer[error] // the error a failed Start stopped at, kept for start_failed
	ready    atomic.Value          // the readiness word
	served   atomic.Bool           // readiness has turned ready once: it never clears
	stepped  func(step string)
	store    *part // the KV's close, kept while one runs past its bound
}

func (l *lifecycle) readiness() string { return l.ready.Load().(string) }

// readiness is the readiness word, read at each call: the lifecycle's, and
// not_ready while it is ready and a running adapter's Health names
// adapter_listener_lost, as clients sent to that listener cannot connect
// (RFC 20 §3.7, RFC 25 §3.6).
//
// decision: once started, the adapters turn the node not_ready only through
// a lost listener. An adapter refused, a failed sidecar, an adapter rebuilt
// for a port change and one enabled after the start leave it ready, though
// RFC 20 §3.7 and RFC 25 §3.6, read literally, make a node whose adapter is
// rebuilt unready for up to its drain_timeout: a load balancer would then
// steer the other adapter's clients away, which RFC 23 §3.4's one adapter at
// a time is there to spare. Start's own gate is the step order, which turns
// the node ready only once step 9 has run. Overturned if RFC 25 §3.6 gives
// a rebuild a row of its own.
func (n *Node) readiness() string {
	w := n.life.readiness()
	if w == readyReady && (n.nfs.lost() || n.smb.lost()) {
		return readyNot
	}
	return w
}

// part is one thing Start started that Stop stops. Every part uses the
// metadata store.
type part struct {
	name string
	// uses names the other parts it calls while it runs: when its stop
	// misses its bound or fails, they are left open under it.
	uses []string
	stop func(ctx context.Context) error
	// bound is how long Stop waits for its stop, read as the wait starts;
	// nil is stopBound.
	bound func() time.Duration

	running chan struct{} // closed once the stop in flight ends; nil when none is
	err     error         // the ended stop's answer, written before running closes
}

// stopped runs p's stop unless one is in flight, and waits for it until bound
// ends: false when it is still running then. A later call waits again for
// the same stop, or, once one ended, runs a new one, so a stop that failed is
// tried again. Only the clock bounds the wait: a Stop whose context ended
// still stops every part.
func (p *part) stopped(ctx context.Context, bound <-chan struct{}) (bool, error) {
	if p.running == nil {
		running := make(chan struct{})
		p.running = running
		go func() {
			defer close(running)
			p.err = p.stop(context.WithoutCancel(ctx))
		}()
	}
	select {
	case <-p.running:
		p.running = nil
		return true, p.err
	case <-bound:
		return false, nil
	}
}

// Start runs RFC 23 §2.2's start, each step complete before the next, and
// then the node is ready. A step that fails stops the start: what earlier
// steps started is stopped in reverse but the listeners and the store,
// readiness answers not_ready, start_failed is raised, and the error names
// the step. Stop then closes the rest.
//
// decision: the node is not restartable: a second Start is an error, after a
// failed Start or a Stop too, because what it starts is single-use: the API
// server refuses a Start after its Stop, and stopped open state answers
// ErrStopped. Overturned if a node must restart in process, and then by
// building a new node, not by restarting its parts.
//
// decision: the API writes records and the node applies them. Reconcile's
// calls are unexported, so no caller, the API included, can change a
// component by a second path. Overturned if something other than the server's
// start command and the test harness must drive a node, and then by a second
// root, not by exporting the calls.
func (n *Node) Start(ctx context.Context) error {
	l := &n.life
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return errors.New("node: already started: a node starts once")
	}
	l.started = true
	for _, s := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"refuse", n.refuse},
		{stepListeners, n.openListeners},
		{"register", n.register},
		{"apply", n.readRecords},
		{stepEngine, n.startEngine},
		// Identity starts after its first Apply, and stops after the
		// filesystem and before the engine.
		{stepIdentity, n.startIdentity},
		{stepFilesystem, n.startFilesystem},
		{stepRunner, n.startRunner},
		// ponytail: the management API's calls are served from the
		// listeners' step, refused 503 starting until readiness turns
		// ready after the first full reconcile rather than from here, and
		// they stop with the listeners, after the engine, rather than
		// before the runner. The API writes only to the store, which
		// outlives every part, and reads stopped parts only through their
		// statistics, so a call taken while the node stops commits a
		// record this node applies at its next start, and a write's wait
		// for this node to apply it runs out its bound. A failed start
		// leaves the listeners open for health, so the API refuses every call
		// but the rows that answer before ready (health and the version) on a
		// node never ready since it started (Served), with no retry. Make the calls'
		// acceptance a part started here, apart from the health listener,
		// once a call reaches a part the stop closes first.
		{stepFirstReconcile, n.firstReconcile},
		{stepAdapters, n.startAdapters},
		{stepReconcile, n.startReconcile},
	} {
		if err := s.run(ctx); err != nil {
			err = fmt.Errorf("node: start step %s: %w", s.name, err)
			l.failed.Store(&err)
			l.ready.Store(readyNot)
			return errors.Join(err, n.halt(ctx, 1))
		}
		if l.stepped != nil {
			l.stepped(s.name)
		}
	}
	l.served.Store(true)
	l.ready.Store(readyReady)
	return nil
}

// Stop turns readiness to draining, then stops what Start started in
// reverse, each part bounded on the node's clock by its own bound, stopBound
// for all but the adapters', and closes the metadata store's KV, bounded by
// stopBound too, and then the storage key last, clearing the signing key's
// versions the access tokens hold. A part that misses its bound or whose stop
// fails leaves open every part it uses, and the store, and the error names
// it; the parts that do not depend on it still stop. A KV close that misses
// its bound leaves the storage key open, and the error names the store. A
// later Stop waits again for a stop still running, and runs a failed one
// again. With no Start, Stop closes only the KV and the storage key. A Stop
// during a Start waits for it: ending Start's context ends a Start that
// waits. Ending Stop's own context stops nothing sooner.
func (n *Node) Stop(ctx context.Context) error {
	l := &n.life
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = true
	if l.failed.Load() == nil {
		l.ready.Store(readyDraining)
	}
	if err := n.halt(ctx, 0); err != nil {
		return errors.Join(err, errors.New("node: stop store: left open, as a part using it did not stop"))
	}
	n.tokens.Close()
	if l.store == nil {
		l.store = &part{name: "store", stop: func(context.Context) error { return n.cfg.KV.Close() }}
	}
	if l.stepped != nil {
		l.stepped("wait store")
	}
	// decision: a KV close that misses its bound is left running, as nothing
	// cancels it: a store that cannot be written retries its flush for as
	// long as it runs. Stop names it and leaves the storage key open, a later
	// Stop waits again on the same close, and dfs start exits on Stop's error,
	// which ends it with the process. Overturned if the KV's Close comes to
	// take a context it honours.
	done, err := l.store.stopped(ctx, n.cfg.Clock.After(stopBound))
	if !done {
		return fmt.Errorf("node: stop store: not stopped within %v", stopBound)
	}
	if err != nil {
		err = fmt.Errorf("node: stop store: %w", err)
	} else if l.stepped != nil {
		l.stepped("stop store")
	}
	return errors.Join(err, n.storageKey.Close())
}

// halt stops the parts from keep on, in reverse start order. Each that stops
// is dropped from the started parts; each that misses its bound or fails,
// and each part it uses, is kept, and named in the error.
func (n *Node) halt(ctx context.Context, keep int) error {
	l := &n.life
	held := map[string]bool{}
	var errs []error
	for i := len(l.parts) - 1; i >= keep; i-- {
		p := l.parts[i]
		var err error
		if held[p.name] {
			err = fmt.Errorf("node: stop %s: left open, as a part using it did not stop", p.name)
		} else {
			d := stopBound
			if p.bound != nil {
				d = p.bound()
			}
			bound := n.cfg.Clock.After(d)
			if l.stepped != nil {
				l.stepped("wait " + p.name)
			}
			done, stopErr := p.stopped(ctx, bound)
			switch {
			case !done:
				err = fmt.Errorf("node: stop %s: not stopped within %v", p.name, d)
			case stopErr != nil:
				err = fmt.Errorf("node: stop %s: %w", p.name, stopErr)
			}
		}
		if err != nil {
			errs = append(errs, err)
			for _, u := range p.uses {
				held[u] = true
			}
			continue
		}
		l.parts = slices.Delete(l.parts, i, i+1)
		if l.stepped != nil {
			l.stepped("stop " + p.name)
		}
	}
	return errors.Join(errs...)
}

// started records a part Start started, for Stop and a failed Start's undo.
func (n *Node) started(name string, stop func(context.Context) error, uses ...string) {
	n.life.parts = append(n.life.parts, &part{name: name, stop: stop, uses: uses})
}

// refuse refuses a KV of another installation than the one each device's
// journal names, and a device whose journal cannot be read, before Start
// writes to it. A device holding no journal is left to the engine, which
// refuses it at its own start. Then it writes a missing node-id copy beside
// each journal the store records for this node and device, and beside no
// other (RFC 23 §2.2 step 1).
func (n *Node) refuse(ctx context.Context) error {
	in, err := n.meta.Installation(ctx)
	if err != nil {
		return err
	}
	opts := append([]journal.Option{journal.WithClock(n.cfg.Clock)}, n.cfg.Journal...)
	for _, j := range n.cfg.Bootstrap.Journals {
		dir := j.Path
		// With no identity expected, Open opens nothing: it names the
		// journal it found and that journal's installation.
		_, err := journal.Open(ctx, dir, journal.Version{}, journal.JournalID{}, nil, opts...)
		var id *journal.IdentityError
		switch {
		case errors.As(err, &id):
			if id.Installation != journal.InstallationID(in.ID) {
				return fmt.Errorf("the journal %x in %s belongs to installation %x, and the metadata store to installation %x", id.Found, dir, id.Installation, in.ID)
			}
		case errors.Is(err, journal.ErrNoJournal):
		default:
			return fmt.Errorf("the journal in %s: %w", dir, err)
		}
	}
	// decision: a copy that cannot be written refuses the start, though the
	// copy is redundant with the host file's identity: a device that cannot
	// take a 24-byte file and its directory's sync is not fit to hold a
	// journal, and starting would leave that device to fail at the engine's
	// first write instead. Overturned if a device that refuses only small
	// writes, such as one at a file-count quota, must still serve.
	b := &n.cfg.Bootstrap
	if err := bootstrap.WriteMissingNodeIDs(ctx, b, b.Node.ID, n.meta.Lifecycle().JournalID, n.cfg.Logger); err != nil {
		return fmt.Errorf("write missing node-id copies: %w", err)
	}
	return nil
}

// openListeners opens the API and the metrics listeners, and logs the
// addresses they bound: from here the health endpoints answer, live and
// starting.
func (n *Node) openListeners(context.Context) error {
	a, m, err := n.api.Start()
	if err != nil {
		return err
	}
	n.cfg.Logger.Info("node: listening", "api", a.String(), "metrics", m.String())
	n.life.api, n.life.met = a, m
	n.started(stepListeners, n.api.Stop)
	return nil
}

// register writes the node's record, naming its API address.
//
// ponytail: the record carries no format version ranges, as no shared format
// is versioned yet, so no registration is refused for them. Add the ranges
// once a format nodes share is.
func (n *Node) register(ctx context.Context) error {
	_, err := n.meta.Nodes().RegisterNode(ctx, entity.Node{
		ID:      n.cfg.Bootstrap.Node.ID,
		Address: n.life.api.String(),
		Roles:   entity.NodeRoleProtocol | entity.NodeRoleStorage,
	})
	return err
}

// readRecords reads the counters and the shares, keeping those reconcile
// binds, and the shares being removed, for the engine's start, then calls
// apply, and hands identity its first State. The counters it read first are
// those reconcile has acted on, so only Start's full first pass, not a
// counter, makes reconcile record the refusals.
//
// decision: reconcile's mark is the counters read before the shares, not the
// later ones identity's walk settled on: the shares and apply acted on the
// first, so a mark at the later ones would skip a share or setting written
// between. Identity may run a newer State than the mark says, which costs
// one more pass, never a missed one.
func (n *Node) readRecords(ctx context.Context) error {
	c, err := n.counters(ctx)
	if err != nil {
		return err
	}
	listed, remote, err := n.readShares(ctx)
	if err != nil {
		return err
	}
	n.life.shares, _ = planShares(n.rec.running, listed, remote)
	if n.life.removing, err = n.readRemoving(ctx); err != nil {
		return err
	}
	if _, err := n.apply(ctx, c, nil); err != nil {
		return err
	}
	if _, err := n.applyIdentity(ctx); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	n.rec.seen = c
	return nil
}

// startEngine starts open state, with the filesystem as its Sink and its
// admission as its Order, then the engine over every share readRecords kept.
// A share being removed is handed to the engine too, which attaches it until
// its removal's step 3 unbinds it and drops its tag, but it joins no running
// set.
//
// ponytail: every share's namespace is taken as held, so every share kept is
// bound, serving or quiesced as its record says, with no journal limit. Add
// the namespace's claim, and the limit, once namespaces are claimed and the
// limit is a setting.
func (n *Node) startEngine(ctx context.Context) error {
	bound := make([]engine.Share, 0, len(n.life.shares)+len(n.life.removing))
	for _, s := range append(slices.Clone(n.life.shares), n.life.removing...) {
		bound = append(bound, enginewire.Binding(s, 0))
	}
	if err := enginewire.Start(ctx, n.open, n.engine, n.fs, n.fs.Admission, bound); err != nil {
		return err
	}
	// A share the engine refused at its start is left out of the running
	// set, so the first full pass binds it again and records the refusal.
	// The running set is written here without the reconciler's mutex: Start
	// runs its steps one after another, and reconcile, the only other
	// writer, starts at a later step, as do the runner and the admitted
	// calls that read it.
	refused := n.engine.Stats().Shares
	for _, s := range n.life.shares {
		if refused[s.ID].Refused == nil {
			n.rec.running[s.ID] = s
		}
	}
	n.started(stepEngine, func(ctx context.Context) error { return enginewire.Stop(ctx, n.open, n.engine) })
	return nil
}

// startFilesystem starts the filesystem, which serves every share's shard,
// recording the zero check's part before the filesystem's, so Stop runs the
// check once the filesystem has stopped; then it runs step 3 of every share
// removal a crash interrupted, before any call is admitted.
func (n *Node) startFilesystem(ctx context.Context) error {
	if err := n.fs.Start(ctx); err != nil {
		return err
	}
	n.started(partBudgets, n.checkBudgets)
	n.started(stepFilesystem, func(ctx context.Context) error {
		return errors.Join(n.fs.Drain(ctx), n.fs.Stop())
	}, stepEngine, stepIdentity, partBudgets)
	return n.joinRemovals(ctx)
}

// startRunner starts the operation runner, watched by the watchdog. Its
// stop leaves each operation at its last durable step.
func (n *Node) startRunner(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	loop := n.watchdog.Loop(stepRunner)
	go func() {
		defer close(done)
		defer loop.Close()
		n.runner.run(ctx, loop)
	}()
	n.started(stepRunner, func(context.Context) error {
		cancel()
		<-done
		return nil
	})
	return nil
}

// startAdapters is start step 9 (RFC 23 §2.2): each slot is called with
// what it is to run, at the settings generation the first full reconcile
// read, and from here apply calls the slots. A refusal does not fail the
// start: it joins the refusals that pass recorded, and one to retry starts
// reconcile's loop on its backoff, as a pass's own does (RFC 20 §3.7, RFC 23
// §2.3). The adapters' part uses the filesystem and the zero check, so a
// drain past its bound leaves both open.
//
// decision: apply calls the slots only once this step has run. It runs
// twice before, at step 4 and in the first full reconcile, and an adapter
// started there would listen before the engine and the filesystem serve,
// or before the node has reconciled, answering its clients retry-later (RFC
// 23 §2.2, §12's "§2.2 order"). Overturned if RFC 23 §2.2 moves the
// adapters' start.
func (n *Node) startAdapters(ctx context.Context) error {
	n.adapting = true
	// Nothing runs yet, so no call drains, and none is handed a loop.
	refusals := n.applyAdapters(ctx, nil, n.rec.seen.Settings)
	r := &n.rec
	r.again = r.again || slices.ContainsFunc(refusals, func(f refusal) bool { return f.retry })
	r.mu.Lock()
	r.applied = append(r.applied, refusals...)
	r.mu.Unlock()
	n.life.parts = append(n.life.parts, &part{name: stepAdapters, stop: n.stopAdapters,
		uses: []string{stepFilesystem, partBudgets}, bound: n.drainBound})
	return nil
}

// drainBound is stop step 2's bound, the drain deadline plus 30 s, which
// halt reads as the step starts (RFC 23 §2.2). It sets the deadline there,
// the step's start plus the longest drain_timeout of the adapters the slots
// hold, and ends the drains' context at it on the node's clock.
//
// decision: the deadline is the step's start plus the longest
// drain_timeout, so no adapter's drain is cut shorter than its own; RFC 23
// §2.2 bounds the step by the deadline plus 30 s but sets no deadline. It
// ends the drains' context on the node's clock, as a share removal's step 3
// ends its own, rather than giving it a Deadline, which an adapter would
// read on the runtime's clock. Overturned if RFC 23 §2.2 comes to set the
// deadline.
func (n *Node) drainBound() time.Duration {
	_, longest := n.adaptersHeld()
	go func(deadline <-chan struct{}) {
		select {
		case <-deadline:
			n.endDrains()
		case <-n.drains.Done():
		}
	}(n.cfg.Clock.After(longest))
	return longest + onCloseBound
}

// stopAdapters is stop step 2 (RFC 23 §2.2): every sidecar is synced with
// the empty set, then every adapter the slots hold is stopped at once under
// the drains' context, which drainBound ends at the drain deadline, and then
// the runner closes. An adapter a rebuild or a disable was still draining
// when reconcile stopped is among them, its drain under the same context,
// so it is joined and ends by the same deadline.
func (n *Node) stopAdapters(context.Context) error {
	held, _ := n.adaptersHeld()
	err := n.sidecars.StopAll(n.drains, held)
	n.endDrains()
	n.sidecars.Close()
	return err
}

// startFailed is the node's start_failed condition after a Start that
// failed, naming the step and its error, and nil otherwise. It does not wait
// for a Start or a Stop in progress. The health evaluator reads it, but
// nothing in production reads the evaluator on a node whose Start failed
// until the health call serves it: the status writer runs only on a started
// node.
//
// ponytail: a Start that failed writes no node status, as the status writer
// runs only on a started node, and the KV a Start refused at its first step
// must not be written. Write the status once from a failed Start, after its
// refuse step, once a reader of the status needs start_failed there.
func (n *Node) startFailed() []health.Condition {
	err := n.life.failed.Load()
	if err == nil {
		return nil
	}
	return []health.Condition{{Name: "start_failed", Subject: n.subject(), Cause: *err}}
}
