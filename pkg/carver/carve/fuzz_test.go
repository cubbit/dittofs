package carve

import (
	"errors"
	"math/bits"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/internal/params"
)

// FuzzNewSettings fuzzes the constructor's own arguments, each key as a byte
// slice copied into a [32]byte, so an empty slice is a zero key. CheckTarget
// and NewSettings must refuse exactly what RFC 2 §3.7 refuses, restated here
// apart from both, with ErrInvalidSettings and the zero value, and an accepted
// value must hold the sizes its Target derives, inside §3.7's bounds.
func FuzzNewSettings(f *testing.F) {
	id, cut := idKey[:], cutKey[:]

	// Every supported Target under each key case of RFC 2 §3.7.
	for _, target := range targets {
		f.Add(target, false, []byte{}, id)       // no encryption, a zero chunking key: accepted
		f.Add(target, true, cut, id)             // encryption with a chunking key: accepted
		f.Add(target, false, []byte{}, []byte{}) // a zero chunk-ID key: refused
		f.Add(target, true, cut, []byte{})       // a zero chunk-ID key, encrypting: refused
		f.Add(target, false, cut, id)            // a chunking key without encryption: refused
		f.Add(target, true, []byte{}, id)        // encryption with a zero chunking key: refused
	}

	// Targets RFC 2 §3.7 refuses, each with valid keys.
	f.Add(32<<10, false, []byte{}, id)           // the power of two below 64 KiB
	f.Add(8<<20, false, []byte{}, id)            // the power of two above 4 MiB
	f.Add(48<<10, false, []byte{}, id)           // not a power of two, below 64 KiB
	f.Add(64<<10+1, false, []byte{}, id)         // not a power of two, just above 64 KiB
	f.Add(96<<10, false, []byte{}, id)           // not a power of two, in range
	f.Add(3<<20, false, []byte{}, id)            // not a power of two, in range
	f.Add(1<<30, false, []byte{}, id)            // a power of two far above 4 MiB
	f.Add(0, false, []byte{}, id)                // zero
	f.Add(-1, false, []byte{}, id)               // negative
	f.Add(256<<10, true, lastByte(), lastByte()) // keys whose only set byte is the last: accepted

	f.Fuzz(func(t *testing.T, target int, encrypting bool, chunking, chunkID []byte) {
		var chunkingKey, chunkIDKey [32]byte
		copy(chunkingKey[:], chunking)
		copy(chunkIDKey[:], chunkID)

		// Checked first, so a Target wrongly accepted fails here rather than
		// reaching a derivation that never ends on one at 1<<62 or above.
		why := targetRefusal(target)
		if err := CheckTarget(target); (why != "") != errors.Is(err, carver.ErrInvalidSettings) || (why == "") != (err == nil) {
			t.Fatalf("CheckTarget(%d): got %v, want a refusal %t (%s)", target, err, why != "", why)
		}

		if why == "" {
			why = keyRefusal(encrypting, chunkingKey, chunkIDKey)
		}

		s, err := NewSettings(target, encrypting, chunkingKey, chunkIDKey)
		if why != "" {
			if !errors.Is(err, carver.ErrInvalidSettings) || s != (carver.Settings{}) {
				t.Fatalf("NewSettings(%d, %t, %x, %x), %s: got %+v, %v, want the zero value and ErrInvalidSettings",
					target, encrypting, chunkingKey, chunkIDKey, why, s, err)
			}
			return
		}

		if err != nil {
			t.Fatalf("NewSettings(%d, %t, %x, %x): got %v, want nil", target, encrypting, chunkingKey, chunkIDKey, err)
		}

		if s.Target() != target || s.Min() != target/4 || s.Max() != 4*target {
			t.Fatalf("Target %d: got Target %d, Min %d, Max %d, want %d, %d, %d",
				target, s.Target(), s.Min(), s.Max(), target, target/4, 4*target)
		}

		if s.Min() < 16<<10 || s.Max() > 16<<20 {
			t.Fatalf("Target %d: got Min %d, Max %d, want Min at least 16 KiB and Max at most 16 MiB", target, s.Min(), s.Max())
		}

		if params.Gear(s) == nil {
			t.Fatalf("Target %d: got a nil table", target)
		}
	})
}

// targetRefusal restates RFC 2 §3.7's Target refusals through Min and Max, as
// the RFC words them, apart from CheckTarget: it names the rule target breaks,
// or returns "".
func targetRefusal(target int) string {
	switch {
	case target <= 0 || bits.OnesCount(uint(target)) != 1:
		return "a Target that is not a power of two"
	case target/4 < 16<<10:
		return "a Min below 16 KiB"
	case target > (16<<20)/4: // 4*target would overflow for a large power of two
		return "a Max above 16 MiB"
	}

	return ""
}

// keyRefusal restates RFC 2 §3.7's key refusals apart from NewSettings.
func keyRefusal(encrypting bool, chunkingKey, chunkIDKey [32]byte) string {
	switch {
	case chunkIDKey == [32]byte{}:
		return "an all-zero chunk-ID key"
	case !encrypting && chunkingKey != [32]byte{}:
		return "a chunking key for a namespace that does not encrypt"
	case encrypting && chunkingKey == [32]byte{}:
		return "an encrypting namespace with no chunking key"
	}

	return ""
}

// lastByte is a 32-byte key whose only set byte is its last.
func lastByte() []byte {
	k := make([]byte, 32)
	k[31] = 1
	return k
}
