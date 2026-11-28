package transformtest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
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

// foreignTooLarge reports an output over max with an error of its own.
type foreignTooLarge struct{ *frame }

func (f foreignTooLarge) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	out, err := f.frame.Decode(ctx, ns, dst, hash, body, max, m)
	if errors.Is(err, transform.ErrTooLarge) {
		return nil, errors.New("frame: too long")
	}

	return out, err
}

// ignoresMax decodes whatever its body holds, however long.
type ignoresMax struct{ *frame }

func (i ignoresMax) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, _ int, m keys.Provider) ([]byte, error) {
	return i.frame.Decode(ctx, ns, dst, hash, body, len(body), m)
}

// The fuzzer's check passes every seed of the correct test transforms, and
// fails a seed of each broken one with the error written for it: a decoder
// that allocates the size its body claims, one returning a foreign error, one
// returning more than max, and an encoder whose bodies decode to other bytes
// than it was given. FuzzDecode's body is this check alone.
func TestFuzzDecodeChecksEachSeed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// A max of 64 KiB, so a body claiming 4 × max is past the allocation
	// slack.
	big := func(wrap func(*frame) transform.Transform) Subject {
		s := frameSubject(wrap)
		s.Max = 64 << 10

		return s
	}

	for _, c := range []struct {
		name string
		s    Subject
		want string // a substring of every failing seed's error; none for a correct transform
	}{
		{"frame", frameSubject(asFrame), ""},
		{"seal", sealSubject(fullMAC, asSeal), ""},
		{"frame at a max of 64 KiB", big(asFrame), ""},
		{"an allocation of the size a body claims", big(func(f *frame) transform.Transform { return claimsAlloc{f} }), "bytes, over max 65536 + Scratch 0 + 65536"},
		{"a foreign error", frameSubject(func(f *frame) transform.Transform { return foreignTooLarge{f} }), "Decode returned frame: too long, not one of RFC 5 §2.7's five errors"},
		{"an output over max", frameSubject(func(f *frame) transform.Transform { return ignoresMax{f} }), "Decode returned 16384 bytes, over max 4096"},
		{"lossy on one input", frameSubject(func(f *frame) transform.Transform { return lossy{f} }), "its decoded bytes, encoded again, decode to other bytes, from byte 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			z, err := newFuzzer(t.Context(), c.s)
			if err != nil {
				t.Fatal(err)
			}

			failed := 0

			for i, sd := range z {
				err := z.check(t.Context(), uint8(i), sd.body)

				switch {
				case err != nil && c.want == "":
					t.Errorf("seed %d: %v, want no error", i, err)
				case err != nil && !strings.Contains(err.Error(), c.want):
					t.Errorf("seed %d: %v, want an error containing %q", i, err, c.want)
				case err != nil:
					failed++
				}
			}

			if c.want != "" && failed == 0 {
				t.Errorf("every one of %d seeds passed, want one to fail with an error containing %q", len(z), c.want)
			}
		})
	}
}

// A fixture that gives an ID the suite's material already holds, under other
// bytes, would leave the seeds encoded under the first unable to decode in
// the fuzzer's one Memory, so the fuzzer refuses to start.
func TestFuzzDecodeRefusesSeedsSpoilingEachOther(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := Fixture{Version: 1, Namespace: suiteNS, Input: []byte("in"), Body: []byte("body")}
	f.Material = append(f.Material, struct {
		Kind, ID string
		Bytes    []byte
	}{sealKind, "transformtest-" + sealKind, []byte("other bytes")})

	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}

	s := sealSubject(fullMAC, asSeal)
	s.fixtures = t.TempDir()

	if err := os.Mkdir(filepath.Join(s.fixtures, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(s.fixtures, "v1", "spoils.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := newFuzzer(t.Context(), s); err == nil || !strings.Contains(err.Error(), "seed 0 does not decode under the fuzzer's one Memory") {
		t.Errorf("newFuzzer: %v, want seed 0 refused as not decoding", err)
	}
}
