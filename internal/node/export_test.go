package node

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/sidecar"
	"github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/resources"
)

// LiveStateBound is how long step 3 of a share's removal waits.
const LiveStateBound = liveStateBound

// BudgetNotZero is the zero check's record.
const BudgetNotZero = budgetNotZero

// NewStepping is New with stepped called after each start step completes,
// named as Start's error names it; before each part's stop is waited on,
// its bound already running, as "wait " and the part's name; and after each
// part stops, as "stop " and its name.
func NewStepping(stepped func(n *Node, step string)) func(Config) (*Node, error) {
	return func(cfg Config) (*Node, error) {
		n, err := New(cfg)
		if err == nil {
			n.life.stepped = func(step string) { stepped(n, step) }
		}
		return n, err
	}
}

// Addrs is the API's and the metrics listener's bound addresses, nil before
// Start opened them.
func (n *Node) Addrs() (api, metrics string) {
	if n.life.api == nil {
		return "", ""
	}
	return n.life.api.String(), n.life.met.String()
}

// StartFailed is the node's start_failed condition, nil unless a Start failed.
func (n *Node) StartFailed() []health.Condition { return n.startFailed() }

// Filesystem is the filesystem the node built.
func (n *Node) Filesystem() *vfs.Filesystem { return n.fs }

// Engine is the engine the node built.
func (n *Node) Engine() *engine.Engine { return n.engine }

// OpenState is the open state the node built.
func (n *Node) OpenState() *openstate.Table { return n.open }

// Progress is op's progress, as the node's operation runner reads it.
func (n *Node) Progress(ctx context.Context, op entity.Operation) apiv1.Progress {
	return n.runner.steps[op.Kind].progress(ctx, op)
}

// NewPausing is New with paused called by each reconcile pass with the
// counters it read, before it acts on them; the pass waits for it.
func NewPausing(paused func(read kvstore.Counters)) func(Config) (*Node, error) {
	return func(cfg Config) (*Node, error) {
		n, err := New(cfg)
		if err == nil {
			n.rec.paused = paused
		}
		return n, err
	}
}

// OnBind has reconcile call binding with each share before it binds it. Call
// it before Start.
func (n *Node) OnBind(binding func(entity.Share)) { n.rec.binding = binding }

// OnBound has reconcile call bound with each share the engine bound, before
// reconcile puts it in the running set. Call it before Start.
func (n *Node) OnBound(bound func(entity.Share)) { n.rec.bound = bound }

// OnEndLiveState has step 3 of each share removal call ending with the
// share once its bound is set on the node's clock. Call it before Start.
func (n *Node) OnEndLiveState(ending func(entity.ShareID)) { n.ending = ending }

// OnApply has reconcile call applying with the counters each apply acts on.
// Call it before Start.
func (n *Node) OnApply(applying func(kvstore.Counters)) { n.rec.applying = applying }

// OnReconcileWait has reconcile's loop call waiting once each of its waits
// is set on the node's clock. Call it before Start.
func (n *Node) OnReconcileWait(waiting func()) { n.rec.waiting = waiting }

// Applied is the wait a write on this node makes.
func (n *Node) Applied(ctx context.Context) error { return n.applied(ctx) }

// Positions is the counters the node has reconciled through.
func (n *Node) Positions() kvstore.Counters {
	n.rec.mu.Lock()
	defer n.rec.mu.Unlock()
	return n.rec.pos
}

// Moved is closed once a pass that read the counters without error ends.
func (n *Node) Moved() <-chan struct{} {
	n.rec.mu.Lock()
	defer n.rec.mu.Unlock()
	return n.rec.moved
}

// Woken reports whether a wake waits for reconcile to take it.
func (n *Node) Woken() bool { return len(n.rec.wake) == 1 }

// Refused is the records the node refused, the shares' first.
func (n *Node) Refused() []entity.RefusedRecord {
	_, refused := n.rec.report()
	return refused
}

// Wake wakes reconcile, as a write on this node does, so a pass runs
// without the clock moving.
func (n *Node) Wake() {
	select {
	case n.rec.wake <- struct{}{}:
	default:
	}
}

// OnRunnerTurn has the operation runner call turned after each turn, once
// the wait for the next is set on the node's clock. Call it before Start.
func (n *Node) OnRunnerTurn(turned func()) { n.runner.turned = turned }

// OnStatus has the status writer call turned at the end of each turn with
// the status it built and whether it committed it. Call it before Start.
func (n *Node) OnStatus(turned func(s entity.NodeStatus, wrote bool)) { n.status.turned = turned }

// Bound is share id's state as reconcile last bound it in the engine, and
// false when it has bound none.
func (n *Node) Bound(id entity.ShareID) (entity.ShareState, bool) {
	n.rec.mu.Lock()
	defer n.rec.mu.Unlock()
	s, ok := n.rec.running[id]
	return s.State, ok
}

// OnSweep has the sweep call turned at the end of each turn with what each
// of its Sweep calls that answered deleted. Call it before Start.
func (n *Node) OnSweep(turned func([]kvstore.Swept)) { n.sweep.turned = turned }

// NewAuthenticating is New with authenticate as the API's Authenticate,
// which New leaves unset.
func NewAuthenticating(authenticate func(*http.Request) (auth.Caller, error)) func(Config) (*Node, error) {
	return func(cfg Config) (*Node, error) { return newNode(cfg, authenticate) }
}

// SetNFS has apply hand the NFS slot enabled and s, as the adapter's
// settings section will. Safe while the node runs: a pass reads it once a
// counter moves or a retry is due.
func (n *Node) SetNFS(enabled bool, s settings.NFS) {
	n.nfs.want.Store(&desired[settings.NFS]{enabled, s})
}

// SetSMB has apply hand the SMB slot enabled and s, as SetNFS does NFS's.
func (n *Node) SetSMB(enabled bool, s settings.SMB) {
	n.smb.want.Store(&desired[settings.SMB]{enabled, s})
}

// SetSidecar has the node's runner drive sc, and no other sidecar. Call it
// before Start.
func (n *Node) SetSidecar(sc adapter.Sidecar) {
	n.sidecars = sidecar.New(sc)
	n.nfs.runner, n.smb.runner = n.sidecars, n.sidecars
}

// OnAdapter has each adapter the slots build handed to wrap with its
// protocol, and the slots run what wrap returns. Call it before Start.
func (n *Node) OnAdapter(wrap func(protocol string, a adapter.Adapter) adapter.Adapter) {
	wrapBuild(n.nfs, wrap)
	wrapBuild(n.smb, wrap)
}

func wrapBuild[S shape](s *slot[S], wrap func(string, adapter.Adapter) adapter.Adapter) {
	build := s.build
	s.build = func(d adapter.Deps, set S) (adapter.Adapter, error) {
		a, err := build(d, set)
		if err != nil {
			return nil, err
		}
		return wrap(s.name, a), nil
	}
}

// Reserve reserves bytes from the node's budget pool names, receive, reply,
// connection or hold, as an adapter would.
func (n *Node) Reserve(ctx context.Context, pool string, bytes int64) (resources.Reservation, error) {
	switch pool {
	case "receive":
		return n.budgets.Receive.Reserve(ctx, netip.Addr{}, bytes)
	case "reply":
		return n.budgets.Reply.Reserve(ctx, netip.Addr{}, bytes)
	case "connection":
		return n.budgets.Connection.Reserve(bytes)
	case "hold":
		return n.budgets.Hold.Reserve(bytes)
	}
	return resources.Reservation{}, fmt.Errorf("node: no budget %q", pool)
}
