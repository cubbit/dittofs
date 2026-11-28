package chain

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/transform"
	"github.com/cubbit/dittofs/pkg/transform/keys"
)

// ponytail: every step but the last encodes into a buffer of its own, and
// every decode step but the first decodes into one, each allocated per call at
// the step's bound; the last encode step's output is copied once, behind the
// envelope, into dst; Encode returns a fresh []Encoded. So with a caller's dst
// a chain of one applied transform allocates once per call, plus one per
// further step. Pool the scratch, or take a caller's buffer in the
// signatures, when a put or get profile shows these allocations.

// Encode runs the configured transforms in stage order, each under its pinned
// material, writes the envelope listing those that applied, and returns the
// body with one Encoded per applied transform (RFC 5 §2.4, §3.1). A declined
// chunk passes to the next step unchanged and is absent from the envelope.
// The chain fills each Encoded's ID and Material from the pin, so a transform
// cannot misreport either to the census, and fails the put when a transform
// reports another Version than its own or writes past its MaxEncodedLen. dst
// must not overlap plain; the body is dst resliced when dst holds
// MaxEncodedLen(len(plain)) bytes, a new slice otherwise.
func (a *attempt) Encode(ctx context.Context, dst []byte, hash [32]byte, plain []byte) ([]byte, []transform.Encoded, error) {
	steps := a.c.steps
	if need := a.MaxEncodedLen(len(plain)); cap(dst) < need {
		dst = make([]byte, 0, need)
	}
	// The last step writes behind the room an envelope of every step takes;
	// the envelope actually written is no longer, so the output moves down
	// once, onto the envelope's end.
	room := 2 + 2*len(steps)
	in := plain
	encs := make([]transform.Encoded, 0, len(steps))
	var ids [3]transform.ID
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
		enc.ID, enc.Material = t.ID(), slices.Clip(m)
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
	return body, encs, nil
}

// Decode parses body's envelope, refusing a malformed one before any
// transform decodes, then undoes the listed transforms in reverse, each
// looked up in the decode set (RFC 5 §2.4, §2.5). Each step's output is
// bounded by limit, the chunk maximum, passed through the MaxEncodedLen, under any material, of the
// steps listed before it, so a step that enlarges is not refused by the next
// (RFC 5 §3.1); an output above its bound is transform.ErrTooLarge. The first
// listed step decodes into dst, every other into a buffer of its own. Every
// error is one of RFC 5 §2.7's five, and no transform's own error crosses it
// (decodeErr).
func (c *chain) Decode(ctx context.Context, ns entity.NamespaceID, dst []byte, hash [32]byte, body []byte, limit int, _ []transform.Encoded) ([]byte, error) {
	p, err := c.decode.parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	var bounds [3]int
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
	for i := p.n - 1; i >= 0; i-- {
		t, bound := p.steps[i], bounds[i]
		buf := dst[:0]
		if i > 0 {
			buf = make([]byte, 0, bound)
		}
		out, err := t.Decode(ctx, ns, buf, hash, in, bound, noMaterial{})
		if err != nil {
			return nil, decodeErr(ctx, t.ID(), err)
		}
		if len(out) > bound {
			return nil, fmt.Errorf("%w: transform %#04x decoded %d bytes, past its bound of %d", transform.ErrTooLarge, t.ID(), len(out), bound)
		}
		in = out
	}
	return in, nil
}

// decodeErr is the chain's own error for a step's Decode error err, so no
// transform's error type is reachable through errors.As: it wraps the sentinel
// err matches and carries err as text only (RFC 5 §2.7). Once ctx has ended,
// any error is keys.ErrMaterialUnavailable wrapping ctx.Err(), since a read
// cut short is retryable and says nothing about the body. Unknown material is
// both keys.ErrMaterialUnavailable, which the codec reports it as, and
// keys.ErrUnknownMaterial, which tells it apart. An error matching no sentinel
// is transform.ErrMalformed.
func decodeErr(ctx context.Context, id transform.ID, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: decoding with transform %#04x: %w", keys.ErrMaterialUnavailable, id, cerr)
	}
	for _, s := range []error{transform.ErrMalformed, transform.ErrTooLarge, keys.ErrMaterialDestroyed, keys.ErrMaterialUnavailable} {
		if errors.Is(err, s) {
			return fmt.Errorf("%w: decoding with transform %#04x: %s", s, id, err)
		}
	}
	if errors.Is(err, keys.ErrUnknownMaterial) {
		return fmt.Errorf("%w: %w: decoding with transform %#04x: %s", keys.ErrMaterialUnavailable, keys.ErrUnknownMaterial, id, err)
	}
	return fmt.Errorf("%w: decoding with transform %#04x: %s", transform.ErrMalformed, id, err)
}

// noMaterial is the provider each step's Decode receives: it answers nothing,
// so a body whose decode needs material fails as transform.ErrMalformed.
//
// ponytail: no decode reaches material yet, so no encrypted body decodes.
// Replace it with a view answering for the material IDs of the block's census
// in the namespace being read (RFC 5 §2.5), when bodies that need material
// are read.
type noMaterial struct{}

func (noMaterial) Current(context.Context, entity.NamespaceID, string) (keys.MaterialID, error) {
	return keys.MaterialID{}, transform.ErrMalformed
}

func (noMaterial) Get(context.Context, keys.MaterialID) ([]byte, error) {
	return nil, transform.ErrMalformed
}
