package v4

import (
	"context"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The CREATE_SESSION flag this adapter grants, and the flavours it can call a
// client under (RFC 8881 section 18.36.1; RFC 5531 section 8.1).
const (
	flagBackChan = 0x00000002 // CREATE_SESSION4_FLAG_CONN_BACK_CHAN
	flavourNone  = 0
	flavourSys   = 1
)

// The shortest fore channel a session may be granted: the reply to a lone
// SEQUENCE, which any session's first call must be able to answer. RFC 8881
// section 18.36.3 gives no number, so this is the minimal size derived from
// the XDR: the accepted RPC reply 24 bytes, the COMPOUND result with an empty
// tag 12, and the SEQUENCE result 44, 80 in all. A longer verifier, as under
// RPCSEC_GSS, makes the reply larger, so 80 is a floor.
const minForeResponse = 80

// maxDecisions bounds how often one CREATE_SESSION decides again after the
// record it read changed under it.
const maxDecisions = 3

// channelAttrs is channel_attrs4 (RFC 8881 section 18.36.1).
type channelAttrs struct {
	HeaderPad  uint32
	MaxRequest uint32
	MaxResp    uint32
	MaxCached  uint32
	MaxOps     uint32
	MaxReqs    uint32
	RDMAIRD    []uint32 `xdr:"max=1"`
}

// authSysParms is authsys_parms: the credential a back channel is called
// under (RFC 5531 section 8.2).
type authSysParms struct {
	Stamp   uint32
	Machine string `xdr:"max=255"`
	UID     uint32
	GID     uint32
	GIDs    []uint32 `xdr:"max=16"`
}

// gssCBHandles is gss_cb_handles4, which the adapter reads and does not use.
type gssCBHandles struct {
	Service    uint32
	FromServer []byte `xdr:"max=1024"`
	FromClient []byte `xdr:"max=1024"`
}

// callbackSecParms is callback_sec_parms4: one way the client may be called.
type callbackSecParms struct {
	Flavour uint32       `xdr:"union"`
	_       struct{}     `xdr:"case=0"` // AUTH_NONE
	Sys     authSysParms `xdr:"case=1"` // AUTH_SYS
	GSS     gssCBHandles `xdr:"case=6"` // RPCSEC_GSS
}

// createSessionArgs is CREATE_SESSION4args.
type createSessionArgs struct {
	ClientID uint64
	Sequence uint32
	Flags    uint32
	Fore     channelAttrs
	Back     channelAttrs
	Program  uint32
	SecParms []callbackSecParms `xdr:"max=16"`
}

// createSessionRes is CREATE_SESSION4res past its status.
type createSessionRes struct {
	Ok *createSessionResOk `xdr:"arm"`
}

// createSessionResOk is CREATE_SESSION4resok.
type createSessionResOk struct {
	SessionID [16]byte
	Sequence  uint32
	Flags     uint32
	Fore      channelAttrs
	Back      channelAttrs
}

// CreateSession answers CREATE_SESSION (RFC 8881 section 18.36; RFC 21
// section 3.1): it builds the session the confirmed client's next sequence
// ID asks for, writes the client record's sequence and reply with it, and
// answers a repeat of a sequence the record holds its stored reply.
func (h *Handlers) CreateSession(
	ctx context.Context,
	r *core.Request,
	a createSessionArgs,
) (createSessionRes, error) {
	if a.Fore.MaxOps == 0 || a.Fore.MaxResp < minForeResponse {
		return createSessionRes{}, errTooSmall
	}

	for range maxDecisions {
		res, again, err := h.decide(ctx, r, a)
		if !again {
			return res, err
		}
	}

	return createSessionRes{}, &vfs.Delay{Cause: vfs.DelayConflict}
}

// decide reads the client record, or else the proposal held for it, and
// answers by the sequence rule: again is true when the state it read changed
// under it and it must be decided afresh.
func (h *Handlers) decide(
	ctx context.Context,
	r *core.Request,
	a createSessionArgs,
) (res createSessionRes, again bool, err error) {
	cl := vfs.ClientID(a.ClientID)

	rec, err := h.VFS.Record(ctx, r.Call, cl)

	switch {
	case err == nil:
		return h.fromRecord(ctx, r, rec, a)
	case !errors.Is(err, &vfs.StateError{Kind: vfs.StateStaleClient}):
		return createSessionRes{}, false, err
	}

	p, ok := h.Sessions.Confirm(cl)
	if !ok || p.Minor == 0 {
		return createSessionRes{}, false, &vfs.StateError{Kind: vfs.StateStaleClient}
	}

	return h.fromProposal(ctx, r, p, a)
}

// fromRecord decides over a client the service holds confirmed. The stored
// sequence ID with its reply is a repeat, answered the reply; the next one
// builds.
func (h *Handlers) fromRecord(
	ctx context.Context,
	r *core.Request,
	stored vfs.ClientRecord,
	a createSessionArgs,
) (createSessionRes, bool, error) {
	rec, err := protocol.ReadRecord(stored.ID, stored.Adapter)
	if err != nil {
		return createSessionRes{}, false, err
	}

	switch {
	case rec.NFS4.Minor == 0:
		return createSessionRes{}, false, &vfs.StateError{Kind: vfs.StateStaleClient}
	case stored.Protect && !provesMachine(r.Call.Identity):
		return createSessionRes{}, false, errAccess
	}

	switch a.Sequence {
	case rec.NFS4.SessionSeq:
		return replay(stored.ID, rec)
	case rec.NFS4.SessionSeq + 1:
	default:
		return createSessionRes{}, false, errSeqMisordered
	}

	info := vfs.ClientInfo{
		Protocol: stored.Protocol,
		Name:     stored.Name,
		Boot:     stored.Boot,
		Protect:  stored.Protect,
		Recovery: stored.Recovery,
	}

	return h.build(ctx, r, a, base{info: info, prev: stored.Adapter, rec: rec})
}

// replay answers a sequence ID the record has applied with the reply it
// stored, byte for byte: none stored is a sequence ID out of order.
//
// decision: a replay is a pure answer: it binds no connection and registers no
// callbacks, since RFC 21 section 3.5 has a resent sequence ID answered the
// stored reply and nothing written. The new connection reaches the fore channel
// through SEQUENCE; BIND_CONN_TO_SESSION binds it to the back channel, and
// BACKCHANNEL_CTL registers the callbacks again. Known exception: when attach's
// Connect failed after AddSession, a resend is answered OK with the callbacks
// still unregistered. Overturned when a replay re-registers them.
func replay(id vfs.ClientID, rec wire.ClientRec) (createSessionRes, bool, error) {
	if len(rec.NFS4.SessionReply) == 0 {
		return createSessionRes{}, false, errSeqMisordered
	}

	var ok createSessionResOk
	if _, err := ok.DecodeWire(rec.NFS4.SessionReply, 1); err != nil {
		return createSessionRes{}, false, fmt.Errorf("decode the stored reply of %d: %w", id, err)
	}

	return createSessionRes{Ok: &ok}, false, nil
}

// fromProposal decides over a client its EXCHANGE_ID proposed on this node:
// only the sequence ID the proposal answered builds.
func (h *Handlers) fromProposal(
	ctx context.Context,
	r *core.Request,
	p protocol.Pending,
	a createSessionArgs,
) (createSessionRes, bool, error) {
	rec, err := protocol.ReadRecord(p.Proposal.Client, p.Info.Adapter)
	if err != nil {
		return createSessionRes{}, false, err
	}

	switch {
	case p.Info.Protect && !provesMachine(r.Call.Identity):
		return createSessionRes{}, false, errAccess
	case a.Sequence != rec.NFS4.SessionSeq+1:
		return createSessionRes{}, false, errSeqMisordered
	}

	return h.build(ctx, r, a, base{
		info:     p.Info,
		prev:     p.Info.Adapter,
		rec:      rec,
		proposal: &p.Proposal,
	})
}

// build makes the session: it takes its grants and pool charge first, so a
// refusal for the pool is answered before the client record is touched, then
// confirms the proposal and writes the record's sequence ID and reply, and
// ends the session again on any failure before the record is written, which
// returns its charge. Once the record holds the reply the session stays: a
// replay finds it there.
func (h *Handlers) build(
	ctx context.Context,
	r *core.Request,
	a createSessionArgs,
	b base,
) (createSessionRes, bool, error) {
	cl := vfs.ClientID(a.ClientID)
	spec := h.spec(a, b)

	sess, err := h.Sessions.Create(cl, spec)
	if err != nil {
		return createSessionRes{}, false, err
	}

	ok := resultOf(a, sess)

	next, err := h.next(b.rec, a.Sequence, ok)
	if err != nil {
		h.Sessions.End(sess.ID)

		return createSessionRes{}, false, err
	}

	b.info.Adapter = next

	if b.proposal != nil {
		again, err := h.confirm(ctx, r, cl, b)
		if err != nil || again {
			h.Sessions.End(sess.ID)

			return createSessionRes{}, again, err
		}
	}

	stored, err := h.VFS.AddSession(ctx, r.Call, cl, b.prev, next)
	if err != nil {
		h.Sessions.End(sess.ID)

		return createSessionRes{}, errors.Is(
			err,
			&vfs.StateError{Kind: vfs.StateRecordChanged},
		), err
	}

	h.Sessions.Drop(cl)

	if err := h.settle(ctx, r, cl, sess.Ref, stored.Reach.Gen); err != nil {
		return createSessionRes{}, false, err
	}

	if err := h.attach(ctx, r, sess, b.info); err != nil {
		return createSessionRes{}, false, err
	}

	return createSessionRes{Ok: &ok}, false, nil
}

// confirm confirms the proposal with Connect, once: a stale answer is a
// client the record already holds, which the caller decides again over, or
// else one that is gone, answered stale. A client name another principal
// holds is refused, and its proposal stays for the principal it belongs to.
func (h *Handlers) confirm(
	ctx context.Context,
	r *core.Request,
	cl vfs.ClientID,
	b base,
) (again bool, err error) {
	info := b.info
	info.Proposal = b.proposal

	_, _, err = h.VFS.Connect(ctx, r.Call, info, h.Callbacks(cl, vfs.ProtocolNFSv4))

	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, &vfs.StateError{Kind: vfs.StateStaleClient}):
		return false, err
	}

	_, err = h.VFS.Record(ctx, r.Call, cl)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, &vfs.StateError{Kind: vfs.StateStaleClient}):
		return false, err
	}

	h.Sessions.Drop(cl)

	return false, err
}

// spec is what the registry grants a session of the arguments' channels, the
// back channel's callback chosen, and the record's protection kept with it.
func (h *Handlers) spec(a createSessionArgs, b base) protocol.Spec {
	sp := protocol.Spec{
		Fore:        channelOf(a.Fore),
		Back:        channelOf(a.Back),
		Protect:     b.info.Protect,
		MustEnforce: b.rec.NFS4.MustEnforce,
		MustAllow:   b.rec.NFS4.MustAllow,
	}

	if a.Flags&flagBackChan != 0 {
		sp.Callback = callbackOf(a)
	}

	return sp
}

// channelOf is the registry's Channel for the attributes a client asked.
func channelOf(c channelAttrs) protocol.Channel {
	return protocol.Channel{
		Slots:       c.MaxReqs,
		MaxRequest:  c.MaxRequest,
		MaxResponse: c.MaxResp,
		MaxCached:   c.MaxCached,
		MaxOps:      c.MaxOps,
	}
}

// callbackOf is how the client asked to be called, or nil where it named
// nothing the adapter can call.
//
// decision: the first AUTH_SYS entry of csa_sec_parms is taken, else the first
// AUTH_NONE, else the back channel is refused and CONN_BACK_CHAN cleared:
// RPCSEC_GSS handles are not used, and the client may bind a back channel
// later with BIND_CONN_TO_SESSION (RFC 8881 section 18.36.3). Overturned when
// the adapter calls under RPCSEC_GSS.
func callbackOf(a createSessionArgs) *protocol.Callback {
	for _, sp := range a.SecParms {
		if sp.Flavour != flavourSys {
			continue
		}

		var buf resources.Buf
		if err := sp.Sys.EncodeWire(1, &buf); err != nil {
			continue
		}

		return &protocol.Callback{
			Minor:   1,
			Program: a.Program,
			Flavour: flavourSys,
			Cred:    buf.B,
		}
	}

	for _, sp := range a.SecParms {
		if sp.Flavour == flavourNone {
			return &protocol.Callback{Minor: 1, Program: a.Program, Flavour: flavourNone}
		}
	}

	return nil
}

// resultOf is CREATE_SESSION4resok for sess: its ID, the sequence ID applied,
// the channels as granted, and CONN_BACK_CHAN where the client can be called.
// PERSIST and RDMA are never granted.
func resultOf(a createSessionArgs, sess *protocol.Session) createSessionResOk {
	var flags uint32
	if sess.Back != nil {
		flags = flagBackChan
	}

	return createSessionResOk{
		SessionID: sess.ID,
		Sequence:  a.Sequence,
		Flags:     flags,
		Fore:      attrsOf(sess.Fore),
		Back:      attrsOf(sess.BackAttrs),
	}
}

// attrsOf is channel_attrs4 for a granted channel.
func attrsOf(c protocol.Channel) channelAttrs {
	return channelAttrs{
		MaxRequest: c.MaxRequest,
		MaxResp:    c.MaxResponse,
		MaxCached:  c.MaxCached,
		MaxOps:     c.MaxOps,
		MaxReqs:    c.Slots,
	}
}

// next is the record's adapter bytes after the build: its sequence ID and the
// reply, which a repeat is answered from on any node.
func (h *Handlers) next(
	rec wire.ClientRec,
	seq uint32,
	ok createSessionResOk,
) ([]byte, error) {
	var buf resources.Buf
	if err := ok.EncodeWire(1, &buf); err != nil {
		return nil, fmt.Errorf("encode the reply to store: %w", err)
	}

	rec.NFS4.SessionSeq = seq
	rec.NFS4.SessionReply = buf.B

	return protocol.WriteRecord(rec)
}

// settle sends the Disconnect the service asks for when, once the record
// lists this node, the node holds no session of the client: its last ended
// while the call ran. It repeats while the service answers a newer entry.
//
// The entry's Gen is the record's Reach.Gen: the record AddSession returns
// lists this node last with the Gen the service just took, so the two are the
// node's own entry's, and the adapter needs no node ID of the service's kind
// to read it by.
func (h *Handlers) settle(
	ctx context.Context,
	r *core.Request,
	cl vfs.ClientID,
	ref vfs.SessionRef,
	gen uint64,
) error {
	for gen != 0 && !h.Sessions.Holds(cl) {
		next, err := h.VFS.Disconnect(ctx, r.Call, cl, ref, vfs.SessionEnd{Last: true, Gen: gen})
		if errors.Is(err, &vfs.StateError{Kind: vfs.StateStaleClient}) {
			return nil
		}

		if err != nil {
			return err
		}

		gen = next
	}

	return nil
}

// attach binds the connection the call came on to the session's fore channel,
// and to its back channel where the client asked to be called, registering
// the callbacks again so the service sees the back channel the session has.
func (h *Handlers) attach(
	ctx context.Context,
	r *core.Request,
	sess *protocol.Session,
	info vfs.ClientInfo,
) error {
	if st, ok := r.State.(*protocol.State); ok && st.Bound != nil {
		h.Sessions.Bind(sess, st.Bound)
	}

	if sess.Back == nil || r.Conn == nil {
		return nil
	}

	sess.Back.Bind(r.Conn)

	_, _, err := h.VFS.Connect(ctx, r.Call, info, h.Callbacks(sess.Client, vfs.ProtocolNFSv4))

	return err
}
