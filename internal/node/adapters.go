package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
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

// drainIdle is how often a call waiting on an adapter's drain reports its
// loop idle: inside RFC 25 §3.5's 30 s, as an accept loop does.
const drainIdle = 10 * time.Second

// listenAddrs resolves adapters.listen (RFC 13 Appendix C) into the
// addresses every adapter binds, given whether a tcp6 socket opens on [::]:0
// on this host. A literal that does not parse is refused naming the key and
// its index, never the value, which may come from the environment (RFC 13
// §6); every such literal is refused together.
//
// decision: an unset adapters.listen is 0.0.0.0, and :: beside it only where
// tcp6 opens, for RFC 13 Appendix C's "every interface": an adapter binds ::
// on tcp6 alone, with IPV6_V6ONLY, and a partial bind is a failed start (RFC
// 20 §3.1), so :: on a host with IPv6 off would refuse every adapter. A list
// that is set is bound as written, :: included. Withdraw it if RFC 13
// Appendix C's default comes to name its addresses.
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
		addrs[i] = a
	}
	if bad != nil {
		return nil, errors.Join(bad...)
	}
	return addrs, nil
}

// adapterDeps is the one Deps the node hands every adapter it builds (RFC 20
// §3.4): the filesystem, the node's budgets, registry and logger, and its
// watchdog, which each accept loop registers with. Every other field is
// zero: the node is handed no tracer, so SampleRatio samples none (RFC 25
// §8.1).
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
	addrs  []netip.Addr // adapters.listen, resolved: what every adapter binds
	taken  []uint16     // the API's and the metrics listener's ports
	clock  engine.Clock // what a drain's idle reports wait on

	running adapter.Adapter // nil while nothing serves
	runs    S               // what running serves with: the last settings that served
	refused S               // what refusal refused
	refusal *refusal        // the last call's, nil once a call applied
}

// newSlots builds the node's slot for each protocol, nfs and smb, over one
// Deps and one runner (RFC 20 §3.4, §3.5).
func newSlots(d adapter.Deps, r *sidecar.Runner, addrs []netip.Addr, taken []uint16, clock engine.Clock) (*slot[settings.NFS], *slot[settings.SMB]) {
	return &slot[settings.NFS]{name: "nfs", build: nfs.New, deps: d, runner: r, addrs: addrs, taken: taken, clock: clock},
		&slot[settings.SMB]{name: "smb", build: smb.New, deps: d, runner: r, addrs: addrs, taken: taken, clock: clock}
}

// apply moves the adapter to what enabled and next say, next's Addrs the
// slot's own (RFC 20 §3.6), and returns its refusal, nil when it applied
// (RFC 23 §2.3). Settings it refused while an adapter served on are refused
// again, untouched, so a call repeated on the same records does nothing; with
// nothing serving, a repeated call tries again.
func (s *slot[S]) apply(ctx context.Context, loop *health.Loop, enabled bool, next S) *refusal {
	next = withAddrs(next, s.addrs)
	if enabled && s.running != nil && s.refusal != nil && reflect.DeepEqual(next, s.refused) {
		return s.refusal
	}
	if err := s.move(ctx, loop, enabled, next); err != nil {
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

// stop drains and stops the running adapter through the runner, reporting
// loop idle every drainIdle on the node's clock until its Stop returns.
//
// decision: the call waits on the drain itself, up to drain_timeout plus
// 30 s, reporting its loop idle meanwhile, since RFC 25 §3.5 counts a
// waiting loop's idle report as its heartbeat and the watchdog fails
// liveness after 60 s without one; other changes wait for the call, one
// adapter at a time (RFC 23 §3.4). Overturned if a drain must not hold back
// other records.
func (s *slot[S]) stop(ctx context.Context, loop *health.Loop) error {
	done := make(chan error, 1)
	go func(a adapter.Adapter) { done <- s.runner.Stop(ctx, s.name, a) }(s.running)
	s.running = nil
	for {
		select {
		case err := <-done:
			return err
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
