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
// decode to itself, unless the transform declines it.
func FuzzDecode(f *testing.F, s Subject) {
	f.Helper()

	if err := s.check(); err != nil {
		f.Fatal(err)
	}

	z, err := newFuzzer(f.Context(), s)
	if err != nil {
		f.Fatal(err)
	}

	for i, sd := range z {
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
	claims bool // the body claims more than max
}

// fuzzer is every seed, their material all given to one Memory.
type fuzzer []fuzzSeed

func newFuzzer(ctx context.Context, s Subject) (fuzzer, error) {
	e, err := newEnv(ctx, s)
	if err != nil {
		return nil, err
	}

	ins, err := e.small(ctx, fuzzRoundTrips)
	if err != nil {
		return nil, err
	}

	var z fuzzer

	seed := func(in input, claims bool) (bool, error) {
		enc, err := e.encode(ctx, e.enc, nil, in.clone())
		if err != nil {
			return false, fmt.Errorf("transformtest: seeding the fuzzer: %s: Encode: %w", in, err)
		}

		if enc.Applied {
			z = append(z, fuzzSeed{e, in.hash, enc.Material, e.enc, enc.Body, claims})
		}

		return enc.Applied, nil
	}

	for _, in := range ins {
		if _, err := seed(in, false); err != nil {
			return nil, err
		}
	}

	// A body claiming 4 × max bytes: the encode of zeros, or of random bytes
	// if the transform declines zeros.
	claimed := false
	for _, plain := range [][]byte{make([]byte, 4*s.Max), randomBytes(4 * s.Max)} {
		if claimed, err = seed(newInput(-1, plain), true); err != nil {
			return nil, err
		}

		if claimed {
			break
		}
	}

	if !claimed {
		return nil, fmt.Errorf("transformtest: the transform declined %d zeros and %d random bytes, so no seed claims 4 × max", 4*s.Max, 4*s.Max)
	}

	cases, err := e.fixtures()
	if err != nil {
		return nil, err
	}

	for _, c := range cases {
		ce, pin := e.forCase(c, e.mem)

		tr, err := e.build(ctx, c.Settings)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c, err)
		}

		z = append(z, fuzzSeed{ce, c.Hash, pin, tr, c.Body, false})
	}

	if len(z) > 256 {
		return nil, fmt.Errorf("transformtest: %d seeds, over the 256 a seed index reaches", len(z))
	}

	// One Memory holds every seed's material, so a fixture giving an ID
	// another seed was given under other bytes would spoil that seed's
	// decode, and every fuzz input on it would pass as refused.
	for i, sd := range z {
		_, err := sd.e.decode(ctx, nil, sd.hash, nil, transform.Encoded{Body: sd.body, Applied: true, Material: sd.census})
		if err != nil && !sd.claims {
			return nil, fmt.Errorf("transformtest: seed %d does not decode under the fuzzer's one Memory: %w", i, err)
		}
	}

	return z, nil
}

// check decodes body under the seed index picks, and returns what it found
// wrong, or nil.
//
// ponytail: the allocation bound covers the whole Decode, not only up to its
// header's validation, counts what any other goroutine allocates meanwhile,
// sees nothing allocated outside the Go heap, and lets an overrun of under
// 64 KiB pass. Move to per-call allocation profiles if a transform ever needs
// that precision.
func (z fuzzer) check(ctx context.Context, seed uint8, body []byte) error {
	sd := z[int(seed)%len(z)]
	max := sd.e.s.Max
	enc := transform.Encoded{Body: body, Applied: true, Material: sd.census}

	dst := make([]byte, 0, max)

	// A first decode, unmeasured, builds whatever the decoder keeps for
	// later calls, so the measured one counts only what this call needs.
	_, _ = sd.e.decode(ctx, dst, sd.hash, nil, enc)

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

	// A declined chunk is stored as it is and never reaches Decode.
	if !again.Applied {
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
