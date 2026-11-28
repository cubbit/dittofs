package s3

import (
	"context"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/remote"
)

// Health makes one probe of direction d against the store's own namespace, at
// its health object, <prefix>control/health (RFC 4 §4.7): DirPut puts the
// object's fixed body whole in one PutObject, verified in transit as a control
// object's put is; DirGet gets it whole in one GetObject and discards it, an
// absent object remote.ErrNotFound. Each returns its request's error as a
// control call's is mapped, keeps nothing, and is recorded once, as op health
// and never as a control call. Any other direction is remote.ErrInvalid with
// no request.
//
// decision: the probe's bound is the caller's ctx, as every other call's is;
// the client bounds only the dial and the TLS handshake, so a service that
// accepts and then stalls holds a probe until ctx ends. The syncer probes on
// its interval under a deadline (RFC 3 §2.8). Give the probe a fixed timeout
// of its own if a caller is ever seen probing without one.
func (s *store) Health(ctx context.Context, d remote.Direction) error {
	switch d {
	case remote.DirPut:
		start := time.Now()
		n, err := s.putControl(ctx, remote.RoleHealth, healthBody)
		s.record(opHealth, err, time.Since(start), n)

		return err
	case remote.DirGet:
		_, err := s.getControl(ctx, remote.RoleHealth, opHealth)

		return err
	}

	err := fmt.Errorf("remote/s3: %s of direction %d: %w", opHealth, d, remote.ErrInvalid)
	s.record(opHealth, err, 0, 0)

	return err
}
