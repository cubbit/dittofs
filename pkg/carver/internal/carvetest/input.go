package carvetest

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
)

// Targets is every supported Target: the powers of two from 64 KiB to 4 MiB
// (RFC 2 §3.7).
var Targets = [...]int{64 << 10, 128 << 10, 256 << 10, 512 << 10, 1 << 20, 2 << 20, 4 << 20}

// Kind is a content kind of RFC 2 §10.2.
type Kind int

const (
	Random       Kind = iota // incompressible: boundaries come from the fingerprint
	Zeros                    // only Max ends a chunk
	Period64                 // a 64-byte pattern drawn from the seed
	Text44                   // a 44-byte ASCII sentence, repeated
	Period4K                 // a 4,096-byte pattern drawn from the seed
	RandomRepeat             // random, with its middle third Period64
)

// text44 is Text44's period.
const text44 = "The quick brown fox jumps over the lazy dog "

// input is one seeded stream. The random bytes come from ChaCha8, a specified
// stream (C2SP chacha8rand), so a seed gives the same bytes on any machine
// and toolchain, and in any split of reads.
type input struct {
	rng      rand.ChaCha8
	pattern  []byte // the period of a patterned kind; RandomRepeat's middle third
	random   bool   // bytes come from rng, under the pattern's range when one is set
	lo, hi   int64  // where pattern replaces the random bytes, for RandomRepeat
	off, end int64
}

// Input returns n bytes of kind k from seed. Read allocates nothing, and the
// reader holds no buffer of n bytes. The seed is spread little-endian into
// the first 8 bytes of ChaCha8's 32-byte seed; a pattern drawn from the seed
// is the first bytes of that stream, and RandomRepeat's bytes outside its
// middle third are Random's at the same offsets.
func Input(k Kind, seed uint64, n int64) io.Reader {
	if n < 0 {
		panic(fmt.Sprintf("carvetest: Input of %d bytes", n))
	}

	var s [32]byte
	binary.LittleEndian.PutUint64(s[:], seed)
	in := &input{end: n}
	in.rng.Seed(s)

	drawn := func(period int) []byte {
		p := make([]byte, period)
		var r rand.ChaCha8
		r.Seed(s)
		_, _ = r.Read(p)
		return p
	}

	switch k {
	case Random:
		in.random = true
	case Zeros:
	case Period64:
		in.pattern = drawn(64)
	case Text44:
		in.pattern = []byte(text44)
	case Period4K:
		in.pattern = drawn(4096)
	case RandomRepeat:
		in.random = true
		in.pattern = drawn(64)
		in.lo, in.hi = n/3, 2*n/3
	default:
		panic(fmt.Sprintf("carvetest: unknown kind %d", k))
	}

	return in
}

func (in *input) Read(p []byte) (int, error) {
	if in.off >= in.end {
		return 0, io.EOF
	}
	if rest := in.end - in.off; int64(len(p)) > rest {
		p = p[:rest]
	}

	switch {
	case in.random:
		_, _ = in.rng.Read(p)
		lo, hi := max(in.lo, in.off), min(in.hi, in.off+int64(len(p)))
		if lo < hi {
			fill(p[lo-in.off:hi-in.off], in.pattern, lo)
		}
	case in.pattern != nil:
		fill(p, in.pattern, in.off)
	default:
		clear(p)
	}

	in.off += int64(len(p))

	return len(p), nil
}

// fill writes into p the bytes of pattern repeated from offset 0, starting at
// offset off.
func fill(p, pattern []byte, off int64) {
	i := int(off % int64(len(pattern)))
	for len(p) > 0 {
		n := copy(p, pattern[i:])
		p, i = p[n:], 0
	}
}

// Bytes reads n bytes of Input into one slice.
func Bytes(k Kind, seed uint64, n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(Input(k, seed, int64(n)), b); err != nil {
		panic(err)
	}

	return b
}
