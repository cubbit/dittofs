package secrets

import (
	"bytes"
	"flag"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
)

// FuzzParseEnvelope feeds the parser arbitrary bytes: it never panics, and
// whatever it accepts has a wrapped key and a nonce and tag, and rebuilds to
// the bytes it was given.
func FuzzParseEnvelope(f *testing.F) {
	// decision: the leak check runs on the seed corpus only. Under -fuzz the
	// fuzzing engine keeps its own signal goroutine running, which the check
	// would report as a leak of this target. Plain go test runs every seed,
	// the refusals included, under the check. Withdraw when the fuzzing
	// engine stops leaving its signal goroutine running.
	if flag.Lookup("test.fuzz").Value.String() == "" {
		prof.Watch(f, prof.Options{})
	}

	for _, tc := range malformedEnvelopes() {
		f.Add(tc.env)
	}

	f.Add(validEnvelope())

	f.Fuzz(func(t *testing.T, env []byte) {
		wrapped, sealed, err := parseEnvelope(env)
		if err != nil {
			return
		}

		if len(wrapped) == 0 || len(sealed) < 28 {
			t.Fatalf("accepted %x as wrapped %x and sealed %x", env, wrapped, sealed)
		}

		if got := buildEnvelope(wrapped, sealed); !bytes.Equal(got, env) {
			t.Fatalf("accepted %x, rebuilt %x", env, got)
		}
	})
}
