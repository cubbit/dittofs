package core

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// Server is one adapter: its listeners, connections, drain and health, for
// every protocol.
type Server struct{}

var _ adapter.Adapter = (*Server)(nil)

// Listener is one protocol on one port, bound on each of the settings' Addrs.
type Listener struct {
	Network  string // "tcp" or "udp"
	Port     uint16
	Protocol Protocol
}

// NewServer returns the adapter New hands back: each listener bound on each
// of the settings' Addrs, all or none at Start; Reconfigure binds them on an
// added address the same way. s is the protocol's whole shape, which New was
// given; core reads its Common part.
func NewServer(d adapter.Deps, s config.Settings, ls ...Listener) (*Server, error) {
	return nil, fmt.Errorf("core: new server: %w", errors.ErrUnsupported)
}

// Start binds every listener, or none.
func (s *Server) Start(ctx context.Context) error {
	return fmt.Errorf("core: start: %w", errors.ErrUnsupported)
}

// Reconfigure applies next, the server's own protocol's shape, to connections
// set up after it returns; the other protocol's, or any other type, is
// refused.
func (s *Server) Reconfigure(ctx context.Context, next config.Settings) error {
	return fmt.Errorf("core: reconfigure: %w", errors.ErrUnsupported)
}

// Stop drains and stops; it is idempotent.
func (s *Server) Stop(ctx context.Context) error {
	return fmt.Errorf("core: stop: %w", errors.ErrUnsupported)
}

// Health is the server's state and readiness.
func (s *Server) Health() adapter.Health {
	panic("not implemented")
}

// Connections lists the connections open now.
func (s *Server) Connections() iter.Seq[adapter.ConnInfo] {
	panic("not implemented")
}
