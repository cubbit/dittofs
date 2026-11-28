package carve

import (
	"encoding/binary"
	"fmt"

	"lukechampine.com/blake3/guts"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// The Target range (RFC 2 §3.7): a 16 KiB Min at the floor, a 16 MiB Max at
// the ceiling.
const (
	minTarget = 64 << 10
	maxTarget = 4 << 20
)

// The derivation labels of RFC 2 §3.2.
const (
	gearContext = "dittofs gear table v1"   // key-derivation context of the unkeyed table
	gearLabel   = "dittofs chunking key v1" // keyed-mode input of the keyed table
)

// NewSettings builds a namespace's settings from its Target, whether it
// encrypts and its two keys. A namespace that does not encrypt passes a zero
// chunking key. It refuses, with carver.ErrInvalidSettings and the zero value,
// a Target CheckTarget refuses, a zero chunk-ID key, a chunking key without
// encryption, and encryption with a zero chunking key (RFC 2 §3.7).
//
// A namespace that does not encrypt cuts under the unkeyed table, so its
// boundaries are public: anyone who holds a file can compute where it cuts.
// An encrypting one cuts under a table its chunking key derives, which hides
// which file a run of chunk lengths belongs to only from a bucket reader who
// can cause no write into the namespace and knows none of its stored content;
// it hides the lengths from no one (RFC 2 §6).
func NewSettings(target int, encrypting bool, chunkingKey, chunkIDKey [32]byte) (carver.Settings, error) {
	if err := CheckTarget(target); err != nil {
		return carver.Settings{}, err
	}

	var zero [32]byte
	switch {
	case chunkIDKey == zero:
		return carver.Settings{}, fmt.Errorf("%w: an all-zero chunk-ID key", carver.ErrInvalidSettings)
	case !encrypting && chunkingKey != zero:
		return carver.Settings{}, fmt.Errorf("%w: a chunking key for a namespace that does not encrypt", carver.ErrInvalidSettings)
	case encrypting && chunkingKey == zero:
		return carver.Settings{}, fmt.Errorf("%w: an encrypting namespace with no chunking key", carver.ErrInvalidSettings)
	}

	g := unkeyedGear()
	if encrypting {
		g = keyedGear(chunkingKey)
	}

	return params.WithChunkIDKey(params.New(target, g), chunkIDKey), nil
}

// CheckTarget refuses, with carver.ErrInvalidSettings, a Target that is not a
// power of two, below 64 KiB or above 4 MiB. It needs no key, so a settings
// validator can call it.
func CheckTarget(target int) error {
	switch {
	case target <= 0 || target&(target-1) != 0:
		return fmt.Errorf("%w: Target %d is not a power of two", carver.ErrInvalidSettings, target)
	case target < minTarget:
		return fmt.Errorf("%w: Target %d is below 64 KiB", carver.ErrInvalidSettings, target)
	case target > maxTarget:
		return fmt.Errorf("%w: Target %d is above 4 MiB", carver.ErrInvalidSettings, target)
	}

	return nil
}

// unkeyedGear is the first 2,048 bytes of BLAKE3's extendable output in
// key-derivation mode, context gearContext, over the empty input.
func unkeyedGear() *[256]uint64 {
	ctx := guts.CompressChunk([]byte(gearContext), &guts.IV, 0, guts.FlagDeriveKeyContext)
	ctx.Flags |= guts.FlagRoot
	out := guts.CompressNode(ctx)

	iv := [8]uint32(out[:8])

	return gear(guts.CompressChunk(nil, &iv, 0, guts.FlagDeriveKeyMaterial))
}

// keyedGear is the first 2,048 bytes of BLAKE3's extendable output in keyed
// mode under key, over gearLabel.
//
// ponytail: a keyed gear table keeps the boundary search at K1's rate but
// falls to known- and chosen-content attacks, so in practice it hides little
// from a bucket reader of a namespace holding any common file. Upgrade to a
// keyed PRF evaluated at each candidate position that passes the gear test, so
// the boundary decision no longer exposes a table that known content can map
// out, if a deployment must hide which files it holds from a bucket reader; it
// costs a PRF call per candidate and re-cuts every namespace that adopts it.
func keyedGear(key [32]byte) *[256]uint64 {
	var k [8]uint32
	for i := range k {
		k[i] = binary.LittleEndian.Uint32(key[4*i:])
	}

	return gear(guts.CompressChunk([]byte(gearLabel), &k, 0, guts.FlagKeyedHash))
}

// gear reads the first 2,048 bytes of the output of the one-chunk input n
// ends, as 256 little-endian words.
//
// decision: both tables are compressed through guts.CompressBlocks, 1,024
// bytes at a time, never read through lukechampine.com/blake3's OutputReader
// or DeriveKey: the OutputReader starts goroutines for one read of two or more
// of its 1,024-byte buffers, and DeriveKey reads its whole output in one call.
// Nothing is cached either: each NewSettings derives its table, so carve holds
// no state. Withdraw the first half if the library stops starting goroutines.
func gear(n guts.Node) *[256]uint64 {
	n.Flags |= guts.FlagRoot

	// Two calls fill the table only while one fills 1,024 bytes; this fails to
	// compile if a library version changes that.
	const _ = uint(guts.MaxSIMD*guts.BlockSize-1024) + uint(1024-guts.MaxSIMD*guts.BlockSize)

	var buf [2048]byte
	for i := range 2 {
		n.Counter = uint64(i * guts.MaxSIMD)
		guts.CompressBlocks((*[guts.MaxSIMD * guts.BlockSize]byte)(buf[i*guts.MaxSIMD*guts.BlockSize:]), n)
	}

	g := new([256]uint64)
	for i := range g {
		g[i] = binary.LittleEndian.Uint64(buf[8*i:])
	}

	return g
}
