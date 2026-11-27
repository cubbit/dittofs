package v4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The SP4_* values of state_protect_how4 (RFC 8881 section 18.35.1).
const (
	sp4None     = 0
	sp4MachCred = 1
	sp4SSV      = 2
)

// exchangeIDArgs is EXCHANGE_ID4args (RFC 8881 section 18.35.1).
type exchangeIDArgs struct {
	Owner   clientOwner
	Flags   uint32
	Protect stateProtectA
	ImplID  []implID `xdr:"max=1"`
}

// clientOwner is client_owner4: the client's boot verifier and its name.
type clientOwner struct {
	Verifier [8]byte
	OwnerID  []byte `xdr:"max=1024"` // NFS4_OPAQUE_LIMIT
}

// stateProtectOps is state_protect_ops4: the operations a machine credential
// must be used for, and may be used for. The bitmaps' bound is the client
// record's: three words, which hold every operation code.
type stateProtectOps struct {
	MustEnforce []uint32 `xdr:"max=3"`
	MustAllow   []uint32 `xdr:"max=3"`
}

// secOID is sec_oid4, an opaque.
type secOID struct {
	OID []byte `xdr:"max=1024"`
}

// ssvSPParms is ssv_sp_parms4. The adapter refuses SP4_SSV, but the arguments
// must still decode for it to say so.
type ssvSPParms struct {
	Ops        stateProtectOps
	HashAlgs   []secOID `xdr:"max=16"`
	EncrAlgs   []secOID `xdr:"max=16"`
	Window     uint32
	NumHandles uint32
}

// stateProtectA is state_protect4_a: the protection the client asks for.
type stateProtectA struct {
	How     uint32          `xdr:"union"`
	_       struct{}        `xdr:"case=0"` // SP4_NONE
	MachOps stateProtectOps `xdr:"case=1"` // SP4_MACH_CRED
	SSV     ssvSPParms      `xdr:"case=2"` // SP4_SSV
}

// stateProtectR is state_protect4_r: SP4_NONE or SP4_MACH_CRED echoed, never
// SP4_SSV, which is refused.
type stateProtectR struct {
	How     uint32          `xdr:"union"`
	_       struct{}        `xdr:"case=0"` // SP4_NONE
	MachOps stateProtectOps `xdr:"case=1"` // SP4_MACH_CRED
}

// implID is nfs_impl_id4.
type implID struct {
	Domain string `xdr:"max=1024"`
	Name   string `xdr:"max=1024"`
	Date   nfsTime
}

// nfsTime is nfstime4.
type nfsTime struct {
	Seconds  int64
	NSeconds uint32
}

// serverOwner is server_owner4.
type serverOwner struct {
	MinorID uint64
	MajorID []byte `xdr:"max=1024"`
}

// exchangeIDRes is EXCHANGE_ID4res past its status: EXCHANGE_ID4resok on a
// success, its fail arm void.
type exchangeIDRes struct {
	Ok *exchangeIDResOk `xdr:"arm"`
}

// exchangeIDResOk is EXCHANGE_ID4resok.
type exchangeIDResOk struct {
	ClientID   uint64
	SequenceID uint32
	Flags      uint32
	Protect    stateProtectR
	Owner      serverOwner
	Scope      []byte   `xdr:"max=1024"`
	ImplID     []implID `xdr:"max=1"`
}

// The EXCHANGE_ID flags this adapter reads and sets (RFC 8881 section
// 18.35.1).
const (
	flagUpdConfirmedRecA = 0x40000000 // EXCHGID4_FLAG_UPD_CONFIRMED_REC_A
	flagConfirmedR       = 0x80000000 // EXCHGID4_FLAG_CONFIRMED_R
	flagUseNonPNFS       = 0x00010000 // EXCHGID4_FLAG_USE_NON_PNFS
)

// argFlags is every eia_flags bit a client may set: SUPP_MOVED_REFER,
// SUPP_MOVED_MIGR, BIND_PRINC_STATEID, the pNFS mask and UPD_CONFIRMED_REC_A.
// Any bit outside it is NFS4ERR_INVAL (RFC 8881 section 18.35.3), CONFIRMED_R
// too, which the RFC says is always off in what a client sends.
const argFlags = 0x00000103 | 0x00070000 | flagUpdConfirmedRecA

// recoverStatus is the recovery kind of an NFSv4.1 client: its loss of state
// is told it by a status.
//
// ponytail: the value of the open state's RecoverStatus, which pkg/vfs does
// not export yet; a test pins the two together. Replace it by the exported
// name when pkg/vfs has one.
const recoverStatus vfs.RecoveryKind = 1

// minorOne is the minor version byte of the boot verifier an NFSv4.1 client
// confirms under.
const minorOne = 1

// bootOf is the Boot a client's verifier stands for under minor version
// minor: the first eight bytes of the SHA-256 of the verifier and the minor
// version's byte.
//
// decision: the minor version is part of the Boot so that a client confirmed
// over NFSv4.0 and one confirmed over NFSv4.1 under one name are a reboot of
// each other, as the one record kind says (RFC 21 section 3.5), and open
// state's Boot is eight bytes, with no room for the minor version beside the
// verifier. Overturned if open state's record gains a field for it.
func bootOf(verifier [8]byte, minor byte) [8]byte {
	sum := sha256.Sum256(append(verifier[:], minor))

	return [8]byte(sum[:8])
}

// ExchangeID answers EXCHANGE_ID (RFC 8881 section 18.35): a confirmed owner
// with the same verifier and principal is answered its client ID again, any
// other is a proposal the adapter holds for CREATE_SESSION to confirm. It
// writes nothing: the service's Propose and Record change no state.
func (h *Handlers) ExchangeID(
	ctx context.Context,
	r *core.Request,
	a exchangeIDArgs,
) (exchangeIDRes, error) {
	mach := a.Protect.How == sp4MachCred

	switch {
	case a.Flags&^argFlags != 0:
		return exchangeIDRes{}, errBadFlags
	case a.Protect.How == sp4SSV:
		return exchangeIDRes{}, errEncrAlgUnsupp
	case mach && !provesMachine(r.Call.Identity):
		return exchangeIDRes{}, errInval
	}

	update := a.Flags&flagUpdConfirmedRecA != 0

	confirm, err := h.Sessions.NewConfirm()
	if err != nil {
		return exchangeIDRes{}, fmt.Errorf("draw a confirm: %w", err)
	}

	seq := protocol.SequenceOf(confirm) - 1
	rec := wire.ClientRec{
		Kind: wire.ClientRecKindNFS4,
		NFS4: wire.ClientRecNFS4{
			Minor:       minorOne,
			SessionSeq:  seq,
			MustEnforce: a.Protect.MachOps.MustEnforce,
			MustAllow:   a.Protect.MachOps.MustAllow,
		},
	}

	adapter, err := protocol.WriteRecord(rec)
	if err != nil {
		return exchangeIDRes{}, err
	}

	info := vfs.ClientInfo{
		Protocol: vfs.ProtocolNFSv4,
		Name:     bytes.Clone(a.Owner.OwnerID),
		Boot:     bootOf(a.Owner.Verifier, minorOne),
		Protect:  mach,
		Adapter:  adapter,
		Recovery: recoverStatus,
	}

	// decision: every EXCHANGE_ID goes through Propose, since open state
	// cannot read a confirmed record by name, so the answers that need that
	// read are not the RFC's: under UPD_CONFIRMED_REC_A a new verifier is
	// NOENT, not NOT_SAME, and a caller that does not prove the credential of
	// a protected owner is PERM, not NOT_SAME. Open state refuses that caller
	// the protected name, so no later check of the stored record is needed.
	// Overturned when open state reads a confirmed record by name.
	proposal, err := h.VFS.Propose(ctx, r.Call, info)
	if errors.Is(err, &vfs.StateError{Kind: vfs.StateClientInUse}) && update {
		return exchangeIDRes{}, errPerm
	}

	if err != nil {
		return exchangeIDRes{}, err
	}

	stored, err := h.VFS.Record(ctx, r.Call, proposal.Client)

	switch {
	case errors.Is(err, &vfs.StateError{Kind: vfs.StateStaleClient}):
		if update {
			return exchangeIDRes{}, errNoEnt
		}

		return h.propose(info, proposal, confirm, a)
	case err != nil:
		return exchangeIDRes{}, err
	}

	return h.confirmed(r, stored, update, a)
}

// provesMachine reports whether the caller is proved by a ticket under
// integrity or privacy, the only proof SP4_MACH_CRED has (RFC 8881 section
// 18.35.3; RFC 21 section 11.4).
func provesMachine(id vfs.Identity) bool {
	return id.Proof >= vfs.ProofTicket && id.Protection >= vfs.ProtectionIntegrity
}

// propose holds a new owner's proposal and answers its client ID, with the
// sequence ID the confirm's CREATE_SESSION must carry.
func (h *Handlers) propose(
	info vfs.ClientInfo,
	proposal vfs.Proposal,
	confirm [8]byte,
	a exchangeIDArgs,
) (exchangeIDRes, error) {
	err := h.Sessions.Propose(info.Name, protocol.Pending{
		Proposal: proposal,
		Minor:    minorOne,
		Confirm:  confirm,
		Info:     info,
	})
	if err != nil {
		return exchangeIDRes{}, err
	}

	return h.reply(uint64(proposal.Client), protocol.SequenceOf(confirm), flagUseNonPNFS, a), nil
}

// confirmed answers an owner the service confirmed already, from its record:
// the client ID and the next sequence ID, CONFIRMED_R set.
func (h *Handlers) confirmed(
	r *core.Request,
	stored vfs.ClientRecord,
	update bool,
	a exchangeIDArgs,
) (exchangeIDRes, error) {
	rec, err := protocol.ReadRecord(stored.ID, stored.Adapter)
	if err != nil {
		return exchangeIDRes{}, err
	}

	flags := uint32(flagConfirmedR | flagUseNonPNFS)

	return h.reply(uint64(stored.ID), rec.NFS4.SessionSeq+1, flags, a), nil
}

// withOp returns the bitmap with the bit of operation op set, without changing
// the one it was given.
func withOp(bitmap []uint32, op int) []uint32 {
	out := make([]uint32, max(len(bitmap), op/32+1))
	copy(out, bitmap)
	out[op/32] |= 1 << (op % 32)

	return out
}

// reply is EXCHANGE_ID4resok: the client ID and sequence ID, the protection
// echoed, and the server owner and scope every node of the installation
// answers alike, the node's own minor ID apart (RFC 21 section 3.3).
func (h *Handlers) reply(id uint64, seq, flags uint32, a exchangeIDArgs) exchangeIDRes {
	installation := h.Server.Installation[:]
	protect := stateProtectR{How: a.Protect.How}

	// decision: the reply names CREATE_SESSION among the operations to
	// enforce, whatever list the client sent, since CREATE_SESSION refuses a
	// caller that is not the owner's proved machine credential even where the
	// list lacks it; RFC 8881 section 18.35.3 lets the reply differ from the
	// request. The record keeps the list as sent. Overturned when
	// CREATE_SESSION enforces only the operations the list names.
	if a.Protect.How == sp4MachCred {
		protect.MachOps = stateProtectOps{
			MustEnforce: withOp(a.Protect.MachOps.MustEnforce, opCreateSession),
			MustAllow:   a.Protect.MachOps.MustAllow,
		}
	}

	return exchangeIDRes{Ok: &exchangeIDResOk{
		ClientID:   id,
		SequenceID: seq,
		Flags:      flags,
		Protect:    protect,
		Owner:      serverOwner{MinorID: h.Server.Node, MajorID: installation},
		Scope:      installation,
	}}
}
