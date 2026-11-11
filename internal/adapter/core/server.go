package core

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/netip"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// Server is one adapter: its listeners, connections, drain and health, for
// every protocol.
type Server struct{}

var _ adapter.Adapter = (*Server)(nil)

// Listener is one protocol on one port, bound on each of the settings' Addrs.
// Addrs reports it under one Socket per service it names, on its network,
// each address at the port bound there.
//
// decision: a listener names its services rather than holding a Socket,
// because one port carries several: NFS's carries NFS, MOUNT, NLM and NSM,
// told apart by RPC program. So Network repeats Socket's field, and a field
// added to Socket must be added here too, or Addrs builds incomplete keys.
type Listener struct {
	Services []string // the names Addrs and a sidecar know it by: "nfs", "mount", "nlm", "nsm", "smb"
	Network  string   // "tcp" or "udp"
	Port     uint16   // 0: one the system picks, which Addrs reports
	Protocol Protocol
}

// NewServer returns the adapter New hands back: each listener bound on each
// of the settings' Addrs, all or none at Start; Reconfigure binds them on an
// added address the same way. s is the protocol's whole shape, which New was
// given; core reads its Adapter. It refuses a listener naming no service, an
// empty one or one twice, and two listeners naming the same Socket or the
// same network and nonzero port.
func NewServer(d adapter.Deps, s settings.Settings, ls ...Listener) (*Server, error) {
	return nil, fmt.Errorf("core: new server: %w", errors.ErrUnsupported)
}

// Start binds every listener, or none.
func (s *Server) Start(ctx context.Context) error {
	return fmt.Errorf("core: start: %w", errors.ErrUnsupported)
}

// Reconfigure applies next, the server's own protocol's shape, to connections
// set up after it returns; the other protocol's, or any other type, is
// refused.
func (s *Server) Reconfigure(ctx context.Context, next settings.Settings) error {
	return fmt.Errorf("core: reconfigure: %w", errors.ErrUnsupported)
}

// Stop drains and stops; it is idempotent.
func (s *Server) Stop(ctx context.Context) error {
	return fmt.Errorf("core: stop: %w", errors.ErrUnsupported)
}

// Addrs is every address each listener is bound on now, by service and
// network, in a fresh map the caller owns, slices included: a port of 0
// resolved, an address Reconfigure added included and one it removed
// gone. It is empty before Start and after Stop. The node builds each
// Serving from it.
func (s *Server) Addrs() map[adapter.Socket][]netip.AddrPort {
	panic("not implemented")
}

// Health is the server's state and readiness.
func (s *Server) Health() adapter.Health {
	panic("not implemented")
}

// Connections lists the connections open now.
func (s *Server) Connections() iter.Seq[adapter.ConnInfo] {
	panic("not implemented")
}
