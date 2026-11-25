// Package transform is the contract of the per-chunk transforms (RFC 5 §3.1):
// the value types, the Transform, Chain and Attempt interfaces and the decode
// errors. It keeps no state and implements nothing. The registry, and the
// chain that implements Chain and Attempt, are package chain beneath it; each
// shipped transform is a package of its own that registers there. The codec
// and the engine import this package and package keys only.
package transform

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// ID names a transform in every body it was applied to (RFC 5 §2.4). It is
// assigned once and never reused.
type ID uint16

// Stage is a transform's fixed place in every chain (RFC 5 §2.3). A chain
// holds at most one transform per stage, encodes in stage order and decodes in
// reverse.
type Stage uint8

const (
	// StageCompress runs first, where the plaintext's redundancy still is.
	StageCompress Stage = iota + 1
	// StageEncrypt covers everything before it.
	StageEncrypt
	// StageRedundancy protects the stored bytes, so it sees them last.
	StageRedundancy
)

// Transform is one invertible, per-chunk step (RFC 5 §2.1, §3.1). It sees one
// chunk's bytes and hash at a time, and never changes the chunk's hash or the
// block's name (RFC 5 §7 T1).
type Transform interface {
	ID() ID

	// Version is the format version Encode writes, which the chain ID covers
	// (RFC 5 §2.8): a pin reads it before anything is encoded.
	Version() uint8

	Stage() Stage

	// Kinds are the material kinds Encode uses (RFC 5 §2.5), which a pin reads
	// as current for the namespace; none for a transform that uses no
	// material.
	Kinds() []string

	// MaxEncodedLen is the largest body Encode can produce from n bytes under
	// material m, the pinned material of this transform's kinds. A nil m asks
	// for the bound under any material, which Chain.MaxDecodeLen uses.
	MaxEncodedLen(n int, m []keys.MaterialID) int

	// LengthOf is the exact body length for n bytes under material m, and
	// whether it is known without encoding. A declined chunk is never known in
	// advance.
	LengthOf(n int, m []keys.MaterialID) (int, bool)

	// LengthKey encodes, canonically, the settings that decide the body's
	// length: this transform's part of the chain ID. The chain ID adds the
	// transform ID, format version and material itself (RFC 5 §2.8).
	LengthKey() []byte

	// Encode transforms plain, deterministically (RFC 5 §2.1), for namespace
	// ns, under material m, the attempt's pinned material: it gets that
	// material's content from keys.Provider.Get and never asks for the current
	// material. Applied=false declines the chunk: plain passes on unchanged.
	// hash is the chunk ID, for transforms that bind to it or derive from it.
	Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (Encoded, error)

	// Decode inverts Encode. It never produces more than max bytes, and asks m
	// only for material the block's census lists (RFC 5 §2.5). Its errors are
	// ErrMalformed, ErrTooLarge and keys' three material errors (RFC 5 §2.7).
	Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) (plain []byte, err error)
}

// Encoded is one transform's output, and what the census records about it
// (RFC 5 §3.1, §5.3).
type Encoded struct {
	ID       ID // the transform that wrote it, filled in by the chain
	Body     []byte
	Applied  bool
	Version  uint8             // the transform's own format version
	Material []keys.MaterialID // the material used, if any
}

// Factory builds a transform from its settings and the store's material
// (RFC 5 §3.1). It fails rather than returning a transform that cannot work
// (RFC 5 §2.7).
type Factory func(ctx context.Context, settings map[string]any, m keys.Provider) (Transform, error)

// Config is one configured transform of a store configuration's chain
// (RFC 5 §3.2).
type Config struct {
	Name     string         // a registered transform's name
	Settings map[string]any // passed to its Factory
}

// Chain is a store's configured list of transforms (RFC 5 §3.1).
type Chain interface {
	// Pin fixes the chain and namespace ns's current material for one block,
	// from the mint of its name to its commit or abandonment: the block's chain
	// ID, bounds and bodies all come from the pin, however the configuration or
	// the current material changes meanwhile (RFC 5 §2.8). It reads
	// keys.Provider.Current once for each kind a transform declares, and fails
	// with the provider's error, unchanged, when the current material cannot
	// be read, or when a key service holds the master key that wraps it in any
	// state but active (RFC 5 Appendix B.1).
	Pin(ctx context.Context, ns entity.NamespaceID) (Attempt, error)

	// Decode undoes the transforms a body's envelope lists, looked up in the
	// registry rather than the configured list, so it decodes what an older
	// configuration wrote (RFC 5 §2.5). census is the block record's
	// (RFC 5 §5.3): a body naming material outside it is ErrMalformed. dst
	// must not overlap body.
	Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, census []Encoded) ([]byte, error)

	// MaxDecodeLen is the largest body any registered transforms could have
	// written from n bytes under any material, one per stage, plus the
	// envelope, answered from the chain's registry. It sizes reads, and the put
	// side's buffers (RFC 5 §3.1).
	MaxDecodeLen(n int) int
}

// Attempt is one pinned chain (RFC 5 §3.1). The engine releases it when the
// block commits or is abandoned.
type Attempt interface {
	// ID is the chain ID (RFC 5 §2.8), from each transform's ID, Version,
	// LengthKey and the pinned material.
	ID() [32]byte

	// MaxEncodedLen composes the transforms' bounds under the pinned material,
	// in stage order, plus the envelope.
	MaxEncodedLen(n int) int

	// LengthOf composes the transforms' LengthOf under the pinned material;
	// false if any one is false. It is exact for the bodies this attempt
	// encodes.
	LengthOf(n int) (int, bool)

	// Encode writes the envelope and returns the body with one Encoded per
	// applied transform, which the codec collects into the block's census
	// (RFC 5 §5.3).
	Encode(ctx context.Context, dst []byte, hash [32]byte, plain []byte) ([]byte, []Encoded, error)

	// Release ends the pin. It returns nothing: the engine releases at commit
	// or abandonment and has nothing to do with a failure.
	Release()
}

// The decode errors of the transforms themselves (RFC 5 §2.7). The material
// errors are package keys'.
var (
	// ErrMalformed is a body this transform did not write, one that fails its
	// own integrity check, or one naming material its block's census does not
	// list.
	ErrMalformed = errors.New("transform: malformed body")
	// ErrTooLarge is an output that would exceed its bound.
	ErrTooLarge = errors.New("transform: output exceeds its bound")
)
