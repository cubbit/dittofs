package carver

import (
	"errors"
	"time"

	"github.com/cubbit/dittofs/pkg/carver/internal/params"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Settings is one namespace's chunking settings. Only carve.NewSettings builds
// one; the zero value is refused. Target, Min and Max report its sizes: a
// caller sizes Cut's buffer from Max.
//
// decision: the seal is Go's rule on internal packages, not a check. Code
// outside pkg/carver can hold, copy and pass a value but cannot import params,
// so it can neither build one nor read its derived fields; code inside
// pkg/carver still can, and is trusted to build values only through the
// constructor. params stays a value package, importing the standard library
// alone, with BLAKE3 the one import it may gain, for a pre-keyed hash state;
// anything more makes the contract reach an implementation through this alias.
type Settings = params.Settings

// Chunk is one chunk Cut emits.
type Chunk struct {
	Offset int64            // file offset of its first byte
	Length int32            // at most Max
	Hash   entity.ChunkHash // its chunk ID (RFC 2 §4.1)
}

// ChunkInfo is one chunk as the fold takes it, in file order, file after file.
type ChunkInfo struct {
	File entity.FileID
	Chunk
	Zero   bool      // all zeros: a zero ref, never carried; set by the caller, which holds the bytes
	Keys   int       // worst-case keys it adds to its block's commit, the refs it replaces counted
	Oldest time.Time // when its oldest byte was written
}

// AssemblerConfig is one pass's limits.
type AssemblerConfig struct {
	BlockTarget int64         // carried bytes per block, MinBlockTarget to MaxBlockTarget
	MaxChunks   int           // the block format's count cap N
	KeyBudget   int           // K, the most keys one commit may write
	BlockKeys   int           // keys every block's commit writes once
	FileKeys    int           // keys a commit writes once per file in it
	MaxAge      time.Duration // the offload maximum age; a short last block older than it is put
}

// BlockPlan is one block, or, with no carried chunk, the commit of zero refs
// that ride in no block. It holds hashes and positions, never chunk bytes.
type BlockPlan struct {
	Carried []ChunkInfo // each carried chunk once, in block order: the put reads these bytes
	Refs    []ChunkInfo // every ref its commit writes, in input order, repeats and zero refs included
}

// Disposition is what the fold did with one chunk.
type Disposition uint8

const (
	Carried Disposition = iota + 1 // in a block plan
	ZeroRef                        // all zeros: a ref with a zero hash, no block carries it
)

// The block target's range (RFC 2 §5.2): a target outside it is refused, never
// clamped.
const (
	MinBlockTarget = 1 << 20  // 1 MiB
	MaxBlockTarget = 64 << 20 // 64 MiB
)

// ErrInvalidSettings refuses a setting RFC 2 §3.7 or §5.2 rules out; it is
// never replaced by a default.
var ErrInvalidSettings = errors.New("carver: invalid settings")
