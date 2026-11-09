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
	// table. The row's Decode decodes the compound's own arguments, sets
	// Invocation.Count to the members they say follow and returns the bytes
	// they took; the runner then decodes exactly Count members after them,
	// each's code read with Members.ReadOp, and refuses bytes left after the
	// last, before any member runs.
	//
	// decision: a compound row is named by one field and split by one hook.
	// Decode returns the bytes it consumed, Members names the members' table
	// and Invocation.Count the members to expect, so the runner checks the
	// cap, decodes every member and refuses trailing bytes before any runs,
	// as judging a compound whole while draining needs. Reading a member's
	// operation code is the members' table's ReadOp, because its encoding is
	// the protocol's and no stage reads inside a body. Revisit if a
	// protocol's compound cannot state its count before its members.
	Members *OpTable
}

// Invocation is one decoded operation: closures over the decoded request and
// its reply, filled by Bind. A struct, not an interface: Bind is its only
// producer.
//
// decision: four additions proposed from prior servers add no surface, since
// each need is met inside a body. No release hook here: a held request keeps
// its receive buffer retained until it ends, so decoded arguments that point
// into it stay valid across every re-drive. No carrier for RPC PROG_MISMATCH's
// low and high versions: NFS's HeaderCodec.Encode writes them from its own
// tables. No non-waiting reserve for Park: a reserve under a context already
// past its deadline tries once and fails at once. No guard against a
// reservation released twice through a copy: Release zeroes the reservation
// it is called on, and one is moved, never copied. Add the surface when a body
// is found that cannot meet its need this way.
type Invocation struct {
	Bound   func() int                                  // the reply bound
	Resolve func(r *Request) error                      // wire IDs to service values
	Call    func(ctx context.Context, r *Request) error // the handler
	Encode  func(out *resources.Buf, v Version) error
	// Handle is the filehandle the request's own arguments name (a PUTFH's),
	// read before Resolve, which core hands the service to learn a draining
	// request's move target; the service resolves it, core never parses it.
	// nil: the request names none.
	Handle func() (vfs.Handle, bool)
	// Count is a compound row's: the members its arguments say follow, which
	// its Decode sets on what Bind returned. 0 on every other row.
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
// Encode the reply the last Call produced; a decode error builds no
// Invocation.
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
			// A parked handler returns its reply value with ErrParked and
			// fills it later, so the reply is kept whatever the error.
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

// Status is a protocol's wire status: an NFS stat, an RPC accept or reject
// state, an NTSTATUS.
type Status uint32

// Call is one step of the middleware chain: the handler, or what wraps it.
type Call func(ctx context.Context, r *Request) error

// Middleware wraps a Call: recovery, trace span, metrics, deadline, in that
// fixed order.
type Middleware func(next Call) Call

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

// Park holds the request until done. A handler calls it once its call reports
// a wait (a waiting lock's ErrBlocked, a watch set): the service has
// registered the waiter by then. Park refuses a member before the last of a
// chain with ErrNotLast, a connection at Stages.MaxPending with
// ErrTooManyPending, a draining connection with the drain's error (ErrDraining
// or ErrMoved), and a reply pool that cannot take the small-class reservation
// with ErrDelay of DelayResources, answered at once; nothing is sent, and the
// handler withdraws its waiter (CancelLock, Unwatch) and returns the error for
// its table to map, or success if CancelLock reports the lock granted. A
// callback that fires before Park returns is kept by the handler and passed
// to done once it has it. On a connection already closed, Park calls cancel at
// once and done's reply is dropped, as a close would.
//
// Otherwise core sends the interim reply at once, releases the receive
// reservation and charges the reply to a small-class reply reservation; a
// reply that would not fit is answered as its protocol answers an overflow. A
// request already pending past its interim gets no second interim: its slot
// moves from held to parked. A parked request holds no worker and no write
// data and is not counted against the hold floor: MaxPending bounds it. On
// done core encodes the reply, wraps it and sends it under Final's header. A
// client cancel calls cancel first, and done's error goes out as the
// protocol's cancel status. The start of a drain calls cancel, and done's
// error is replaced by the drain's, answered as its table maps it. Either way
// a done(nil), a call that took effect, is answered success. A closed
// connection calls cancel, and done's reply is dropped.
func (r *Request) Park(cancel func()) (done func(err error), err error) {
	return nil, fmt.Errorf("core: park: %w", errors.ErrUnsupported)
}
