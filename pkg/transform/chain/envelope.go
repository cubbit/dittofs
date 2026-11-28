package chain

import (
	"encoding/binary"
	"fmt"

	"github.com/cubbit/dittofs/pkg/transform"
)

// envelopeV1 is the only envelope layout (RFC 5 §2.4): its version, a count,
// then each applied transform's ID, 2 bytes big-endian, in stage order.
const envelopeV1 = 1

// stages is how many stages a chain holds, and so the most IDs an envelope
// lists (RFC 5 §2.3).
const stages = 3

// envelopeLen is the length of an envelope listing n IDs (RFC 5 §2.4).
func envelopeLen(n int) int { return 2 + 2*n }

// appendEnvelope appends version 1's envelope listing ids to b.
func appendEnvelope(b []byte, ids []transform.ID) []byte {
	b = append(b, envelopeV1, byte(len(ids)))
	for _, id := range ids {
		b = binary.BigEndian.AppendUint16(b, uint16(id))
	}
	return b
}

// parsed is an envelope as parseEnvelope read it: the applied transforms'
// decoders in stage order, and the offset of the last one's output.
type parsed struct {
	steps [stages]transform.Transform
	n     int
	off   int
}

// parseEnvelope reads body's envelope against the decode set, before anything
// is decoded. It refuses with transform.ErrMalformed an unknown version, a
// count past the body's end, an ID the set does not hold, and any list whose
// stages do not strictly rise, which refuses an ID twice, two IDs of one stage
// and IDs out of stage order alike (RFC 5 §2.4).
func (s *decodeSet) parseEnvelope(body []byte) (parsed, error) {
	var p parsed
	if len(body) < 2 {
		return p, fmt.Errorf("%w: a %d-byte body holds no envelope", transform.ErrMalformed, len(body))
	}
	if body[0] != envelopeV1 {
		return p, fmt.Errorf("%w: envelope version %d is unknown", transform.ErrMalformed, body[0])
	}
	count := int(body[1])
	p.off = envelopeLen(count)
	if p.off > len(body) {
		return p, fmt.Errorf("%w: an envelope of %d IDs runs past the %d-byte body", transform.ErrMalformed, count, len(body))
	}
	var prev transform.Stage
	for i := range count {
		id := transform.ID(binary.BigEndian.Uint16(body[2+2*i:]))
		t, ok := s.byID[id]
		if !ok {
			return p, fmt.Errorf("%w: envelope names transform %#04x, which is not registered", transform.ErrMalformed, id)
		}
		st := t.Stage()
		if st <= prev {
			return p, fmt.Errorf("%w: envelope lists transform %#04x of stage %d after stage %d", transform.ErrMalformed, id, st, prev)
		}
		prev = st
		p.steps[i] = t // stages that strictly rise are at most three
	}
	p.n = count
	return p, nil
}
