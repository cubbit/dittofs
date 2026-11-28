package chain

import (
	"cmp"
	"encoding/binary"
	"slices"
	"strings"

	"lukechampine.com/blake3"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// chainIDContext is the chain ID's BLAKE3 key-derivation context, versioned
// so that a change to the encoding is a new context (RFC 5 §2.8).
const chainIDContext = "dittofs transform chain id v1"

// chainID is RFC 5 §2.8's chain ID of steps, in stage order, under material,
// each step's pinned material in the order of its Kinds: BLAKE3-derive-key
// over the transform count, then per transform its ID, format version, length
// key and material, ordered by kind then ID, each with its fingerprint.
// Lengths are 4 bytes and integers big-endian. NewChain refuses a step with
// more kinds than the one-byte material count holds.
func chainID(steps []transform.Transform, material [][]keys.MaterialID) [32]byte {
	b := []byte{byte(len(steps))}
	for i, t := range steps {
		b = binary.BigEndian.AppendUint16(b, uint16(t.ID()))
		b = append(b, t.Version())
		b = appendPrefixed(b, t.LengthKey())
		ms := slices.SortedFunc(slices.Values(material[i]), func(x, y keys.MaterialID) int {
			return cmp.Or(strings.Compare(x.Kind, y.Kind), strings.Compare(x.ID, y.ID))
		})
		b = append(b, byte(len(ms)))
		for _, m := range ms {
			b = appendPrefixed(b, []byte(m.Kind))
			b = appendPrefixed(b, []byte(m.ID))
			b = append(b, m.Fingerprint[:]...)
		}
	}
	var id [32]byte
	blake3.DeriveKey(id[:], chainIDContext, b)
	return id
}

// appendPrefixed appends field's length as 4 bytes big-endian, then field.
func appendPrefixed(b, field []byte) []byte {
	return append(binary.BigEndian.AppendUint32(b, uint32(len(field))), field...)
}
