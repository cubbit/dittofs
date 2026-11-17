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

// stopBound is how long Stop waits for one part to stop, on the node's clock.
//
// ponytail: one fixed bound for every part, as RFC 23 sets the drain deadline
// but no per-step figure. Make it a setting once an operator needs another.
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
	api, met net.Addr
	failed   atomic.Pointer[error] // the error a failed Start stopped at, kept for start_failed
	ready    atomic.Value          // the readiness word
	stepped  func(step string)
}

func (l *lifecycle) readiness() string { return l.ready.Load().(string) }

// part is one thing Start started that Stop stops. Every part uses the
// metadata store.
type part struct {
	name string
	// uses names the other parts it calls while it runs: when its stop
	// misses its bound or fails, they are left open under it.
	uses []string
	stop func(ctx context.Context) error

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
		{"apply", n.apply},
		{stepEngine, n.startEngine},
		// A slot for identity, started after its first Apply, and stopped
		// after the filesystem and before the engine.
		{stepFilesystem, n.startFilesystem},
		{stepRunner, n.startRunner},
		// ponytail: the management API starts accepting calls here, and
		// stops accepting them, its calls in flight answered, before the
		// runner stops, while health goes on answering. The API serves no
		// call but health yet, so there is nothing to start or drain: add
		// the part with the API's first call.
		//
		// A slot for the first full reconcile.
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
	l.ready.Store(readyReady)
	return nil
}

// Stop turns readiness to draining, then stops what Start started in
// reverse, each part bounded by stopBound on the node's clock, and closes the
// metadata store's KV last. A part that misses its bound or whose stop fails
// leaves open every part it uses, and the store, and the error names it; the
// parts that do not depend on it still stop. A later Stop waits again for a
// stop still running, and runs a failed one again. With no Start, Stop closes
// only the KV. A Stop during a Start waits for it: ending Start's context ends
// a Start that waits. Ending Stop's own context stops nothing sooner.
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
	if l.stepped != nil {
		l.stepped("stop store")
	}
	return n.cfg.KV.Close()
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
			bound := n.cfg.Clock.After(stopBound)
			if l.stepped != nil {
				l.stepped("wait " + p.name)
			}
			done, stopErr := p.stopped(ctx, bound)
			switch {
			case !done:
				err = fmt.Errorf("node: stop %s: not stopped within %v", p.name, stopBound)
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
// refuses it at its own start.
func (n *Node) refuse(ctx context.Context) error {
	in, err := n.meta.Installation(ctx)
	if err != nil {
		return err
	}
	for _, dir := range n.cfg.Bootstrap.Devices {
		// With no identity expected, Open opens nothing: it names the
		// journal it found and that journal's installation.
		_, err := journal.Open(ctx, dir, journal.Version{}, journal.JournalID{}, nil, journal.WithClock(n.cfg.Clock))
		var id *journal.IdentityError
		switch {
		case errors.As(err, &id):
			if id.Installation != journal.InstallationID(in.ID) {
				return fmt.Errorf("the journal in %s belongs to installation %x, and the metadata store to installation %x", dir, id.Installation, in.ID)
			}
		case errors.Is(err, journal.ErrNoJournal):
		default:
			return fmt.Errorf("the journal in %s: %w", dir, err)
		}
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
		ID:      n.cfg.Bootstrap.Node,
		Address: n.life.api.String(),
		Roles:   entity.NodeRoleProtocol | entity.NodeRoleStorage,
	})
	return err
}

// apply reads the counters and the shares, then hands the engine its first
// settings.
//
// ponytail: apply reads the counters and applies RFC 13 Appendix B's engine
// defaults, as no settings record carries the engine's values yet. Read them
// from the settings records once those carry them.
func (n *Node) apply(ctx context.Context) error {
	if _, err := n.meta.Nodes().Counters(ctx); err != nil {
		return err
	}
	for r, err := range n.meta.Nodes().List(ctx, entity.ResourceShare, nil) {
		if err != nil {
			return err
		}
		n.life.shares = append(n.life.shares, r.Share)
	}
	return n.engine.Apply(engineDefaults)
}

// startEngine starts open state, with the filesystem as its Sink and its
// admission as its Order, then the engine over every share read at apply.
//
// ponytail: every share's namespace is taken as held, so every share is
// bound, serving or quiesced as its record says, with no journal limit. Add
// the namespace's claim, and the limit, once namespaces are claimed and the
// limit is a setting.
func (n *Node) startEngine(ctx context.Context) error {
	bound := make([]engine.Share, 0, len(n.life.shares))
	for _, s := range n.life.shares {
		bound = append(bound, enginewire.Binding(s, 0))
	}
	if err := enginewire.Start(ctx, n.open, n.engine, n.fs, n.fs.Admission, bound); err != nil {
		return err
	}
	n.started(stepEngine, func(ctx context.Context) error { return enginewire.Stop(ctx, n.open, n.engine) })
	return nil
}

// startFilesystem starts the filesystem, which serves every share's shard.
func (n *Node) startFilesystem(ctx context.Context) error {
	if err := n.fs.Start(ctx); err != nil {
		return err
	}
	n.started(stepFilesystem, func(ctx context.Context) error {
		return errors.Join(n.fs.Drain(ctx), n.fs.Stop())
	}, stepEngine)
	return nil
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

// startFailed is the node's start_failed condition after a Start that
// failed, naming the step and its error, and nil otherwise. It does not wait
// for a Start or a Stop in progress.
//
// ponytail: nothing reads it yet but tests. The node status writer reports
// it with the engine's conditions once it exists.
func (n *Node) startFailed() []health.Condition {
	err := n.life.failed.Load()
	if err == nil {
		return nil
	}
	return []health.Condition{{
		Name:    "start_failed",
		Subject: health.Subject{Scope: health.ScopeNode, ID: fmt.Sprintf("%x", n.cfg.Bootstrap.Node[:])},
		Cause:   *err,
	}}
}
