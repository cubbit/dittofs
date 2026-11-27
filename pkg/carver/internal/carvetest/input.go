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
	rng      *rand.ChaCha8 // nil but for the random kinds
	pattern  []byte        // repeated from offset 0 over [lo, hi)
	lo, hi   int64
	off, end int64
}

// Input returns n bytes of kind k from seed. Read allocates nothing, and the
// reader holds no buffer of n bytes. RandomRepeat's middle third is
// [n/3, n-n/3), and its bytes outside it are Random's at the same offsets.
func Input(k Kind, seed uint64, n int64) io.Reader {
	var s [32]byte
	binary.LittleEndian.PutUint64(s[:], seed)
	// A pattern comes from its own stream of the seed, so it is not Random's prefix.
	drawn := func(period int) []byte {
		ps := s
		ps[8] = 1
		p := make([]byte, period)
		_, _ = rand.NewChaCha8(ps).Read(p)
		return p
	}

	in := &input{hi: n, end: n}
	switch k {
	case Random:
		in.rng, in.hi = rand.NewChaCha8(s), 0
	case Zeros:
		in.pattern = make([]byte, 64)
	case Period64:
		in.pattern = drawn(64)
	case Text44:
		in.pattern = []byte(text44)
	case Period4K:
		in.pattern = drawn(4096)
	case RandomRepeat:
		in.rng, in.pattern, in.lo, in.hi = rand.NewChaCha8(s), drawn(64), n/3, n-n/3
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

	if in.rng != nil {
		_, _ = in.rng.Read(p)
	}
	if lo, hi := max(in.lo, in.off), min(in.hi, in.off+int64(len(p))); lo < hi {
		fill(p[lo-in.off:hi-in.off], in.pattern, lo)
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
