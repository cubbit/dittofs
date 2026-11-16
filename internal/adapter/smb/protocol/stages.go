// Package protocol adapts SMB2's codecs to core: the header stage that
// decodes a request's header and encodes its reply's, and the router that
// hands every request to SMB's one table (RFC 20 §4.7, §4.8).
package protocol

import (
	"errors"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
	"github.com/cubbit/dittofs/pkg/resources"
)

var (
	_ core.HeaderCodec = headerStage{}
	_ core.Router      = (*router)(nil)
)

var errForeign = errors.New("smb: a header this stage did not decode")

// header is the SMB2 header at offset 0 of its record.
//
// decision: a header is a view of its record, one pointer to its 64 bytes, so
// handing it to core as a core.Header allocates nothing. It is valid while the
// record's Buf is retained, which core does for a request's life. A member
// past the first carries its offset besides, since NextCommand counts from its
// own header and Next from the record's start; writing the offset into the
// protocol ID, already checked, would allocate nothing but change bytes a
// signature covers. Withdraw the view, and copy the fields out, if core ever
// releases a record before its request ends.
type header struct{ h *wire.Header }

func (v header) Op() uint32 { return uint32(v.h.Command()) }

// Version is 0: a request's dialect is its connection's, which NEGOTIATE
// fixes, and none is negotiated here.
func (v header) Version() core.Version { return 0 }

// Next is the offset of the next member, NextCommand itself at offset 0.
func (v header) Next() int { return int(v.h.NextCommand()) }

// Related reports SMB2_FLAGS_RELATED_OPERATIONS: the member takes its
// session, tree and file from the one before it (MS-SMB2 2.2.1.2, 3.3.5.2.7.2).
func (v header) Related() bool { return v.h.Flags()&wire.FlagRelatedOperations != 0 }

func (v header) raw() *wire.Header { return v.h }
func (v header) at() int           { return 0 }

// badNext is a header at offset 0 refused for its NextCommand. Its Next is
// pastEnd, a step the walk refuses, so the chain ends there and the replies
// before it go out before the connection closes (RFC 20 §4.7, RFC 22 §16.5).
type badNext struct{ header }

func (badNext) Next() int { return pastEnd }

// pastEnd is an offset past the end of every record: a Direct TCP header
// counts at most MaxRecord bytes.
const pastEnd = wire.MaxRecord + 1

// member is a header past the first in its record, with its offset and the
// record's offset of the next member: 0 for the last, pastEnd for a refused
// step.
//
// ponytail: each member past the first allocates once, for its offsets. Give
// a record one array of offsets, filled as its chain is walked, if a profile
// shows chains.
type member struct {
	header
	off, next int
}

func (m *member) Next() int { return m.next }
func (m *member) at() int   { return m.off }

// view is a header this stage decoded: its bytes and where they lie.
type view interface {
	core.Header
	raw() *wire.Header
	at() int
}

// headerStage is SMB2's core.HeaderCodec.
type headerStage struct{}

// Decode returns the header at off in rec, as a view of its bytes: nil with
// wire's error for a header no reply can be addressed to, which closes the
// connection, and the header with its error for a malformed one, which is
// answered. core wraps either error in ErrDecode (RFC 20 §4.7).
func (headerStage) Decode(rec *resources.Buf, off int) (core.Header, error) {
	h, err := wire.DecodeHeader(rec.B, off)
	if h == nil {
		return nil, err
	}
	stepped := !errors.Is(err, wire.ErrNextCommand)
	if off == 0 {
		if !stepped {
			return badNext{header{h}}, err
		}
		return header{h}, err
	}
	m := &member{header: header{h}, off: off}
	switch {
	case !stepped:
		m.next = pastEnd
	case h.NextCommand() != 0:
		m.next = off + int(h.NextCommand())
	}
	return m, err
}

// Encode appends the reply's header for s and, for a status other than
// success, the ERROR response's body, which a refusal answered with a status
// alone needs (RFC 22 §16.5, MS-SMB2 3.3.4.4). A success's body is its
// command's. Every other status gets the error body: no command is served
// whose encoder writes the body of a status MS-SMB2 3.3.4.4 does not count as
// a failure.
func (headerStage) Encode(h core.Header, s core.Status, out *resources.Buf) error {
	v, ok := h.(view)
	if !ok {
		return errForeign
	}
	// ponytail: every reply grants one credit, which never takes a client past
	// any credit target and keeps its balance above zero. Grant what RFC 22
	// §3.3 states, what the client asks, at least 1, up to the connection's
	// target, once a stage keeps the connection's credit count.
	out.B = wire.AppendReply(out.B, v.raw(), uint32(s), 1)
	if s != 0 {
		out.B = wire.AppendError(out.B)
	}
	return nil
}

// Chain pads out to the reply at prev plus a multiple of 8 and points that
// reply's NextCommand at the one appended after it (MS-SMB2 3.3.4.1.3).
func (headerStage) Chain(out *resources.Buf, prev int) error {
	var err error
	out.B, err = wire.Chain(out.B, prev)
	return err
}

// Args is the bytes of h's message past its 64-byte header, to the next
// member or the record's end; nil for a header this stage did not decode.
func (headerStage) Args(rec *resources.Buf, h core.Header) []byte {
	v, ok := h.(view)
	if !ok {
		return nil
	}
	end := v.Next()
	if end == 0 || end > len(rec.B) {
		end = len(rec.B)
	}
	return rec.B[v.at()+wire.HeaderLen : end]
}

// replyLen is the most Encode writes, with Chain's padding: the reply's
// header, the ERROR response's body, and at most 7 bytes of padding to 8.
const replyLen = wire.HeaderLen + wire.ErrorLen + 7

// ReplyLen is the most Encode writes for any header, with Chain's padding
// (RFC 20 §4.7). No transform is added in place: none is served yet.
func (headerStage) ReplyLen(core.Header) int { return replyLen }

// router is SMB's core.Router for one connection: SMB has one table for every
// dialect (RFC 20 §4.8).
type router struct {
	table *core.OpTable
	conn  *core.Conn
}

// Route returns the table and a Request of the request's own.
//
// decision: every request is routed at version 0 and level NeedsNothing, with
// no state. A request's dialect is the one NEGOTIATE fixes for its
// connection, and its level is what its session and tree reach; no stage
// negotiates or keeps a session or a tree yet, so none has either, and a row
// that needs more than nothing is refused by core's check. Withdrawn once
// NEGOTIATE fixes a dialect and a request's session and tree are resolved
// here.
func (r *router) Route(core.Header, *core.Request) (*core.OpTable, *core.Request, error) {
	return r.table, &core.Request{Version: 0, Level: core.NeedsNothing, Conn: r.conn}, nil
}
