// Package blake3 is BLAKE3's keyed and key-derivation modes with 32 bytes of
// output, for the paths that hash whole chunks: it allocates nothing and
// starts no goroutine, at any input length.
//
// decision: the tree is assembled here, over lukechampine.com/blake3/guts's
// compression primitives, because the library's own Hasher.Write starts a
// goroutine per subtree once its input spans several BLAKE3 chunks and
// allocates in proportion to it, so a chunk hashed through it per call could
// keep neither count at zero. Measured against github.com/zeebo/blake3 v0.2.4
// in both modes, from 0 bytes to 16 MiB, on arm64 and on amd64 under
// emulation, which has neither AVX2 nor AVX-512 and so runs guts' generic
// path: neither started a goroutine over 100 calls per length, this tree made
// 0 allocations at every length, and zeebo's made 1 at every length, its
// Hasher escaping to the heap, so this tree won, and it adds no module. guts'
// AVX2 and AVX-512 paths take their arguments without escaping them; the
// package's tests count them wherever they run on such a host. Withdraw the
// package, and call the library directly, if a later lukechampine.com/blake3
// neither allocates nor starts a goroutine on a long write.
package blake3

import (
	"encoding/binary"
	"math/bits"

	"lukechampine.com/blake3/guts"
)

const (
	// group is how many bytes guts compresses at once: 16 BLAKE3 chunks, a
	// subtree of height groupHeight.
	group       = guts.MaxSIMD * guts.ChunkSize
	groupHeight = 4

	// Neither difference is negative only while a group is 2^groupHeight
	// chunks, so a guts with another MaxSIMD fails to compile here.
	_ = uint(group-guts.ChunkSize<<groupHeight) + uint(guts.ChunkSize<<groupHeight-group)

	// maxHeight is one chaining value per subtree height: BLAKE3 hashes at
	// most 2^64 bytes, 2^54 chunks.
	maxHeight = 54
)

// Hasher hashes in one mode. It holds no pointer to the heap, so a caller
// declares it on its stack and starts it in place, with no copy of its
// buffers; its zero value is not a mode, and only InitKeyed and InitDeriveKey
// start one.
type Hasher struct {
	key   [8]uint32
	flags uint32

	// stack holds the chaining value of each complete subtree written, at
	// its height; bit i of chunks says whether stack[i] is set.
	stack  [maxHeight][8]uint32
	chunks uint64

	// buf holds the last group written, which Sum256 compresses as the
	// tree's rightmost subtree, so it is never compressed before more input
	// follows it.
	buf    [group]byte
	buflen int
}

// InitKeyed starts h in BLAKE3's keyed mode under key, dropping what h held.
func (h *Hasher) InitKeyed(key *[32]byte) {
	var words [8]uint32
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(key[4*i:])
	}
	h.start(words, guts.FlagKeyedHash)
}

// InitDeriveKey starts h in BLAKE3's key-derivation mode under context,
// dropping what h held; what is written next is the key material.
func (h *Hasher) InitDeriveKey(context string) {
	h.start(guts.IV, guts.FlagDeriveKeyContext)
	// The context is copied into buf rather than converted with
	// []byte(context), which allocates above 32 bytes unless the compiler
	// proves the slice never escapes.
	for len(context) > 0 {
		h.flush()
		n := copy(h.buf[h.buflen:], context)
		h.buflen += n
		context = context[n:]
	}
	sum := h.Sum256()
	var words [8]uint32
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(sum[4*i:])
	}
	h.start(words, guts.FlagDeriveKeyMaterial)
}

// Write absorbs p. Whole groups of BLAKE3 chunks are compressed straight from
// p when nothing is buffered, so hashing one slice copies at most its last
// group.
func (h *Hasher) Write(p []byte) {
	for len(p) > 0 {
		h.flush()
		if h.buflen == 0 && len(p) > group {
			h.push((*[group]byte)(p))
			p = p[group:]
			continue
		}
		n := copy(h.buf[h.buflen:], p)
		h.buflen += n
		p = p[n:]
	}
}

// Sum256 returns the first 32 bytes of output over what was written.
func (h *Hasher) Sum256() [32]byte {
	n := guts.CompressBuffer(&h.buf, h.buflen, &h.key, h.chunks, h.flags)
	for i := bits.TrailingZeros64(h.chunks); i < bits.Len64(h.chunks); i++ {
		if h.chunks&(1<<i) != 0 {
			n = guts.ParentNode(h.stack[i], guts.ChainingValue(n), &h.key, h.flags)
		}
	}
	n.Flags |= guts.FlagRoot
	out := guts.WordsToBytes(guts.CompressNode(n))

	return [32]byte(out[:32])
}

func (h *Hasher) start(key [8]uint32, flags uint32) {
	h.key = key
	h.flags = flags
	h.chunks = 0
	h.buflen = 0
}

// flush compresses a full buffer, which input still to come proves is not
// the tree's last group.
func (h *Hasher) flush() {
	if h.buflen == group {
		h.push(&h.buf)
		h.buflen = 0
	}
}

// push adds a whole group to the tree, merging it with every complete subtree
// of its height to its left, as BLAKE3's tree is built left to right. Every
// group but the last is whole, so the chunk count stays a multiple of a group
// and each pushed group is a subtree of the tree.
func (h *Hasher) push(g *[group]byte) {
	cv := guts.ChainingValue(guts.CompressBuffer(g, group, &h.key, h.chunks, h.flags))
	i := groupHeight
	for ; h.chunks&(1<<i) != 0; i++ {
		cv = guts.ChainingValue(guts.ParentNode(h.stack[i], cv, &h.key, h.flags))
	}
	h.stack[i] = cv
	h.chunks += 1 << groupHeight
}
