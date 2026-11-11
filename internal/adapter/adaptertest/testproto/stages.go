package testproto

import (
	"encoding/binary"
	"errors"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

var errNoPrev = errors.New("testproto: a member that inherits its session with none before it")

// offsetOf is where h lies in body, a header a walk of body's chain reaches;
// false for one it does not.
func offsetOf(body []byte, h *header) (int, bool) {
	for off := 0; ; {
		at, err := headerAt(body, off)
		if err != nil {
			return 0, false
		}
		if at == h {
			return off, true
		}
		if off = at.Next(); off == 0 {
			return 0, false
		}
	}
}

// security checks and opens a request sealed under a session, and seals its
// reply.
type security struct{}

// Unwrap resolves the session a related member inherits from prev, checks the
// message's checksum under it, opens its arguments in place, and writes the
// session it resolved into the header, so every later stage reads a real one.
func (security) Unwrap(h core.Header, rec *resources.Buf, prev core.Header) (core.Header, error) {
	hd, ok := h.(*header)
	if !ok {
		return nil, errors.New("testproto: unwrap a header of another protocol")
	}
	off, ok := offsetOf(rec.B, hd)
	if !ok {
		return nil, errShort
	}
	s := hd.session()
	if s == inherited {
		ph, ok := prev.(*header)
		if !ok || ph == nil || ph.session() == inherited {
			return nil, errNoPrev
		}
		s = ph.session()
	}
	if s != 0 {
		args := arguments(rec.B, off, hd)
		if checksum(rec.B[off:off+headerLen+len(args)], s) != hd.checksum() {
			return nil, errChecksum
		}
		xorKey(args, s)
	}
	binary.BigEndian.PutUint32(hd[offSession:], s)

	return hd, nil
}

// Wrap seals every reply in reply under its session, in place: once for a
// chain, since a second pass would XOR the arguments back and sum them clear.
func (security) Wrap(_ core.Header, reply *resources.Buf) error {
	for off := 0; ; {
		h, err := headerAt(reply.B, off)
		if err != nil {
			return err
		}
		if s := h.session(); s != 0 && s != inherited {
			args := arguments(reply.B, off, h)
			xorKey(args, s)
			binary.BigEndian.PutUint32(h[offChecksum:], checksum(reply.B[off:off+headerLen+len(args)], s))
		}
		if off = h.Next(); off == 0 {
			return nil
		}
	}
}
