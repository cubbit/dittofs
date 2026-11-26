// Package codec is the block codec over one remote store (RFC 4 §3, §4.3):
// it encodes a block's chunks through the store's transform chain, puts the
// block whole, and reads each chunk back decoded and verified.
//
// The codec imports the remote tier's contract and the root transform package,
// never a transform, the carver, the engine or a store: the node's content
// wiring hands it the chain, the name derivation, the chunk-ID function and a
// census lookup (RFC 4 §1.2, §4.3). It declares no error of its own and returns
// RFC 4 §4.8's set, and the transforms' material errors, as they are.
package codec

import (
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/remote"
	"github.com/cubbit/dittofs/pkg/transform"
)

// Config is what the node's content wiring hands one store's codec.
type Config struct {
	// Namespace is the namespace the store serves: Chain.Decode's ns
	// (RFC 5 §3.1).
	Namespace entity.NamespaceID
	// Chain is the store's configured chain, built by the content wiring.
	Chain transform.Chain
	// ChunkID is a chunk's ID from its plaintext, under the namespace's
	// chunk-ID key (RFC 2 §4.1). Each chunk read is checked against it (RFC 4
	// §3.4).
	ChunkID func(plain []byte) [32]byte
	// Name is a block's name from its nonce, its chain ID and its chunk IDs,
	// the key scope bound in (RFC 2 §4.2).
	Name func(nonce [16]byte, chain [32]byte, ids [][32]byte) remote.Name
	// Census returns block name's census from its block record (RFC 6 §2.3),
	// for Get to pass to Chain.Decode (RFC 4 §3.4, §4.3). An absent record is
	// remote.ErrNotFound; any other failure is returned as itself, a local
	// failure that counts against no store's health.
	//
	// ponytail: Get reads one block record per call through this lookup.
	// Upgrade to the census passed in with Get if a read-path profile shows
	// the lookup.
	//
	// decision: a reader with no block record, such as a read at a backup
	// location or one made before a recovery import publishes, has no census
	// source, and Get answers it remote.ErrNotFound (RFC 4 §4.3). Give the
	// codec a second census source, or pass the census in with Get, when the
	// first such reader is built.
	Census func(ctx context.Context, name remote.Name) ([]transform.Encoded, error)
	// MaxChunk is the carver's largest chunk (RFC 2 §3.2); a body is bounded
	// by Chain.MaxDecodeLen(MaxChunk).
	MaxChunk int
	// Version is the block format version to write (RFC 4 §3.5).
	Version uint8
}

// Recorder is what the codec records its series through (RFC 4 §4.12). The
// content wiring implements it over the root's registry, one per store, which
// supplies the store's label.
type Recorder interface {
	// Verified counts n chunks verified on a read: codec_chunks_verified_total.
	Verified(n int)
	// MeasuringPass counts one measuring pass of a put:
	// codec_measuring_passes_total.
	MeasuringPass()
	// VerifyFailed counts a read that failed verification, by kind: hash,
	// name, version or malformed. codec_verify_failures_total.
	VerifyFailed(kind string)
	// Took records the time to encode (remote.DirPut) or decode
	// (remote.DirGet) one block: codec_seconds.
	Took(d remote.Direction, took time.Duration)
}

// CPU is the background CPU token (RFC 24 §6.2). The codec takes one per
// chunk it encodes or hashes on the put path and returns it before its next
// network wait. A read takes none: demand reads and read-aheads are exempt.
type CPU interface {
	Acquire(ctx context.Context) (release func(), err error)
}

// Deps are what the root injects (RFC 25 §8.1).
type Deps struct {
	Logger   *slog.Logger
	Recorder Recorder
	CPU      CPU
}

// store is what the codec calls on a remote store; remote.Store satisfies it,
// so a call the contract gains later changes nothing handed to New.
type store interface {
	Checksum() remote.Checksum
	Put(ctx context.Context, name remote.Name, body io.Reader, size int64, sum []byte) error
	Get(ctx context.Context, name remote.Name, r remote.Range) (io.ReadCloser, remote.StoredSum, error)
	Health(ctx context.Context, d remote.Direction) error
}

// Store is the block codec over one remote store (RFC 4 §4.3). It is the
// syncer's Store as it is (RFC 3 §1.3).
type Store struct{}

// New builds the codec over st. It refuses a nil store, chain, function,
// Logger, Recorder or CPU, and a MaxChunk below 1, rather than default them
// (RFC 0 §1.2). An unknown Version is not refused: such a codec refuses to
// write and still reads (RFC 4 §3.5).
func New(st store, cfg Config, d Deps) (*Store, error) {
	return nil, unimplemented("New")
}

// Get reads the chunks named in want, or the whole block when want is empty.
// Ranges adjacent in the block are read with one request. Each chunk comes back
// decoded and verified against its own hash (RFC 4 §3.4). m receives the
// encoded bytes as they arrive (RFC 4 §4.3, §4.4).
func (s *Store) Get(ctx context.Context, name remote.Name, want []remote.ChunkRange, m remote.Meter) iter.Seq2[remote.Chunk, error] {
	return func(yield func(remote.Chunk, error) bool) {
		yield(remote.Chunk{}, unimplemented("Get"))
	}
}

// Health is the remote store's probe of direction d, passed straight through
// (RFC 4 §4.7).
func (s *Store) Health(ctx context.Context, d remote.Direction) error {
	return unimplemented("Health")
}

// MaxHeaderLen is the largest header format version v allows, and false for a
// version this codec does not know (RFC 4 §3.2, §3.5). The content wiring
// reads it before any Store exists, to bound the largest encoded block.
func MaxHeaderLen(v uint8) (n int, ok bool) {
	return 0, false
}

// unimplemented is what a body not yet written returns: an error of RFC 4
// §4.8's set, so a caller that reaches it sees a refusal rather than a panic.
func unimplemented(op string) error {
	return fmt.Errorf("codec: %s is not implemented: %w", op, remote.ErrInvalid)
}
