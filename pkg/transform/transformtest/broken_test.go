package transformtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// testMax is the chunk maximum the test transforms run under: small, since
// the suite's own test runs every row on every broken subject.
const testMax = 4 << 10

// frame is the correct test transform that uses no material: a version
// byte, a pad length and that many zeros before the input, so its one
// setting, pad, changes the body's length.
//
//	body = 0x01 ‖ pad (1 byte) ‖ 0x00 × pad ‖ plain
type frame struct {
	pad int
	m   keys.Provider
}

// maxPad is the largest pad frame accepts, so its bound under any setting.
const maxPad = 255

// setting reads an integer setting the way encoding/json and Go code hand
// one over: an int, or an integral float64.
func setting(settings map[string]any, key string, limit int) (int, error) {
	v, ok := settings[key]
	if !ok {
		return 0, nil
	}

	var n int

	switch x := v.(type) {
	case int:
		n = x
	case float64:
		n = int(x)
		if float64(n) != x {
			return 0, fmt.Errorf("%s = %v, want an integer", key, v)
		}
	default:
		return 0, fmt.Errorf("%s = %v, want an integer", key, v)
	}

	if n < 0 || n > limit {
		return 0, fmt.Errorf("%s = %d, want 0 to %d", key, n, limit)
	}

	return n, nil
}

func newFrame(settings map[string]any, m keys.Provider) (*frame, error) {
	for k := range settings {
		if k != "pad" {
			return nil, fmt.Errorf("frame: unknown setting %q", k)
		}
	}

	pad, err := setting(settings, "pad", maxPad)
	if err != nil {
		return nil, fmt.Errorf("frame: %w", err)
	}

	return &frame{pad: pad, m: m}, nil
}

func (f *frame) ID() transform.ID       { return 0x8001 }
func (f *frame) Version() uint8         { return 1 }
func (f *frame) Stage() transform.Stage { return transform.StageCompress }
func (f *frame) Kinds() []string        { return nil }
func (f *frame) LengthKey() []byte      { return []byte{byte(f.pad)} }

func (f *frame) MaxEncodedLen(n int, m []keys.MaterialID) int {
	if m == nil {
		return n + 2 + maxPad
	}

	return n + 2 + f.pad
}

func (f *frame) LengthOf(n int, _ []keys.MaterialID) (int, bool) { return n + 2 + f.pad, true }

func (f *frame) Encode(_ context.Context, _ entity.NamespaceID, _ []keys.MaterialID, dst []byte, _ [32]byte, plain []byte) (transform.Encoded, error) {
	body := append(dst[:0], 1, byte(f.pad))
	body = append(body, make([]byte, f.pad)...)
	body = append(body, plain...)

	return transform.Encoded{Body: body, Applied: true, Version: 1}, nil
}

// header is the length of frame's header in body, or an error.
func (f *frame) header(body []byte) (int, error) {
	if len(body) < 2 || body[0] != 1 {
		return 0, transform.ErrMalformed
	}

	pad := int(body[1])
	if len(body) < 2+pad || slices.ContainsFunc(body[2:2+pad], func(b byte) bool { return b != 0 }) {
		return 0, transform.ErrMalformed
	}

	return 2 + pad, nil
}

func (f *frame) Decode(_ context.Context, _ entity.NamespaceID, dst []byte, _ [32]byte, body []byte, max int, _ keys.Provider) ([]byte, error) {
	h, err := f.header(body)
	if err != nil {
		return nil, err
	}

	if len(body)-h > max {
		return nil, transform.ErrTooLarge
	}

	return append(dst[:0], body[h:]...), nil
}

// frameSubject is frame under the suite, built through wrap.
func frameSubject(wrap func(*frame) transform.Transform) Subject {
	return Subject{
		Factory: func(_ context.Context, settings map[string]any, m keys.Provider) (transform.Transform, error) {
			f, err := newFrame(settings, m)
			if err != nil {
				return nil, err
			}

			return wrap(f), nil
		},
		Settings:        map[string]any{"pad": 2},
		LengthAffecting: []map[string]any{{"pad": 5.0}},
		Max:             testMax,
		WorstCase:       func(n int) []byte { return make([]byte, n) },
	}
}

// sealKind is the kind of material seal authenticates under.
const sealKind = "transformtest-seal-key"

// seal is the correct test transform that authenticates: the ID of its
// material, the input, and an HMAC-SHA256 under that material over the
// chunk's hash and everything before the tag.
//
//	body = 0x01 ‖ len(id) (1 byte) ‖ id ‖ plain ‖ tag (32 bytes)
type seal struct {
	m keys.Provider

	// mac is the tag over hash, the header and plain; a broken seal changes
	// what it covers.
	mac func(key []byte, hash [32]byte, head, plain []byte) []byte

	// key gets the content of the pinned material; a broken seal changes
	// where from.
	key func(ctx context.Context, s *seal, id keys.MaterialID) ([]byte, error)
}

func fullMAC(key []byte, hash [32]byte, head, plain []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(hash[:])
	h.Write(head)
	h.Write(plain)

	return h.Sum(nil)
}

func pinnedKey(ctx context.Context, s *seal, id keys.MaterialID) ([]byte, error) {
	return s.m.Get(ctx, id)
}

const tagLen = sha256.Size

func (s *seal) ID() transform.ID       { return 0x8002 }
func (s *seal) Version() uint8         { return 1 }
func (s *seal) Stage() transform.Stage { return transform.StageEncrypt }
func (s *seal) Kinds() []string        { return []string{sealKind} }
func (s *seal) LengthKey() []byte      { return nil }

func (s *seal) MaxEncodedLen(n int, m []keys.MaterialID) int {
	if len(m) == 0 {
		return n + 2 + 255 + tagLen
	}

	return n + 2 + len(m[0].ID) + tagLen
}

func (s *seal) LengthOf(n int, m []keys.MaterialID) (int, bool) {
	return s.MaxEncodedLen(n, m), len(m) != 0
}

func (s *seal) Encode(ctx context.Context, _ entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	if len(m) != 1 || m[0].Kind != sealKind || len(m[0].ID) > 255 {
		return transform.Encoded{}, fmt.Errorf("seal: pinned material %+v, want one %s", m, sealKind)
	}

	key, err := s.key(ctx, s, m[0])
	if err != nil {
		return transform.Encoded{}, err
	}

	body := append(dst[:0], 1, byte(len(m[0].ID)))
	body = append(body, m[0].ID...)
	head := len(body)
	body = append(body, plain...)
	body = append(body, s.mac(key, hash, body[:head], plain)...)

	return transform.Encoded{Body: body, Applied: true, Version: 1, Material: slices.Clone(m)}, nil
}

func (s *seal) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	if len(body) < 2 || body[0] != 1 {
		return nil, transform.ErrMalformed
	}

	head := 2 + int(body[1])
	if len(body) < head+tagLen {
		return nil, transform.ErrMalformed
	}

	plain, tag := body[head:len(body)-tagLen], body[len(body)-tagLen:]
	if len(plain) > max {
		return nil, transform.ErrTooLarge
	}

	key, err := m.Get(ctx, keys.MaterialID{Namespace: ns, Kind: sealKind, ID: string(body[2:head])})
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}

	if !hmac.Equal(tag, s.mac(key, hash, body[:head], plain)) {
		return nil, transform.ErrMalformed
	}

	return append(dst[:0], plain...), nil
}

// sealSubject is seal under the suite, its tag computed by mac, built
// through wrap.
func sealSubject(mac func(key []byte, hash [32]byte, head, plain []byte) []byte, wrap func(*seal) transform.Transform) Subject {
	return Subject{
		Factory: func(_ context.Context, settings map[string]any, m keys.Provider) (transform.Transform, error) {
			if len(settings) != 0 {
				return nil, fmt.Errorf("seal: takes no settings, got %v", settings)
			}

			return wrap(&seal{m: m, mac: mac, key: pinnedKey}), nil
		},
		Max:       testMax,
		WorstCase: randomBytes,
		fixtures:  sealFixtures,
	}
}

// sealFixtures holds seal's fixture apart from frame's, as seal's own
// package would.
const sealFixtures = "testdata/seal/fixtures"

func asFrame(f *frame) transform.Transform { return f }
func asSeal(s *seal) transform.Transform   { return s }

// lossy changes the last byte of every 2-byte input it encodes.
type lossy struct{ *frame }

func (l lossy) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	enc, err := l.frame.Encode(ctx, ns, m, dst, hash, plain)
	if len(plain) == 2 {
		enc.Body[len(enc.Body)-1] ^= 0x01
	}

	return enc, err
}

// unwrapsNested decodes again whatever still looks like its own body, so an
// input that begins with its own output loses its head.
type unwrapsNested struct{ *frame }

func (u unwrapsNested) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	out, err := u.frame.Decode(ctx, ns, nil, hash, body, max, m)
	for err == nil {
		h, herr := u.header(out)
		if herr != nil {
			break
		}

		out = out[h:]
	}

	return append(dst[:0], out...), err
}

// wrongVersion records a format version other than the one Version names.
type wrongVersion struct{ *frame }

func (w wrongVersion) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	enc, err := w.frame.Encode(ctx, ns, m, dst, hash, plain)
	enc.Version = 2

	return enc, err
}

// boundBy declares a bound off by its deltas, under the pin and under nil,
// at one n only when at is set.
type boundBy struct {
	*frame

	pinned, loose int
	at            int
}

func (b boundBy) MaxEncodedLen(n int, m []keys.MaterialID) int {
	if b.at != 0 && n != b.at {
		return b.frame.MaxEncodedLen(n, m)
	}

	if m == nil {
		return b.frame.MaxEncodedLen(n, m) + b.loose
	}

	return b.frame.MaxEncodedLen(n, m) + b.pinned
}

// settingsBound answers the bound under nil from its own pad, so the
// default decoder's bound is below every padded body.
type settingsBound struct{ *frame }

func (s settingsBound) MaxEncodedLen(n int, _ []keys.MaterialID) int { return n + 2 + s.pad }

// shared keeps the input of the last Encode under a mutex and encodes that,
// rather than its own. The first call the concurrent row marks waits until a
// marked call with other bytes has entered before reading it, so two overlap
// on every scheduler and the second's bytes reach the first's body.
type shared struct {
	*frame

	mu    sync.Mutex
	last  []byte
	first []byte // the bytes of the first marked call
	both  chan struct{}
}

func newShared(f *frame) transform.Transform { return &shared{frame: f, both: make(chan struct{})} }

func (s *shared) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	marked := ctx.Value(overlap{}) != nil

	s.mu.Lock()
	s.last = slices.Clone(plain)
	waits := false

	switch {
	case !marked:
	case s.first == nil:
		s.first, waits = s.last, true
	case !bytes.Equal(plain, s.first) && s.both != nil:
		close(s.both)
		s.both = nil
	}

	both := s.both
	s.mu.Unlock()

	if waits {
		// A backstop only: the second call's entry ends the wait.
		timer := time.NewTimer(time.Minute)
		select {
		case <-both:
		case <-timer.C:
		}
		timer.Stop()
	}

	s.mu.Lock()
	last := s.last
	s.mu.Unlock()

	return s.frame.Encode(ctx, ns, m, dst, hash, last)
}

// foreign returns an error of its own for a body it did not write.
type foreign struct{ *frame }

func (f foreign) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	out, err := f.frame.Decode(ctx, ns, dst, hash, body, max, m)
	if errors.Is(err, transform.ErrMalformed) {
		return nil, errors.New("frame: bad body")
	}

	return out, err
}

// ignoresVersion decodes a body whatever its version byte says.
type ignoresVersion struct{ *frame }

func (f ignoresVersion) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	if len(body) > 0 {
		body = slices.Clone(body)
		body[0] = 1
	}

	return f.frame.Decode(ctx, ns, dst, hash, body, max, m)
}

// masked XORs every input with a byte next gives, which it appends to the
// body: next differing per call makes two encodes disagree, and differing
// per instance makes two instances disagree.
type masked struct {
	*frame

	next func() byte
}

func (r masked) MaxEncodedLen(n int, m []keys.MaterialID) int { return r.frame.MaxEncodedLen(n, m) + 1 }

func (r masked) LengthOf(n int, m []keys.MaterialID) (int, bool) {
	l, ok := r.frame.LengthOf(n, m)
	return l + 1, ok
}

func (r masked) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	mask := r.next()

	enc, err := r.frame.Encode(ctx, ns, m, dst, hash, xorWith(plain, mask))
	enc.Body = append(enc.Body, mask)

	return enc, err
}

func xorWith(b []byte, mask byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ mask
	}

	return out
}

func (r masked) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	if len(body) == 0 {
		return nil, transform.ErrMalformed
	}

	out, err := r.frame.Decode(ctx, ns, nil, hash, body[:len(body)-1], max, m)
	if err != nil {
		return nil, err
	}

	return append(dst[:0], xorWith(out, body[len(body)-1])...), nil
}

// counter is a fresh count, each call one more.
func counter() func() byte {
	var n atomic.Uint32
	return func() byte { return byte(n.Add(1)) }
}

// perCall masks each body with the next count.
func perCall(f *frame) transform.Transform { return masked{f, counter()} }

// perInstance returns a wrap whose instances each mask with a byte of their
// own.
func perInstance() func(*frame) transform.Transform {
	instances := counter()

	return func(f *frame) transform.Transform {
		b := instances()
		return masked{f, func() byte { return b }}
	}
}

// lengthOfOff answers LengthOf one byte long.
type lengthOfOff struct{ *frame }

func (l lengthOfOff) LengthOf(n int, m []keys.MaterialID) (int, bool) {
	got, ok := l.frame.LengthOf(n, m)
	return got + 1, ok
}

// clamps cuts an output over max down to max rather than refusing it.
type clamps struct{ *frame }

func (c clamps) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	out, err := c.frame.Decode(ctx, ns, dst, hash, body, len(body), m)
	if err == nil && len(out) > max {
		out = out[:max]
	}

	return out, err
}

// blindKey leaves the pad out of its LengthKey.
type blindKey struct{ *frame }

func (blindKey) LengthKey() []byte { return nil }

// instanceKey puts a count of its instances into its LengthKey.
type instanceKey struct {
	*frame

	n byte
}

func (k instanceKey) LengthKey() []byte { return []byte{byte(k.pad), k.n} }

func countedKeys() func(*frame) transform.Transform {
	instances := counter()
	return func(f *frame) transform.Transform { return instanceKey{f, instances()} }
}

// writesInput turns its input's first byte over once it has encoded it.
type writesInput struct{ *frame }

func (w writesInput) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	enc, err := w.frame.Encode(ctx, ns, m, dst, hash, plain)
	if len(plain) > 0 {
		plain[0] ^= 0xff
	}

	return enc, err
}

// identity applies itself without copying: its body is its input.
type identity struct{}

func (identity) ID() transform.ID                                { return 0x8003 }
func (identity) Version() uint8                                  { return 1 }
func (identity) Stage() transform.Stage                          { return transform.StageCompress }
func (identity) Kinds() []string                                 { return nil }
func (identity) LengthKey() []byte                               { return nil }
func (identity) MaxEncodedLen(n int, _ []keys.MaterialID) int    { return n }
func (identity) LengthOf(n int, _ []keys.MaterialID) (int, bool) { return n, true }

func (identity) Encode(_ context.Context, _ entity.NamespaceID, _ []keys.MaterialID, _ []byte, _ [32]byte, plain []byte) (transform.Encoded, error) {
	return transform.Encoded{Body: plain, Applied: true, Version: 1}, nil
}

func (identity) Decode(_ context.Context, _ entity.NamespaceID, dst []byte, _ [32]byte, body []byte, max int, _ keys.Provider) ([]byte, error) {
	if len(body) > max {
		return nil, transform.ErrTooLarge
	}

	return append(dst[:0], body...), nil
}

// decodeAliases returns the tail of its body as the output.
type decodeAliases struct{ *frame }

func (d decodeAliases) Decode(_ context.Context, _ entity.NamespaceID, _ []byte, _ [32]byte, body []byte, max int, _ keys.Provider) ([]byte, error) {
	h, err := d.header(body)
	if err != nil {
		return nil, err
	}

	if len(body)-h > max {
		return nil, transform.ErrTooLarge
	}

	return body[h:], nil
}

// zeroedDst writes only the nonzero bytes of its output into dst, trusting
// dst's spare capacity to hold zeros.
type zeroedDst struct{ *frame }

func (z zeroedDst) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	plain, err := z.frame.Decode(ctx, ns, nil, hash, body, max, m)
	if err != nil {
		return nil, err
	}

	out := make([]byte, len(plain))
	if cap(dst) >= len(plain) {
		out = dst[:len(plain)]
	}

	for i, b := range plain {
		if b != 0 {
			out[i] = b
		}
	}

	return out, nil
}

// asksCurrent calls the provider's Current from Encode, and ignores it.
type asksCurrent struct{ *frame }

func (a asksCurrent) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	_, _ = a.m.Current(ctx, ns, "frame-kind")
	return a.frame.Encode(ctx, ns, m, dst, hash, plain)
}

// lastByteUncovered leaves plain's last byte out of the tag, so a flip there
// decodes.
func lastByteUncovered(key []byte, hash [32]byte, head, plain []byte) []byte {
	return fullMAC(key, hash, head, plain[:max(len(plain)-1, 0)])
}

// hashUncovered leaves the chunk's hash out of the tag.
func hashUncovered(key []byte, _ [32]byte, head, plain []byte) []byte {
	return fullMAC(key, [32]byte{}, head, plain)
}

// destroyedAsMalformed reports destroyed material as ErrMalformed.
type destroyedAsMalformed struct{ *seal }

func (d destroyedAsMalformed) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	out, err := d.seal.Decode(ctx, ns, dst, hash, body, max, m)
	if errors.Is(err, keys.ErrMaterialDestroyed) {
		return nil, transform.ErrMalformed
	}

	return out, err
}

// aroundCensus decodes through the provider its Factory was given rather
// than the one it is handed.
type aroundCensus struct{ *seal }

func (a aroundCensus) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, _ keys.Provider) ([]byte, error) {
	return a.seal.Decode(ctx, ns, dst, hash, body, max, a.m)
}

// keepsFirstKey encodes every body under the first key it was given,
// whatever material it is pinned to.
func keepsFirstKey(s *seal) transform.Transform {
	var (
		once  sync.Once
		first []byte
		err   error
	)

	s.key = func(ctx context.Context, s *seal, id keys.MaterialID) ([]byte, error) {
		once.Do(func() { first, err = s.m.Get(ctx, id) })
		return first, err
	}

	return s
}

// frameFixture is the input and body of frame's fixture.
var frameFixture = struct{ input, body []byte }{
	[]byte("transformtest fixture: frame, version 1, pad 7"),
	append([]byte{1, 7, 0, 0, 0, 0, 0, 0, 0}, "transformtest fixture: frame, version 1, pad 7"...),
}

// changedEncoder turns the last byte of the body of frame's fixture input
// over, as an encoder changed under an unchanged format version would.
type changedEncoder struct{ *frame }

func (c changedEncoder) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	enc, err := c.frame.Encode(ctx, ns, m, dst, hash, plain)
	if bytes.Equal(plain, frameFixture.input) {
		enc.Body[len(enc.Body)-1] ^= 0x01
	}

	return enc, err
}

// stoppedDecoding refuses the body of frame's fixture, as a decoder that
// dropped what an older encoder wrote would.
type stoppedDecoding struct{ *frame }

func (s stoppedDecoding) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, max int, m keys.Provider) ([]byte, error) {
	if bytes.Equal(body, frameFixture.body) {
		return nil, transform.ErrMalformed
	}

	return s.frame.Decode(ctx, ns, dst, hash, body, max, m)
}

// version2 writes format version 2, for which frame ships no fixture.
type version2 struct{ *frame }

func (version2) Version() uint8 { return 2 }

func (v version2) Encode(ctx context.Context, ns entity.NamespaceID, m []keys.MaterialID, dst []byte, hash [32]byte, plain []byte) (transform.Encoded, error) {
	enc, err := v.frame.Encode(ctx, ns, m, dst, hash, plain)
	enc.Version = 2

	return enc, err
}

// brokenSubject is one subject of the suite's own test, and what each row it
// must fail says: none for a correct transform.
type brokenSubject struct {
	name string
	s    Subject
	want map[string]string // row name to a substring of its error
}

func brokenSubjects() []brokenSubject {
	broken := func(wrap func(*frame) transform.Transform) Subject { return frameSubject(wrap) }

	return []brokenSubject{
		{"frame", frameSubject(asFrame), nil},
		{"seal", sealSubject(fullMAC, asSeal), nil},
		{"lossy on one input", broken(func(f *frame) transform.Transform { return lossy{f} }),
			map[string]string{"roundtrip": "input 1 (2 bytes): Decode(Encode(p)) differs from p at byte 1"}},
		{"its own output unwrapped twice", broken(func(f *frame) transform.Transform { return unwrapsNested{f} }),
			map[string]string{"roundtrip": "Decode(Encode(p)) differs from p"}},
		{"a format version Version does not name", broken(func(f *frame) transform.Transform { return wrongVersion{f} }),
			map[string]string{"roundtrip": "Encoded.Version = 2, Version() = 1"}},
		{"a bound one byte short", broken(func(f *frame) transform.Transform { return boundBy{frame: f, pinned: -1} }),
			map[string]string{"bound": "over MaxEncodedLen(1, pinned) = 4"}},
		{"a bound one byte loose", broken(func(f *frame) transform.Transform { return boundBy{frame: f, pinned: 1} }),
			map[string]string{"bound": "the worst case of 1 bytes stored 5 bytes, short of MaxEncodedLen(1, pinned) = 6"}},
		{"a bound under any material below its bodies", broken(func(f *frame) transform.Transform { return boundBy{frame: f, loose: -maxPad - 1} }),
			map[string]string{"bound": "over MaxEncodedLen(1, nil) = 2"}},
		{"a bound under any material following its own settings", broken(func(f *frame) transform.Transform { return settingsBound{f} }),
			map[string]string{"bound": "over the default decoder's MaxEncodedLen(1, nil) = 3"}},
		{"a bound that shrinks as n grows", broken(func(f *frame) transform.Transform { return boundBy{frame: f, pinned: 2, loose: 2, at: 99} }),
			map[string]string{"monotone": "MaxEncodedLen(100, pinned) = 104, below MaxEncodedLen(99, pinned) = 105"}},
		{"state shared between calls", broken(newShared),
			map[string]string{"concurrent": "a concurrent Encode gave another body than a sequential one"}},
		{"a foreign error on a flip", broken(func(f *frame) transform.Transform { return foreign{f} }),
			map[string]string{"flips": "Decode returned frame: bad body, not one of RFC 5 §2.7's five errors"}},
		{"a flip decoding back to the input", broken(func(f *frame) transform.Transform { return ignoresVersion{f} }),
			map[string]string{"flips": "byte 0 of the body flipped: Decode returned the input unchanged"}},
		{"another body each call", broken(perCall),
			map[string]string{
				"buffers":        "Encode into a junk dst gave another body than into a nil one",
				"concurrent":     "a concurrent Encode gave another body",
				"deterministic":  "a second Encode gave another body",
				"fixture-decode": "pad7.json: decodes to other bytes than its input, from byte 0",
				"fixture-encode": "pad7.json: Encode gave another body than the recorded one",
			}},
		{"another body each instance", broken(perInstance()),
			map[string]string{
				"deterministic":  "an Encode by a second instance gave another body",
				"fixture-decode": "pad7.json: decodes to other bytes than its input, from byte 0",
				"fixture-encode": "pad7.json: Encode gave another body than the recorded one",
			}},
		{"a LengthOf one byte off", broken(func(f *frame) transform.Transform { return lengthOfOff{f} }),
			map[string]string{"deterministic": "LengthOf answered 6, the body is 5 bytes"}},
		{"an output clamped to max", broken(func(f *frame) transform.Transform { return clamps{f} }),
			map[string]string{"too-large": "decoded with max 4096: got <nil>, want transform: output exceeds its bound"}},
		{"a LengthKey without the pad", broken(func(f *frame) transform.Transform { return blindKey{f} }),
			map[string]string{"length-key": "give bodies of different lengths under one LengthKey"}},
		{"a LengthKey differing between instances", broken(countedKeys()),
			map[string]string{"length-key": "two instances of settings map[pad:2] give LengthKeys"}},
		{"an Encode writing into its input", broken(func(f *frame) transform.Transform { return writesInput{f} }),
			map[string]string{"buffers": "Encode wrote into its input at byte 0"}},
		{"a body that is its input", broken(func(*frame) transform.Transform { return identity{} }),
			map[string]string{
				"buffers":        "the body Encode returned shares memory with its input",
				"fixture-decode": "pad7.json: decodes to other bytes than its input, from byte 0",
				"fixture-encode": "pad7.json: Encode gave another body than the recorded one",
			}},
		{"an output that is part of its body", broken(func(f *frame) transform.Transform { return decodeAliases{f} }),
			map[string]string{"buffers": "the bytes Decode returned share memory with its body"}},
		{"a decoder trusting dst to hold zeros", broken(func(f *frame) transform.Transform { return zeroedDst{f} }),
			map[string]string{"buffers": "Decode into a junk dst differs from one into nil"}},
		{"an Encode calling Current", broken(func(f *frame) transform.Transform { return asksCurrent{f} }),
			map[string]string{"no-current": "Encode called the provider's Current 1 times, want 0"}},
		{"a tag accepting one flip", sealSubject(lastByteUncovered, asSeal),
			map[string]string{
				"authenticated-flips": "flipped: Decode returned <nil>, want transform: malformed body",
				"fixture-decode":      "seal.json: Decode: transform: malformed body",
				"fixture-encode":      "seal.json: Encode gave another body than the recorded one",
			}},
		{"a tag ignoring the hash", sealSubject(hashUncovered, asSeal),
			map[string]string{
				"hash":           "hash: got <nil>, want transform: malformed body",
				"fixture-decode": "seal.json: Decode: transform: malformed body",
				"fixture-encode": "seal.json: Encode gave another body than the recorded one",
			}},
		{"destroyed material reported as malformed", sealSubject(fullMAC, func(s *seal) transform.Transform { return destroyedAsMalformed{s} }),
			map[string]string{"destroyed": "want keys: material destroyed"}},
		{"a decoder going around the census", sealSubject(fullMAC, func(s *seal) transform.Transform { return aroundCensus{s} }),
			map[string]string{
				"census-only":         "Decode called its Factory's provider",
				"authenticated-flips": "want transform: malformed body",
				"fixture-decode":      "seal.json: Decode: seal: transformtest-seal-key \"seal-fixture-key\" of namespace 10111213-1415-1617-1819-1a1b1c1d1e1f: keys: unknown material",
			}},
		{"the first key kept", sealSubject(fullMAC, keepsFirstKey),
			map[string]string{"rotated": "under the rotated pin: Decode: transform: malformed body"}},
		{"an encoder whose body for the fixture's input changed one byte", broken(func(f *frame) transform.Transform { return changedEncoder{f} }),
			map[string]string{"fixture-encode": "pad7.json: Encode gave another body than the recorded one, from byte 54"}},
		{"a decoder that stopped decoding the fixture's body", broken(func(f *frame) transform.Transform { return stoppedDecoding{f} }),
			map[string]string{"fixture-decode": "pad7.json: Decode: transform: malformed body"}},
		{"a format version with no fixture", broken(func(f *frame) transform.Transform { return version2{f} }),
			map[string]string{"fixture-current": "no fixture under testdata/fixtures/v2 for format version 2"}},
	}
}
