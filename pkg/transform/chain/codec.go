package chain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unsafe"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// ponytail: each encode step but the last, and each decode step but the
// first, gets a buffer of its own allocated per call at the step's full
// bound, however short the body, so a crafted body listing three stages
// costs two chunk-sized buffers per read; Encode returns a fresh []Encoded,
// each with a copy of its pinned material, and copies the last output once
// behind the envelope; Decode hands its steps one census view, which
// escapes. Pool the scratch, or take a caller's buffer in the signatures,
// when a put or get profile shows these allocations.

// Encode runs the configured transforms in stage order, each under its pinned
// material, writes the envelope listing those that applied, and returns the
// body with one Encoded per applied transform (RFC 5 §2.4, §3.1). A declined
// chunk passes to the next step unchanged and is absent from the envelope.
// The chain fills each Encoded's ID, and a copy of its pinned Material, so a
// transform cannot misreport either to the census nor reach the pin through
// it. It fails the put when a transform reports another Version than its
// own, writes past its MaxEncodedLen, or makes a body of another length than
// a LengthOf that answered. dst must not overlap plain; the body is dst
// resliced when dst holds MaxEncodedLen(len(plain)) bytes, a new slice
// otherwise.
func (a *attempt) Encode(ctx context.Context, dst []byte, hash [32]byte, plain []byte) ([]byte, []transform.Encoded, error) {
	steps := a.c.steps
	need := a.MaxEncodedLen(len(plain))
	if cap(dst) < need {
		dst = make([]byte, 0, need)
	}
	// The last step writes behind the room an envelope of every step takes;
	// the envelope actually written is no longer, so the output moves down
	// once, onto the envelope's end.
	room := envelopeLen(len(steps))
	in := plain
	encs := make([]transform.Encoded, 0, len(steps))
	var ids [stages]transform.ID
	for i, t := range steps {
		m := a.material[i]
		bound := t.MaxEncodedLen(len(in), m)
		buf := dst[room:room]
		if i < len(steps)-1 {
			buf = make([]byte, 0, bound)
		}
		enc, err := t.Encode(ctx, a.ns, m, buf, hash, in)
		if err != nil {
			return nil, nil, fmt.Errorf("chain: encoding with transform %#04x: %w", t.ID(), err)
		}
		if !enc.Applied {
			continue
		}
		if enc.Version != t.Version() {
			return nil, nil, fmt.Errorf("chain: transform %#04x wrote format version %d, not its version %d, which the chain ID names", t.ID(), enc.Version, t.Version())
		}
		if len(enc.Body) > bound {
			return nil, nil, fmt.Errorf("chain: transform %#04x wrote %d bytes from %d, past its bound of %d", t.ID(), len(enc.Body), len(in), bound)
		}
		enc.ID, enc.Material = t.ID(), nil
		if len(m) > 0 {
			enc.Material = slices.Clone(m)
		}
		ids[len(encs)] = t.ID()
		encs = append(encs, enc)
		in = enc.Body
	}
	body := appendEnvelope(dst[:0], ids[:len(encs)])
	off := len(body)
	body = append(body, in...)
	if len(encs) > 0 {
		encs[len(encs)-1].Body = body[off:]
	}
	if l, ok := a.LengthOf(len(plain)); len(body) > need || ok && len(body) != l {
		return nil, nil, fmt.Errorf("chain: the chain wrote a %d-byte body from %d bytes, against a bound of %d and a length of %d known %v in advance", len(body), len(plain), need, l, ok)
	}
	return body, encs, nil
}

// Decode parses body's envelope, refusing a malformed one before any
// transform decodes, then undoes the listed transforms in reverse, each
// looked up in the decode set (RFC 5 §2.4, §2.5). Step i's output is bounded
// by limit grown through the bounds, under any material, of the steps listed
// before it, so a step that enlarges is not refused by the next (RFC 5
// §3.1); an output above its bound is transform.ErrTooLarge. The first listed
// step decodes into dst, every other into a buffer of its own, and a result
// sharing memory with body is copied into dst, so the caller may reuse body.
// A body without a transform the chain requires is refused before any step
// decodes (required), and each step reaches material only through a view of
// census in ns (census), whose own refusal or material error outranks the
// error the step returns. Every error is one of RFC 5 §2.7's five, and no
// transform's own error crosses it (decodeErr).
func (c *chain) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, limit int, listed []transform.Encoded) ([]byte, error) {
	p, err := c.decode.parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	if err := c.required(ns, &p); err != nil {
		return nil, err
	}
	var bounds [stages]int
	for i, b := 0, limit; i < p.n; i++ {
		bounds[i] = b
		b = max(b, p.steps[i].MaxEncodedLen(b, nil))
	}
	in := body[p.off:]
	if p.n == 0 {
		if len(in) > limit {
			return nil, fmt.Errorf("%w: a body of no transform holds %d bytes, past %d", transform.ErrTooLarge, len(in), limit)
		}
		return append(dst[:0], in...), nil
	}
	view := &census{p: c.keys, ns: ns, listed: listed}
	for i := p.n - 1; i >= 0; i-- {
		t, bound := p.steps[i], bounds[i]
		buf := dst[:0]
		if i > 0 {
			buf = make([]byte, 0, bound)
		}
		out, err := t.Decode(ctx, ns, buf, hash, in, bound, view)
		if v := view.take(); err != nil && v != nil {
			err = v
		}
		if err != nil {
			return nil, decodeErr(ctx, t.ID(), err)
		}
		if len(out) > bound {
			return nil, fmt.Errorf("%w: transform %#04x decoded %d bytes, past its bound of %d", transform.ErrTooLarge, t.ID(), len(out), bound)
		}
		in = out
	}
	if overlaps(in, body) {
		in = append(dst[:0], in...)
	}
	return in, nil
}

// overlaps reports whether a and b share a byte.
func overlaps(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	pa, pb := uintptr(unsafe.Pointer(unsafe.SliceData(a))), uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return pa < pb+uintptr(len(b)) && pb < pa+uintptr(len(a))
}

// decodeErr is the chain's own error for a step's Decode error err, so no
// transform's error type is reachable through errors.As: it wraps the sentinel
// err matches and carries err as text only (RFC 5 §2.7). Once ctx has ended,
// any error is keys.ErrMaterialUnavailable wrapping ctx.Err(), since a read
// cut short is retryable and says nothing about the body. Unknown material is
// both keys.ErrMaterialUnavailable, which the codec reports it as, and
// keys.ErrUnknownMaterial, which tells it apart. The material errors are
// matched first, so an error that also wraps transform.ErrMalformed is never
// reported as corruption (RFC 5 §2.7), and unknown first among them, so an
// error that is both unknown and unavailable keeps its tag. An error matching
// no sentinel is transform.ErrMalformed.
func decodeErr(ctx context.Context, id transform.ID, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: decoding with transform %#04x: %w", keys.ErrMaterialUnavailable, id, cerr)
	}
	if errors.Is(err, keys.ErrUnknownMaterial) {
		return fmt.Errorf("%w: %w: decoding with transform %#04x: %s", keys.ErrMaterialUnavailable, keys.ErrUnknownMaterial, id, err)
	}
	for _, s := range []error{keys.ErrMaterialDestroyed, keys.ErrMaterialUnavailable, transform.ErrTooLarge, transform.ErrMalformed} {
		if errors.Is(err, s) {
			return fmt.Errorf("%w: decoding with transform %#04x: %s", s, id, err)
		}
	}
	return fmt.Errorf("%w: decoding with transform %#04x: %s", transform.ErrMalformed, id, err)
}

// required refuses, with transform.ErrMalformed, a parsed envelope that lacks
// a transform the store configuration's require names, or, in a namespace
// that encrypts, any transform of the encrypt stage (RFC 5 §2.5, §3.2). It
// runs before any step decodes and only refuses: it never changes how a body
// decodes. Any encrypt-stage transform satisfies an encrypting namespace, so
// replacing one encryption transform by another keeps older bodies readable.
//
// decision: a required transform that declined a chunk is absent from its
// envelope, so requiring a compressor refuses every chunk it declined, which
// the store wrote itself. RFC 5 §2.5 leaves require to the operator, to turn
// on once the census shows no body lacks it, and frames it around
// encryption, which never declines. Refuse such a name at NewChain if a
// store ever requires a transform that can decline.
func (c *chain) required(ns entity.NamespaceID, p *parsed) error {
	listed := p.steps[:p.n]
	for _, id := range c.require {
		if !slices.ContainsFunc(listed, func(t transform.Transform) bool { return t.ID() == id }) {
			return fmt.Errorf("%w: the body lacks required transform %#04x", transform.ErrMalformed, id)
		}
	}
	if c.encrypts(ns) && !slices.ContainsFunc(listed, func(t transform.Transform) bool { return t.Stage() == transform.StageEncrypt }) {
		return fmt.Errorf("%w: a body of namespace %s, which encrypts, has no encrypt stage", transform.ErrMalformed, ns)
	}
	return nil
}
