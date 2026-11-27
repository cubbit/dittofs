package transformtest

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// FuzzDecode fuzzes the Decode of the transform s builds (RFC 5 §2.1, §2.7,
// §7 T4), from a transform's own fuzz target. Its seeds are the bodies of
// the transform's fixtures, of round-trip inputs, and of inputs of 4 × max
// bytes. Each fuzz input is a seed index and a body: the index, modulo the
// seed count, picks the namespace, hash and census the body decodes under,
// so a mutated body, or a crasher replayed from testdata/fuzz, still finds
// its material. Every decode must not panic, must return one of RFC 5 §2.7's
// five errors or at most max bytes, and must keep within the allocation
// bound; a decoded output, encoded again under the seed's material, must
// decode to itself.
func FuzzDecode(f *testing.F, s Subject) {
	f.Helper()

	if err := s.check(); err != nil {
		f.Fatal(err)
	}

	z, err := newFuzzer(f.Context(), s)
	if err != nil {
		f.Fatal(err)
	}

	for i, sd := range z.seeds {
		f.Add(uint8(i), sd.body)
	}

	f.Fuzz(func(t *testing.T, seed uint8, body []byte) {
		if err := z.check(t.Context(), seed, body); err != nil {
			t.Fatal(err)
		}
	})
}

// fuzzRoundTrips is how many round-trip inputs' bodies seed the fuzzer.
const fuzzRoundTrips = 8

// allocSlack is what a Decode may allocate beyond max and Subject.Scratch.
const allocSlack = 64 << 10

// fuzzSeed is one seed body and what it decodes under: an env in its
// namespace, its hash, its census, and the encoder of its settings.
type fuzzSeed struct {
	e      *env
	hash   [32]byte
	census []keys.MaterialID
	enc    transform.Transform
	body   []byte
}

// fuzzer holds every seed, their material all given to one Memory.
type fuzzer struct {
	seeds []fuzzSeed
}

func newFuzzer(ctx context.Context, s Subject) (*fuzzer, error) {
	e, err := newEnv(ctx, s)
	if err != nil {
		return nil, err
	}

	z := &fuzzer{}

	ins, err := e.small(ctx, fuzzRoundTrips)
	if err != nil {
		return nil, err
	}

	seed := func(in input) (bool, error) {
		enc, err := e.encode(ctx, e.enc, nil, in.clone())
		if err != nil {
			return false, fmt.Errorf("transformtest: seeding the fuzzer: %s: Encode: %w", in, err)
		}

		if enc.Applied {
			z.seeds = append(z.seeds, fuzzSeed{e, in.hash, enc.Material, e.enc, enc.Body})
		}

		return enc.Applied, nil
	}

	for _, in := range ins {
		if _, err := seed(in); err != nil {
			return nil, err
		}
	}

	// A body claiming 4 × max bytes: the encode of zeros, or of random bytes
	// if the transform declines zeros.
	for _, plain := range [][]byte{make([]byte, 4*s.Max), randomBytes(4 * s.Max)} {
		if ok, err := seed(newInput(-1, plain)); ok || err != nil {
			if err != nil {
				return nil, err
			}

			break
		}
	}

	cases, err := e.fixtures()
	if err != nil {
		return nil, err
	}

	for _, c := range cases {
		ce := *e
		ce.ns = c.ns

		pin := []keys.MaterialID{}
		for _, m := range c.Material {
			pin = append(pin, e.mem.Give(c.ns, m.Kind, m.ID, m.Bytes))
		}

		tr, err := e.build(ctx, c.Settings)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c, err)
		}

		z.seeds = append(z.seeds, fuzzSeed{&ce, c.hash, pin, tr, c.Body})
	}

	return z, nil
}

// check decodes body under the seed index picks, and returns what it found
// wrong, or nil.
//
// ponytail: the allocation bound covers the whole Decode, not only up to its
// header's validation, and an overrun of under 64 KiB passes. Move to
// per-call allocation profiles if a transform ever needs that precision.
func (z *fuzzer) check(ctx context.Context, seed uint8, body []byte) error {
	sd := z.seeds[int(seed)%len(z.seeds)]
	max := sd.e.s.Max
	enc := transform.Encoded{Body: body, Applied: true, Material: sd.census}

	dst := make([]byte, 0, max)

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	out, err := sd.e.decode(ctx, dst, sd.hash, nil, enc)
	runtime.ReadMemStats(&after)

	if got, limit := after.TotalAlloc-before.TotalAlloc, uint64(max+sd.e.s.Scratch+allocSlack); got > limit {
		return fmt.Errorf("seed %d, a body of %d bytes: Decode allocated %d bytes, over max %d + Scratch %d + %d", seed, len(body), got, max, sd.e.s.Scratch, allocSlack)
	}

	switch {
	case err != nil && isDecodeError(err):
		return nil
	case err != nil:
		return fmt.Errorf("seed %d, a body of %d bytes: Decode returned %v, not one of RFC 5 §2.7's five errors", seed, len(body), err)
	case len(out) > max:
		return fmt.Errorf("seed %d, a body of %d bytes: Decode returned %d bytes, over max %d", seed, len(body), len(out), max)
	}

	again, err := sd.e.encodeUnder(ctx, sd.enc, sd.census, nil, input{index: -1, plain: slices.Clone(out), hash: sd.hash})
	if err != nil {
		return fmt.Errorf("seed %d, a body of %d bytes: Encode of its %d decoded bytes: %w", seed, len(body), len(out), err)
	}

	if !again.Applied {
		// A declined chunk is stored as it is and never reaches Decode.
		if !bytes.Equal(again.Body, out) {
			return fmt.Errorf("seed %d, a body of %d bytes: Encode declined its %d decoded bytes with a body other than them", seed, len(body), len(out))
		}

		return nil
	}

	twice, err := sd.e.decode(ctx, nil, sd.hash, nil, again)
	if err != nil {
		return fmt.Errorf("seed %d, a body of %d bytes: Decode of its decoded bytes encoded again: %w", seed, len(body), err)
	}

	if !bytes.Equal(twice, out) {
		return fmt.Errorf("seed %d, a body of %d bytes: its decoded bytes, encoded again, decode to other bytes, from byte %d", seed, len(body), firstDiff(twice, out))
	}

	return nil
}
