package codec

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/remote"
)

// syncerStore is the syncer's Store as RFC 3 §1.3 declares it. The codec's
// Store is that Store as it is, with nothing adapting it (RFC 4 §4.3).
type syncerStore interface {
	Get(ctx context.Context, name remote.Name, want []remote.ChunkRange, m remote.Meter) iter.Seq2[remote.Chunk, error]
	Health(ctx context.Context, d remote.Direction) error
}

// The compiler holds the seam: a Store method or Attempt drifting from what
// the syncer calls fails the build of this package's tests.
var (
	_ syncerStore = (*Store)(nil)
	// Attempt's method value is the attempt Flow's Upload runs (RFC 3 §1.3).
	_ func(ctx context.Context, m remote.Meter) error = (*Put)(nil).Attempt
	// New takes the remote tier's contract as it is.
	_ store = remote.Store(nil)
)
