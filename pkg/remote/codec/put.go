package codec

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/transform"
)

// NewPut builds the put of block name, minted from nonce and the chain ID of
// pin, the chain the engine pinned for this attempt (RFC 8 §6.6, RFC 5 §3.1);
// the header carries both, and every body is encoded under pin. Each range
// over src yields the block's chunks again from the first; a measuring pass
// and each Attempt start one. Attempt recomputes the name from nonce, pin's
// chain ID, the chunk IDs and the key scope (RFC 4 §3.4) before it sends a
// byte, and a name that differs is remote.ErrInvalid (RFC 4 §4.3).
func (s *Store) NewPut(name remote.Name, nonce [16]byte, pin transform.Attempt, src iter.Seq2[remote.Chunk, error]) *Put {
	return &Put{}
}

// Put is one block's put across the syncer's tries (RFC 4 §4.3). It keeps the
// measured lengths and checksum across its Attempts, so a retry only sends,
// and drops them after an Attempt that ends in remote.ErrCorrupt, so the next
// one measures again.
type Put struct{}

// Attempt is one try: it measures when it must, then sends the header and each
// chunk encoded as it goes, in one remote Put. It calls m.Moved with each count
// of the block's bytes sent, header and index included, and m.Sent after the
// last, and returns nil only once the block is stored. Its method value is the
// attempt the syncer's Upload runs (RFC 4 §4.3, RFC 3 §1.3).
//
// Two Attempts of one Put may overlap, when the syncer retries a slow try
// before the first has returned, and are safe when they do. An Attempt whose
// ctx has ended drops its chunk buffers before it returns.
func (p *Put) Attempt(ctx context.Context, m remote.Meter) error {
	return unimplemented("Put.Attempt")
}

// Stored returns each chunk's range in the encoded block and the census the
// header carries, once an Attempt has returned nil (RFC 4 §4.3). The census
// entries carry a nil Body: the census records what was applied, not the
// bytes.
func (p *Put) Stored() (ranges []remote.ChunkRange, census []transform.Encoded) {
	return nil, nil
}
