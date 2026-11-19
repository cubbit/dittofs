package identity

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Applied is the State calls now read; nil before the first Apply and after
// Stop.
func (s *Server) Applied() *State { return s.state.Load() }

// ErrNotApplied is Start's refusal before the first Apply.
var ErrNotApplied = errNotApplied

// OnStateLoaded has every Resolve pass call f just after it loads the
// State, before its first store read, so a test can Apply in between.
func (s *Server) OnStateLoaded(f func()) { s.loaded = f }

// Mint runs the mint path over plan.
func (s *Server) Mint(ctx context.Context, plan func(context.Context) (entity.MintBatch, int, error)) error {
	return s.mint(ctx, plan)
}

// DefaultSettings is the identity section's value where no row sets a key.
var DefaultSettings = defaultSettings

// KeyOf is the key the applied State's ring holds for a service name, realm,
// key version number and encryption type.
func (s *Server) KeyOf(name, realm string, kvno uint32, etype int32) ([]byte, bool) {
	k, ok := s.state.Load().keys.key(name, realm, kvno, etype)
	return k.KeyValue, ok
}

// Len is the number of entries the replay cache holds.
func (c *replayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
