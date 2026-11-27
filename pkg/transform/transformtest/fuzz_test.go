package transformtest

import (
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// The correct test transforms under FuzzDecode; plain go test runs their
// seeds.
func FuzzFrameDecode(f *testing.F) {
	watchSeeds(f)
	FuzzDecode(f, frameSubject(asFrame))
}

func FuzzSealDecode(f *testing.F) {
	watchSeeds(f)
	FuzzDecode(f, sealSubject(fullMAC, asSeal))
}

// watchSeeds runs the profile watch over a fuzz target's seed run.
//
// decision: the leak check runs on the seed corpus only. Under -fuzz the
// fuzzing engine keeps its own signal goroutine running, which the check
// would report as a leak of this target. Plain go test runs every seed under
// the check. Withdraw when the fuzzing engine stops leaving its signal
// goroutine running.
func watchSeeds(f *testing.F) {
	if flag.Lookup("test.fuzz").Value.String() == "" {
		prof.Watch(f, prof.Options{})
	}
}

// claimsAlloc allocates the whole of what its body claims before refusing an
// output over max.
type claimsAlloc struct{ *frame }

func (c claimsAlloc) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	out := make([]byte, 0, len(body))

	plain, err := c.frame.Decode(ctx, ns, out, hash, body, len(body), m)
	if err == nil && len(plain) > max {
		return nil, transform.ErrTooLarge
	}

	return append(dst[:0], plain...), err
}

// The fuzzer's check passes every seed of the correct test transforms, and
// fails a seed of each broken one with the error written for it: a decoder
// that allocates the size its body claims, and an encoder whose bodies decode
// to other bytes than it was given.
func TestFuzzDecodeChecksEachSeed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// Over 64 KiB, so a body claiming 4 × max is past the allocation slack.
	big := frameSubject(func(f *frame) transform.Transform { return claimsAlloc{f} })
	big.Max = 64 << 10

	for _, c := range []struct {
		name string
		s    Subject
		want string // a substring of every failing seed's error; none for a correct transform
	}{
		{"frame", frameSubject(asFrame), ""},
		{"seal", sealSubject(fullMAC, asSeal), ""},
		{"an allocation of the size a body claims", big, "bytes, over max 65536 + Scratch 0 + 65536"},
		{"lossy on one input", frameSubject(func(f *frame) transform.Transform { return lossy{f} }), "its decoded bytes, encoded again, decode to other bytes, from byte 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			z, err := newFuzzer(t.Context(), c.s)
			if err != nil {
				t.Fatal(err)
			}

			failed := 0

			for i, sd := range z.seeds {
				err := z.check(t.Context(), uint8(i), sd.body)

				switch {
				case err != nil && (c.want == "" || !strings.Contains(err.Error(), c.want)):
					t.Errorf("seed %d: %v, want %s", i, err, map[bool]string{true: "no error", false: "an error containing " + c.want}[c.want == ""])
				case err != nil:
					failed++
				}
			}

			if c.want != "" && failed == 0 {
				t.Errorf("every one of %d seeds passed, want one to fail with an error containing %q", len(z.seeds), c.want)
			}
		})
	}
}
