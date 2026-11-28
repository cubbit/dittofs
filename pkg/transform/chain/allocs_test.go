package chain_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// allocRuns is how many times each check runs its call, past AllocsPerRun's
// warm-up.
const allocRuns = 100

// allocsRegistry registers an identity transform per stage, c, e and r, a
// compressor declining every chunk, skip, and an identity compressor
// declaring a data key, k, whose pin so holds material.
func allocsRegistry() chain.Registry {
	r := chain.NewRegistry()
	r.Register(idA, "c", pad{id: idA, stage: transform.StageCompress}.factory())
	r.Register(idB, "e", pad{id: idB, stage: transform.StageEncrypt}.factory())
	r.Register(idC, "r", pad{id: idC, stage: transform.StageRedundancy}.factory())
	r.Register(idD, "skip", decline{id: idD, stage: transform.StageCompress}.factory())
	r.Register(idD+1, "k", pad{id: idD + 1, stage: transform.StageCompress, kinds: []string{keys.KindDataKey}}.factory())
	return r
}

// tally is a recorder keeping only how many calls each of its series was
// given, an atomic add each, so it allocates nothing per call and what a
// check counts is the chain's own.
type tally struct{ calls atomic.Int64 }

func (t *tally) For(string) chain.TransformRecorder { return t }

func (t *tally) Encoded(bool, int, int, time.Duration) { t.calls.Add(1) }
func (t *tally) Decoded(int, int, time.Duration)       { t.calls.Add(1) }
func (t *tally) DecodeFailed(chain.FailureReason)      { t.calls.Add(1) }

// allocsChain builds cfg over allocsRegistry, recording into rec, and pins it
// for ns, under a provider holding a data key.
func allocsChain(t *testing.T, rec *tally, cfg ...string) (transform.Chain, transform.Attempt) {
	t.Helper()
	p := keystest.NewMemory()
	p.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	c := buildInto(t, allocsRegistry(), p, rec, nil, cfg...)
	a, err := c.Pin(context.Background(), ns)
	if err != nil {
		t.Fatalf("Pin(%v): %v", cfg, err)
	}
	return c, a
}

// TestAllocsEncode holds Attempt.Encode, with a caller's dst, to its
// allocations: the []Encoded it returns, one buffer per configured step but
// the last, a declining one's included, since the buffer is made before the
// step answers, and a copy of each applied step's pinned material. Recording
// each step's call, a declined one's included, allocates nothing, so a series
// resolved per call, or a label built for one, fails the count while the
// recorder shows every call reached it (RFC 25 §2.9).
func TestAllocsEncode(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector allocates on its own; unit/transform-allocs runs this without it")
	}
	prof.Watch(t, prof.Options{})
	plain := bytes.Repeat([]byte("chunk"), 200)
	for _, tc := range []struct {
		name     string
		cfg      []string
		want     float64
		recorded int64 // series calls per Encode
	}{
		{"one applied step", []string{"c"}, 1, 1},
		{"two applied steps", []string{"c", "r"}, 2, 2},
		{"three applied steps", []string{"c", "e", "r"}, 3, 3},
		{"a declined step before an applied one", []string{"skip", "r"}, 2, 2},
		{"one step holding material", []string{"k"}, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &tally{}
			_, a := allocsChain(t, rec, tc.cfg...)
			dst := make([]byte, 0, a.MaxEncodedLen(len(plain)))
			before := rec.calls.Load()
			got := testing.AllocsPerRun(allocRuns, func() {
				if _, _, err := a.Encode(context.Background(), dst, hash, plain); err != nil {
					t.Fatalf("Encode: %v", err)
				}
			})
			if got != tc.want {
				t.Errorf("Encode over %v allocates %v times per call, want %v", tc.cfg, got, tc.want)
			}
			if n, want := rec.calls.Load()-before, (allocRuns+1)*tc.recorded; n != want {
				t.Errorf("Encode over %v made %d series calls, want %d", tc.cfg, n, want)
			}
		})
	}
}

// TestAllocsDecode holds Chain.Decode, with a caller's dst, to its
// allocations: the census view its steps share, and one buffer per listed
// step but the first. Recording each step's decode allocates nothing.
func TestAllocsDecode(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector allocates on its own; unit/transform-allocs runs this without it")
	}
	prof.Watch(t, prof.Options{})
	plain := bytes.Repeat([]byte("chunk"), 200)
	for _, tc := range []struct {
		name string
		cfg  []string
		want float64 // also the series calls per Decode, one per step
	}{
		{"one applied step", []string{"c"}, 1},
		{"two applied steps", []string{"c", "r"}, 2},
		{"three applied steps", []string{"c", "e", "r"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &tally{}
			c, a := allocsChain(t, rec, tc.cfg...)
			body, encs, err := a.Encode(context.Background(), nil, hash, plain)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			dst := make([]byte, 0, len(plain))
			before := rec.calls.Load()
			got := testing.AllocsPerRun(allocRuns, func() {
				out, err := c.Decode(context.Background(), ns, dst, hash, body, len(plain), encs)
				if err != nil || !bytes.Equal(out, plain) {
					t.Fatalf("Decode = %d bytes, %v; want the %d plain bytes", len(out), err, len(plain))
				}
			})
			if got != tc.want {
				t.Errorf("Decode over %v allocates %v times per call, want %v", tc.cfg, got, tc.want)
			}
			if n, want := rec.calls.Load()-before, int64(allocRuns+1)*int64(tc.want); n != want {
				t.Errorf("Decode over %v made %d series calls, want %d", tc.cfg, n, want)
			}
		})
	}
}

// TestAllocsEnvelopeParse holds the parse of an envelope listing a transform
// of each stage to no allocation: it reads the body in place and looks each ID
// up in the decode set.
func TestAllocsEnvelopeParse(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector allocates on its own; unit/transform-allocs runs this without it")
	}
	prof.Watch(t, prof.Options{})
	parse, err := allocsRegistry().EnvelopeCounter(context.Background(), keystest.NewMemory())
	if err != nil {
		t.Fatalf("EnvelopeCounter: %v", err)
	}
	body := append(chain.AppendEnvelope(nil, []transform.ID{idA, idB, idC}), "chunk"...)
	got := testing.AllocsPerRun(allocRuns, func() {
		if n, err := parse(body); n != 3 || err != nil {
			t.Fatalf("parse = %d IDs, %v; want 3, nil", n, err)
		}
	})
	if got != 0 {
		t.Errorf("the envelope parse allocates %v times per call, want 0", got)
	}
}
