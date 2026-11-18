package core

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// Reconfigure applies next, the server's own protocol's shape alone, to
// connections set up after it returns. The other protocol's shape, or any
// other type, is refused; so is what next.Validate refuses, and a changed
// Port, with adapter.ErrPortChanged, compared on that field alone; a refusal
// leaves the server as it was. While configured nothing is bound: the
// settings Start will bind are replaced. While draining or stopped it is
// refused naming the state. An added address's listeners are bound all or
// none before the snapshot changes, with their accept loops started; a
// removed address's listeners and every connection accepted on them are
// closed after it and torn down before Reconfigure returns (RFC 20
// sections 3.1, 3.4 and 3.6).
//
// decision: Addrs is live, though a host's listen addresses are restart-only
// in its bootstrap file: a takeover must add an address to a serving server,
// and a restart would drop every connection on the others. The node, not the
// operator, changes Addrs while serving (RFC 20 section 3.6).
func (s *Server) Reconfigure(ctx context.Context, next settings.Settings) error {
	if isNil(next) {
		return fmt.Errorf("core: reconfigure: %w", errNoSettings)
	}
	stored := s.settings.Load()
	if stored == nil {
		return errors.New("core: reconfigure: the server was not built by NewServer")
	}
	if cur := *stored; reflect.TypeOf(next) != reflect.TypeOf(cur) {
		return fmt.Errorf("core: reconfigure: %w", errOtherShape)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("core: reconfigure: %w", err)
	}
	if next.Common().Port != (*stored).Common().Port {
		return fmt.Errorf("core: reconfigure: %w", adapter.ErrPortChanged)
	}
	s.mu.Lock()
	switch s.state {
	case adapter.StateDraining, adapter.StateStopped:
		st := s.state
		s.mu.Unlock()
		return fmt.Errorf("core: reconfigure: the server is %s", st)
	case adapter.StateConfigured:
		// Nothing is bound, and only Start binds: the settings are what it
		// will bind.
		s.settings.Store(&next)
		s.addrs = slices.Clone(next.Common().Addrs)
		s.mu.Unlock()
		return nil
	}
	add, drop := diffAddrs(s.addrs, next.Common().Addrs)
	opened, err := s.bindAdded(ctx, add)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("core: reconfigure: %w", err)
	}
	for j := range opened {
		opened[j].stopped = make(chan struct{})
		opened[j].loopDone = make(chan struct{})
	}
	s.bound = append(s.bound, opened...)
	for _, b := range opened {
		// The loop runs before the snapshot changes, so the added address
		// serves from the moment the settings name it (RFC 20 section 3.6).
		go s.acceptLoop(b)
	}
	s.settings.Store(&next)
	s.addrs = slices.Clone(next.Common().Addrs)
	// A removed address's listeners close after the snapshot changes, so a
	// moved address is never unserved between the two (RFC 20 section 3.6).
	off := make([]*bound, 0, len(drop))
	kept := s.bound[:0]
	for _, b := range s.bound {
		if slices.Contains(drop, b.addr.Addr()) {
			off = append(off, b)
			continue
		}
		kept = append(kept, b)
	}
	s.bound = kept
	s.mu.Unlock()
	for _, b := range off {
		close(b.stopped)
		_ = b.ln.Close()
	}
	// Each removed loop hands every connection it accepted to the registry
	// before it ends, so the walk after this wait is complete.
	for _, b := range off {
		<-b.loopDone
	}
	s.mu.Lock()
	gone := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		if slices.Contains(drop, c.accept) {
			gone = append(gone, c)
		}
	}
	s.mu.Unlock()
	for _, c := range gone {
		c.Close(errRemoved)
	}
	for _, c := range gone {
		<-c.done
	}
	return nil
}

// errOtherShape is a Reconfigure handed the other protocol's settings, or any
// other type: the server keeps its own.
var errOtherShape = errors.New("another protocol's settings")

// errRemoved is a connection's close, its bound address removed by
// Reconfigure.
var errRemoved = errors.New("core: the address was removed")

// bindAdded binds every listener on each of as, all or none, closing what it
// opened on a failure. The caller holds s.mu. A listener at a configured port
// binds it; at port 0 it joins the port its sockets already hold, and one
// bound nowhere, as on a server serving on no address, gets a fresh pick
// shared by the addresses added together.
//
// decision: an added address never repicks a port, since rebinding the
// others for a free one would drop the connections a takeover keeps: a held
// port is a refusal, as a configured port's is.
func (s *Server) bindAdded(ctx context.Context, as []netip.Addr) ([]*bound, error) {
	opened := make([]*bound, 0, len(as)*len(s.ls))
	for i := range s.ls {
		port := s.ls[i].Port
		if port == 0 {
			port = s.boundPort(i)
		}
		for _, a := range as {
			b, err := bind(ctx, s.listen, &s.ls[i], a, port)
			if err != nil {
				for _, ob := range opened {
					_ = ob.ln.Close()
				}
				return nil, err
			}
			if s.ls[i].Port == 0 && port == 0 {
				port = b.addr.Port()
			}
			opened = append(opened, b)
		}
	}
	return opened, nil
}

// boundPort is the port listener i's open sockets hold, or 0 where it has
// none. The caller holds s.mu.
func (s *Server) boundPort(i int) uint16 {
	for _, b := range s.bound {
		if b.l == &s.ls[i] {
			return b.addr.Port()
		}
	}
	return 0
}

// diffAddrs is which of next the server does not hold, and which of its own
// next drops, each in its list's order.
func diffAddrs(cur, next []netip.Addr) (add, drop []netip.Addr) {
	for _, a := range next {
		if !slices.Contains(cur, a) {
			add = append(add, a)
		}
	}
	for _, a := range cur {
		if !slices.Contains(next, a) {
			drop = append(drop, a)
		}
	}
	return add, drop
}
