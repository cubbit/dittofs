package v4

import (
	"errors"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// errIllegal is ILLEGAL's error: an operation code no minor version defines.
var errIllegal = errors.New("nfs4: an operation code no minor version defines")

// minor40 is NFSv4.0 alone.
var minor40 = &core.Range{First: 0, Last: 0}

// accept is an RPC accept state's status, as the header stage encodes it.
func accept(state uint32) core.Status { return core.Status(wire.StatusAccept | state) }

// movedOps lists the NFSv4 operation codes whose arguments name the current
// filehandle: over them the members' table answers a move NFS4ERR_MOVED,
// everywhere else no reply (RFC 8881 section 18, RFC 7530 section 16).
//
// decision: the list is every code those two RFCs name CURRENT_FH in the
// arguments of, whether or not its row has landed, so a later row that adds
// such an operation does not touch this list. NFSv4.2's operations that read
// the current filehandle (RFC 7862) are not in it: no 4.2 member row exists,
// and the first 4.2 operation row to land carries its code into the list;
// until then the list holds none. Overturned only by a revision moving the filehandle out
// of an operation's arguments.
var movedOps = []uint32{
	3, 4, 5, 6, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 25, 26,
	27, 28, 29, 32, 33, 34, 37, 38, 46, 48, 49, 50, 51, 52, 56, 58,
}

// compoundErrors is NFSv4's table's error table, which answers a COMPOUND
// refused whole, with no operation: its arguments not decoding GARBAGE_ARGS, a
// procedure the table lacks PROC_UNAVAIL, a minor version not served
// NFS4ERR_MINOR_VERS_MISMATCH, one past the compound cap
// NFS4ERR_TOO_MANY_OPS, and a reply past what it may be NFS4ERR_REP_TOO_BIG,
// each of those two NFS4ERR_RESOURCE over NFSv4.0, which has neither;
// ErrNotLast, ErrTooManyPending and ErrCancelled, which never reach NFS (it
// parks and holds nothing), and a failed logon at the fault; and a moved
// filehandle or the adapter draining with no reply at all; anything else the
// fault (RFC 21 section 11.5).
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
		core.ErrorRow{Err: core.ErrMoved, NoReply: true},
		core.ErrorRow{Err: core.ErrDraining, NoReply: true},
		core.ErrorRow{Err: core.ErrNotLast, Status: core.Status(wire.NFS4ErrServerFault)},
		core.ErrorRow{Err: core.ErrTooManyPending, Status: core.Status(wire.NFS4ErrServerFault)},
		core.ErrorRow{Err: core.ErrCancelled, Status: core.Status(wire.NFS4ErrServerFault)},
		core.ErrorRow{Err: vfs.ErrLogonFailure, Status: core.Status(wire.NFS4ErrServerFault)},
	)
}

// memberErrors is the members' table's error table, which answers one
// operation: arguments that do not decode NFS4ERR_BADXDR, an operation the
// minor version defines and no row serves NFS4ERR_NOTSUPP, one the minor
// version does not define, by its row's Versions or by ILLEGAL,
// NFS4ERR_OP_ILLEGAL, a member that needs the current filehandle and finds
// none NFS4ERR_NOFILEHANDLE, and a move the member's own
// NFS4ERR_MOVED, elsewhere no reply; and, as any table's, ErrNotLast,
// ErrTooManyPending and ErrCancelled, which never reach NFS (it parks and
// holds nothing), and a failed logon at the fault, and a move elsewhere or the
// adapter draining with no reply (RFC 21 section 11.5).
func memberErrors() *core.ErrorTable {
	return core.NewErrorTable(core.Status(wire.NFS4ErrServerFault),
		core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.NFS4ErrBadXDR)},
		core.ErrorRow{Err: core.ErrNotSupported, Status: core.Status(wire.NFS4ErrNotSupp)},
		core.ErrorRow{Err: core.ErrVersion, Status: core.Status(wire.NFS4ErrOpIllegal)},
		core.ErrorRow{Err: errIllegal, Status: core.Status(wire.NFS4ErrOpIllegal)},
		core.ErrorRow{Err: core.ErrNoFH, Status: core.Status(wire.NFS4ErrNoFileHandle)},
		core.ErrorRow{Err: core.ErrMoved, Ops: movedOps, Status: core.Status(wire.NFS4ErrMoved)},
		core.ErrorRow{Err: core.ErrMoved, NoReply: true},
		core.ErrorRow{Err: core.ErrDraining, NoReply: true},
		core.ErrorRow{Err: core.ErrNotLast, Status: core.Status(wire.NFS4ErrServerFault)},
		core.ErrorRow{Err: core.ErrTooManyPending, Status: core.Status(wire.NFS4ErrServerFault)},
		core.ErrorRow{Err: core.ErrCancelled, Status: core.Status(wire.NFS4ErrServerFault)},
		core.ErrorRow{Err: vfs.ErrLogonFailure, Status: core.Status(wire.NFS4ErrServerFault)},
	)
}
