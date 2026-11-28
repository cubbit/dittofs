package chain_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
)

// FuzzChainDecode fuzzes Chain.Decode over the test transforms, seeded with
// the bodies every combination of them writes and with malformed envelopes.
// Every decode must not panic, which bounds every read inside the body, must
// return one of RFC 5 §2.7's five errors or at most the chunk maximum, and an
// envelope the parse accepts must end inside the body and re-encode to the
// body's own bytes. It decodes with no census, which no test transform reads.
func FuzzChainDecode(f *testing.F) {
	// No prof.Watch: under -fuzz the engine's own signal goroutine and
	// descriptors outlive the target and read as leaks.
	ctx := context.Background()
	r, p := codecRegistry(), withKey()
	parse, err := r.EnvelopeParser(ctx, p)
	if err != nil {
		f.Fatalf("EnvelopeParser: %v", err)
	}
	// Every chain of r decodes through r's decode set, whatever it is
	// configured with, so the last one built serves.
	var c transform.Chain
	for _, cfg := range combos() {
		cc, err := build(r, p, cfg...)
		if err != nil {
			f.Fatalf("NewChain(%v): %v", cfg, err)
		}
		c = cc
		a, err := cc.Pin(ctx, ns)
		if err != nil {
			f.Fatalf("Pin: %v", err)
		}
		for _, in := range inputs() {
			body, _, err := a.Encode(ctx, nil, hash, in)
			if err != nil {
				f.Fatalf("Encode: %v", err)
			}
			f.Add(body)
		}
	}
	for _, b := range [][]byte{nil, {1}, {0, 0}, {2, 0}, {1, 2, 0x80, 0x01}, chain.AppendEnvelope(nil, []transform.ID{idB, idA})} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, nil)
		ids, off, perr := parse(body)
		if perr == nil && (off > len(body) || !bytes.Equal(chain.AppendEnvelope(nil, ids), body[:off])) {
			t.Fatalf("the parse accepted envelope %x as %#04x, which re-encodes to other bytes", body[:off], ids)
		}
		if err != nil {
			if !isOneOfFive(err) {
				t.Fatalf("Decode error %v is none of RFC 5 §2.7's five", err)
			}
			return
		}
		if len(out) > chunkMax {
			t.Fatalf("Decode returned %d bytes, past the chunk maximum %d", len(out), chunkMax)
		}
	})
}
