// Package core is what every adapter shares: listeners, connections, the
// request pipeline, the compound runner, held requests, the middleware chain
// and the error-table type. A protocol plugs in a framer, its stages and its
// operation tables; core imports no protocol and knows no dialect.
//
// Buf in core's signatures is the pooled buffer of package resources; core
// declares no alias of its own.
package core

import (
	"context"

	"github.com/cubbit/dittofs/pkg/resources"
)

// Protocol is what one listener speaks: ONC RPC for NFS and its auxiliary
// services, SMB2 for SMB. A datagram listener runs the same stages, one record
// per datagram.
type Protocol interface {
	Framer() Framer
	NewConn(c *Conn) Stages // at accept; c is what the stages send on (callbacks)
}

// Framer finds a record's length from its header, and writes a reply
// record's: the writer frames each record after Wrap, so reply buffers hold
// the body alone.
type Framer interface {
	HeaderLen() int
	// BodyLen reads a record's header: the length of the body that follows it,
	// and whether that body ends the record. A header may say it does not, and
	// the reader then reads the next fragment's header and body into the same
	// record; SMB's header always ends it. Past the protocol's cap: an error,
	// and the connection closes.
	BodyLen(hdr []byte) (n int, last bool, err error)
	// MaxRecord is the protocol's record cap and the most fragments a record
	// may come in, 1 where every header ends its record.
	MaxRecord() (bytes, fragments int)
	// Frame appends to dst the HeaderLen-byte header of a record whose body is
	// body bytes.
	Frame(dst []byte, body int) []byte
}

// Stages are a protocol's implementations of the pipeline's stages for one
// connection. core fills each nil stage with its default at accept, so the
// pipeline never checks for nil. A stage is a field, present or nil, never a
// capability discovered by type assertion.
type Stages struct {
	Header     HeaderCodec               // required: decode the request header, encode the reply's
	Router     Router                    // required: the version's table and the Request
	Errors     *ErrorTable               // required: maps, with no operation, a refusal before Route returns a table: Unwrap's, Admit's, Lookup's, a header's
	Security   Security                  // nil: messages pass unchanged
	Flow       FlowControl               // nil: the fixed cap of 64
	Replay     ReplayCache               // nil: no replay check
	Hold       HoldCodec                 // required where a table Holds, nil elsewhere
	MaxPending int                       // requests pending per connection, held and parked together; 0: none pend
	OnClose    func(ctx context.Context) // drop the connection's state; nil: nothing
	Protocol   string                    // required: the label of what a request records before Route returns its table
}

// HeaderCodec decodes a request's header and encodes a reply's.
type HeaderCodec interface {
	Decode(rec *resources.Buf, off int) (Header, error) // the header at off in the record: 0, then each member's Next
	// Encode writes the reply's header for s and, where s is a failure
	// answered with a status alone, what such a reply needs: the whole reply
	// of a request answered its status alone, before its arguments decoded or
	// refused whole after, such as an NFSv4 COMPOUND's status, tag and empty
	// results, the tag read with the header.
	Encode(h Header, s Status, out *resources.Buf) error
	Chain(out *resources.Buf, prev int) error // points the reply at prev to the one appended after it (SMB2's NextCommand, padded to 8); nil-op where nothing chains
	Args(rec *resources.Buf, h Header) []byte // the arguments of the message h in rec, as Decode and Unwrap left it
	ReplyLen(h Header) int                    // the most Encode writes for h, a status-alone body included, with Chain's padding and the transform Wrap adds in place: the pipeline reserves Bound() + ReplyLen(h); for a compound row's message it also covers the compound's head and one result's head
}

// Router chooses the version's table and builds the Request.
type Router interface {
	Route(h Header, prev *Request) (*OpTable, *Request, error) // prev: the member before a related one, else nil
}

// Security verifies or decrypts a request and signs or encrypts its reply.
type Security interface {
	// Unwrap decrypts or verifies; an encrypted SMB message yields its inner
	// header. prev is the member before a related one, else nil: the IDs a
	// related member inherits (SMB's all-ones SessionId) are taken from it
	// before the check, so the check never runs against an unresolved ID.
	Unwrap(h Header, rec *resources.Buf, prev Header) (Header, error)
	Wrap(h Header, reply *resources.Buf) error // sign or encrypt, in place; a chain once, joined
}

// FlowControl admits a request against the connection's slots, credits or cap.
type FlowControl interface {
	Admit(ctx context.Context, h Header) (release func(), err error) // slot, credit or cap; the reader waits here
}

// ReplayCache answers a retransmission from the reply it cached.
type ReplayCache interface {
	// Lookup returns a fresh copy of a cached reply, charged to a reply
	// reservation of its size that waits under ctx, as the pipeline's own
	// reservation does; the pipeline wraps the copy in place and sends it. Or
	// a sequence error, or the reservation's ErrDelay of DelayResources past
	// ctx's deadline, either mapped by the table; or nil, nil to run.
	Lookup(ctx context.Context, h Header) (*resources.Buf, error)
	// Store keeps its own clear copy of a cacheable row's encoded reply, taken
	// before Wrap; the caller's reply is then wrapped in place.
	//
	// ponytail: the copies Store keeps for NFSv3's and NFSv4.0's duplicate
	// request cache and for SMB's replay are charged to no budget; only
	// NFSv4.1's slot cache is charged. Charge them to the connection-state
	// budget when a replay cache's size shows in a profile.
	Store(h Header, reply *resources.Buf)
}

// Header is a protocol's decoded header. core reads it only through these
// methods; the protocol's codec owns every field.
type Header interface {
	Op() uint32       // the code the operation table is keyed by
	Version() Version // the version or dialect the router chose
	Next() int        // offset of the next message chained in the record; 0 for the last
	Related() bool    // takes its handles from the member before it (SMB's related flag)
}
