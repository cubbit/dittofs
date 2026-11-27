package carvetest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// kinds is every Kind, in order.
var kinds = []Kind{Random, Zeros, Period64, Text44, Period4K, RandomRepeat}

// Targets is every power of two from 64 KiB to 4 MiB, so no profile is left
// out of the checks that range over it.
func TestTargets(t *testing.T) {
	want := 64 << 10
	for i, got := range Targets {
		if got != want {
			t.Fatalf("Targets[%d] = %d, want %d", i, got, want)
		}
		want *= 2
	}
	if last := Targets[len(Targets)-1]; last != 4<<20 {
		t.Errorf("the last of Targets is %d, want %d", last, 4<<20)
	}
}

// minimalPeriod is the smallest p with b[i] == b[i-p] for every i, or len(b).
func minimalPeriod(b []byte) int {
	for p := 1; p < len(b); p++ {
		if bytes.Equal(b[p:], b[:len(b)-p]) {
			return p
		}
	}
	return len(b)
}

// Each kind is its definition in RFC 2 §10.2: the patterned ones repeat with
// exactly their period, Zeros is zero, Random repeats nothing, and
// RandomRepeat is Random but for a middle third of Period64.
func TestKinds(t *testing.T) {
	const seed, n = 7, 5*4096 + 17

	random := Bytes(Random, seed, n)
	if p := minimalPeriod(random); p != n {
		t.Errorf("Random: repeats with period %d, want no period", p)
	}
	if bytes.Equal(random, Bytes(Random, seed+1, n)) {
		t.Errorf("Random: seed %d and seed %d gave the same bytes", seed, seed+1)
	}

	for i, c := range Bytes(Zeros, seed, n) {
		if c != 0 {
			t.Fatalf("Zeros: byte %d is %#x, want 0", i, c)
		}
	}

	for _, c := range []struct {
		k      Kind
		period int
	}{{Period64, 64}, {Text44, 44}, {Period4K, 4096}} {
		b := Bytes(c.k, seed, n)
		if p := minimalPeriod(b); p != c.period {
			t.Errorf("kind %d: period %d, want %d", c.k, p, c.period)
		}
	}

	text := Bytes(Text44, seed, n)
	if want := "The quick brown fox jumps over the lazy dog "; string(text[:44]) != want {
		t.Errorf("Text44: starts %q, want %q", text[:44], want)
	}
	for i, c := range text {
		if c < 0x20 || c > 0x7e {
			t.Fatalf("Text44: byte %d is %#x, not printable ASCII", i, c)
		}
	}

	// The drawn patterns come from the seed: another seed draws another.
	for _, k := range []Kind{Period64, Period4K} {
		if bytes.Equal(Bytes(k, seed, 4096), Bytes(k, seed+1, 4096)) {
			t.Errorf("kind %d: seed %d and seed %d drew the same pattern", k, seed, seed+1)
		}
	}

	rr := Bytes(RandomRepeat, seed, n)
	lo, hi := n/3, 2*n/3
	if !bytes.Equal(rr[:lo], random[:lo]) || !bytes.Equal(rr[hi:], random[hi:]) {
		t.Errorf("RandomRepeat: outside [%d, %d) the bytes are not Random's", lo, hi)
	}
	if want := Bytes(Period64, seed, n)[lo:hi]; !bytes.Equal(rr[lo:hi], want) {
		t.Errorf("RandomRepeat: [%d, %d) is not Period64 at the same offsets", lo, hi)
	}
	if rr[lo-1] == rr[lo-1+64] && rr[hi] == rr[hi-64] {
		t.Errorf("RandomRepeat: the repeat runs past [%d, %d)", lo, hi)
	}
}

// firstDiff is the first index where a and b, of one length, differ, or -1.
func firstDiff(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}

// The bytes do not depend on how they are read: every split of reads gives
// Bytes, and the stream ends at n with io.EOF.
func TestInputReadSplits(t *testing.T) {
	const seed, n = 3, 3*4096 + 5

	for _, k := range kinds {
		want := Bytes(k, seed, n)
		for _, size := range []int{1, 7, 64, 4097, 2 * n} {
			r := Input(k, seed, n)
			var got []byte
			buf := make([]byte, size)
			for {
				m, err := r.Read(buf)
				got = append(got, buf[:m]...)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("kind %d, reads of %d: %v", k, size, err)
				}
			}
			if len(got) != len(want) {
				t.Errorf("kind %d, reads of %d: read %d bytes, want %d", k, size, len(got), len(want))
			} else if i := firstDiff(got, want); i >= 0 {
				t.Errorf("kind %d, reads of %d: byte %d is %#x, Bytes has %#x", k, size, i, got[i], want[i])
			}
		}
	}
}

// The first 32 bytes of Random at seed 1 are pinned, so a seed spread another
// way or a changed ChaCha8 stream fails here before it moves every golden
// boundary.
func TestSeededPrefix(t *testing.T) {
	const want = "6ae6783f4fbde91b6eb88b73a48ed247dbe5882e2579683432c1bfc525454add"

	if got := hex.EncodeToString(Bytes(Random, 1, 32)); got != want {
		t.Errorf("Random at seed 1 starts %s, want %s", got, want)
	}
}

// Reading 256 MiB of each kind through a 1 MiB buffer allocates nothing per
// Read, so a count taken over these inputs counts only the code under test.
func TestAllocsSeededInput(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("allocation counts are taken without the race detector")
	}

	const n, size = 256 << 20, 1 << 20

	buf := make([]byte, size)
	for _, k := range kinds {
		r := Input(k, 1, n)
		var read int
		allocs := testing.AllocsPerRun(n/size-1, func() {
			m, _ := r.Read(buf)
			read += m
		})
		if allocs != 0 {
			t.Errorf("kind %d: %v allocations per Read, want 0", k, allocs)
		}
		if read != n {
			t.Errorf("kind %d: read %d bytes, want %d", k, read, n)
		}
	}
}
