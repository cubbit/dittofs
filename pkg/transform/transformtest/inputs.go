package transformtest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// seed is the one source of every round-trip input: input i is drawn from
// (seed, i) alone, so a failing input is found again from its index and no
// row holds more than the inputs it is checking.
const seed = 1

// The round-trip inputs: how many; how many of the small ones, those of at
// most smallSize bytes, the flip rows turn every byte of; and how many the
// rows that run once per setting encode, spread over the whole set.
//
// ponytail: the flip rows flip the first flipCount small inputs, not every
// one, and the per-setting rows encode spreadCount inputs, so their cost is
// fixed whatever the chunk maximum. Flip every small input if a transform's
// format ever decodes differently by input kind past the first few.
const (
	inputCount  = 10_000
	flipCount   = 16
	smallSize   = 4 << 10
	spreadCount = 200
)

// input is one chunk the rows encode: its bytes, the hash it is encoded
// under, and its index among the round-trip inputs, or -1 for one a row
// built.
type input struct {
	index int
	plain []byte
	hash  [32]byte
}

func newInput(index int, plain []byte) input {
	return input{index: index, plain: plain, hash: sha256.Sum256(plain)}
}

func (in input) String() string {
	if in.index < 0 {
		return fmt.Sprintf("an input of %d bytes", len(in.plain))
	}

	return fmt.Sprintf("seed %d, input %d (%d bytes)", seed, in.index, len(in.plain))
}

// clone is in with its own copy of the bytes, so a transform writing into
// its input spoils no other call's.
func (in input) clone() input {
	in.plain = slices.Clone(in.plain)
	return in
}

// fill fills b from r, eight bytes a draw.
func fill(r *rand.Rand, b []byte) {
	var w [8]byte

	for i := 0; i < len(b); i += 8 {
		binary.LittleEndian.PutUint64(w[:], r.Uint64())
		copy(b[i:], w[:])
	}
}

// randomBytes is n bytes drawn from the seed.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	fill(rand.New(rand.NewPCG(seed, ^uint64(n))), b)

	return b
}

// source is input i's random source.
func source(i int) *rand.Rand { return rand.New(rand.NewPCG(seed, uint64(i))) }

// sizeFrom is input i's size, the first draw of r: 1, 2, Max - 1 and Max
// for the first four, log-uniform from 1 to Max after.
func (s Subject) sizeFrom(r *rand.Rand, i int) int {
	n := int(math.Exp(r.Float64() * math.Log(float64(s.Max))))
	if fixed := []int{1, 2, s.Max - 1, s.Max}; i < len(fixed) {
		n = fixed[i]
	}

	return min(max(n, 1), s.Max)
}

// size is input i's size before a prefix lengthens it.
func (s Subject) size(i int) int { return s.sizeFrom(source(i), i) }

// shipped are transform IDs an envelope prefix names beside the subject's
// own: the shipped encryption and zstd transforms (RFC 5 Appendix B).
var shipped = []uint16{0x0010, 0x0001}

// input is round-trip input i: random bytes, all zeros, all one byte, or
// random bytes that begin with the transform's own output of another input
// or with an envelope of 0 to 3 transform IDs (RFC 5 §2.4), so a transform
// mistaking its input for its output is caught.
func (e *env) input(ctx context.Context, i int) (input, error) {
	r := source(i)
	n := e.s.sizeFrom(r, i)

	var prefix []byte

	switch i % 5 {
	case 0, 1:
	case 2:
		return newInput(i, make([]byte, n)), nil
	case 3:
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(1 + (i/5)%255)
		}

		return newInput(i, b), nil
	case 4:
		if (i/5)%2 == 0 {
			own := make([]byte, 1+r.IntN(64))
			fill(r, own)

			// A transform that declines random bytes, a compressor, still
			// applies itself to zeros.
			for _, try := range [][]byte{own, make([]byte, len(own))} {
				enc, err := e.encode(ctx, e.enc, nil, newInput(-1, try))
				if err != nil {
					return input{}, fmt.Errorf("transformtest: building input %d: Encode: %w", i, err)
				}

				if enc.Applied {
					prefix = enc.Body
					break
				}
			}
		} else {
			count := (i / 10) % 4
			prefix = []byte{1, byte(count)}

			for _, id := range append([]uint16{uint16(e.enc.ID())}, shipped...)[:count] {
				prefix = binary.BigEndian.AppendUint16(prefix, id)
			}
		}

		n = min(max(n, len(prefix)), e.s.Max)
	}

	b := make([]byte, n)
	fill(r, b)
	copy(b, prefix)

	return newInput(i, b), nil
}

// each calls fn on every round-trip input in turn, stopping at its first
// error.
func (e *env) each(ctx context.Context, fn func(input) error) error {
	for i := range inputCount {
		in, err := e.input(ctx, i)
		if err != nil {
			return err
		}

		if err := fn(in); err != nil {
			return err
		}
	}

	return nil
}

// spread calls fn on spreadCount round-trip inputs spread over the set, the
// one of Max bytes among them.
func (e *env) spread(ctx context.Context, fn func(input) error) error {
	for k := range spreadCount {
		i := k * (inputCount / spreadCount)
		if k == 1 {
			i = 3
		}

		in, err := e.input(ctx, i)
		if err != nil {
			return err
		}

		if err := fn(in); err != nil {
			return err
		}
	}

	return nil
}

// small is the first count round-trip inputs of at most smallSize bytes.
func (e *env) small(ctx context.Context, count int) ([]input, error) {
	var ins []input

	for i := 0; i < inputCount && len(ins) < count; i++ {
		if e.s.size(i) > smallSize {
			continue
		}

		in, err := e.input(ctx, i)
		if err != nil {
			return nil, err
		}

		if len(in.plain) <= smallSize {
			ins = append(ins, in)
		}
	}

	if len(ins) < count {
		return nil, fmt.Errorf("transformtest: %d round-trip inputs of at most %d bytes, want %d", len(ins), smallSize, count)
	}

	return ins, nil
}
