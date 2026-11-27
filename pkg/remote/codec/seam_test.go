package codec

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/remote"
)

// syncerStore is the syncer's Store as RFC 3 §1.3 declares it, copied here
// because no syncer package declares it yet. The codec's Store is that Store as
// it is, with nothing adapting it (RFC 4 §4.3). When the syncer's package
// declares Store, the check becomes _ syncer.Store = (*Store)(nil) and this
// copy goes.
type syncerStore interface {
	Get(ctx context.Context, name remote.Name, want []remote.ChunkRange, m remote.Meter) iter.Seq2[remote.Chunk, error]
	Health(ctx context.Context, d remote.Direction) error
}

// The compiler holds the seam against the declarations copied here: a Store
// method or Attempt drifting from them fails the build of this package's
// tests. Nothing ties the copy to the syncer's own until the syncer declares it.
var (
	_ syncerStore = (*Store)(nil)
	// Attempt's method value is the attempt Flow's Upload runs (RFC 3 §1.3).
	_ func(ctx context.Context, m remote.Meter) error = (*Put)(nil).Attempt
	// New takes the remote tier's contract as it is.
	_ store = remote.Store(nil)
)
