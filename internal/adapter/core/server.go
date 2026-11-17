package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// Server is one adapter: its listeners, connections, drain and health, for
// every protocol.
type Server struct {
	ls []Listener
	// decision: the pointer is atomic because Reconfigure (T4.6's subtask) swaps
	// the snapshot while connections read it; the one other store is
	// NewServer's, before the server is shared.
	settings atomic.Pointer[settings.Settings] // the protocol's whole shape, as New or the last Reconfigure gave it
	addrs    []netip.Addr                      // the settings' addresses, the server's own copy: the caller's settings may change under it

	mu    sync.Mutex
	state adapter.State
	bound []bound // every socket open now, by listener, then by address in the settings' order
}

// bound is one listener's socket on one address.
type bound struct {
	l    *Listener
	addr netip.AddrPort // the address as the settings name it, at the port the socket bound
	ln   net.Listener
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
// given; core reads its Adapter. It refuses an empty listener set, a listener
// naming no service, an empty one or one twice, two listeners naming the same Socket or
// the same network and nonzero port, and a listener on any network but tcp,
// with an error wrapping errors.ErrUnsupported. It keeps its own copy of ls,
// so a later change to the caller's leaves the server as checked. It opens
// nothing: the server is configured.
func NewServer(d adapter.Deps, s settings.Settings, ls ...Listener) (*Server, error) {
	if s == nil {
		// Settings are the protocol's whole shape; a nil one is a program bug
		// that would panic at the first snapshot read, so it is refused here,
		// as an empty listener set is.
		return nil, errors.New("no settings")
	}
	ls = slices.Clone(ls)
	for i := range ls {
		ls[i].Services = slices.Clone(ls[i].Services)
	}
	if err := checkListeners(ls); err != nil {
		return nil, fmt.Errorf("core: new server: %w", err)
	}
	// A listener whose stages accept would refuse shows its wiring fault
	// here, at configured, rather than at its first request (RFC 20 section
	// 3.1): NewConn(nil) is the one call that reads no connection.
	for i := range ls {
		st := ls[i].Protocol.NewConn(nil)
		if st.Header == nil || st.Router == nil || st.Protocol == "" {
			return nil, fmt.Errorf("core: new server: listener %d %q: stages without Header, Router or Protocol", i, ls[i].Services)
		}
	}
	srv := &Server{ls: ls}
	// The settings are the server's own snapshot from here: the caller's copy
	// may change under it, its Addrs included (RFC 20 section 3.6).
	srv.addrs = slices.Clone(s.Common().Addrs)
	srv.settings.Store(&s)
	return srv, nil
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
		if l.Protocol == nil {
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
		port := s.ls[i].Port
		for j, a := range s.addrs {
			// One listener is one port: one the system picked is resolved on
			// the first address it binds and used on the rest, so a service
			// never reports a port of its own per address.
			if j > 0 && port == 0 {
				port = s.bound[len(s.bound)-1].addr.Port()
			}
			b, err := bind(ctx, &s.ls[i], a, port)
			if err != nil {
				s.closeBound()
				return fmt.Errorf("core: start: %w", err)
			}
			s.bound = append(s.bound, b)
		}
	}
	s.state = adapter.StateServing
	return nil
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
func bind(ctx context.Context, l *Listener, a netip.Addr, port uint16) (bound, error) {
	ip, network := a.Unmap(), "tcp6"
	if ip.Is4() {
		network = "tcp4"
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, network, netip.AddrPortFrom(ip, port).String())
	if err != nil {
		return bound{}, fmt.Errorf("listener %q on %s: %w", l.Services, netip.AddrPortFrom(a, port), err)
	}
	bound := bound{l: l, ln: ln}
	bound.addr = netip.AddrPortFrom(a, ln.Addr().(*net.TCPAddr).AddrPort().Port())
	return bound, nil
}

// closeBound closes every socket open now. The caller holds s.mu.
func (s *Server) closeBound() {
	for _, b := range s.bound {
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

// Stop closes every listener and moves the server to stopped, from any
// state; a second call returns nil.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeBound()
	s.state = adapter.StateStopped
	return nil
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

// Health is the server's state; it raises no condition, so it is ready while
// serving.
func (s *Server) Health() adapter.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	return adapter.Health{State: s.state}
}

// Connections lists the connections open now.
func (s *Server) Connections() iter.Seq[adapter.ConnInfo] {
	panic("not implemented")
}
