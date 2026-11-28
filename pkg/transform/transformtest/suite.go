// Package transformtest holds every transform to RFC 5 §2.1, §2.7, §3.1 and
// §4: Run, the transform conformance suite each transform's package calls
// with a Subject (RFC 5 §8.1).
package transformtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// Subject is a transform under the suite: what only the transform's own
// package knows about it.
type Subject struct {
	Factory         transform.Factory
	Settings        map[string]any   // the settings the rows run under
	LengthAffecting []map[string]any // settings that change body length; empty if none

	// Max is the chunk maximum (RFC 2 §3.2) the rows run under. The suite's
	// cost grows with it, so a transform whose format has no threshold near
	// the store's maximum may pass a smaller one.
	Max int

	WorstCase     func(n int) []byte // an n-byte input whose body reaches MaxEncodedLen under the pinned material
	Scratch       int                // Decode's working memory beyond dst, which the fuzz helper's allocation bound allows
	Authenticates bool               // adds the authenticating rows outside the encrypt stage

	// fixtures is the directory the fixture rows read in place of
	// testdata/fixtures: set only by this package's own tests, whose two
	// test transforms would otherwise read each other's fixtures.
	fixtures string
}

// Run holds the transform s builds to RFC 5 §2.1, §2.7, §3.1 and §4, one
// subtest per row, each over a fresh keystest.Memory holding the material of
// the transform's kinds, and to the fixtures under testdata/fixtures in the
// calling package's directory. The authenticating rows run for a transform of the
// encrypt stage, and for any other whose Subject sets Authenticates.
//
// decision: Run is called from each transform's own tests, not from the
// registry, because only the transform knows its worst-case input, its
// length-affecting settings and its fixtures; RFC 5 §4 item 7 makes the call
// part of registering a transform, which meets §8.1's "a new transform gets it
// by registering". A registry-wide check belongs to the chain's tests if a
// registered transform is ever found without the call.
func Run(t *testing.T, s Subject) {
	t.Helper()

	if err := s.check(); err != nil {
		t.Fatal(err)
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ran, err := r.run(t.Context(), s)
			if !ran {
				t.Skip("the transform does not authenticate")
			}

			if err != nil {
				t.Error(err)
			}
		})
	}
}

// check refuses a Subject no row can run on.
func (s Subject) check() error {
	switch {
	case s.Factory == nil:
		return errors.New("transformtest: Subject.Factory is nil")
	case s.Max < 2:
		return fmt.Errorf("transformtest: Subject.Max = %d, want at least 2", s.Max)
	}

	return nil
}

// row is one check of the suite. It returns an error naming what it found
// wrong, or nil.
type row struct {
	name  string
	auth  bool // runs only for a transform that authenticates
	check func(ctx context.Context, e *env) error
}

// run runs r on a fresh env of s, reporting whether it ran at all.
func (r row) run(ctx context.Context, s Subject) (bool, error) {
	e, err := newEnv(ctx, s)
	if err != nil {
		return true, err
	}

	if r.auth && !e.authenticates() {
		return false, nil
	}

	return true, r.check(ctx, e)
}

// rows is the suite, in the order Run runs it.
var rows = []row{
	{"roundtrip", false, roundTrip},
	{"buffers", false, buffers},
	{"bound", false, bound},
	{"monotone", false, monotone},
	{"concurrent", false, concurrent},
	{"flips", false, flips},
	{"deterministic", false, deterministic},
	{"too-large", false, tooLarge},
	{"length-key", false, lengthKey},
	{"no-current", false, noCurrent},
	{"census-only", false, censusOnly},
	{"rotated", false, rotated},
	{"fixture-decode", false, fixtureDecode},
	{"fixture-encode", false, fixtureEncode},
	{"fixture-current", false, fixtureCurrent},
	{"authenticated-flips", true, authenticatedFlips},
	{"hash", true, otherHash},
	{"destroyed", true, destroyed},
}

// suiteNS is the namespace the rows read and write in.
var suiteNS = entity.NamespaceID{15: 0x7e}

// material is the content the suite gives one ID: fixed, so every row's
// Memory holds the same, and distinct per ID.
func material(id string) []byte {
	b := sha256.Sum256([]byte(id))
	return b[:]
}

// env is one row's subject: a fresh Memory holding the material of the
// transform's kinds, the encoder built with the Subject's settings, the
// decoder built with defaults as the chain's decode set builds it (RFC 5
// §2.5), and the material a pin of the namespace reads.
type env struct {
	s    Subject
	ns   entity.NamespaceID
	mem  *keystest.Memory
	encs *counted // the provider every encoder's Factory is given
	decs *counted // the provider the decoder's Factory is given
	enc  transform.Transform
	dec  transform.Transform
	pin  []keys.MaterialID
}

func newEnv(ctx context.Context, s Subject) (*env, error) {
	e := &env{s: s, ns: suiteNS, mem: keystest.NewMemory()}
	e.encs = &counted{Provider: e.mem}
	e.decs = &counted{Provider: e.mem}

	var err error
	if e.enc, err = e.build(ctx, s.Settings); err != nil {
		return nil, err
	}

	if e.dec, err = s.Factory(ctx, map[string]any{}, e.decs); err != nil {
		return nil, fmt.Errorf("transformtest: Factory with default settings: %w", err)
	}

	// A transform with no kinds is pinned to no material, which is not nil:
	// a nil m asks MaxEncodedLen for the bound under any material and
	// setting.
	if e.pin, err = e.give(ctx, ""); err != nil {
		return nil, err
	}

	return e, nil
}

// give gives Memory one ID of each of the encoder's kinds, suffixed, and
// returns the material a pin then reads as current.
func (e *env) give(ctx context.Context, suffix string) ([]keys.MaterialID, error) {
	pin := []keys.MaterialID{}

	for _, kind := range e.enc.Kinds() {
		id := "transformtest-" + kind + suffix
		e.mem.Give(e.ns, kind, id, material(id))

		cur, err := e.mem.Current(ctx, e.ns, kind)
		if err != nil {
			return nil, fmt.Errorf("transformtest: the pin's Current of %s: %w", kind, err)
		}

		pin = append(pin, cur)
	}

	return pin, nil
}

// build calls the Factory with settings over the encoders' provider.
func (e *env) build(ctx context.Context, settings map[string]any) (transform.Transform, error) {
	tr, err := e.s.Factory(ctx, settings, e.encs)
	if err != nil {
		return nil, fmt.Errorf("transformtest: Factory(%v): %w", settings, err)
	}

	return tr, nil
}

// authenticates reports whether the authenticating rows run.
func (e *env) authenticates() bool {
	return e.s.Authenticates || e.enc.Stage() == transform.StageEncrypt
}

// counted is a provider a Factory is given: Memory, with each call counted,
// since Encode gets its material through Get alone and Decode through the
// provider it is handed alone (RFC 5 §3.1, §2.5).
type counted struct {
	keys.Provider

	currents, gets atomic.Uint64
}

func (p *counted) Current(ctx context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	p.currents.Add(1)
	return p.Provider.Current(ctx, ns, kind)
}

func (p *counted) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	p.gets.Add(1)
	return p.Provider.Get(ctx, id)
}

// census is the provider a Decode is handed, as the chain hands it (RFC 5
// §2.5, §3.1): it answers Get only for material its block's census lists, of
// the namespace read, handing the provider the census's entry, since a body
// names its material by ID alone; every other ID, and Current, is
// ErrMalformed without asking the provider. A provider error that is none of
// keys' three material errors is ErrMaterialUnavailable, as the chain's view
// reports it.
type census struct {
	p      keys.Provider
	ns     entity.NamespaceID
	listed []keys.MaterialID
}

func (c census) Current(_ context.Context, ns entity.NamespaceID, kind string) (keys.MaterialID, error) {
	return keys.MaterialID{}, fmt.Errorf("transformtest: a decode asked for the current %s of namespace %s: %w", kind, ns, transform.ErrMalformed)
}

func (c census) Get(ctx context.Context, id keys.MaterialID) ([]byte, error) {
	if id.Namespace == c.ns {
		for _, l := range c.listed {
			if l.Namespace == c.ns && l.Kind == id.Kind && l.ID == id.ID {
				b, err := c.p.Get(ctx, l)
				if err != nil && !errors.Is(err, keys.ErrUnknownMaterial) && !errors.Is(err, keys.ErrMaterialUnavailable) && !errors.Is(err, keys.ErrMaterialDestroyed) {
					return nil, fmt.Errorf("transformtest: reading %s %q of namespace %s: %s: %w", l.Kind, l.ID, l.Namespace, err, keys.ErrMaterialUnavailable)
				}
				return b, err
			}
		}
	}

	return nil, fmt.Errorf("transformtest: %s %q of namespace %s is not in the census: %w", id.Kind, id.ID, id.Namespace, transform.ErrMalformed)
}

// encode calls tr.Encode under the pin, turning a panic into an error.
func (e *env) encode(ctx context.Context, tr transform.Transform, dst []byte, in input) (transform.Encoded, error) {
	return e.encodeUnder(ctx, tr, e.pin, dst, in)
}

func (e *env) encodeUnder(ctx context.Context, tr transform.Transform, pin []keys.MaterialID, dst []byte, in input) (enc transform.Encoded, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Encode panicked: %v", r)
		}
	}()

	return tr.Encode(ctx, e.ns, pin, dst, in.hash, in.plain)
}

// decode calls the decoder's Decode of enc through enc's census, under hash
// and with the Subject's max, turning a panic into an error. A declined
// chunk is plain passed on unchanged, so it decodes to plain without the
// transform.
func (e *env) decode(ctx context.Context, dst []byte, hash [32]byte, plain []byte, enc transform.Encoded) (out []byte, err error) {
	if !enc.Applied {
		return plain, nil
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Decode panicked: %v", r)
		}
	}()

	return e.dec.Decode(ctx, e.ns, dst, hash, enc.Body, e.s.Max, census{e.mem, e.ns, enc.Material})
}

// trip encodes in into a nil dst and decodes the body back into a nil dst.
func (e *env) trip(ctx context.Context, in input) (transform.Encoded, []byte, error) {
	enc, err := e.encode(ctx, e.enc, nil, in.clone())
	if err != nil {
		return enc, nil, fmt.Errorf("%s: Encode: %w", in, err)
	}

	out, err := e.decode(ctx, nil, in.hash, in.plain, enc)
	if err != nil {
		return enc, nil, fmt.Errorf("%s: Decode: %w", in, err)
	}

	return enc, out, nil
}

// stored is the length a chunk takes after the transform: its body, or n
// when declined, since plain then passes on unchanged.
func stored(n int, enc transform.Encoded) int {
	if !enc.Applied {
		return n
	}

	return len(enc.Body)
}

// isDecodeError reports whether err is one of RFC 5 §2.7's five.
func isDecodeError(err error) bool {
	for _, want := range []error{transform.ErrMalformed, transform.ErrTooLarge, keys.ErrUnknownMaterial, keys.ErrMaterialUnavailable, keys.ErrMaterialDestroyed} {
		if errors.Is(err, want) {
			return true
		}
	}

	return false
}

// firstDiff is the first index at which a and b differ.
func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	return min(len(a), len(b))
}

// Decode(Encode(p)) is p, byte for byte, for every round-trip input,
// decoded by a transform built with default settings through the census
// (RFC 5 §2.1, §7 T2), and an applied body records the transform's format
// version and exactly the pinned material (RFC 5 §3.1). Catches a lossy
// transform, one mistaking an input that begins with its own output or an
// envelope for that output, and one whose census entry misnames its body.
func roundTrip(ctx context.Context, e *env) error {
	return e.each(ctx, func(in input) error {
		enc, out, err := e.trip(ctx, in)
		if err != nil {
			return err
		}

		if !bytes.Equal(out, in.plain) {
			return fmt.Errorf("%s: Decode(Encode(p)) differs from p at byte %d", in, firstDiff(out, in.plain))
		}

		if !enc.Applied {
			return nil
		}

		if enc.Version != e.enc.Version() {
			return fmt.Errorf("%s: Encoded.Version = %d, Version() = %d", in, enc.Version, e.enc.Version())
		}

		if !slices.Equal(enc.Material, e.pin) {
			return fmt.Errorf("%s: Encoded.Material = %+v, want the pinned %+v", in, enc.Material, e.pin)
		}

		return nil
	})
}

// junk is an empty dst whose spare capacity of n holds stale bytes.
func junk(n int) []byte {
	return bytes.Repeat([]byte{0x5a}, n)[:0]
}

// scramble turns every byte of b over.
func scramble(b []byte) {
	for i := range b {
		b[i] ^= 0xff
	}
}

// Neither call writes into its input nor returns memory its input owns, and
// a dst of spare capacity holding junk gives what a nil one gives (RFC 5
// §3.1). Catches a transform that writes into its input, a body or an output
// aliasing the input, and a decoder reading stale dst bytes.
func buffers(ctx context.Context, e *env) error {
	return e.each(ctx, func(in input) error {
		mine := in.clone()

		want, err := e.encode(ctx, e.enc, nil, mine)
		if err != nil {
			return fmt.Errorf("%s: Encode: %w", in, err)
		}

		if !bytes.Equal(mine.plain, in.plain) {
			return fmt.Errorf("%s: Encode wrote into its input at byte %d", in, firstDiff(mine.plain, in.plain))
		}

		if !want.Applied {
			return nil
		}

		body := slices.Clone(want.Body)
		if scramble(mine.plain); !bytes.Equal(want.Body, body) {
			return fmt.Errorf("%s: the body Encode returned shares memory with its input", in)
		}

		got := want
		got.Body = body

		wantOut, err := e.decode(ctx, nil, in.hash, in.plain, got)
		if err != nil {
			return fmt.Errorf("%s: Decode: %w", in, err)
		}

		if !bytes.Equal(body, want.Body) {
			return fmt.Errorf("%s: Decode wrote into its body at byte %d", in, firstDiff(body, want.Body))
		}

		kept := slices.Clone(wantOut)
		if scramble(body); !bytes.Equal(wantOut, kept) {
			return fmt.Errorf("%s: the bytes Decode returned share memory with its body", in)
		}

		got.Body = slices.Clone(want.Body)

		out, err := e.decode(ctx, junk(len(in.plain)), in.hash, in.plain, got)
		if err != nil {
			return fmt.Errorf("%s: Decode into junk: %w", in, err)
		}

		if !bytes.Equal(out, kept) {
			return fmt.Errorf("%s: Decode into a junk dst differs from one into nil at byte %d", in, firstDiff(out, kept))
		}

		enc, err := e.encode(ctx, e.enc, junk(e.enc.MaxEncodedLen(len(in.plain), e.pin)), in.clone())
		if err != nil {
			return fmt.Errorf("%s: Encode into junk: %w", in, err)
		}

		if !enc.Applied || !bytes.Equal(enc.Body, want.Body) {
			return fmt.Errorf("%s: Encode into a junk dst gave another body than into a nil one", in)
		}

		return nil
	})
}

// worstSizes are the input sizes the worst case is built at.
func (e *env) worstSizes() []int {
	return slices.Compact([]int{1, 2, e.s.Max / 2, e.s.Max - 1, e.s.Max})
}

// Every body is no longer than MaxEncodedLen under the pinned material, nor
// than the bound under any material of the encoder and of the decoder the
// chain sizes reads with, under the Subject's settings and every
// length-affecting one; the worst case reaches the first. A declined chunk
// is n long and reaches a bound of n (RFC 5 §2.1, §3.1, §4 item 2). Catches
// a bound one byte short, a bound one byte loose, and a bound under any
// material below some setting's bodies.
func bound(ctx context.Context, e *env) error {
	check := func(tr transform.Transform, settings map[string]any, in input) (transform.Encoded, error) {
		enc, err := e.encode(ctx, tr, nil, in.clone())
		if err != nil {
			return enc, fmt.Errorf("%s under %v: Encode: %w", in, settings, err)
		}

		n, got := len(in.plain), stored(len(in.plain), enc)
		for _, b := range []struct {
			name  string
			limit int
		}{
			{"MaxEncodedLen(%d, pinned)", tr.MaxEncodedLen(n, e.pin)},
			{"MaxEncodedLen(%d, nil)", tr.MaxEncodedLen(n, nil)},
			{"the default decoder's MaxEncodedLen(%d, nil)", e.dec.MaxEncodedLen(n, nil)},
		} {
			if got > b.limit {
				return enc, fmt.Errorf("%s under %v: body of %d bytes, over "+b.name+" = %d", in, settings, got, n, b.limit)
			}
		}

		return enc, nil
	}

	if err := e.each(ctx, func(in input) error { _, err := check(e.enc, e.s.Settings, in); return err }); err != nil {
		return err
	}

	for _, s := range e.s.LengthAffecting {
		tr, err := e.build(ctx, s)
		if err != nil {
			return err
		}

		if err := e.spread(ctx, func(in input) error { _, err := check(tr, s, in); return err }); err != nil {
			return err
		}
	}

	if e.s.WorstCase == nil {
		return errors.New("Subject.WorstCase is nil, so no input reaches the bound")
	}

	for _, n := range e.worstSizes() {
		w := e.s.WorstCase(n)
		if len(w) != n {
			return fmt.Errorf("WorstCase(%d) is %d bytes long", n, len(w))
		}

		enc, err := check(e.enc, e.s.Settings, newInput(-1, w))
		if err != nil {
			return err
		}

		if got, want := stored(n, enc), e.enc.MaxEncodedLen(n, e.pin); got != want {
			return fmt.Errorf("the worst case of %d bytes stored %d bytes, short of MaxEncodedLen(%d, pinned) = %d", n, got, n, want)
		}
	}

	return nil
}

// denseBound is how far MaxEncodedLen is checked at every n.
const denseBound = 4096

// MaxEncodedLen never decreases as n grows, under the pinned material and
// under nil, of the encoder and of the default decoder, at every n up to
// denseBound and at every round-trip size beyond it (RFC 5 §3.1), so a bound
// at the chunk maximum bounds every smaller chunk. Catches a bound that
// shrinks somewhere.
func monotone(_ context.Context, e *env) error {
	sizes := make([]int, 0, denseBound+inputCount)
	for n := range min(denseBound, e.s.Max) + 1 {
		sizes = append(sizes, n)
	}

	for i := range inputCount {
		sizes = append(sizes, e.s.size(i))
	}

	slices.Sort(sizes)
	sizes = slices.Compact(sizes)

	for _, m := range []struct {
		name string
		tr   transform.Transform
		m    []keys.MaterialID
	}{{"pinned", e.enc, e.pin}, {"nil", e.enc, nil}, {"nil, by the default decoder", e.dec, nil}} {
		for i := 1; i < len(sizes); i++ {
			a, b := sizes[i-1], sizes[i]
			if la, lb := m.tr.MaxEncodedLen(a, m.m), m.tr.MaxEncodedLen(b, m.m); lb < la {
				return fmt.Errorf("MaxEncodedLen(%d, %s) = %d, below MaxEncodedLen(%d, %s) = %d", b, m.name, lb, a, m.name, la)
			}
		}
	}

	return nil
}

// overlap marks the calls the concurrent row makes, so a subject can tell
// them from the sequential ones.
type overlap struct{}

// goroutines and perGoroutine size the concurrent row.
const (
	goroutines   = 64
	perGoroutine = 8
)

// Encode and Decode from 64 goroutines at once give what sequential calls
// gave (RFC 5 §2.1, stateless per call); the race detector checks the rest.
// Catches state shared between calls.
func concurrent(ctx context.Context, e *env) error {
	ins, err := e.small(ctx, goroutines*perGoroutine)
	if err != nil {
		return err
	}

	wantEnc := make([]transform.Encoded, len(ins))
	wantOut := make([][]byte, len(ins))

	for i, in := range ins {
		if wantEnc[i], wantOut[i], err = e.trip(ctx, in); err != nil {
			return err
		}
	}

	ctx = context.WithValue(ctx, overlap{}, true)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := g; i < len(ins); i += goroutines {
				in := ins[i]

				enc, err := e.encode(ctx, e.enc, nil, in.clone())
				if err != nil {
					errs[g] = fmt.Errorf("%s: concurrent Encode: %w", in, err)
					return
				}

				if enc.Applied != wantEnc[i].Applied || !bytes.Equal(enc.Body, wantEnc[i].Body) {
					errs[g] = fmt.Errorf("%s: a concurrent Encode gave another body than a sequential one", in)
					return
				}

				out, err := e.decode(ctx, nil, in.hash, in.plain, wantEnc[i])
				if err != nil {
					errs[g] = fmt.Errorf("%s: concurrent Decode: %w", in, err)
					return
				}

				if !bytes.Equal(out, wantOut[i]) {
					errs[g] = fmt.Errorf("%s: a concurrent Decode gave other bytes than a sequential one", in)
					return
				}
			}
		})
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// flipped calls fn with every byte of every applied flip body turned over in
// turn: the input the body came from, the byte's index, and what Decode
// returned.
func (e *env) flipped(ctx context.Context, fn func(in input, i int, out []byte, err error) error) error {
	ins, err := e.small(ctx, flipCount)
	if err != nil {
		return err
	}

	for _, in := range ins {
		enc, err := e.encode(ctx, e.enc, nil, in.clone())
		if err != nil {
			return fmt.Errorf("%s: Encode: %w", in, err)
		}

		if !enc.Applied {
			continue
		}

		body := enc.Body
		for i := range body {
			f := enc
			f.Body = slices.Clone(body)
			f.Body[i] ^= 0xff

			out, err := e.decode(ctx, nil, in.hash, nil, f)
			if err := fn(in, i, out, err); err != nil {
				return err
			}
		}
	}

	return nil
}

// Every byte of a body turned over in turn: Decode never panics, and returns
// one of RFC 5 §2.7's five errors or at most max bytes other than the input,
// which the codec's hash check then rejects (RFC 5 §2.7, §4 item 6). Catches
// a foreign error, a panic, and a byte the decoder ignores.
func flips(ctx context.Context, e *env) error {
	return e.flipped(ctx, func(in input, i int, out []byte, err error) error {
		switch {
		case err != nil && !isDecodeError(err):
			return fmt.Errorf("%s: byte %d of the body flipped: Decode returned %v, not one of RFC 5 §2.7's five errors", in, i, err)
		case err == nil && bytes.Equal(out, in.plain):
			return fmt.Errorf("%s: byte %d of the body flipped: Decode returned the input unchanged", in, i)
		case err == nil && len(out) > e.s.Max:
			return fmt.Errorf("%s: byte %d of the body flipped: Decode returned %d bytes, over max %d", in, i, len(out), e.s.Max)
		}

		return nil
	})
}

// A transform that authenticates returns ErrMalformed itself for every byte
// flipped, a flip in the ID of its material included, which the census
// refuses before the provider is asked (RFC 5 §2.7, §8.1). Catches a flip it
// accepts.
func authenticatedFlips(ctx context.Context, e *env) error {
	return e.flipped(ctx, func(in input, i int, _ []byte, err error) error {
		if !errors.Is(err, transform.ErrMalformed) {
			return fmt.Errorf("%s: byte %d of the body flipped: Decode returned %v, want %v", in, i, err, transform.ErrMalformed)
		}

		return nil
	})
}

// A transform that authenticates refuses chunk A's body decoded under chunk
// B's hash as ErrMalformed (RFC 5 §2.7). Catches a body that does not bind
// its hash.
func otherHash(ctx context.Context, e *env) error {
	ins, err := e.small(ctx, 2)
	if err != nil {
		return err
	}

	a, b := ins[0], ins[1]

	enc, err := e.encode(ctx, e.enc, nil, a.clone())
	if err != nil {
		return fmt.Errorf("%s: Encode: %w", a, err)
	}

	if !enc.Applied {
		return fmt.Errorf("%s: an authenticating transform declined it", a)
	}

	if _, err := e.decode(ctx, nil, b.hash, nil, enc); !errors.Is(err, transform.ErrMalformed) {
		return fmt.Errorf("%s's body decoded under %s's hash: got %v, want %v", a, b, err, transform.ErrMalformed)
	}

	return nil
}

// A transform that authenticates reports a body naming destroyed material as
// ErrMaterialDestroyed (RFC 5 §2.7). Catches a destroyed key folded into
// another error.
func destroyed(ctx context.Context, e *env) error {
	ins, err := e.small(ctx, 1)
	if err != nil {
		return err
	}

	in := ins[0]

	enc, err := e.encode(ctx, e.enc, nil, in.clone())
	if err != nil {
		return fmt.Errorf("%s: Encode: %w", in, err)
	}

	if !enc.Applied || len(enc.Material) == 0 {
		return fmt.Errorf("%s: Encode used no material, so none can be destroyed", in)
	}

	for _, id := range enc.Material {
		if !slices.Contains(e.pin, id) {
			return fmt.Errorf("%s: Encoded.Material names %+v, which is not pinned", in, id)
		}

		e.mem.Destroy(id)
	}

	if _, err := e.decode(ctx, nil, in.hash, nil, enc); !errors.Is(err, keys.ErrMaterialDestroyed) {
		return fmt.Errorf("%s: a body naming destroyed material: Decode returned %v, want %v", in, err, keys.ErrMaterialDestroyed)
	}

	return nil
}

// Encode of each input twice, and through a second instance from the same
// Factory and settings, gives identical bodies; where LengthOf answers, the
// body is applied and its length is the answer (RFC 5 §2.1, §7 T9). Catches
// a nondeterministic encoder and a LengthOf that lies.
func deterministic(ctx context.Context, e *env) error {
	other, err := e.build(ctx, e.s.Settings)
	if err != nil {
		return err
	}

	return e.each(ctx, func(in input) error {
		first, err := e.encode(ctx, e.enc, nil, in.clone())
		if err != nil {
			return fmt.Errorf("%s: Encode: %w", in, err)
		}

		for _, c := range []struct {
			name string
			tr   transform.Transform
		}{{"a second Encode", e.enc}, {"an Encode by a second instance", other}} {
			again, err := e.encode(ctx, c.tr, nil, in.clone())
			if err != nil {
				return fmt.Errorf("%s: %s: %w", in, c.name, err)
			}

			if again.Applied != first.Applied || !bytes.Equal(again.Body, first.Body) {
				return fmt.Errorf("%s: %s gave another body", in, c.name)
			}
		}

		if n, ok := e.enc.LengthOf(len(in.plain), e.pin); ok {
			if !first.Applied {
				return fmt.Errorf("%s: LengthOf answered %d, but Encode declined the chunk", in, n)
			}

			if len(first.Body) != n {
				return fmt.Errorf("%s: LengthOf answered %d, the body is %d bytes", in, n, len(first.Body))
			}
		}

		return nil
	})
}

// A body declaring an output of max + 1 bytes fails with ErrTooLarge when
// decoded with max (RFC 5 §2.7, §8.1 T4): the encode of max + 1 zeros, or
// of max + 1 random bytes if the transform declines zeros. Catches a decoder
// that clamps rather than refusing.
//
// decision: the body comes from the transform's own Encode of max + 1
// bytes, so the row needs an Encode that accepts an input over the chunk
// maximum; nothing in RFC 5 §3.1 bounds Encode's input. Give Subject a body
// of its own for the row if a transform ever refuses such an input.
func tooLarge(ctx context.Context, e *env) error {
	for _, plain := range [][]byte{make([]byte, e.s.Max+1), randomBytes(e.s.Max + 1)} {
		in := newInput(-1, plain)

		enc, err := e.encode(ctx, e.enc, nil, in.clone())
		if err != nil {
			return fmt.Errorf("%s: Encode: %w", in, err)
		}

		if !enc.Applied {
			continue
		}

		if _, err := e.decode(ctx, nil, in.hash, nil, enc); !errors.Is(err, transform.ErrTooLarge) {
			return fmt.Errorf("%s decoded with max %d: got %v, want %v", in, e.s.Max, err, transform.ErrTooLarge)
		}

		return nil
	}

	return fmt.Errorf("the transform declined %d zeros and %d random bytes, so no body declares max + 1", e.s.Max+1, e.s.Max+1)
}

// LengthKey is the same from two instances of one setting, and two settings
// whose bodies differ in length for one input have different LengthKeys,
// over the Subject's settings and every length-affecting one (RFC 5 §2.8,
// §4 item 2). Catches a LengthKey that omits a setting, and one that is not
// canonical.
func lengthKey(ctx context.Context, e *env) error {
	settings := append([]map[string]any{e.s.Settings}, e.s.LengthAffecting...)

	type built struct {
		key     []byte
		lengths []int
	}

	all := make([]built, len(settings))
	for i, s := range settings {
		tr, err := e.build(ctx, s)
		if err != nil {
			return err
		}

		again, err := e.build(ctx, s)
		if err != nil {
			return err
		}

		if all[i].key = tr.LengthKey(); !bytes.Equal(all[i].key, again.LengthKey()) {
			return fmt.Errorf("two instances of settings %v give LengthKeys %x and %x", s, all[i].key, again.LengthKey())
		}

		if err := e.spread(ctx, func(in input) error {
			enc, err := e.encode(ctx, tr, nil, in.clone())
			if err != nil {
				return fmt.Errorf("%s under %v: Encode: %w", in, s, err)
			}

			all[i].lengths = append(all[i].lengths, stored(len(in.plain), enc))

			return nil
		}); err != nil {
			return err
		}
	}

	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if bytes.Equal(all[i].key, all[j].key) && !slices.Equal(all[i].lengths, all[j].lengths) {
				return fmt.Errorf("settings %v and %v give bodies of different lengths under one LengthKey %x", settings[i], settings[j], all[i].key)
			}
		}
	}

	return nil
}

// Encode asks the provider for no current material: it gets the pinned
// material's content through Get alone (RFC 5 §3.1). Catches an Encode that
// calls Current.
func noCurrent(ctx context.Context, e *env) error {
	before := e.encs.currents.Load()

	return e.each(ctx, func(in input) error {
		if _, err := e.encode(ctx, e.enc, nil, in.clone()); err != nil {
			return fmt.Errorf("%s: Encode: %w", in, err)
		}

		if got := e.encs.currents.Load() - before; got != 0 {
			return fmt.Errorf("%s: Encode called the provider's Current %d times, want 0", in, got)
		}

		return nil
	})
}

// Decode asks for material only through the provider it is handed, the
// census, never through the one its Factory was given (RFC 5 §2.5, §3.1).
// Catches a decoder that goes around the census.
func censusOnly(ctx context.Context, e *env) error {
	ins, err := e.small(ctx, flipCount)
	if err != nil {
		return err
	}

	for _, in := range ins {
		if _, _, err := e.trip(ctx, in); err != nil {
			return err
		}

		if c, g := e.decs.currents.Load(), e.decs.gets.Load(); c+g != 0 {
			return fmt.Errorf("%s: Decode called its Factory's provider (%d Current, %d Get), want only the provider it was handed", in, c, g)
		}
	}

	return nil
}

// A body is encoded under the material it is pinned to, whatever was pinned
// before: once new material of each kind is current and pinned, each body
// names it and decodes, through a census listing it alone, to what the body
// under the first material decoded to (RFC 5 §2.8, §3.1).
// Catches a transform that keeps the first material it was given.
func rotated(ctx context.Context, e *env) error {
	ins, err := e.small(ctx, flipCount)
	if err != nil {
		return err
	}

	before := make([][]byte, len(ins))
	for i, in := range ins {
		if _, before[i], err = e.trip(ctx, in); err != nil {
			return err
		}
	}

	pin, err := e.give(ctx, "-rotated")
	if err != nil {
		return err
	}

	for i, in := range ins {
		enc, err := e.encodeUnder(ctx, e.enc, pin, nil, in.clone())
		if err != nil {
			return fmt.Errorf("%s under the rotated pin: Encode: %w", in, err)
		}

		if enc.Applied && !slices.Equal(enc.Material, pin) {
			return fmt.Errorf("%s under the rotated pin: Encoded.Material = %+v, want %+v", in, enc.Material, pin)
		}

		out, err := e.decode(ctx, nil, in.hash, in.plain, enc)
		if err != nil {
			return fmt.Errorf("%s under the rotated pin: Decode: %w", in, err)
		}

		if !bytes.Equal(out, before[i]) {
			return fmt.Errorf("%s under the rotated pin: decodes to other bytes than under the first, from byte %d", in, firstDiff(out, before[i]))
		}
	}

	return nil
}
