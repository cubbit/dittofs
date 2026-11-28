package memory

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/remote"
)

// healthKey is the health object's key (RFC 4 Appendix C.2).
const healthKey = "control/health"

// Health makes one probe of direction d against the store's own namespace
// (RFC 4 §4.7). The put probe puts the health object again, keeping the body
// it holds, the empty one New puts or one a control put stored, as a health
// object's nonce is kept by every later put; the get probe reads it, and finds it absent only when
// something removed it beneath the store, remote.ErrNotFound. The store keeps
// no health status: each call returns its own probe's outcome. A direction
// outside the two is remote.ErrInvalid.
func (s *Store) Health(ctx context.Context, d remote.Direction) error {
	if err := s.open("Health"); err != nil {
		return err
	}

	if d != remote.DirPut && d != remote.DirGet {
		return fmt.Errorf("memory: health probe of direction %d: %w", d, remote.ErrInvalid)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.ns.mu.Lock()
	defer s.ns.mu.Unlock()

	if d == remote.DirPut {
		body := s.ns.objects[healthKey].body
		if body == nil {
			body = []byte{}
		}

		s.ns.objects[healthKey] = object{body: body, modTime: s.now()}

		return nil
	}

	if _, ok := s.ns.objects[healthKey]; !ok {
		return fmt.Errorf("memory: get probe: the health object is absent: %w", remote.ErrNotFound)
	}

	return nil
}
