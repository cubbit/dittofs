package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
)

// Server is one adapter: its listeners, connections, drain and health, for
// every protocol.
type Server struct {
	ls []Listener
	// listen opens the socket a bind names. It is a field because no real
	// bind can be made to answer with what a test needs on demand: an
	// in-use refusal, to hold a family's port out from under a port-0
	// listener's pick or to fault a configured one, or a listener that
	// reports an address no TCP one is. A test's server injects its answer
	// through SetListen alone; NewServer sets the real opener.
	listen func(context.Context, string, string) (net.Listener, error)
	// decision: the pointer is atomic because Reconfigure swaps the snapshot
	// while connections read it; the one other store is NewServer's, before
	// the server is shared.
	settings atomic.Pointer[settings.Settings] // the protocol's whole shape, as New or the last Reconfigure gave it
	addrs    []netip.Addr                      // the settings' addresses, the server's own copy: the caller's settings may change under it
	d        adapter.Deps
	recv     *resources.BufPool // the receive pool every record is charged to
	reply    *resources.BufPool // the reply pool every reply is taken from
	sh       *shared            // the pools, metrics, clock and logger every connection shares

	mu       sync.RWMutex
	state    adapter.State
	bound    []*bound           // every socket open now, by listener, then by address in the settings' order
	conns    map[*Conn]struct{} // every connection open now
	stopDone chan struct{}      // the running Stop closes it once the server is stopped; nil before
}

// bound is one listener's socket on one address, with the state its accept
// loop and the server's health share.
type bound struct {
	l            *Listener
	addr         netip.AddrPort // the address as the settings name it, at the port the socket bound
	ln           net.Listener   // the socket itself; its close ends the accept loop
	stopped      chan struct{}  // closed with the socket, so a backoff's sleep ends too
	loopDone     chan struct{}  // closed once the accept loop ended
	firstFailure atomic.Int64   // the streak's first Accept failure, 0 while the listener is accepting
	lastFailure  atomic.Int64   // the streak's last one, from which Health derives the lost listener
}

var _ adapter.Adapter = (*Server)(nil)

// Listener is one protocol on one port, bound on each of the settings' Addrs.
// Addrs reports it under one Socket per service it names, on its network,
// each address at the port bound there.
//
// decision: a listener names its services rather than holding a Socket,
// because one port carries several: NFS's carries NFS, MOUNT, NLM and NSM,
// told apart by RPC program. So Network repeats Socket's field, and a field
// added to Socket must be added here too, or Addrs builds incomplete keys.
// Withdraw it, and give Listener a Socket, if each port comes to carry one
// service, as it would were NFS's auxiliary programs given ports of their own.
type Listener struct {
	Services []string // the names Addrs and a sidecar know it by: "nfs", "mount", "nlm", "nsm", "smb"
	Network  string   // "tcp" or "udp"
	Port     uint16   // 0: one the system picks, which Addrs reports
	Protocol Protocol
}

// NewServer returns the adapter New hands back: each listener bound on each
// of the settings' Addrs, all or none at Start; Reconfigure binds them on an
// added address the same way. s is the protocol's whole shape, which New was
// given; core reads its Adapter: a nil one is refused, a typed nil such as an
// unassigned *settings.NFS among them, so Start never dereferences a
// snapshot the caller never gave. It refuses an empty listener set, a
// listener naming no protocol (a typed nil among them), a listener naming no
// service, an empty one or one twice, two listeners naming the same Socket or
// the same network and nonzero port, and a listener on any network but tcp,
// with an error wrapping errors.ErrUnsupported. It keeps its own copy of ls,
// so a later change to the caller's leaves the server as checked. It opens
// nothing: the server is configured.
func NewServer(d adapter.Deps, s settings.Settings, ls ...Listener) (*Server, error) {
	if isNil(s) {
		// Settings are the protocol's whole shape; a nil one is a program bug
		// that would leave the server reading a snapshot it does not have, so
		// it is refused with the prefix its sibling refusals carry.
		return nil, fmt.Errorf("core: new server: %w", errNoSettings)
	}
	ls = slices.Clone(ls)
	for i := range ls {
		ls[i].Services = slices.Clone(ls[i].Services)
	}
	if err := checkListeners(ls); err != nil {
		return nil, fmt.Errorf("core: new server: %w", err)
	}
	if d.Budgets == nil {
		return nil, fmt.Errorf("core: new server: %w", errNoBudgets)
	}
	// A listener whose stages accept would refuse shows its wiring fault
	// here, at configured, rather than at its first request (RFC 20 section
	// 3.1): NewConn(nil) is the one call that reads no connection, and
	// withDefaults is accept's own check, so the faults one refuses can
	// never drift from the faults the other would.
	for i := range ls {
		if _, err := withDefaults(ls[i].Protocol.NewConn(nil)); err != nil {
			return nil, fmt.Errorf("core: new server: listener %d %q: %w", i, ls[i].Services, err)
		}
	}
	srv := &Server{ls: ls, d: d, conns: map[*Conn]struct{}{}, listen: (&net.ListenConfig{}).Listen}
	srv.recv = resources.NewBufPool(d.Budgets.Receive)
	srv.reply = resources.NewBufPool(d.Budgets.Reply)
	srv.sh = newShared(d, srv.reply)
	// The server owns the Addrs alone: srv.addrs is its own copy, and Start
	// binds that. The rest of the settings is the snapshot as New handed it,
	// shared with the caller where the settings sit behind a pointer - the
	// DisabledOperations slice among them - and no field of it is read yet;
	// the first one that is takes a copy of its own (RFC 20 section 3.6).
	srv.addrs = slices.Clone(s.Common().Addrs)
	srv.settings.Store(&s)
	return srv, nil
}

// errNoSettings is a NewServer handed no settings at all.
var errNoSettings = errors.New("no settings")

// isNil reports v as nil whether it is a nil interface or holds a nil
// pointer: a Settings or a Protocol that is a typed nil, an unassigned
// *settings.NFS say, is refused where the check dereferences nothing, as the
// untyped nil is.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// checkListeners refuses what NewServer refuses, naming the listener by its
// position and its services.
func checkListeners(ls []Listener) error {
	type port struct {
		network string
		n       uint16
	}
	if len(ls) == 0 {
		return errors.New("no listener")
	}
	sockets, ports := map[adapter.Socket]int{}, map[port]int{}
	for i, l := range ls {
		// decision: a listener on any network but tcp is refused, since core
		// has no datagram listener: NFS's NLM and NSM over udp wait for one.
		// Withdraw it when core reads datagrams.
		if l.Network != "tcp" {
			return fmt.Errorf("listener %d %q: network %q: %w", i, l.Services, l.Network, errors.ErrUnsupported)
		}
		if isNil(l.Protocol) {
			return fmt.Errorf("listener %d %q names no protocol", i, l.Services)
		}
		if len(l.Services) == 0 {
			return fmt.Errorf("listener %d %q names no service", i, l.Services)
		}
		for _, svc := range l.Services {
			if svc == "" {
				return fmt.Errorf("listener %d %q names an empty service", i, l.Services)
			}
			k := adapter.Socket{Service: svc, Network: l.Network}
			if j, ok := sockets[k]; ok {
				if j == i {
					return fmt.Errorf("listener %d %q names %s twice", i, l.Services, svc)
				}
				return fmt.Errorf("listener %d %q names %s over %s, as listener %d does", i, l.Services, svc, l.Network, j)
			}
			sockets[k] = i
		}
		if l.Port != 0 {
			k := port{l.Network, l.Port}
			if j, ok := ports[k]; ok {
				return fmt.Errorf("listener %d %q binds %s port %d, as listener %d does", i, l.Services, l.Network, l.Port, j)
			}
			ports[k] = i
		}
	}
	return nil
}

// Start moves configured to serving once every listener is bound on each of
// the settings' Addrs. A bind that fails closes what Start opened and leaves
// the server configured; its error names the listener and the address. Start
// from any other state is refused with an error naming the state.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != adapter.StateConfigured {
		return fmt.Errorf("core: start: the server is %s", s.state)
	}
	if s.settings.Load() == nil {
		// A Server is what NewServer builds: a zero value carries no snapshot
		// and never served. Reading its absent settings would panic.
		return fmt.Errorf("core: start: the server was not built by NewServer")
	}
	for i := range s.ls {
		opened, err := s.bindEach(ctx, i)
		if err != nil {
			s.closeBound()
			return fmt.Errorf("core: start: %w", err)
		}
		for j := range opened {
			opened[j].stopped = make(chan struct{})
			opened[j].loopDone = make(chan struct{})
		}
		s.bound = append(s.bound, opened...)
	}
	// The loops run only over sockets Start opened, so the all-or-none bind
	// above is not undone by a later failure.
	s.state = adapter.StateServing
	for _, b := range s.bound {
		go s.acceptLoop(b)
	}
	return nil
}

// portRepicks is how many fresh system picks a port-0 listener may take when
// a later address's family holds the port an earlier one picked.
const portRepicks = 2

// bindEach binds listener i on each of the settings' Addrs: all of its
// sockets, or none. At its own configured port each address is bound once,
// since a taken one is a real refusal; at port 0 the system picks on the
// first address and the rest reuse that port.
//
// decision: a system-picked port that a later address's family still holds
// is repicked, the whole group at a time, at most portRepicks times, since
// IPv4 and IPv6 ports are allocated apart and Start cannot plan for a port
// freedom this process does not own. A port the node configured that is
// taken is a real refusal, never repicked. Withdraw the repicks when a Start
// can ask for one port on every family at once.
func (s *Server) bindEach(ctx context.Context, i int) ([]*bound, error) {
	for repick := 0; ; repick++ {
		opened := make([]*bound, 0, len(s.addrs))
		port := s.ls[i].Port
		for j, a := range s.addrs {
			// One listener is one port: a port the system picked is resolved
			// on the first address it binds and used on the rest, so a service
			// never reports a port of its own per address.
			if j > 0 && port == 0 {
				port = opened[len(opened)-1].addr.Port()
			}
			b, err := bind(ctx, s.listen, &s.ls[i], a, port)
			if err == nil {
				opened = append(opened, b)
				continue
			}
			for _, ob := range opened {
				_ = ob.ln.Close()
			}
			// Only a system pick is repicked, and only on the runtime's
			// in-use refusal, whichever family answered it: the group binds
			// again, from its first address, with a fresh one.
			if s.ls[i].Port == 0 && repick < portRepicks && errors.Is(err, syscall.EADDRINUSE) {
				break
			}
			return nil, err
		}
		if len(opened) == len(s.addrs) {
			return opened, nil
		}
	}
}

// bind opens l's socket on a, at port (l's own, or the one the first address's
// socket resolved). An IPv4-mapped address binds the IPv4 address it maps, as
// the settings read it.
//
// decision: each address is bound on its family's network, tcp4 or tcp6,
// never tcp, and is reported as the settings name it with the port its socket
// bound. On tcp, Go binds a wildcard as one dual-stack IPv6 socket, which
// reports [::] and collides with a second listener on :: at the same port; on
// tcp6 it sets IPV6_V6ONLY. Withdraw it if a deployment needs one dual-stack
// socket per wildcard.
func bind(ctx context.Context, listen func(context.Context, string, string) (net.Listener, error), l *Listener, a netip.Addr, port uint16) (*bound, error) {
	ip, network := a.Unmap(), "tcp6"
	if ip.Is4() {
		network = "tcp4"
	}
	ln, err := listen(ctx, network, netip.AddrPortFrom(ip, port).String())
	if err != nil {
		return nil, fmt.Errorf("listener %q on %s: %w", l.Services, netip.AddrPortFrom(a, port), err)
	}
	ta, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		// A listener that reports no TCP address cannot be named at the
		// address the settings gave, so it is closed and refused, not read
		// for a port that is not there.
		_ = ln.Close()
		return nil, fmt.Errorf("listener %q on %s: the opener answered a %T, not a TCP listener", l.Services, netip.AddrPortFrom(a, port), ln.Addr())
	}
	b := &bound{l: l, ln: ln}
	b.addr = netip.AddrPortFrom(a, ta.AddrPort().Port())
	return b, nil
}

// closeBound closes every socket open now, ending its accept loop, in its
// Accept or in its backoff's sleep. The caller holds s.mu.
func (s *Server) closeBound() {
	for _, b := range s.bound {
		close(b.stopped)
		_ = b.ln.Close()
	}
	s.bound = nil
}

// Reconfigure applies next, the server's own protocol's shape, to connections
// set up after it returns; the other protocol's, or any other type, is
// refused.
func (s *Server) Reconfigure(ctx context.Context, next settings.Settings) error {
	return fmt.Errorf("core: reconfigure: %w", errors.ErrUnsupported)
}

// Stop moves a serving server through draining to stopped, and a configured
// one straight to stopped. It moves under the first lock to draining, one of
// the four states (RFC 20 section 3.1), before it waits its loops, so while
// the loops are waited a socket handed over closes before it reads and
// Health reports the state. The listeners close first, so no connection is
// accepted while draining, and every connection's pipeline is told the server
// drains, one being built before its first record included, since the accept
// loops' end waits every connection into the registry. Draining ends when
// nothing runs on any connection, or at the drain deadline, the earlier of
// this context's deadline and drain_timeout from the snapshot, on the
// server's clock; what still runs at it is abandoned, its context cancelled
// and its reply not sent (RFC 20 sections 3.1 and 3.4). Every connection is
// then closed and torn down, and Stop returns once every OnClose has, at most
// 30 s past the drain deadline: nothing a protocol's registry handed to
// Deps.Retry is waited for (RFC 20 sections 3.2 and 3.4). A concurrent or
// later Stop returns once the server is stopped.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.state == adapter.StateStopped {
		s.mu.Unlock()
		return nil
	}
	if s.stopDone != nil {
		// A Stop is already running: this one returns once it has stopped
		// the server.
		done := s.stopDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.stopDone = make(chan struct{})
	wasServing := s.state == adapter.StateServing
	s.state = adapter.StateDraining
	looped := slices.Clone(s.bound)
	s.closeBound()
	s.mu.Unlock()
	s.stopOnLoops(looped)
	// The loops have ended, so not one more socket can be accepted: every
	// connection the registry will ever hold while this server stops is in
	// this snapshot, and a socket its loop was handing to serveConn as it
	// ended was refused at its registration and torn down on that loop,
	// OnClose included, before the loop ended.
	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	if wasServing {
		// Every pipeline is told before Health names the state, so a request
		// that meets draining meets the refusal too (RFC 20 section 3.1).
		for _, c := range conns {
			c.p.drain()
		}
		s.state = adapter.StateDraining
		s.mu.Unlock()
		s.drainConns(ctx, conns)
	} else {
		s.mu.Unlock()
	}
	for _, c := range conns {
		c.Close(errStop)
	}
	for _, c := range conns {
		<-c.done
	}
	s.mu.Lock()
	s.state = adapter.StateStopped
	s.mu.Unlock()
	close(s.stopDone)
	return nil
}

// drainConns waits until nothing runs on any connection, or until the drain
// deadline, the earlier of ctx's deadline and drain_timeout from the
// snapshot, on the server's clock; what still runs at it is left for the
// close that follows (RFC 20 sections 3.1 and 3.4).
func (s *Server) drainConns(ctx context.Context, conns []*Conn) {
	if len(conns) == 0 {
		return
	}
	at := s.sh.clock.Now().Add((*s.settings.Load()).Common().DrainTimeout)
	wait, cancel := withClockDeadline(ctx, s.sh.clock, at)
	defer cancel()
	// decision: the sweep repeats, never one wait per connection, since a
	// request the drain serves, a release say, starts while it waits, so a
	// connection that fell quiet earlier is waited again. It ends when one
	// full pass reads every connection quiet, and the moment that decides
	// races a request admitted in the same instant, which the close then
	// abandons as at the deadline. Overturned if the pipeline grows a place
	// to latch the drain on each request's arrival.
	for {
		waited := false
		for _, c := range conns {
			if c.p.quiet() {
				continue
			}
			waited = true
			if err := c.p.wait(wait); err != nil {
				return // the drain deadline: the close abandons what runs
			}
		}
		if !waited {
			return
		}
	}
}

// errStop is a connection's close, the server stopping.
var errStop = errors.New("core: the server stopped")

// errNoBudgets is a server built without the budgets its pools need.
var errNoBudgets = errors.New("no budgets")

// register adds a connection to the registry, and reports false when the
// server is not serving: such a connection is closed at once, before it reads,
// so Stop's snapshot is never missed.
func (s *Server) register(c *Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != adapter.StateServing {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// unregister drops a connection from the registry once its teardown ended.
func (s *Server) unregister(c *Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// Addrs is every address each listener is bound on now, by service and
// network, in a fresh map the caller owns, slices included: each address
// as the settings name it, its port the one bound, so a port of 0 is
// resolved and a wildcard such as 0.0.0.0 is reported as itself, never as
// the [::] its socket reports; an address Reconfigure added included and
// one it removed gone. It is empty before Start, after Stop, and while
// serving on an empty Addrs. The node builds each Serving from it.
func (s *Server) Addrs() map[adapter.Socket][]netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[adapter.Socket][]netip.AddrPort)
	for _, b := range s.bound {
		for _, svc := range b.l.Services {
			k := adapter.Socket{Service: svc, Network: b.l.Network}
			m[k] = append(m[k], b.addr)
		}
	}
	return m
}

// Health is the server's state with the conditions its sockets' own evidence
// derives: a lost listener raises adapter_listener_lost until its next
// accepted connection. The server reports no other condition.
func (s *Server) Health() adapter.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := adapter.Health{State: s.state}
	now := s.sh.clock.Now()
	for _, b := range s.bound {
		if b.lost(now) {
			h.Conditions = []string{conditionListenerLost}
			break
		}
	}
	return h
}

// Connections lists the connections open now, as the API's client list reads
// them; a connection whose protocol names no session reports none.
func (s *Server) Connections() iter.Seq[adapter.ConnInfo] {
	s.mu.RLock()
	infos := make([]adapter.ConnInfo, 0, len(s.conns))
	for c := range s.conns {
		infos = append(infos, adapter.ConnInfo{Protocol: c.protocol(), Remote: c.remoteAddr, Since: c.since})
	}
	s.mu.RUnlock()
	return func(yield func(adapter.ConnInfo) bool) {
		for _, info := range infos {
			if !yield(info) {
				return
			}
		}
	}
}
