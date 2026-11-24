package v4

import (
	"errors"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// errIllegal is ILLEGAL's error: an operation code no minor version defines.
var errIllegal = errors.New("nfs4: an operation code no minor version defines")

// minor40 is NFSv4.0 alone.
var minor40 = &core.Range{First: 0, Last: 0}

// accept is an RPC accept state's status, as the header stage encodes it.
func accept(state uint32) core.Status { return core.Status(wire.StatusAccept | state) }

// compoundErrors is NFSv4's table's error table, which answers a COMPOUND
// refused whole, with no operation: its arguments not decoding GARBAGE_ARGS, a
// procedure the table lacks PROC_UNAVAIL, a minor version not served
// NFS4ERR_MINOR_VERS_MISMATCH, one past the compound cap
// NFS4ERR_TOO_MANY_OPS, and a reply past what it may be NFS4ERR_REP_TOO_BIG,
// each of those two NFS4ERR_RESOURCE over NFSv4.0, which has neither; anything
// else the fault (RFC 21 section 11.5).
func compoundErrors() *core.ErrorTable {
	return core.NewErrorTable(core.Status(wire.NFS4ErrServerFault),
		core.ErrorRow{Err: core.ErrDecode, Status: accept(wire.AcceptGarbageArgs)},
		core.ErrorRow{Err: core.ErrNotSupported, Status: accept(wire.AcceptProcUnavail)},
		core.ErrorRow{Err: core.ErrVersion, Status: core.Status(wire.NFS4ErrMinorVersMismatch)},
		core.ErrorRow{Err: core.ErrTooLong, Status: core.Status(wire.NFS4ErrTooManyOps)},
		core.ErrorRow{Err: core.ErrTooLong, Versions: minor40, Status: core.Status(wire.NFS4ErrResource)},
		core.ErrorRow{Err: core.ErrReplyTooBig, Status: core.Status(wire.NFS4ErrRepTooBig)},
		core.ErrorRow{Err: core.ErrReplyTooBig, Versions: minor40, Status: core.Status(wire.NFS4ErrResource)},
	)
}

// memberErrors is the members' table's error table, which answers one
// operation: arguments that do not decode NFS4ERR_BADXDR, an operation the
// minor version defines and no row serves NFS4ERR_NOTSUPP, and one the minor
// version does not define, by its row's Versions or by ILLEGAL,
// NFS4ERR_OP_ILLEGAL; anything else the fault (RFC 21 section 11.5).
func memberErrors() *core.ErrorTable {
	return core.NewErrorTable(core.Status(wire.NFS4ErrServerFault),
		core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.NFS4ErrBadXDR)},
		core.ErrorRow{Err: core.ErrNotSupported, Status: core.Status(wire.NFS4ErrNotSupp)},
		core.ErrorRow{Err: core.ErrVersion, Status: core.Status(wire.NFS4ErrOpIllegal)},
		core.ErrorRow{Err: errIllegal, Status: core.Status(wire.NFS4ErrOpIllegal)},
	)
}
