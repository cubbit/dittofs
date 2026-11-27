package carve

import (
	"github.com/cubbit/dittofs/internal/blake3"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// nameContext separates a block's name from a chunk's ID, which is a keyed
// hash, by BLAKE3's key-derivation mode (RFC 2 §4.2).
const nameContext = "dittofs content-defined block name v2"

// BlockName is the name of a block holding ids, in block order, under scope,
// for the put attempt that drew nonce, with the chain ID chain (RFC 2 §4.2).
// It draws no nonce itself.
func BlockName(scope entity.NamespaceID, nonce [16]byte, chain [32]byte, ids []entity.ChunkHash) entity.BlockName {
	// The prefix is len(scope) ‖ scope ‖ nonce ‖ len(chain) ‖ chain: the
	// scope is the namespace ID's raw bytes, never its text form.
	var prefix [1 + len(scope) + len(nonce) + 1 + len(chain)]byte
	prefix[0] = byte(len(scope))
	n := 1 + copy(prefix[1:], scope[:])
	n += copy(prefix[n:], nonce[:])
	prefix[n] = byte(len(chain))
	copy(prefix[n+1:], chain[:])

	var h blake3.Hasher
	h.InitDeriveKey(nameContext)
	h.Write(prefix[:])
	for i := range ids {
		h.Write(ids[i][:])
	}

	return h.Sum256()
}
