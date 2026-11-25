package remote

import (
	"context"
	"io"
)

// Store is one remote block store (RFC 4 §4.1). A store performs one operation
// on one block and remembers nothing between calls (RFC 4 §2, §4.9). Every
// failure wraps exactly one error of this package (RFC 4 §4.8). Each consumer
// declares the methods it needs on its own side; a backend implements all of
// them.
//
// The calls a backup location makes, and the settings re-check with its
// version walk, are not declared: each lands with its first caller, its
// signature as RFC 4 §4.1 writes it.
type Store interface {
	// Checksum says which checksum Put needs before the first byte, if any
	// (RFC 4 §4.3, §4.11).
	Checksum() Checksum

	// Put streams one whole encoded block of exactly size bytes, with sum when
	// Checksum is not None. It returns nil only once the block is stored
	// (RFC 4 §4.3).
	Put(ctx context.Context, name Name, body io.Reader, size int64, sum []byte) error

	// Get returns exactly the bytes of r, or of the whole block when r is the
	// zero Range. The reader yields exactly that many bytes or fails. stored is
	// the service's whole-object checksum, reported with a whole-block get
	// (RFC 4 §4.4).
	Get(ctx context.Context, name Name, r Range) (body io.ReadCloser, stored StoredSum, err error)

	// Delete removes blocks, as many as the caller passes. It returns one error
	// per name, in order; nil means that block is gone, as the reply stated for
	// that name. Deleting an absent block succeeds (RFC 4 §4.5).
	Delete(ctx context.Context, names []Name) []error

	// List reads one page, with one request, of at most n stored blocks in
	// ascending name order, starting after the given name (the zero Name starts
	// at the beginning); next is the last name of the page or, when the page
	// ends on a key that is not a block, the smallest block name after that
	// key, and done is true once the walk has passed the last block (RFC 4 §4.6).
	List(ctx context.Context, after Name, n int) (page []Info, next Name, done bool, err error)

	// Health makes one bounded probe round trip of direction d against the
	// store's own namespace: a probe put for DirPut, a probe get for DirGet
	// (RFC 4 §4.7).
	Health(ctx context.Context, d Direction) error

	// PutControl and GetControl store and read one small control object by
	// role. They are never listed as blocks. GetControl also returns the
	// version the service holds as current, empty on a mutable store, so an
	// open can adopt it (RFC 4 §4.7, §4.13).
	PutControl(ctx context.Context, role Role, body []byte) error
	GetControl(ctx context.Context, role Role) (body []byte, v Version, err error)

	// Close releases the client. Stores are shared across shares; only the
	// holder that opened one closes it (RFC 4 §4.10).
	Close() error
}
