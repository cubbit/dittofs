package protocol

import (
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var (
	errRPCMismatch = errors.New("nfs: the RPC version is not 2")
	errProgUnavail = errors.New("nfs: no version of the program is served")
	errAuthCred    = errors.New("nfs: the credential is refused")
	errAuthVerf    = errors.New("nfs: the verifier is refused")
	errNotCall     = errors.New("nfs: the message is not a call")
)

// decision: the two high bits of a status name a refusal's layer, since one
// uint32 holds a program's status or an RPC accept or reject state (RFC 20
// section 4.7): 0 a program's status inside an accepted success, 1 an accept
// state, 2 RPC_MISMATCH, 3 AUTH_ERROR with its auth_stat in the low bits. No
// program's status reaches those bits: nfsstat3, nfsstat4, mountstat3, NLM's
// and NSM's codes all lie below 2^16. A row of the RFC 21 plan that names
// these constants carries its status among them.
var (
	statusRPCMismatch  = core.Status(wire.StatusMismatch)
	statusProgUnavail  = core.Status(wire.StatusAccept | wire.AcceptProgUnavail)
	statusProgMismatch = core.Status(wire.StatusAccept | wire.AcceptProgMismatch)
	statusAuthCred     = core.Status(wire.StatusAuth | wire.AuthBadCred)
	statusAuthVerf     = core.Status(wire.StatusAuth | wire.AuthBadVerf)
)

// header is one call, read where it lies: an RPC record holds one message, at
// offset 0. Its methods read the call through the wire's accessors, which the
// parse in Decode has bounded.
//
// decision: a header is a view of its record, one pointer to its Buf, so
// handing it to core as a core.Header allocates nothing, and each call has its
// own, which the calls core runs at once on one connection read apart. It is
// valid while the record's Buf is retained, which core does for a request's
// life but a park's: that it releases once the handler returns ErrParked,
// having taken Final's header, which must not point into the record (RFC 20
// section 4.7). Withdraw the view, and copy the fields out, if core ever reads
// a header after it releases the record.
type header struct{ rec *resources.Buf }

func (h header) call() wire.Call { return wire.Call(h.rec.B) }

func (h header) Op() uint32 { return h.call().Procedure() }

// Version is the call's version, but for NFS version 4: a COMPOUND's minor
// version, and 0 for its other procedures (RFC 20 section 4.7).
func (h header) Version() core.Version {
	c := h.call()
	if minor, ok := c.MinorVersion(); ok {
		return core.Version(minor)
	}
	if c.Program() == wire.ProgNFS && c.Version() == wire.VersNFS4 {
		return 0
	}
	return core.Version(c.Version())
}

func (h header) Next() int     { return 0 }
func (h header) Related() bool { return false }

// compound reports whether h is an NFSv4 COMPOUND.
func (h header) compound() bool {
	_, ok := h.call().MinorVersion()
	return ok
}

// stage is one connection's header stage and router: the served set with its
// tables, the stages' error table, the connection a reply goes through, and
// whether a call on it has been routed yet.
type stage struct {
	served      served
	errs        *core.ErrorTable
	conn        *core.Conn
	bound       *Bound // the connection's sessions; nil without a connection
	established atomic.Bool
}

// Decode reads the call at offset 0 in rec, the only one an RPC record holds:
// nil with the wire's error for a message too short to address a reply, the
// header with it for a call whose later fields do not decode, each wrapped in
// ErrDecode by core (RFC 20 section 4.7). A message that is not a call comes
// back with no error, for Route to drop. Its header's Next is 0, so no other
// offset is asked for.
func (s *stage) Decode(rec *resources.Buf, _ int) (core.Header, error) {
	c, err := wire.ParseCall(rec.B)
	if len(c) == 0 {
		return nil, err
	}
	if errors.Is(err, wire.ErrNotCall) {
		return header{rec}, nil
	}
	return header{rec}, err
}

// Encode writes the reply for s, the two high bits telling a program's status
// from an accept or reject state (RFC 20 section 4.7). A program's status,
// success or not, is the accepted header and what the procedure's result
// holds ahead of its body (wire.AppendStatus): the status word where the
// result begins with one, NLM's cookie ahead of it, nothing for a void
// result; the procedure's encoder writes the rest, a successful COMPOUND's
// status, tag and results among them.
func (s *stage) Encode(h core.Header, st core.Status, out *resources.Buf) error {
	hd, ok := h.(header)
	if !ok {
		return fmt.Errorf("nfs: a header of another protocol")
	}
	c := hd.call()
	xid, layer := c.XID(), uint32(st)&(1<<30-1)
	switch uint32(st) >> 30 {
	case wire.StatusProgram >> 30:
		out.B = wire.AppendAccepted(out.B, xid, wire.AcceptSuccess)
		out.B = wire.AppendStatus(out.B, c, layer)
	case wire.StatusAccept >> 30:
		if layer == wire.AcceptProgMismatch {
			out.B = wire.AppendProgMismatch(out.B, xid, uint32(s.served.low[c.Program()]), uint32(s.served.high[c.Program()]))
			return nil
		}
		out.B = wire.AppendAccepted(out.B, xid, layer)
	case wire.StatusMismatch >> 30:
		out.B = wire.AppendDenied(out.B, xid, true, 0)
	case wire.StatusAuth >> 30:
		out.B = wire.AppendDenied(out.B, xid, false, layer)
	}
	return nil
}

// Chain does nothing: an RPC record holds one message.
func (s *stage) Chain(out *resources.Buf, prev int) error { return nil }

// Args is the call's bytes past its verifier, which for a COMPOUND begin at its
// tag.
func (s *stage) Args(rec *resources.Buf, h core.Header) []byte {
	hd, ok := h.(header)
	if !ok {
		return nil
	}
	return hd.call().Args()
}

// ReplyLen is the most Encode writes for h: the reply header with an empty
// AUTH_NONE verifier and a mismatch's two versions, or the accepted header
// with what wire.AppendStatus writes, an NLM cookie's bytes among them; for a
// COMPOUND, its status, its tag as it came and its results count, with the
// one result's head a reply that fails before or at a member writes beside
// the members' bounds (RFC 20 section 4.7).
func (s *stage) ReplyLen(h core.Header) int {
	hd, ok := h.(header)
	if !ok {
		return wire.MaxReplyHeaderLen
	}
	if hd.compound() {
		return wire.AcceptedLen + wire.StatusLen(hd.call()) + wire.ResultHeadLen
	}
	return max(wire.MaxReplyHeaderLen, wire.AcceptedLen+wire.StatusLen(hd.call()))
}

// Route chooses the table and builds the request, one of the call's own, in
// the order of RFC 20 section 4.1's route row: a message that is not a call
// is dropped, which has no RPC version to judge; then the RPC version, the
// credential and the verifier (RFC 21 section 11.1), then the program, its
// version and, for a COMPOUND alone, its minor version (RFC 20 section 4.8, RFC
// 21 section 2.1). A COMPOUND's request carries a new, empty State; no other
// call's carries one. The connection's first routed call ends its handshake
// deadline.
func (s *stage) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	hd, ok := h.(header)
	if !ok {
		return nil, nil, fmt.Errorf("%w: a header of another protocol", core.ErrDecode)
	}
	c := hd.call()
	if _, err := wire.ParseCall(c); errors.Is(err, wire.ErrNotCall) {
		return nil, nil, errNotCall
	}
	if c.RPCVersion() != 2 {
		return nil, nil, errRPCMismatch
	}

	flavour, sys, err := checkAuth(c)
	if err != nil {
		return nil, nil, err
	}
	prog, vers := c.Program(), c.Version()
	if !s.served.has(prog) {
		return nil, nil, errProgUnavail
	}
	table := s.served.table(prog, vers)
	if table == nil {
		return nil, nil, core.ErrVersion
	}
	minor, compound := c.MinorVersion()
	if compound && !s.served.minor(core.Version(minor)) {
		return table, nil, core.ErrVersion
	}
	if s.conn != nil && s.established.CompareAndSwap(false, true) {
		s.conn.Established()
	}

	var parms *wire.AuthSysParms
	if flavour == wire.AuthSys {
		parms = &sys
	}
	if compound {
		// decision: Route keeps one allocation per call, a COMPOUND's state
		// inside it: the state lives as long as the request, which core holds
		// until the compound's last member has run. Overturned if core ever
		// releases a request before its compound's members finish with the
		// state.
		cr := &compoundRequest{
			req: core.Request{Version: hd.Version(), Level: core.NeedsNothing, Conn: s.conn},
		}
		cr.req.State = &cr.state

		cr.state.Bound = s.bound
		if err := s.build(&cr.req, &cr.gids, flavour, parms, c); err != nil {
			return nil, nil, err
		}
		return table, &cr.req, nil
	}

	r := &callRequest{
		req: core.Request{Version: hd.Version(), Level: core.NeedsNothing, Conn: s.conn},
	}
	if err := s.build(&r.req, &r.gids, flavour, parms, c); err != nil {
		return nil, nil, err
	}

	return table, &r.req, nil
}

// callRequest is a call's request and the group list its Identity's claims
// point into, allocated together.
//
// decision: the claims' groups point into an array held in the request's own
// allocation, so Route stays at one allocation per call whatever the
// credential carries. Overturned if core ever releases a request while a
// service call still reads its Identity.
type callRequest struct {
	req  core.Request
	gids [16]uint32
}

// compoundRequest is a COMPOUND's request, its state and the group list its
// Identity's claims point into, allocated together.
type compoundRequest struct {
	req   core.Request
	state State
	gids  [16]uint32
}

// build sets r's Call and Protocol from the decoded credential and the call's
// program, version and procedure.
func (s *stage) build(
	r *core.Request,
	gids *[16]uint32,
	flavour uint32,
	sys *wire.AuthSysParms,
	c wire.Call,
) error {
	var addr netip.Addr
	if s.conn != nil {
		addr = s.conn.Remote()
	}

	id, err := identity(flavour, sys, addr, gids)
	if err != nil {
		return err
	}

	prog, vers := c.Program(), c.Version()
	r.Call = vfs.Call{Identity: id, Caps: callCaps(prog, vers, c.Procedure())}
	r.Protocol = callProtocol(prog, vers)

	return nil
}

// checkAuth judges the credential and the verifier: AUTH_NONE or a whole
// AUTH_SYS credential, and a verifier of an empty AUTH_NONE, everything else
// refused (RFC 21 section 11.1). It returns the credential's flavour and, for
// AUTH_SYS, the credential it decoded.
func checkAuth(c wire.Call) (flavour uint32, sys wire.AuthSysParms, err error) {
	flavour, body := c.Credential()
	switch flavour {
	case wire.AuthNone:
		if len(body) != 0 {
			return 0, sys, errAuthCred
		}
	case wire.AuthSys:
		if sys, err = wire.DecodeAuthSys(body); err != nil {
			return 0, sys, errAuthCred
		}
	default:
		// decision: RPCSEC_GSS is refused AUTH_BADCRED with every flavour not
		// served. RFC 21 section 10 has a GSS stage take it at unwrap, before
		// Route; with no such stage, the router meets it as any flavour it does
		// not serve and answers it as RFC 21 section 11.1 answers those.
		// Withdrawn by that stage, which the deferral ledger's row "RPCSEC_GSS
		// context table and window; NFSv4.1 slots and reply cache" brings.
		return 0, sys, errAuthCred
	}
	vflavour, vbody := c.Verifier()
	if vflavour != wire.AuthNone || len(vbody) != 0 {
		return 0, sys, errAuthVerf
	}

	return flavour, sys, nil
}

// newErrors is the stages' error table: the refusals above, GARBAGE_ARGS for a
// record that does not decode, and the fault for anything else (RFC 21 section
// 11.5). The reply cache's two answers send nothing: a copy the reply budget
// could not reserve, which the client retries, and a duplicate of a call still
// running, which it retries once the first has ended.
//
// decision: the resources delay row maps every ErrDelay raised before Route,
// which is where this table applies: core reads it only until Route finds the
// program's table and then reads that table's own rows. A delay raised later
// by a row's handler is answered by the fault status, since the program
// tables have no delay row, and later delays of other stages inherit this row
// only where they are raised before Route. An NFSv4 refusal before Route
// that needs the same silence must close the connection as Lookup does,
// since RFC 7530 section 3.1.1 forbids a silent drop on TCP. Overturned if a
// delay of another cause needs an answer its cause does not choose.
func newErrors() *core.ErrorTable {
	return core.NewErrorTable(
		core.Status(wire.StatusAccept|wire.AcceptSystemErr),
		core.ErrorRow{Err: errNotCall, NoReply: true},
		core.ErrorRow{Err: &vfs.Delay{Cause: vfs.DelayResources}, NoReply: true},
		core.ErrorRow{Err: errReplaying, NoReply: true},
		core.ErrorRow{Err: errRPCMismatch, Status: statusRPCMismatch},
		core.ErrorRow{Err: errProgUnavail, Status: statusProgUnavail},
		core.ErrorRow{Err: core.ErrVersion, Status: statusProgMismatch},
		core.ErrorRow{Err: errAuthCred, Status: statusAuthCred},
		core.ErrorRow{Err: errAuthVerf, Status: statusAuthVerf},
		core.ErrorRow{
			Err:    core.ErrDecode,
			Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs),
		},
	)
}
