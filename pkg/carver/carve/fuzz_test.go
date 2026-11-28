package carve

import (
	"errors"
	"math/bits"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// FuzzNewSettings fuzzes the constructor's own arguments, each key as a byte
// slice copied into a [32]byte, so an empty slice is a zero key. NewSettings
// must refuse exactly what RFC 2 §3.7 refuses, restated here apart from
// CheckTarget, with ErrInvalidSettings and the zero value, and an accepted
// value must hold the sizes and bit counts its Target derives.
func FuzzNewSettings(f *testing.F) {
	id, cut := idKey[:], cutKey[:]

	// Every supported Target under each key case of RFC 2 §3.7.
	for _, target := range targets {
		f.Add(target, false, []byte{}, id)        // no encryption, a zero chunking key: accepted
		f.Add(target, true, cut, id)              // encryption with a chunking key: accepted
		f.Add(target, false, []byte{}, []byte{})  // a zero chunk-ID key
		f.Add(target, true, cut, []byte{})        // a zero chunk-ID key, encrypting
		f.Add(target, false, cut, id)             // a chunking key without encryption
		f.Add(target, true, []byte{}, id)         // encryption with a zero chunking key
		f.Add(target, true, make([]byte, 32), id) // encryption with an all-zero 32-byte chunking key
	}

	// Targets at, under and over each limit, and not powers of two.
	for _, target := range []int{48 << 10, 96 << 10, 32 << 10, 8 << 20, 0, -1, 64<<10 + 1, 3 << 20, 1 << 30} {
		f.Add(target, false, []byte{}, id)
	}

	// A key whose only set byte is its last is not zero.
	last := make([]byte, 32)
	last[31] = 1
	f.Add(256<<10, true, last, last)

	f.Fuzz(func(t *testing.T, target int, encrypting bool, chunking, chunkID []byte) {
		var chunkingKey, chunkIDKey [32]byte
		copy(chunkingKey[:], chunking)
		copy(chunkIDKey[:], chunkID)

		s, err := NewSettings(target, encrypting, chunkingKey, chunkIDKey)

		if why := refusal(target, encrypting, chunkingKey, chunkIDKey); why != "" {
			if !errors.Is(err, carver.ErrInvalidSettings) || s != (carver.Settings{}) {
				t.Fatalf("NewSettings(%d, %t, %x, %x), %s: got %+v, %v, want the zero value and ErrInvalidSettings",
					target, encrypting, chunkingKey, chunkIDKey, why, s, err)
			}
			return
		}

		if err != nil {
			t.Fatalf("NewSettings(%d, %t, %x, %x): got %v, want nil", target, encrypting, chunkingKey, chunkIDKey, err)
		}

		if s.Target() != target || s.Min() != target>>2 || s.Max() != target<<2 {
			t.Fatalf("Target %d: got Target %d, Min %d, Max %d, want %d, %d, %d",
				target, s.Target(), s.Min(), s.Max(), target, target>>2, target<<2)
		}

		if s.Min() < 16<<10 || s.Max() > 16<<20 {
			t.Fatalf("Target %d: got Min %d, Max %d, want Min at least 16 KiB and Max at most 16 MiB", target, s.Min(), s.Max())
		}

		tt := bits.TrailingZeros(uint(target))
		if before, after := params.Bits(s); before != tt+1 || after != tt-3 {
			t.Fatalf("Target %d = 2^%d: got Bits %d, %d, want %d, %d", target, tt, before, after, tt+1, tt-3)
		}

		if params.Gear(s) == nil {
			t.Fatalf("Target %d: got a nil table", target)
		}
	})
}

// refusal restates RFC 2 §3.7's refusals, written apart from CheckTarget and
// NewSettings: it names the rule a setting breaks, or returns "".
func refusal(target int, encrypting bool, chunkingKey, chunkIDKey [32]byte) string {
	zero := func(k [32]byte) bool {
		for _, b := range k {
			if b != 0 {
				return false
			}
		}
		return true
	}

	switch {
	case target <= 0 || bits.OnesCount(uint(target)) != 1:
		return "a Target that is not a power of two"
	case target/4 < 16<<10:
		return "a Min below 16 KiB"
	case target > (16<<20)/4: // target*4 would overflow for a large power of two
		return "a Max above 16 MiB"
	case zero(chunkIDKey):
		return "an all-zero chunk-ID key"
	case !encrypting && !zero(chunkingKey):
		return "a chunking key for a namespace that does not encrypt"
	case encrypting && zero(chunkingKey):
		return "an encrypting namespace with no chunking key"
	}

	return ""
}
