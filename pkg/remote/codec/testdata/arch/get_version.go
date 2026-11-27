package codec

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/remote"
)

// GetVersion declared before its first caller.
func (s *Store) GetVersion(ctx context.Context, name remote.Name, v remote.Version, want []remote.ChunkRange, m remote.Meter) iter.Seq2[remote.Chunk, error] {
	return nil
}
