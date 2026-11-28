package chain_test

import (
	"context"
	"sync/atomic"
	"unsafe"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// pad is a test transform whose bound adds fixed bytes to its input, plus half
// the input's length when half is set, so with neither it is an identity. Its
// ID, stage and kinds are parameters, so one type stands in any stage and
// under any ID, a wrong one included. It uses no material, even when it
// declares a kind.
type pad struct {
	id    transform.ID
	stage transform.Stage
	fixed int
	half  bool
	kinds []string
}

// The test transforms' IDs, from the range reserved for custom transforms.
const (
	idA transform.ID = 0x8001 + iota
	idB
	idC
)

func (p pad) extra(n int) int {
	e := p.fixed
	if p.half {
		e += n / 2
	}
	return e
}

func (p pad) ID() transform.ID                                { return p.id }
func (p pad) Version() uint8                                  { return 1 }
func (p pad) Stage() transform.Stage                          { return p.stage }
func (p pad) Kinds() []string                                 { return p.kinds }
func (p pad) MaxEncodedLen(n int, _ []keys.MaterialID) int    { return n + p.extra(n) }
func (p pad) LengthOf(n int, _ []keys.MaterialID) (int, bool) { return n + p.extra(n), true }

func (p pad) LengthKey() []byte { return nil }

// padByte fills pad's padding, which Decode checks.
const padByte = 0xa5

// Encode appends extra(n) padding bytes to plain.
func (p pad) Encode(_ context.Context, _ entity.NamespaceID, _ []keys.MaterialID, dst []byte, _ [32]byte, plain []byte) (transform.Encoded, error) {
	body := append(dst[:0], plain...)
	for range p.extra(len(plain)) {
		body = append(body, padByte)
	}
	return transform.Encoded{Body: body, Applied: true, Version: p.Version()}, nil
}

// Decode finds the n with n + extra(n) = len(body), checks the padding, and
// returns the first n bytes.
func (p pad) Decode(_ context.Context, _ entity.NamespaceID, dst []byte, _ [32]byte, body []byte, limit int, _ keys.Provider) ([]byte, error) {
	l := len(body) - p.fixed
	if l < 0 {
		return nil, transform.ErrMalformed
	}
	n := l
	if p.half {
		// n + n/2 is 3k for n = 2k and 3k+1 for n = 2k+1, never 3k+2.
		if l%3 == 2 {
			return nil, transform.ErrMalformed
		}
		n = l/3*2 + l%3
	}
	for _, b := range body[n:] {
		if b != padByte {
			return nil, transform.ErrMalformed
		}
	}
	if n > limit {
		return nil, transform.ErrTooLarge
	}
	return append(dst, body[:n]...), nil
}

// factory builds p, whatever the settings.
func (p pad) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
		return p, nil
	}
}

// failing is a factory that fails with err, as a transform that cannot start
// does; nilTransform is one that returns neither a transform nor an error.
func failing(err error) transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
		return nil, err
	}
}

func nilTransform(context.Context, map[string]any, keys.Provider) (transform.Transform, error) {
	return nil, nil
}

// onSetting is a fault wrapper over factory f: a build whose settings carry
// key runs fault instead, so a transform builds for decoding, with empty
// settings, and fails, or builds another transform, when configured with key.
func onSetting(key string, f, fault transform.Factory) transform.Factory {
	return func(ctx context.Context, s map[string]any, m keys.Provider) (transform.Transform, error) {
		if _, ok := s[key]; ok {
			return fault(ctx, s, m)
		}
		return f(ctx, s, m)
	}
}

// decline is a test transform that declines every chunk, so its Decode, which
// no body listing it can reach honestly, refuses everything.
type decline struct {
	id    transform.ID
	stage transform.Stage
}

func (d decline) ID() transform.ID                             { return d.id }
func (d decline) Version() uint8                               { return 1 }
func (d decline) Stage() transform.Stage                       { return d.stage }
func (d decline) Kinds() []string                              { return nil }
func (d decline) MaxEncodedLen(n int, _ []keys.MaterialID) int { return n }
func (d decline) LengthOf(int, []keys.MaterialID) (int, bool)  { return 0, false }
func (d decline) LengthKey() []byte                            { return nil }
func (d decline) Encode(context.Context, entity.NamespaceID, []keys.MaterialID, []byte, [32]byte, []byte) (transform.Encoded, error) {
	return transform.Encoded{}, nil
}
func (d decline) Decode(context.Context, entity.NamespaceID, []byte, [32]byte, []byte, int, keys.Provider) ([]byte, error) {
	return nil, transform.ErrMalformed
}
func (d decline) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) { return d, nil }
}

// fault is a fault wrapper over a test transform: onDecode, when set,
// answers Decode in its place, with the limit the chain gave it; returnsInput
// makes Decode return its input; onEncode, when set, rewrites what its Encode
// returned; and onEncodeErr, when set, fails Encode with it.
type fault struct {
	transform.Transform
	onDecode     func(ctx context.Context, limit int) ([]byte, error)
	returnsInput bool
	onEncode     func(transform.Encoded) transform.Encoded
	onEncodeErr  error
}

func (f fault) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	if f.onEncodeErr != nil {
		return transform.Encoded{}, f.onEncodeErr
	}
	e, err := f.Transform.Encode(ctx, ns, m, dst, hash, plain)
	if f.onEncode != nil {
		e = f.onEncode(e)
	}
	return e, err
}

func (f fault) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, limit int, m keys.Provider) ([]byte, error) {
	if f.onDecode != nil {
		return f.onDecode(ctx, limit)
	}
	if f.returnsInput {
		return body, nil
	}
	return f.Transform.Decode(ctx, ns, dst, hash, body, limit, m)
}

func (f fault) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) { return f, nil }
}

// oddError is an error type of a transform's own, which must not cross the
// chain; wrapped, when set, is the error it wraps.
type oddError struct{ wrapped error }

func (e *oddError) Error() string { return "a transform's own error" }
func (e *oddError) Unwrap() error { return e.wrapped }

// watched wraps a test transform and passes every call through, keeping how
// many Encode and Decode calls reached it and how many of them were given a
// dst sharing memory with their input: whether a step's dst overlaps its input
// is what the chain must never do (RFC 5 §3.1).
type watched struct {
	transform.Transform
	calls, overlaps *atomic.Int64
}

func newWatched(t transform.Transform) watched {
	return watched{t, new(atomic.Int64), new(atomic.Int64)}
}

func (w watched) note(dst, in []byte) {
	w.calls.Add(1)
	if overlap(dst, in) {
		w.overlaps.Add(1)
	}
}

func (w watched) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	w.note(dst, plain)
	return w.Transform.Encode(ctx, ns, m, dst, hash, plain)
}

func (w watched) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, limit int, m keys.Provider) ([]byte, error) {
	w.note(dst, body)
	return w.Transform.Decode(ctx, ns, dst, hash, body, limit, m)
}

func (w watched) factory() transform.Factory {
	return func(context.Context, map[string]any, keys.Provider) (transform.Transform, error) { return w, nil }
}

// overlap reports whether the memory dst may write, up to its capacity,
// shares a byte with in.
func overlap(dst, in []byte) bool {
	if cap(dst) == 0 || len(in) == 0 {
		return false
	}
	d := uintptr(unsafe.Pointer(unsafe.SliceData(dst)))
	i := uintptr(unsafe.Pointer(unsafe.SliceData(in)))
	return d < i+uintptr(len(in)) && i < d+uintptr(cap(dst))
}
