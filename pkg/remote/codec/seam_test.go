package codec

import "github.com/cubbit/dittofs/pkg/remote"

// New takes the remote tier's contract as it is. The seam on the syncer's side,
// Store and Put.Attempt against the syncer's own Store and Upload, is held by
// the syncer's seam test.
var _ store = remote.Store(nil)
