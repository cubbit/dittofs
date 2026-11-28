package carvetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// kinds is every Kind, in order.
var kinds = []Kind{Random, Zeros, Period64, Text44, Period4K, RandomRepeat}

func (k Kind) String() string {
	return [...]string{"Random", "Zeros", "Period64", "Text44", "Period4K", "RandomRepeat"}[k]
}

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
			t.Errorf("%v: period %d, want %d", c.k, p, c.period)
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

	// The drawn patterns come from the seed: another seed draws another, and
	// neither is Random's prefix.
	for _, k := range []Kind{Period64, Period4K} {
		if bytes.Equal(Bytes(k, seed, 4096), Bytes(k, seed+1, 4096)) {
			t.Errorf("%v: seed %d and seed %d drew the same pattern", k, seed, seed+1)
		}
		if p := Bytes(k, seed, 64); bytes.Equal(p, random[:64]) {
			t.Errorf("%v: the pattern is Random's first bytes", k)
		}
	}

	rr := Bytes(RandomRepeat, seed, n)
	lo, hi := n/3, n-n/3
	if !bytes.Equal(rr[:lo], random[:lo]) || !bytes.Equal(rr[hi:], random[hi:]) {
		t.Errorf("RandomRepeat: outside [%d, %d) the bytes are not Random's", lo, hi)
	}
	if want := Bytes(Period64, seed, n)[lo:hi]; !bytes.Equal(rr[lo:hi], want) {
		t.Errorf("RandomRepeat: [%d, %d) is not Period64 at the same offsets", lo, hi)
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
				if err != nil || m == 0 {
					t.Fatalf("%v, reads of %d: read %d bytes, error %v, at %d of %d", k, size, m, err, len(got), n)
				}
			}
			if len(got) != len(want) {
				t.Errorf("%v, reads of %d: read %d bytes, want %d", k, size, len(got), len(want))
			} else if i := firstDiff(got, want); i >= 0 {
				t.Errorf("%v, reads of %d: byte %d is %#x, Bytes has %#x", k, size, i, got[i], want[i])
			}
		}
	}
}

// The first 32 bytes of Random at seed 1 are pinned, and the SHA-256 of each
// kind's first 64 KiB at seed 1, past ChaCha8's rekeying, so a seed spread
// another way or a changed stream fails here before it moves every golden
// boundary.
func TestSeededPrefix(t *testing.T) {
	const want = "6ae6783f4fbde91b6eb88b73a48ed247dbe5882e2579683432c1bfc525454add"

	if got := hex.EncodeToString(Bytes(Random, 1, 32)); got != want {
		t.Errorf("Random at seed 1 starts %s, want %s", got, want)
	}

	sums := map[Kind]string{
		Random:       "9da579aa3de9865283db669648ec1ace67973e1a14b78841526d0ed405d0dcb1",
		Zeros:        "de2f256064a0af797747c2b97505dc0b9f3df0de4f489eac731c23ae9ca9cc31",
		Period64:     "23d092a8a3872529f75c72fc636455a32c0b90a826abf2cee528a6d913cebfce",
		Text44:       "d252abff0f101cc87ca4db96ae16bdb7dde3e01949809903427636aa59aec08e",
		Period4K:     "a75555ea281371e81706b4a8eb92eff4161d7442db5cb9180bc672cb7c5170d4",
		RandomRepeat: "2fc7f88f1fd5a21ea18a09eb07cf5a96a78252d141ce22b2e47a935422cfa4a0",
	}
	for _, k := range kinds {
		sum := sha256.Sum256(Bytes(k, 1, 64<<10))
		if got := hex.EncodeToString(sum[:]); got != sums[k] {
			t.Errorf("%v at seed 1: the first 64 KiB hash to %s, want %s", k, got, sums[k])
		}
	}
}

// Reading 256 MiB of Random, and 8 MiB of each other kind, through a 1 MiB
// buffer allocates nothing in any Read, and the reader holds no buffer of the
// input's size, so a count taken over these inputs counts only the code under
// test. The counts are taken over the whole stream rather than per call, so a
// reader that allocates on some reads only is caught too.
func TestAllocsSeededInput(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("allocation counts are taken without the race detector")
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	buf := make([]byte, 1<<20)
	for _, k := range kinds {
		n := 8 << 20
		if k == Random {
			n = 256 << 20
		}

		var start, built, done runtime.MemStats
		runtime.ReadMemStats(&start)
		r := Input(k, 1, int64(n))
		runtime.ReadMemStats(&built)
		read := 0
		for read < n {
			m, err := r.Read(buf)
			if err != nil || m == 0 {
				t.Fatalf("%v: read %d bytes, error %v, at %d of %d", k, m, err, read, n)
			}
			read += m
		}
		runtime.ReadMemStats(&done)

		if b := built.TotalAlloc - start.TotalAlloc; b > 64<<10 {
			t.Errorf("%v: Input of %d bytes allocated %d bytes, want at most %d", k, n, b, 64<<10)
		}
		if a := done.Mallocs - built.Mallocs; a != 0 {
			t.Errorf("%v: %d allocations over %d Reads, want 0", k, a, n/len(buf))
		}
	}
}
