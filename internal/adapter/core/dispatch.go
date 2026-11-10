package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// OpTable maps an operation code to its row, for one program and version.
type OpTable struct {
	Ops    map[uint32]*Op
	Errors *ErrorTable // per operation table, so NFSv3 and NFSv4 map differently
	Holds  bool        // the table holds and re-drives what its protocol holds, by error and cause; else every error is answered at once
	// ReadOp reads the operation code at the head of a compound member's bytes,
	// and how many bytes it took: NFSv4's argop. Set on a table that is a
	// compound row's Members; nil elsewhere.
	ReadOp func(b []byte) (op uint32, n int, err error)
}

// Op is one operation's row: its preconditions and versions are columns the
// pipeline checks, never checks in its handler.
type Op struct {
	Code      uint32
	Name      string
	Versions  Range // first and last version or dialect served
	Needs     Needs // nothing < negotiated < session < tree; plus a current filehandle
	Cacheable bool
	Role      Role                                                            // what the operation always does to state; per request: Invocation.Role
	Expired   bool                                                            // runs on an expired credential: re-authentication, logoff
	Decode    func(body []byte, v Version) (inv Invocation, n int, err error) // n: the bytes of body it consumed
	// Members is non-nil on a compound row (NFSv4's COMPOUND): its members'
	// table, which the runner decodes from after the bytes Decode consumed.
	//
	// decision: a compound row is named by this field and split by
	// Invocation.Count and OpTable.ReadOp, so the runner can check the cap and
	// refuse trailing bytes before any member runs. Revisit if a protocol's
	// compound cannot state its count before its members.
	Members *OpTable
}

// Invocation is one decoded operation: closures over the decoded request and
// its reply. A struct, not an interface. Bind produces it for every row but a
// compound row, whose Decode the protocol writes and which builds its
// Invocation directly, setting Count from the arguments it decoded.
type Invocation struct {
	Bound   func() int                                  // the reply bound
	Resolve func(r *Request) error                      // wire IDs to service values
	Call    func(ctx context.Context, r *Request) error // the handler
	// Encode runs on every request that gets a final reply, so its reply may
	// be nil: the request refused before Call, or a handler that returned
	// (nil, err). An encoder accepts a nil reply and encodes the status alone.
	// A handler may return a reply with its error, as an NFSv3 failure carries
	// its WCC data: Call keeps whatever reply the handler returned.
	Encode func(out *resources.Buf, v Version) error
	// Handle is the filehandle the request's own arguments name (a PUTFH's),
	// read before Resolve, which core hands the service to learn a draining
	// request's move target; the service resolves it, core never parses it.
	// nil: the request names none.
	Handle func() (vfs.Handle, bool)
	// Count is a compound row's: the members its arguments say follow, set by
	// that row's own Decode. 0 on every other row.
	Count int
	// Role says what this request does to state: an SMB LOCK whose elements
	// unlock releases it, a WRITE to an open under a break flushes it, an
	// NFSv4 OPEN that claims a delegated open flushes it. It is called in two
	// cases only. Judging a single-reply compound whole while draining, the
	// runner calls it before Resolve, and it reads only the decoded arguments
	// and the state an explicit stateid among them names. Everywhere else it
	// is called after Resolve, and only while draining or on an expired
	// credential, so a request served normally pays for no second lookup.
	// nil: Op.Role decides alone.
	Role func(r *Request) Role
}

// Role is what a request does to state the service waits on. It decides what
// still runs while draining and on an expired credential.
type Role uint8

const (
	RoleNone    Role = iota // refused while draining and on an expired credential
	RoleContext             // sets a compound's context or only reads attributes (SEQUENCE, PUTFH, GETATTR, RENEW): served while draining
	RoleFlush               // flushes state the service waits on: served while draining
	RoleRelease             // releases it (CLOSE, an unlock, a break acknowledgement, DELEGRETURN): served while draining and on an expired credential
)

// Handler is the function for one wire operation. It makes at most one
// service call that changes state, and reads what is per request from r.
type Handler[Req, Rep any] func(ctx context.Context, r *Request, req *Req) (*Rep, error)

// Bind joins a row's codec pair and handler, type-checked against each other,
// and returns the row's Decode. The Invocation it returns hands Bound,
// Resolve, Role, Handle, Call and Encode the one request dec decoded, and
// Encode the reply the last Call returned, with or without its error, and the
// version Encode is given; a decode error builds no Invocation.
func Bind[Req, Rep any](
	dec func(b []byte, v Version, req *Req) (n int, err error), // n: the bytes of b it consumed, which Decode returns
	bound func(req *Req) int,
	resolve func(r *Request, req *Req) error, // nil: the operation names no wire ID
	role func(r *Request, req *Req) Role, // nil: Op.Role decides alone
	handle func(req *Req) (vfs.Handle, bool), // nil: the arguments name no filehandle
	h Handler[Req, Rep],
	enc func(rep *Rep, v Version, out *resources.Buf) error,
) func([]byte, Version) (Invocation, int, error) {
	return func(b []byte, v Version) (Invocation, int, error) {
		req := new(Req)
		n, err := dec(b, v, req)
		if err != nil {
			return Invocation{}, 0, err
		}
		var rep *Rep
		inv := Invocation{
			Bound: func() int { return bound(req) },
			Resolve: func(r *Request) error {
				if resolve == nil {
					return nil
				}
				return resolve(r, req)
			},
			Call: func(ctx context.Context, r *Request) error {
				var err error
				rep, err = h(ctx, r, req)
				return err
			},
			Encode: func(out *resources.Buf, v Version) error { return enc(rep, v, out) },
		}
		if role != nil {
			inv.Role = func(r *Request) Role { return role(r, req) }
		}
		if handle != nil {
			inv.Handle = func() (vfs.Handle, bool) { return handle(req) }
		}
		return inv, n, nil
	}
}

// NewOpTable builds a table and checks it: codes unique, ranges well formed,
// and no ErrorRow.Hold unless holds.
func NewOpTable(errs *ErrorTable, holds bool, ops ...*Op) *OpTable {
	panic("not implemented")
}

// Version is a protocol's version, minor version or dialect; Range a closed
// span of them. For NFSv4 a Version is the minor version, so Range{0, 0} is
// NFSv4.0 alone.
type Version uint32

// Range is a closed span of versions.
type Range struct{ First, Last Version }

// Needs is an operation's ordered precondition, plus a flag for a current
// filehandle.
type Needs uint8

const (
	NeedsNothing Needs = iota
	NeedsNegotiated
	NeedsSession
	NeedsTree
	NeedsFH Needs = 1 << 7 // or-ed with one of the above
)

// Request is one message as the router built it. A handler reads Call and its
// protocol's State; core reads Level and the rest.
type Request struct {
	Call    vfs.Call // built by the Router; the handler passes it to every service call
	Version Version  // the version or dialect the router chose
	Level   Needs    // what this message's session and tree reached; Op.Needs is checked against it
	Conn    *Conn    // what a handler keeps state on (Keep) and the stages send callbacks on
	// State is the protocol's own: SMB's session, tree, capabilities and
	// FileId table and the chain's current open; NFS's session and current
	// filehandle. The Router sets it, from prev for a related member; core
	// carries it and never reads it.
	State any
}

// Park holds the request until done: core sends the interim, and answers
// done's result under Final's header, as a cancel, a drain or a close
// replaces it. It refuses with ErrNotLast, ErrTooManyPending or the drain's
// error, and the handler then withdraws the waiter its call registered.
func (r *Request) Park(cancel func()) (done func(err error), err error) {
	return nil, fmt.Errorf("core: park: %w", errors.ErrUnsupported)
}
