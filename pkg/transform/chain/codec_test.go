package chain_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/chain"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// chunkMax is the chunk maximum the codec tests pass as Decode's limit.
const chunkMax = 4096

// idD is the declining compressor's ID.
const idD transform.ID = 0x8010

var hash = [32]byte{7}

// codecRegistry registers the transforms the codec tests compose: a
// compressor adding 1000 bytes and one declining every chunk, an encrypter
// writing its data key's ID, and a redundancy step adding half.
func codecRegistry() chain.Registry {
	r := chain.NewRegistry()
	r.Register(idA, "pad1000", pad{id: idA, stage: transform.StageCompress, fixed: 1000}.factory())
	r.Register(idD, "skip", decline{id: idD, stage: transform.StageCompress}.factory())
	r.Register(idB, "seal", keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{keys.KindDataKey}}.factory())
	r.Register(idC, "parity", pad{id: idC, stage: transform.StageRedundancy, half: true}.factory())
	return r
}

// withKey is an in-memory provider whose current data key in ns is d1.
func withKey() *keystest.Memory {
	p := keystest.NewMemory()
	p.Give(ns, keys.KindDataKey, "d1", span(0, 32))
	return p
}

// buildLogged builds cfg over r with provider p and a Logger writing JSON
// lines into the buffer it returns.
func buildLogged(t *testing.T, r chain.Registry, p keys.Provider, cfg ...transform.Config) (transform.Chain, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	d := depsFor()
	d.Logger = slog.New(slog.NewJSONHandler(&buf, nil))
	c, err := r.NewChain(context.Background(), cfg, p, d)
	if err != nil {
		t.Fatalf("NewChain(%v): %v", cfg, err)
	}
	return c, &buf
}

// depsFor is a chain's Deps with a Logger that discards its lines.
func depsFor() chain.Deps {
	return chain.Deps{
		Logger:   slog.New(slog.DiscardHandler),
		Recorder: sink{new([]string)},
		Encrypts: func(entity.NamespaceID) bool { return false },
	}
}

func mustBuild(t *testing.T, r chain.Registry, p keys.Provider, cfg ...transform.Config) transform.Chain {
	t.Helper()
	c, _ := buildLogged(t, r, p, cfg...)
	return c
}

func pinOf(t *testing.T, c transform.Chain) transform.Attempt {
	t.Helper()
	a, err := c.Pin(context.Background(), ns)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	return a
}

// combos is every chain codecRegistry's transforms make: no compressor, one
// adding 1000 bytes or one declining, with or without the encrypter and the
// redundancy step.
func combos() [][]transform.Config {
	var out [][]transform.Config
	accept := map[string]any{"with_encryption": "accept"}
	for _, c := range []string{"", "pad1000", "skip"} {
		for _, e := range []string{"", "seal"} {
			for _, rd := range []string{"", "parity"} {
				var cfg []transform.Config
				for _, n := range []string{c, e, rd} {
					if n != "" {
						cfg = append(cfg, transform.Config{Name: n, Settings: accept})
					}
				}
				out = append(out, cfg)
			}
		}
	}
	return out
}

// inputs runs from 1 byte to chunkMax, and includes inputs that begin with a
// valid envelope, so a chain that sniffs its input instead of trusting the
// envelope it wrote mistakes them for bodies.
func inputs() [][]byte {
	var out [][]byte
	for _, n := range []int{1, 2, 3, 100, chunkMax - 1, chunkMax} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + n)
		}
		out = append(out, b)
	}
	out = append(out,
		chain.AppendEnvelope(nil, []transform.ID{idA}),
		append(chain.AppendEnvelope(nil, []transform.ID{idA, idB, idC}), "plain"...),
		append(chain.AppendEnvelope(nil, nil), "plain"...))
	return out
}

// applied is the IDs cfg's transforms write into the envelope: all but the
// declining compressor.
func applied(r map[string]transform.ID, cfg []transform.Config) []transform.ID {
	var ids []transform.ID
	for _, c := range cfg {
		if c.Name != "skip" {
			ids = append(ids, r[c.Name])
		}
	}
	return ids
}

var codecIDs = map[string]transform.ID{"pad1000": idA, "skip": idD, "seal": idB, "parity": idC}

// TestRoundTrips encodes every input through every combination of the test
// transforms and decodes it back, holding the attempt's bounds and length,
// the body's determinism within an attempt and across two chains of one
// configuration, one Encoded per applied transform, and no log line.
func TestRoundTrips(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := codecRegistry(), withKey()
	d1, err := p.Current(ctx, ns, keys.KindDataKey)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	for _, cfg := range combos() {
		c, logs := buildLogged(t, r, p, cfg...)
		a, a2 := pinOf(t, c), pinOf(t, mustBuild(t, r, p, cfg...))
		for _, in := range inputs() {
			n := len(in)
			body, encs, err := a.Encode(ctx, make([]byte, 0, a.MaxEncodedLen(n)), hash, in)
			if err != nil {
				t.Fatalf("%v: Encode(%d bytes): %v", cfg, n, err)
			}
			if got, bound, dec := len(body), a.MaxEncodedLen(n), c.MaxDecodeLen(n); got > bound || bound > dec {
				t.Errorf("%v, %d bytes: body %d, MaxEncodedLen %d, MaxDecodeLen %d, want each no more than the next", cfg, n, got, bound, dec)
			}
			if l, ok := a.LengthOf(n); ok && l != len(body) {
				t.Errorf("%v, %d bytes: LengthOf = %d, want the body's length %d", cfg, n, l, len(body))
			}
			for what, again := range map[string]transform.Attempt{"the same attempt": a, "another chain of the configuration": a2} {
				if b, _, err := again.Encode(ctx, nil, hash, in); err != nil || !bytes.Equal(b, body) {
					t.Errorf("%v, %d bytes: encoding again through %s gave other bytes (err %v)", cfg, n, what, err)
				}
			}
			want := applied(codecIDs, cfg)
			var got []transform.ID
			for _, e := range encs {
				got = append(got, e.ID)
				wantM := []keys.MaterialID(nil)
				if e.ID == idB {
					wantM = []keys.MaterialID{d1}
				}
				if !e.Applied || e.Version != 1 || !slices.Equal(e.Material, wantM) {
					t.Errorf("%v: Encoded %#04x = applied %v, version %d, material %v, want applied, version 1, material %v", cfg, e.ID, e.Applied, e.Version, e.Material, wantM)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%v: Encoded IDs = %#04x, want one per applied transform %#04x", cfg, got, want)
			}
			if !bytes.Equal(body[:2+2*len(want)], chain.AppendEnvelope(nil, want)) {
				t.Errorf("%v: body begins %x, want the envelope listing %#04x", cfg, body[:min(len(body), 8)], want)
			}
			out, err := c.Decode(ctx, ns, make([]byte, 0, chunkMax), hash, body, chunkMax, encs)
			if err != nil || !bytes.Equal(out, in) {
				t.Errorf("%v: Decode of a %d-byte input's body = %d bytes, %v, want the input", cfg, n, len(out), err)
			}
		}
		if logs.Len() != 0 {
			t.Errorf("%v: the round trips logged %q, want no line", cfg, logs)
		}
	}
}

// TestEnvelopeVectors writes RFC 5 §2.4's envelope over identity transforms,
// byte for byte against vectors from testdata/envelope-v1.py, and parses the
// vectors back to their IDs: a writer and a parser sharing a little-endian ID
// or a count off by one agree with each other, not with the vectors.
//
// ponytail: the envelope vectors are pinned here, from testdata/envelope-v1.py.
// Read them from RFC 5's golden vector file through transformtest.LoadVectors
// once it carries them, so one vector serves every implementation.
func TestEnvelopeVectors(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r := chain.NewRegistry()
	r.Register(idA, "c", pad{id: idA, stage: transform.StageCompress}.factory())
	r.Register(idB, "e", pad{id: idB, stage: transform.StageEncrypt}.factory())
	r.Register(idC, "r", pad{id: idC, stage: transform.StageRedundancy}.factory())
	p := keystest.NewMemory()
	parse, err := r.EnvelopeParser(ctx, p)
	if err != nil {
		t.Fatalf("EnvelopeParser: %v", err)
	}
	payload := []byte("dittofs")
	for _, tc := range []struct {
		name string
		cfg  []string
		ids  []transform.ID
		want string
	}{
		{"no transform", nil, nil, "0100646974746f6673"},
		{"one", []string{"e"}, []transform.ID{idB}, "01018002646974746f6673"},
		{"three", []string{"c", "e", "r"}, []transform.ID{idA, idB, idC}, "0103800180028003646974746f6673"},
	} {
		cfg := names(tc.cfg...)
		if len(cfg) > 1 {
			cfg[0].Settings = map[string]any{"with_encryption": "accept"}
		}
		body, _, err := pinOf(t, mustBuild(t, r, p, cfg...)).Encode(ctx, nil, hash, payload)
		if err != nil {
			t.Fatalf("%s: Encode: %v", tc.name, err)
		}
		if got := hex.EncodeToString(body); got != tc.want {
			t.Errorf("%s: body %s, want the vector %s", tc.name, got, tc.want)
		}
		vec, _ := hex.DecodeString(tc.want)
		ids, off, err := parse(vec)
		if err != nil || !slices.Equal(ids, tc.ids) || off != len(vec)-len(payload) {
			t.Errorf("%s: parse of the vector = %#04x at %d, %v, want %#04x at %d", tc.name, ids, off, err, tc.ids, len(vec)-len(payload))
		}
		if got, want := len(body)-len(payload), 2+2*len(tc.ids); got != want {
			t.Errorf("%s: envelope of %d IDs is %d bytes, want 2 + 2n = %d", tc.name, len(tc.ids), got, want)
		}
	}
	// Two IDs, which no vector holds, cost 2 + 2n too.
	body, _, err := pinOf(t, mustBuild(t, r, p, names("c", "r")...)).Encode(ctx, nil, hash, payload)
	if err != nil || len(body)-len(payload) != 2+2*2 {
		t.Errorf("an envelope of 2 IDs is %d bytes (err %v), want 6", len(body)-len(payload), err)
	}
}

// TestEnvelopeRefusals holds that Decode refuses each malformed envelope with
// ErrMalformed before any transform decodes: every transform is watched, and
// none is called until a well-formed body is decoded.
func TestEnvelopeRefusals(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r := chain.NewRegistry()
	var ws []watched
	for _, d := range []struct {
		id    transform.ID
		name  string
		stage transform.Stage
	}{{idA, "c", transform.StageCompress}, {idB, "e", transform.StageEncrypt}, {idC, "r", transform.StageRedundancy}, {idC + 1, "c2", transform.StageCompress}} {
		w := newWatched(pad{id: d.id, stage: d.stage})
		ws = append(ws, w)
		r.Register(d.id, d.name, w.factory())
	}
	calls := func() (n int64) {
		for _, w := range ws {
			n += w.calls.Load()
		}
		return n
	}
	c := mustBuild(t, r, keystest.NewMemory())
	env := func(ids ...transform.ID) []byte { return append(chain.AppendEnvelope(nil, ids), 'x') }
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"an empty body", nil},
		{"one byte", []byte{1}},
		{"version 0", []byte{0, 0, 'x'}},
		{"version 2", []byte{2, 0, 'x'}},
		{"an unregistered ID", env(0x8fff)},
		{"a repeated ID", env(idA, idA)},
		{"two IDs of one stage", env(idA, idC+1)},
		{"IDs out of stage order", env(idB, idA)},
		{"a count past the body's end", []byte{1, 2, 0x80, 0x01}},
		{"a count one byte past the body's end", []byte{1, 1, 0x80}},
	} {
		if out, err := c.Decode(ctx, ns, nil, hash, tc.body, chunkMax, nil); !errors.Is(err, transform.ErrMalformed) || out != nil {
			t.Errorf("Decode(%s) = %q, %v, want ErrMalformed", tc.name, out, err)
		}
	}
	if n := calls(); n != 0 {
		t.Errorf("the refused envelopes reached %d transform calls, want 0", n)
	}
	if out, err := c.Decode(ctx, ns, nil, hash, env(idA, idB, idC), chunkMax, nil); err != nil || string(out) != "x" || calls() != 3 {
		t.Errorf("Decode of a well-formed body = %q, %v after %d calls, want \"x\" through 3 calls", out, err, calls())
	}
}

// TestDecodeGivesEachStepItsBound decodes a chunk of exactly the maximum
// through a step adding half, then an identity: the identity's output is half
// as large again as the chunk, so a chain giving it the chunk maximum refuses
// a body it wrote itself (RFC 5 §3.1).
func TestDecodeGivesEachStepItsBound(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r := chain.NewRegistry()
	r.Register(idA, "half", pad{id: idA, stage: transform.StageCompress, half: true}.factory())
	r.Register(idC, "same", pad{id: idC, stage: transform.StageRedundancy}.factory())
	c := mustBuild(t, r, keystest.NewMemory(), names("half", "same")...)
	in := bytes.Repeat([]byte{1}, chunkMax)
	body, _, err := pinOf(t, c).Encode(ctx, nil, hash, in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, nil); err != nil || !bytes.Equal(out, in) {
		t.Errorf("Decode of a chunk of exactly the maximum = %d bytes, %v, want the chunk", len(out), err)
	}
}

// TestDecodeRefusesAnOutputPastItsBound holds that a step returning more than
// the bound the chain gave it, and a body of no transform longer than the
// chunk maximum, are ErrTooLarge.
func TestDecodeRefusesAnOutputPastItsBound(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r := chain.NewRegistry()
	r.Register(idC, "over", fault{Transform: pad{id: idC, stage: transform.StageRedundancy}, onDecode: func(_ context.Context, limit int) ([]byte, error) {
		return make([]byte, limit+1), nil
	}}.factory())
	c := mustBuild(t, r, keystest.NewMemory(), names("over")...)
	body, _, err := pinOf(t, c).Encode(ctx, nil, hash, []byte("chunk"))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, nil); !errors.Is(err, transform.ErrTooLarge) {
		t.Errorf("Decode through a step writing past its bound = %d bytes, %v, want ErrTooLarge", len(out), err)
	}
	bare := append(chain.AppendEnvelope(nil, nil), make([]byte, chunkMax+1)...)
	if out, err := c.Decode(ctx, ns, nil, hash, bare, chunkMax, nil); !errors.Is(err, transform.ErrTooLarge) {
		t.Errorf("Decode of a body of no transform past the maximum = %d bytes, %v, want ErrTooLarge", len(out), err)
	}
}

// TestBodiesOutliveTheChain writes under one chain and reads on chains
// rebuilt in the same process without its transforms, as a restart rebuilds
// them: one with another transform added, and the empty chain. Decode looks
// transforms up in the registry and bounds reads by it, never by the
// configured chain (RFC 5 §2.5, §3.1).
func TestBodiesOutliveTheChain(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := codecRegistry(), withKey()
	a := pinOf(t, mustBuild(t, r, p, names("pad1000", "parity")...))
	in := bytes.Repeat([]byte("chunk"), 100)
	body, encs, err := a.Encode(ctx, nil, hash, in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for what, cfg := range map[string][]transform.Config{"the encrypter alone": names("seal"), "the empty chain": nil} {
		b := mustBuild(t, r, p, cfg...)
		if got := b.MaxDecodeLen(len(in)); len(body) > got {
			t.Errorf("%s: body of %d bytes is past its MaxDecodeLen %d", what, len(body), got)
		}
		if out, err := b.Decode(ctx, ns, nil, hash, body, chunkMax, encs); err != nil || !bytes.Equal(out, in) {
			t.Errorf("%s: Decode of a body its transforms wrote = %d bytes, %v, want the input", what, len(out), err)
		}
	}
}

// TestEncodeUsesThePinnedKeyAfterARotation rotates the data key after the
// pin: the attempt's body and census still name the key it pinned.
func TestEncodeUsesThePinnedKeyAfterARotation(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := codecRegistry(), withKey()
	a := pinOf(t, mustBuild(t, r, p, names("seal")...))
	p.Give(ns, keys.KindDataKey, "d2", span(32, 64))
	body, encs, err := a.Encode(ctx, nil, hash, []byte("chunk"))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(encs) != 1 || len(encs[0].Material) != 1 || encs[0].Material[0].ID != "d1" || !bytes.Contains(body, []byte("\x02d1chunk")) {
		t.Errorf("after a rotation the attempt wrote %q with census %+v, want the pinned key d1", body, encs)
	}
}

// TestDeclinedTransformIsAbsent holds that a declined chunk leaves its
// transform out of the envelope and the census, and the body decodes.
func TestDeclinedTransformIsAbsent(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := codecRegistry(), withKey()
	cfg := names("skip", "seal")
	cfg[0].Settings = map[string]any{"with_encryption": "accept"}
	c := mustBuild(t, r, p, cfg...)
	body, encs, err := pinOf(t, c).Encode(ctx, nil, hash, []byte("chunk"))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := chain.AppendEnvelope(nil, []transform.ID{idB}); !bytes.HasPrefix(body, want) || len(encs) != 1 || encs[0].ID != idB {
		t.Errorf("body %x with %d Encoded, want the envelope %x and one Encoded of the encrypter", body, len(encs), want)
	}
	if out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, encs); err != nil || string(out) != "chunk" {
		t.Errorf("Decode = %q, %v, want the chunk", out, err)
	}
}

// fiveErrors are RFC 5 §2.7's decode errors.
var fiveErrors = []error{transform.ErrMalformed, transform.ErrTooLarge, keys.ErrMaterialUnavailable, keys.ErrUnknownMaterial, keys.ErrMaterialDestroyed}

func isOneOfFive(err error) bool {
	return slices.ContainsFunc(fiveErrors, func(s error) bool { return errors.Is(err, s) })
}

// TestDecodeErrorsAreTheChainsOwn holds that whatever a step's Decode
// returns, Decode returns one of the five errors and no transform's own error
// type is reachable through errors.As (RFC 5 §2.7): a foreign error is
// ErrMalformed, a sentinel it wraps survives, unknown material is also
// unavailable, and any error once the call's context has ended is
// ErrMaterialUnavailable, never ErrMalformed.
func TestDecodeErrorsAreTheChainsOwn(t *testing.T) {
	prof.Watch(t, prof.Options{})
	for _, tc := range []struct {
		name    string
		err     error
		cancel  bool
		want    []error
		notWant error
	}{
		{"a foreign error", &oddError{}, false, []error{transform.ErrMalformed}, nil},
		{"a foreign error wrapping ErrMalformed", &oddError{transform.ErrMalformed}, false, []error{transform.ErrMalformed}, nil},
		{"a foreign error wrapping ErrTooLarge", &oddError{transform.ErrTooLarge}, false, []error{transform.ErrTooLarge}, transform.ErrMalformed},
		{"a foreign error wrapping destroyed material", &oddError{keys.ErrMaterialDestroyed}, false, []error{keys.ErrMaterialDestroyed}, transform.ErrMalformed},
		{"a foreign error wrapping unavailable material", &oddError{keys.ErrMaterialUnavailable}, false, []error{keys.ErrMaterialUnavailable}, transform.ErrMalformed},
		{"a foreign error wrapping unknown material", &oddError{keys.ErrUnknownMaterial}, false, []error{keys.ErrMaterialUnavailable, keys.ErrUnknownMaterial}, transform.ErrMalformed},
		{"the context's error once cancelled", nil, true, []error{keys.ErrMaterialUnavailable, context.Canceled}, transform.ErrMalformed},
		{"ErrMalformed once cancelled", transform.ErrMalformed, true, []error{keys.ErrMaterialUnavailable, context.Canceled}, transform.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chain.NewRegistry()
			r.Register(idC, "f", fault{Transform: pad{id: idC, stage: transform.StageRedundancy}, onDecode: func(ctx context.Context, _ int) ([]byte, error) {
				if tc.err == nil {
					return nil, ctx.Err()
				}
				return nil, tc.err
			}}.factory())
			c := mustBuild(t, r, keystest.NewMemory(), names("f")...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body, _, err := pinOf(t, c).Encode(ctx, nil, hash, []byte("chunk"))
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if tc.cancel {
				cancel()
			}
			_, err = c.Decode(ctx, ns, nil, hash, body, chunkMax, nil)
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("Decode error = %v, want it to be %v", err, w)
				}
			}
			if tc.notWant != nil && errors.Is(err, tc.notWant) {
				t.Errorf("Decode error = %v, want it not to be %v", err, tc.notWant)
			}
			var odd *oddError
			if errors.As(err, &odd) {
				t.Errorf("Decode error %v lets the transform's own error type through errors.As", err)
			}
			if !isOneOfFive(err) {
				t.Errorf("Decode error %v is none of RFC 5 §2.7's five", err)
			}
		})
	}
}

// TestEncodeTakesNothingTheTransformMisreports holds that the chain fills each
// Encoded's ID and material from the pin, whatever the transform reports, and
// fails the put when a transform reports another format version than the one
// the chain ID names, or writes past its own bound.
func TestEncodeTakesNothingTheTransformMisreports(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	base := keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{keys.KindDataKey}}
	encodeWith := func(rewrite func(transform.Encoded) transform.Encoded) ([]byte, []transform.Encoded, error) {
		r := chain.NewRegistry()
		r.Register(idB, "f", fault{Transform: base, onEncode: rewrite}.factory())
		p := withKey()
		return pinOf(t, mustBuild(t, r, p, names("f")...)).Encode(ctx, nil, hash, []byte("chunk"))
	}
	_, encs, err := encodeWith(func(e transform.Encoded) transform.Encoded {
		e.ID, e.Material = 0x9999, []keys.MaterialID{{Namespace: ns, Kind: keys.KindDataKey, ID: "planted"}}
		return e
	})
	if err != nil || len(encs) != 1 || encs[0].ID != idB || len(encs[0].Material) != 1 || encs[0].Material[0].ID != "d1" {
		t.Errorf("a transform misreporting its ID and material gave %+v, %v, want ID %#04x and the pinned d1", encs, err, idB)
	}
	for what, rewrite := range map[string]func(transform.Encoded) transform.Encoded{
		"another version":       func(e transform.Encoded) transform.Encoded { e.Version = 2; return e },
		"a body past its bound": func(e transform.Encoded) transform.Encoded { e.Body = append(e.Body, 0); return e },
	} {
		if body, encs, err := encodeWith(rewrite); err == nil || body != nil || encs != nil {
			t.Errorf("a transform reporting %s: Encode = %d bytes, %d Encoded, %v, want the put failed", what, len(body), len(encs), err)
		}
	}
}

// TestStepsGetBuffersOfTheirOwn holds that no step's dst shares memory with
// its input, on encode or decode, through the full chain.
func TestStepsGetBuffersOfTheirOwn(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	ws := []watched{
		newWatched(pad{id: idA, stage: transform.StageCompress, fixed: 1000}),
		newWatched(keyed{id: idB, stage: transform.StageEncrypt, version: 1, kinds: []string{keys.KindDataKey}}),
		newWatched(pad{id: idC, stage: transform.StageRedundancy, half: true}),
	}
	r := chain.NewRegistry()
	for i, name := range []string{"c", "e", "r"} {
		r.Register(ws[i].ID(), name, ws[i].factory())
	}
	cfg := names("c", "e", "r")
	cfg[0].Settings = map[string]any{"with_encryption": "accept"}
	c := mustBuild(t, r, withKey(), cfg...)
	a := pinOf(t, c)
	for _, in := range inputs() {
		body, encs, err := a.Encode(ctx, make([]byte, 0, a.MaxEncodedLen(len(in))), hash, in)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if _, err := c.Decode(ctx, ns, make([]byte, 0, chunkMax), hash, body, chunkMax, encs); err != nil {
			t.Fatalf("Decode: %v", err)
		}
	}
	for i, w := range ws {
		if calls, over := w.calls.Load(), w.overlaps.Load(); calls != int64(2*len(inputs())) || over != 0 {
			t.Errorf("step %d: %d of %d calls had a dst sharing memory with the input, want 0 of %d", i, over, calls, 2*len(inputs()))
		}
	}
}

// TestConcurrentEncodeAndDecode runs 64 goroutines encoding and decoding
// through one chain and one attempt, under the race detector, against the
// sequential results.
func TestConcurrentEncodeAndDecode(t *testing.T) {
	prof.Watch(t, prof.Options{})
	ctx := context.Background()
	r, p := codecRegistry(), withKey()
	cfg := names("pad1000", "seal", "parity")
	cfg[0].Settings = map[string]any{"with_encryption": "accept"}
	c := mustBuild(t, r, p, cfg...)
	a := pinOf(t, c)
	ins := inputs()
	want := make([][]byte, len(ins))
	for i, in := range ins {
		b, _, err := a.Encode(ctx, nil, hash, in)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		want[i] = b
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64*len(ins))
	for g := range 64 {
		wg.Go(func() {
			for k := range ins {
				i := (k + g) % len(ins)
				body, encs, err := a.Encode(ctx, nil, hash, ins[i])
				if err != nil || !bytes.Equal(body, want[i]) {
					errs <- "an Encode differs from the sequential body"
					continue
				}
				if out, err := c.Decode(ctx, ns, nil, hash, body, chunkMax, encs); err != nil || !bytes.Equal(out, ins[i]) {
					errs <- "a Decode differs from the input"
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
