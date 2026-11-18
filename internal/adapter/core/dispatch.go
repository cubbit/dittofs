package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// OpTable maps an operation code to its row, for one program and version.
type OpTable struct {
	Ops    map[uint32]*Op
	Errors *ErrorTable // per operation table, so NFSv3 and NFSv4 map differently
	Holds  bool        // the table holds and re-drives what its protocol holds, by error and cause; else every error is answered at once
	// Protocol is the label of every series this table's requests record from
	// Route on (§10.1), set after NewOpTable as ReadOp is; a server refuses a
	// table without one at the first Route that returns it. A compound row's
	// Members take their parent's.
	//
	// decision: the label is a field the protocol sets, not a name NewOpTable
	// is given or a registry it resolves into: core first sees a table when
	// Route returns it, and two servers may share one table. Overturned if a
	// table is built per server.
	Protocol string
	// ReadOp reads the operation code at the head of a compound member's bytes,
	// and how many bytes it took: NFSv4's argop. Its error is its own, which
	// the runner wraps (ErrDecode). Set on a table that is a compound row's
	// Members; nil elsewhere.
	ReadOp func(b []byte) (op uint32, n int, err error)
	// EncodeResult writes a member's operation code and status before the
	// member's own Encode appends its reply: NFSv4's nfs_resop4 head. Set on
	// a compound row's Members; nil elsewhere.
	EncodeResult func(out *resources.Buf, op uint32, s Status, v Version) error
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
	// MaxMembers is a compound row's cap on its members: a Count past it is
	// refused ErrTooLong before any member is decoded. NewOpTable refuses a
	// row with Members and none.
	//
	// decision: the cap is a column of the compound row, read by the runner,
	// since a cap each compound's Decode checked would be a check a protocol
	// can forget. Overturned if a protocol's cap must vary per request.
	MaxMembers int
}

// Invocation is one decoded operation: closures over the decoded request and
// its reply. A struct, not an interface. Bind produces it for every row but a
// compound row, whose Decode the protocol writes and which builds its
// Invocation directly, setting Count and EncodeHead from the arguments it
// decoded.
type Invocation struct {
	// Bound is the reply bound for room: the operation's own reply, without
	// ReplyLen(h). A READ's row clamps its count in its hook so the bound
	// fits room, never lowered below one byte, and reports ok false where
	// room cannot hold one byte, which refuses the compound (RFC 20 §4.3).
	// Every other row ignores room and reports ok. core first calls it with
	// the record cap less ReplyLen(h), and a compound's runner again with
	// each member's room, so a count is only ever lowered.
	Bound   func(r *Request, room int) (n int, ok bool)
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
	// EncodeHead is a compound row's, set by that row's own Decode over what
	// it decoded: it writes the compound reply's head, ahead of its results,
	// NFSv4's status, the request's tag and the count of results. Its size
	// must not depend on s or n, so the runner writes it once before the
	// members run and again, over its own bytes, once their status and count
	// are known. nil on every other row.
	EncodeHead func(out *resources.Buf, s Status, n int, v Version) error
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
	bound func(r *Request, req *Req, room int) (n int, ok bool), // Invocation.Bound: a READ's clamps its count in req to room, never below one byte, ok false where room cannot hold one; any other ignores room
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
			Bound: func(r *Request, room int) (int, bool) { return bound(r, req, room) },
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
// each name, lower-cased, lower-case letters, digits and underscores alone,
// no ErrorRow.Hold unless holds, and, where it holds, a row answering
// ErrCancelled for every operation and version (one naming neither Ops nor
// Versions that replies at once with a status other than the fault or
// success) and no row for it, whatever its Ops or Versions, that holds, sends
// no reply or answers the fault or success; and, where it holds, the same of
// a typed row for &vfs.Delay{Cause: vfs.DelayResources}, read among the typed
// rows alone.
//
// decision: a table that fails a check panics, naming the row, as
// NewErrorTable does: a table is program text and NewOpTable returns no
// error. Overturned if a table is ever built from configuration.
func NewOpTable(errs *ErrorTable, holds bool, ops ...*Op) *OpTable {
	t := &OpTable{Ops: make(map[uint32]*Op, len(ops)), Errors: errs, Holds: holds}
	for _, op := range ops {
		if prev, dup := t.Ops[op.Code]; dup {
			panic(fmt.Sprintf("core: op table: %s and %s share code %d", prev.Name, op.Name, op.Code))
		}
		if op.Versions.First > op.Versions.Last {
			panic(fmt.Sprintf("core: op table: %s serves versions %d to %d, first past last", op.Name, op.Versions.First, op.Versions.Last))
		}
		if !labelName(op.Name) {
			panic(fmt.Sprintf("core: op table: %q, lower-cased, is not lower-case letters, digits and underscores alone", op.Name))
		}
		if op.Members != nil && op.MaxMembers <= 0 {
			panic(fmt.Sprintf("core: op table: %s has members and no MaxMembers", op.Name))
		}
		if m := op.Members; m != nil && (m.ReadOp == nil || m.EncodeResult == nil) {
			panic(fmt.Sprintf("core: op table: %s's members table lacks ReadOp or EncodeResult", op.Name))
		}
		t.Ops[op.Code] = op
	}
	// A compound is refused whole, with no operation, past its cap or past a
	// quarter of the reply pool's ceiling, so a table with a compound row
	// maps both refusals for every operation and version; one without needs
	// neither, since nothing else raises them.
	if slices.ContainsFunc(ops, func(op *Op) bool { return op.Members != nil }) {
		for _, err := range []error{ErrTooLong, ErrReplyTooBig} {
			if !errs.mapsEverywhere(err) {
				panic(fmt.Sprintf("core: op table: it has a compound row, and no row of its error table answers %v for every operation and version", err))
			}
		}
	}
	// A row that sends no reply answers a request no final reply follows,
	// which holds nothing (§4.6), so no row both holds and sends no reply.
	for _, r := range errs.rows {
		switch {
		case r.Hold != nil && !holds:
			panic(fmt.Sprintf("core: op table: error row %s holds, in a table that does not", describe(r)))
		case r.Hold != nil && r.NoReply:
			panic(fmt.Sprintf("core: op table: error row %s holds and sends no reply", describe(r)))
		}
	}
	// A cancel ends a hold or a park of any operation in any version, and is
	// answered (§6.5), so a table that holds maps ErrCancelled with a row
	// naming neither, and no row for it holds, sends no reply or answers the
	// fault or success.
	if holds {
		if r, ok := errs.badRow(ErrCancelled); ok {
			panic(fmt.Sprintf("core: op table: it holds, and error row %s matches ErrCancelled but holds, sends no reply or answers the fault or success", describe(r)))
		}
		if !errs.mapsEverywhere(ErrCancelled) {
			panic("core: op table: it holds, and no row of its error table answers ErrCancelled for every operation and version")
		}
		// A node pool's refusal, the hold pool's among them, is ErrDelay of
		// DelayResources, answered at once and never held (§6.3), so it is
		// mapped as ErrCancelled is, by a typed row. The value also matches
		// the holding ErrDelay row, which every typed row outranks (§4.7), so
		// only typed rows are read.
		typed, resources := errs.typedOnly(), &vfs.Delay{Cause: vfs.DelayResources}
		if r, ok := typed.badRow(resources); ok {
			panic(fmt.Sprintf("core: op table: it holds, and error row %s matches ErrDelay of DelayResources but holds, sends no reply or answers the fault or success", describe(r)))
		}
		if !typed.mapsEverywhere(resources) {
			panic("core: op table: it holds, and no typed row of its error table answers ErrDelay of DelayResources for every operation and version")
		}
	}
	return t
}

// labelName says whether name, lower-cased, is a value of the op label:
// lower-case letters, digits and underscores (§10.1). The empty name is
// refused as well, which that set admits vacuously: a row without one has no
// label to record, and nothing to be named by when a check panics.
func labelName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range strings.ToLower(name) {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
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

	// What the pipeline keeps with the request for its row's chain: the
	// handler the chain ends in, and the stage clock's times around the
	// call, resolve_ids' start and the call's end, from which map_error is
	// timed (§10.1).
	invoke       Call
	resolveStart time.Time
	callEnd      time.Time
	trace        trace.TraceID // every log line for the request carries it
}

// Park holds the request until done: core sends the interim, and answers
// done's result under Final's header, as a cancel, a drain or a close
// replaces it. It refuses with ErrNotLast, ErrTooManyPending or the drain's
// error, and the handler then withdraws the waiter its call registered.
func (r *Request) Park(cancel func()) (done func(err error), err error) {
	return nil, fmt.Errorf("core: park: %w", errors.ErrUnsupported)
}
