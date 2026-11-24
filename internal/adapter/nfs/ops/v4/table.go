package v4

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
)

// The operation codes that bound what some minor version defines: NFSv4.0's
// first, ACCESS (RFC 7530 section 16), through the last of NFSv4.2's,
// REMOVEXATTR (RFC 8276 section 8.4), with NFSv4.1's (RFC 8881 section 18) and
// NFSv4.2's (RFC 7862 section 15) between; and OP_ILLEGAL, whose row answers
// every other code.
const (
	opFirst   = 3     // OP_ACCESS
	opLast    = 75    // OP_REMOVEXATTR
	opIllegal = 10044 // OP_ILLEGAL
)

// lastMinor is the highest minor version the table serves: NFSv4.2.
const lastMinor = 2

// maxOps is the compound cap: a COMPOUND of more operations is refused whole,
// before any of them runs (RFC 20 section 4.3, RFC 21 section 11.5).
//
// decision: the cap is 128 operations, above every compound a supported
// client was seen to send; a session granted fewer is refused at its
// SEQUENCE, not here. The ceiling: a client that sends more is refused whole,
// NFS4ERR_TOO_MANY_OPS, or NFS4ERR_RESOURCE over NFSv4.0. Raise it when a
// client is seen sending more.
const maxOps = 128

// Table is NFS version 4's one table, labelled nfs4, for every minor version:
// its COMPOUND row, procedure 1, whose members' table holds each operation as a
// row naming the minor versions it serves, ILLEGAL among them (RFC 20 section
// 4.8). A procedure it lacks, NULL among them, is PROC_UNAVAIL.
func Table(h *Handlers) *core.OpTable {
	illegal := core.Row[illegalArgs, illegalRes](opIllegal, "ILLEGAL", core.NeedsNothing, h.illegal)
	illegal.Versions = core.Range{First: 0, Last: lastMinor}
	members := core.NewOpTable(memberErrors(), false, illegal)
	members.ReadOp, members.EncodeResult = readOp, encodeResult
	t := core.NewOpTable(compoundErrors(), false, &core.Op{
		Code:       wire.ProcCompound,
		Name:       "COMPOUND",
		Versions:   core.Range{First: 0, Last: lastMinor},
		Needs:      core.NeedsNothing,
		Decode:     decodeCompound,
		Members:    members,
		MaxMembers: maxOps,
	})
	t.Protocol = "nfs4"
	return t
}

// argop is nfs_argop4's head: a member's operation code.
type argop struct {
	Op uint32
}

// resultHead is nfs_resop4's head: a member's operation code and its status.
type resultHead struct {
	Op     uint32
	Status uint32
}

// defined reports whether some minor version defines op.
func defined(op uint32) bool { return opFirst <= op && op <= opLast || op == opIllegal }

// readOp reads a member's operation code, nfs_argop4's argop: a code some
// minor version defines as itself, any other as OP_ILLEGAL, whose row takes
// the rest of the COMPOUND.
//
// decision: readOp reads no minor version, so a code some minor version
// defines reaches its row, whose Versions refuses it under another, ErrVersion,
// which the members' table answers NFS4ERR_OP_ILLEGAL, as it answers ILLEGAL.
// A defined code no row holds yet is ErrNotSupported, NFS4ERR_NOTSUPP, under
// every minor version, until its row lands with its Versions. Overturned if a
// client is seen relying on NFS4ERR_OP_ILLEGAL for such a code.
func readOp(b []byte) (uint32, int, error) {
	var a argop
	n, err := a.DecodeWire(b, 0)
	if err != nil {
		return 0, 0, err
	}
	if !defined(a.Op) {
		a.Op = opIllegal
	}
	return a.Op, n, nil
}

// encodeResult writes a member's result head, nfs_resop4's resop and its
// status: OP_ILLEGAL for an NFS4ERR_OP_ILLEGAL status, so the client reads
// ILLEGAL's result whether the code was undefined or refused under its minor
// version, and for a code no minor version defines, the 0 core passes for a
// code it could not read among them, so a result never names an operation
// that does not exist.
//
// decision: core writes the row's own reply after this head, so a member a
// row's Versions refuses is answered OP_ILLEGAL followed by that row's fail
// arm, which is ILLEGAL4res only while every NFSv4 reply's zero value, its
// fail arm, encodes nothing beside a status that does not select an arm. Every
// nfs_resop4 holds it today. Overturned by a row whose zero reply writes
// bytes, which its own failure vectors show; such a row would need core to
// skip the reply under NFS4ERR_OP_ILLEGAL.
func encodeResult(out *resources.Buf, op uint32, s core.Status, v core.Version) error {
	if s == nfs4ErrOpIllegal || !defined(op) {
		op = opIllegal
	}
	return (&resultHead{Op: op, Status: uint32(s)}).EncodeWire(v, out)
}
