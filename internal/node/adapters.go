package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/internal/sidecar"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/nfs"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/adapter/smb"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

const (
	// drainIdle is how often a call waiting on an adapter's drain reports
	// its loop idle: inside RFC 25 §3.5's 30 s, as an accept loop does.
	drainIdle = 10 * time.Second
	// onCloseBound is how long past its drain deadline an adapter's Stop
	// may wait for its connections' OnClose (RFC 20 §3.4).
	onCloseBound = 30 * time.Second
	// stepAdapters names start step 9 and the part stop step 2 stops.
	stepAdapters = "adapters"
)

// listenAddrs resolves adapters.listen (RFC 13 Appendix C) into the
// addresses every adapter binds, given whether a tcp6 socket opens on [::]:0
// on this host. A literal that does not parse, and an address that overlaps
// one listed before it, are refused naming the key and the index, never the
// value, which may come from the environment (RFC 13 §6); every such entry
// is refused together.
//
// decision: an unset adapters.listen is 0.0.0.0, and :: beside it only where
// tcp6 opens, for RFC 13 Appendix C's "every interface": an adapter binds ::
// on tcp6 alone, with IPV6_V6ONLY, and a partial bind is a failed start (RFC
// 20 §3.1), so :: on a host with IPv6 off would refuse every adapter. A list
// that is set is bound as written, :: included. Withdraw it if RFC 13
// Appendix C's default comes to name its addresses.
//
// decision: an address that repeats one listed before it, or sits beside
// its family's unspecified address, is refused here, as each adapter's
// Validate would refuse it, so New fails naming the operator's key. Left to
// Validate, every adapter would be refused at New naming Addrs, and retried
// on reconcile's backoff for good. Withdraw it if RFC 13 Appendix C comes to
// say how the bootstrap reads such a list.
func listenAddrs(listen []string, tcp6 bool) ([]netip.Addr, error) {
	if len(listen) == 0 {
		if tcp6 {
			return []netip.Addr{netip.IPv4Unspecified(), netip.IPv6Unspecified()}, nil
		}
		return []netip.Addr{netip.IPv4Unspecified()}, nil
	}
	addrs := make([]netip.Addr, len(listen))
	var bad []error
	for i, l := range listen {
		a, err := netip.ParseAddr(l)
		if err != nil {
			bad = append(bad, fmt.Errorf("node: adapters.listen[%d]: not an IP address", i))
			continue
		}
		// An IPv4-mapped address is its IPv4 address, and an unspecified
		// address binds every other address of its family.
		for j, prev := range addrs[:i] {
			p, u := prev.Unmap(), a.Unmap()
			if prev.IsValid() && (p == u || p.Is4() == u.Is4() && (p.IsUnspecified() || u.IsUnspecified())) {
				bad = append(bad, fmt.Errorf("node: adapters.listen[%d]: overlaps adapters.listen[%d]", i, j))
				break
			}
		}
		addrs[i] = a
	}
	if bad != nil {
		return nil, errors.Join(bad...)
	}
	return addrs, nil
}

// listenPort is the port addr names, 0 when it names none: the API cannot
// bind such an address, so Start fails at step 2, before any adapter starts.
func listenPort(addr string) uint16 {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := net.LookupPort("tcp", p)
	if err != nil {
		return 0
	}
	return uint16(port)
}

// adapterDeps is the one Deps the node hands every adapter it builds (RFC 20
// §3.4): the filesystem, the node's budgets, registry and logger, and its
// watchdog, which each accept loop registers with. Every other field is
// zero: the node hands its adapters no tracer (`Config.Tracer` is the
// journals'), so SampleRatio samples none (RFC 25 §8.1).
//
// decision: an accept loop reports idle every 10 s on the runtime's clock,
// as Deps hands an adapter no clock (RFC 20 §3.4), while the watchdog reads
// the node's (RFC 25 §3.5). Outside a test both are the runtime's. A test
// that moves the node's clock past 60 s while an adapter serves finds
// liveness failed, so the node's tests serve none while they do. Overturned
// when a test must, and then by a clock in Deps.
func adapterDeps(fs vfs.Service, b *resources.Budgets, m *metrics.Registry, log *slog.Logger, w *health.Watchdog) adapter.Deps {
	return adapter.Deps{
		VFS:     fs,
		Budgets: b,
		Metrics: m,
		Logger:  log,
		Watch:   func(loop string) adapter.Loop { return w.Loop(loop) },
	}
}

// shape is a protocol's settings shape, which its slot runs.
type shape interface {
	settings.NFS | settings.SMB
	settings.Settings
}

// slot is one protocol's adapter as the node runs it: built through the
// protocol's New, moved through the sidecar runner by RFC 23 §2.3's adapter
// rows, one call at a time.
type slot[S shape] struct {
	name   string                                         // the protocol: the runner's name for the adapter, and the refusal's
	build  func(adapter.Deps, S) (adapter.Adapter, error) // the protocol's New
	deps   adapter.Deps
	runner *sidecar.Runner
	addrs  []netip.Addr    // adapters.listen, resolved: what every adapter binds
	taken  []uint16        // the API's and the metrics listener's ports
	clock  engine.Clock    // what a drain's idle reports wait on
	drains context.Context // what a drain runs under: the node's, which stop step 2 ends

	// want is what apply hands the slot, nil while nothing sets it: the
	// adapter disabled.
	want atomic.Pointer[desired[S]]

	running adapter.Adapter // nil while nothing serves; still the draining one when reconcile stopped waiting on it
	runs    S               // what running serves with: the last settings that served
	refused S               // what refusal refused
	refusal *refusal        // the last call's, nil once a call applied
	gen     uint64          // the settings generation of the last call that applied: the one the slot runs

	// last is what the last call left, which the node's health, its
	// connection listing and its status read from other goroutines; nil
	// before the first call. A call in flight leaves it as the call before
	// left it, but for a drain, which marks it draining: a rebuild waiting on
	// a drain reads as the adapter it replaces, draining.
	last atomic.Pointer[outcome]
}

// outcome is what a slot's call left: the adapter running, nil when none;
// while the adapter is enabled and nothing serves it, the error that refused
// it; whether a drain of running is under way; and the settings generation
// the slot runs.
type outcome struct {
	running    adapter.Adapter
	refused    error
	draining   bool
	generation uint64
}

// desired is what a slot is to run: whether its adapter is enabled, and its
// protocol's settings.
type desired[S shape] struct {
	enabled  bool
	settings S
}

// applyWanted is apply with what the slot is to run, read at settings
// generation gen.
func (s *slot[S]) applyWanted(ctx context.Context, loop *health.Loop, gen uint64) *refusal {
	var w desired[S]
	if p := s.want.Load(); p != nil {
		w = *p
	}
	return s.apply(ctx, loop, gen, w.enabled, w.settings)
}

// newSlots builds the node's slot for each protocol, nfs and smb, over one
// Deps and one runner (RFC 20 §3.4, §3.5), each drain under drains.
func newSlots(d adapter.Deps, r *sidecar.Runner, addrs []netip.Addr, taken []uint16, clock engine.Clock, drains context.Context) (*slot[settings.NFS], *slot[settings.SMB]) {
	return &slot[settings.NFS]{name: "nfs", build: nfs.New, deps: d, runner: r, addrs: addrs, taken: taken, clock: clock, drains: drains},
		&slot[settings.SMB]{name: "smb", build: smb.New, deps: d, runner: r, addrs: addrs, taken: taken, clock: clock, drains: drains}
}

// apply moves the adapter to what enabled and next say, read at settings
// generation gen, next's Addrs the slot's own (RFC 20 §3.6), and returns its
// refusal, nil when it applied (RFC 23 §2.3). Once it applied, the slot runs
// gen; a refused call leaves the generation it ran. Settings it refused while
// an adapter served on are refused again, untouched, so a call repeated on
// the same records does nothing; with nothing serving, a repeated call tries
// again. Every other call leaves its outcome in last.
func (s *slot[S]) apply(ctx context.Context, loop *health.Loop, gen uint64, enabled bool, next S) *refusal {
	next = withAddrs(next, s.addrs)
	if enabled && s.running != nil && s.refusal != nil && reflect.DeepEqual(next, s.refused) {
		return s.refusal
	}
	err := s.move(ctx, loop, enabled, next)
	if err == nil {
		s.gen = gen
	}
	o := outcome{running: s.running, generation: s.gen}
	if enabled && s.running == nil {
		o.refused = err
	}
	s.last.Store(&o)
	if err != nil {
		s.refused, s.refusal = next, s.refuse(err)
		return s.refusal
	}
	s.refusal = nil
	return nil
}

// move is RFC 23 §2.3's adapter row for enabled and next. Disabled, a
// running adapter is drained and stopped. Enabled with nothing running, one
// is built and started. Enabled and running, next equal to what it runs does
// nothing; the same port reaches it by Reconfigure; a changed port builds a
// new adapter, and only once New accepted next drains and stops the old one
// and starts the new, starting one with the settings that last served if the
// new one's start fails.
func (s *slot[S]) move(ctx context.Context, loop *health.Loop, enabled bool, next S) error {
	switch {
	case !enabled:
		if s.running == nil {
			return nil
		}
		return s.stop(ctx, loop)
	case s.running == nil:
		return s.start(ctx, next)
	case reflect.DeepEqual(next, s.runs):
		return nil
	case next.Common().Port == s.runs.Common().Port:
		if err := s.runner.Reconfigure(ctx, s.name, s.running, next); err != nil {
			return err
		}
		s.runs = next
		return nil
	}
	a, err := s.configure(next)
	if err != nil {
		return err
	}
	last := s.runs
	if err := s.stop(ctx, loop); err != nil {
		return err
	}
	if err := s.runner.Start(ctx, s.name, a); err != nil {
		return errors.Join(err, s.start(ctx, last))
	}
	s.running, s.runs = a, next
	return nil
}

// configure builds next's adapter, configured. A port other than 0 that is
// the API's or the metrics listener's is refused before New, as a port
// another socket holds is refused at Start (RFC 13 §6).
func (s *slot[S]) configure(next S) (adapter.Adapter, error) {
	if p := next.Common().Port; p != 0 && slices.Contains(s.taken, p) {
		return nil, fmt.Errorf("node: adapters.%s.port: %d is the API's or the metrics listener's port", s.name, p)
	}
	return s.build(s.deps, next)
}

// start builds next's adapter and starts it through the runner; once it
// serves, the slot runs it.
func (s *slot[S]) start(ctx context.Context, next S) error {
	a, err := s.configure(next)
	if err != nil {
		return err
	}
	if err := s.runner.Start(ctx, s.name, a); err != nil {
		return err
	}
	s.running, s.runs = a, next
	return nil
}

// stop drains and stops the running adapter through the runner, its
// sidecar syncs included, under the slot's drains, reporting loop idle
// every drainIdle on the node's clock until its Stop returns, or until ctx
// ends: the drain then goes on, its adapter still the slot's.
//
// decision: the call waits on the drain itself, up to drain_timeout plus
// 30 s, reporting its loop idle meanwhile, since RFC 25 §3.5 counts a
// waiting loop's idle report as its heartbeat and the watchdog fails
// liveness after 60 s without one; other changes wait for the call, one
// adapter at a time (RFC 23 §3.4). Overturned if a drain must not hold back
// other records.
//
// decision: the drain runs under the node's drains, not ctx, and the call
// stops waiting on it once ctx ends, as reconcile's stop ends it: waiting on
// would hold reconcile's stop past its bound, and ending the drain with ctx
// would cut it short. Stop step 2 ends the drains at its drain deadline, so
// this drain and a sidecar Sync it holds end by then, never sooner, and
// cannot hold step 2 past its bound. The adapter stays the slot's, so step 2
// stops it with the others, and its Stop, called again, returns once the
// first has stopped it (RFC 20 §3.4, RFC 23 §2.2). Overturned if a drain
// must end with the change that started it.
func (s *slot[S]) stop(ctx context.Context, loop *health.Loop) error {
	s.last.Store(&outcome{running: s.running, draining: true, generation: s.gen})
	done := make(chan error, 1)
	go func(a adapter.Adapter) { done <- s.runner.Stop(s.drains, s.name, a) }(s.running)
	for {
		select {
		case err := <-done:
			s.running = nil
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-s.clock.After(drainIdle):
			loop.Idle()
		}
	}
}

// refuse is the call's refusal of err, naming the adapter by its protocol,
// retried on reconcile's backoff only while nothing serves the adapter (RFC
// 23 §2.3). Its Generation is the caller's, which read the settings
// generation.
func (s *slot[S]) refuse(err error) *refusal {
	p := apiv1.Problem{Status: http.StatusUnprocessableEntity, Title: "the adapter was refused", Detail: err.Error()}
	b, _ := json.Marshal(p) // a Problem holds strings, ints and bools, which always marshal
	return &refusal{retry: s.running == nil, rec: entity.RefusedRecord{
		Record: entity.ResourceRef{Kind: entity.ResourceAdapter, ID: []byte(s.name)},
		Error:  b,
	}}
}

// withAddrs is s with addrs as its Addrs: the node's, never the record's
// (RFC 20 §3.6).
func withAddrs[S shape](s S, addrs []netip.Addr) S {
	switch p := any(&s).(type) {
	case *settings.NFS:
		p.Addrs = addrs
	case *settings.SMB:
		p.Addrs = addrs
	}
	return s
}

// applyAdapters calls each slot with what it is to run, and returns their
// refusals, each at settings generation gen, which the caller read.
func (n *Node) applyAdapters(ctx context.Context, loop *health.Loop, gen uint64) []refusal {
	var refusals []refusal
	for _, r := range []*refusal{n.nfs.applyWanted(ctx, loop, gen), n.smb.applyWanted(ctx, loop, gen)} {
		if r != nil {
			f := *r
			f.rec.Generation = gen
			refusals = append(refusals, f)
		}
	}
	return refusals
}

// adaptersHeld is every adapter the slots hold, serving or still draining,
// by protocol, and the longest drain_timeout they run.
func (n *Node) adaptersHeld() (map[string]adapter.Adapter, time.Duration) {
	held := map[string]adapter.Adapter{}
	var longest time.Duration
	if a := n.nfs.running; a != nil {
		held[n.nfs.name], longest = a, max(longest, n.nfs.runs.DrainTimeout)
	}
	if a := n.smb.running; a != nil {
		held[n.smb.name], longest = a, max(longest, n.smb.runs.DrainTimeout)
	}
	return held, longest
}

// status is the slot's adapter as the node's status reports it (RFC 23
// §3.4): its state, in RFC 20 §3.1's terms, and the settings generation it
// runs. An adapter enabled but refused, nothing serving it, stays configured
// (RFC 23 §3.4). Start step 9 calls every slot before the status writer's
// first turn, so a call has always left last.
//
// decision: a drain under way reports draining, the old adapter's, as RFC 20
// §3.1 names that state, for a disable's drain and a rebuild's alike, so the
// status says why an adapter still holds connections it no longer accepts.
func (s *slot[S]) status() entity.AdapterStatus {
	o := s.last.Load()
	state := entity.AdapterStopped
	switch {
	case o.draining:
		state = entity.AdapterDraining
	case o.running != nil:
		state = entity.AdapterServing
	case o.refused != nil:
		state = entity.AdapterConfigured
	}
	return entity.AdapterStatus{Protocol: s.name, State: state, Generation: o.generation}
}

// conditionListenerLost is the condition an adapter's Health names for a
// lost listener (RFC 20 §3.7).
const conditionListenerLost = "adapter_listener_lost"

// lost reports whether the adapter the slot runs names
// adapter_listener_lost in its Health now (RFC 20 §3.7).
func (s *slot[S]) lost() bool {
	o := s.last.Load()
	return o != nil && o.running != nil && slices.Contains(o.running.Health().Conditions, conditionListenerLost)
}

// conditions appends the slot's conditions to dst, each on the node and
// naming the adapter: adapter_listener_lost while lost, and adapter_refused
// while its last call left it refused, its cause that call's error (RFC 20
// §3.7, RFC 25 §3.9).
func (s *slot[S]) conditions(dst []health.Condition, node health.Subject) []health.Condition {
	node.Name = s.name
	if s.lost() {
		dst = append(dst, health.Condition{Name: conditionListenerLost, Subject: node})
	}
	if o := s.last.Load(); o != nil && o.refused != nil {
		dst = append(dst, health.Condition{Name: "adapter_refused", Subject: node, Cause: o.refused})
	}
	return dst
}

// adapterConditions is the adapters' conditions as the health evaluator
// reads them, derived at each evaluation: each slot's, and sidecar_failed on
// the node naming each sidecar whose last Sync failed (RFC 20 §3.5, §3.7;
// RFC 25 §3.9).
type adapterConditions struct{ n *Node }

func (c *adapterConditions) Conditions(dst []health.Condition) []health.Condition {
	node := c.n.subject()
	dst = c.n.nfs.conditions(dst, node)
	dst = c.n.smb.conditions(dst, node)
	for _, name := range c.n.sidecars.Failed() {
		sub := node
		sub.Name = name
		dst = append(dst, health.Condition{Name: "sidecar_failed", Subject: sub})
	}
	return dst
}
