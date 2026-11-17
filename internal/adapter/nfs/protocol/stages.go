package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
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
// life. Withdraw the view, and copy the fields out, if core ever releases a
// record before its request ends.
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
	if c.Program() == progNFS && c.Version() == versNFS4 {
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

// stage is one connection's header stage and router: the served set, the
// tables, the stages' error table, the connection a reply goes through, and
// whether a call on it has been routed yet.
type stage struct {
	served      served
	tables      Tables
	errs        *core.ErrorTable
	conn        *core.Conn
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
// from an accept or reject state (RFC 20 section 4.7). A success is the
// accepted header alone: the procedure's encoder writes its result, a
// COMPOUND's status, tag and results among them.
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
		if layer == 0 {
			return nil
		}
		if hd.compound() {
			out.B = wire.AppendCompoundStatus(out.B, layer, c.Tag(), 0)
			return nil
		}
		out.B = binary.BigEndian.AppendUint32(out.B, layer)
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
// AUTH_NONE verifier and a mismatch's two versions, and for a COMPOUND its
// status, its tag as it came and its results count besides (RFC 20 section
// 4.7).
func (s *stage) ReplyLen(h core.Header) int {
	n := 32
	hd, ok := h.(header)
	if !ok || !hd.compound() {
		return n
	}
	tag := len(hd.call().Tag())
	return 24 + 4 + 4 + tag + pad(tag) + 4
}

// pad is XDR's padding of n bytes to four.
func pad(n int) int { return (4 - n%4) % 4 }

// Route chooses the table and builds the request, one of the call's own, in
// the order of RFC 20 section 4.1's route row: a message that is not a call
// is dropped, which has no RPC version to judge; then the RPC version, the
// credential and the verifier (RFC 21 section 11.1), then the program, its
// version and, for a COMPOUND alone, its minor version (RFC 20 section 4.8, RFC
// 21 section 2.1). The connection's first routed call ends its handshake
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
	if err := checkAuth(c); err != nil {
		return nil, nil, err
	}
	prog, vers := c.Program(), c.Version()
	if !s.served.has(prog) {
		return nil, nil, errProgUnavail
	}
	var table *core.OpTable
	switch {
	case prog == progNFS && vers == versNFS3:
		table = s.tables.V3
	case prog == progNFS && vers == versNFS4:
		table = s.tables.V4
	case prog == progMount:
		table = s.tables.Mount
	case prog == progNLM:
		table = s.tables.NLM
	case prog == progNSM:
		table = s.tables.NSM
	}
	if table == nil || !s.served.serves(prog, vers) {
		return nil, nil, core.ErrVersion
	}
	if minor, ok := c.MinorVersion(); ok && !s.served.minor(core.Version(minor)) {
		return table, nil, core.ErrVersion
	}
	if s.conn != nil && s.established.CompareAndSwap(false, true) {
		s.conn.Established()
	}
	return table, &core.Request{Version: hd.Version(), Level: core.NeedsNothing, Conn: s.conn}, nil
}

// checkAuth judges the credential and the verifier: AUTH_NONE or a whole
// AUTH_SYS credential, and a verifier of an empty AUTH_NONE, everything else
// refused (RFC 21 section 11.1).
func checkAuth(c wire.Call) error {
	flavour, body := c.Credential()
	switch flavour {
	case wire.AuthNone:
		if len(body) != 0 {
			return errAuthCred
		}
	case wire.AuthSys:
		if _, err := wire.DecodeAuthSys(body); err != nil {
			return errAuthCred
		}
	default:
		// decision: RPCSEC_GSS is refused AUTH_BADCRED with every flavour not
		// served. RFC 21 section 10 has a GSS stage take it at unwrap, before
		// Route; with no such stage, the router meets it as any flavour it does
		// not serve and answers it as RFC 21 section 11.1 answers those.
		// Withdrawn by that stage, which the deferral ledger's row "RPCSEC_GSS
		// context table and window; NFSv4.1 slots and reply cache" brings.
		return errAuthCred
	}
	vflavour, vbody := c.Verifier()
	if vflavour != wire.AuthNone || len(vbody) != 0 {
		return errAuthVerf
	}
	return nil
}

// newErrors is the stages' error table: the refusals above, GARBAGE_ARGS for a
// record that does not decode, and the fault for anything else (RFC 21 section
// 11.5).
func newErrors() *core.ErrorTable {
	return core.NewErrorTable(core.Status(wire.StatusAccept|wire.AcceptSystemErr),
		core.ErrorRow{Err: errNotCall, NoReply: true},
		core.ErrorRow{Err: errRPCMismatch, Status: statusRPCMismatch},
		core.ErrorRow{Err: errProgUnavail, Status: statusProgUnavail},
		core.ErrorRow{Err: core.ErrVersion, Status: statusProgMismatch},
		core.ErrorRow{Err: errAuthCred, Status: statusAuthCred},
		core.ErrorRow{Err: errAuthVerf, Status: statusAuthVerf},
		core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs)},
	)
}
