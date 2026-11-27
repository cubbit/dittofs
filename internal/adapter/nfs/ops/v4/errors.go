package v4

import (
	"errors"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// errIllegal is ILLEGAL's error: an operation code no minor version defines.
var errIllegal = errors.New("nfs4: an operation code no minor version defines")

// The refusals EXCHANGE_ID answers that no service error stands for, each its
// own status (RFC 8881 section 18.35.4).
var (
	errPerm          = errors.New("nfs4: another principal than the client's")
	errNoEnt         = errors.New("nfs4: no confirmed client of that owner")
	errInval         = errors.New("nfs4: a machine credential not proved by integrity or privacy")
	errEncrAlgUnsupp = errors.New("nfs4: SP4_SSV is not served")
)

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
	return core.NewErrorTable(
		core.Status(wire.NFS4ErrServerFault),
		core.ErrorRow{Err: core.ErrDecode, Status: accept(wire.AcceptGarbageArgs)},
		core.ErrorRow{Err: core.ErrNotSupported, Status: accept(wire.AcceptProcUnavail)},
		core.ErrorRow{Err: core.ErrVersion, Status: core.Status(wire.NFS4ErrMinorVersMismatch)},
		core.ErrorRow{Err: core.ErrTooLong, Status: core.Status(wire.NFS4ErrTooManyOps)},
		core.ErrorRow{
			Err:      core.ErrTooLong,
			Versions: minor40,
			Status:   core.Status(wire.NFS4ErrResource),
		},
		core.ErrorRow{Err: core.ErrReplyTooBig, Status: core.Status(wire.NFS4ErrRepTooBig)},
		core.ErrorRow{
			Err:      core.ErrReplyTooBig,
			Versions: minor40,
			Status:   core.Status(wire.NFS4ErrResource),
		},
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
		core.ErrorRow{Err: errPerm, Status: core.Status(wire.NFS4ErrPerm)},
		core.ErrorRow{Err: errNoEnt, Status: core.Status(wire.NFS4ErrNoEnt)},
		core.ErrorRow{Err: errInval, Status: core.Status(wire.NFS4ErrInval)},
		core.ErrorRow{Err: errEncrAlgUnsupp, Status: core.Status(wire.NFS4ErrEncrAlgUnsupp)},
		core.ErrorRow{
			Err:    &vfs.StateError{Kind: vfs.StateClientInUse},
			Status: core.Status(wire.NFS4ErrClientIDInUse),
		},
		core.ErrorRow{
			Err:    &vfs.Delay{Cause: vfs.DelayResources},
			Status: core.Status(wire.NFS4ErrDelay),
		},
	)
}
