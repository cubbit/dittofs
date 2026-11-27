package protocol

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// ErrorTable is a program's error table at fault: a procedure its table lacks
// answered PROC_UNAVAIL, arguments that do not decode GARBAGE_ARGS, a
// security flavour the share refuses, answered AUTH_ERROR AUTH_TOOWEAK; and
// ErrMoved and ErrDraining with no reply at all: an error is final to the
// application, and an unanswered request is replayed after the reconnect (RFC
// 20 section 3.1).
// ErrNotLast, ErrTooManyPending and ErrCancelled never reach NFS, which parks
// and holds nothing, and ErrLogonFailure is met only by an RPCSEC_GSS
// context's creation, which runs no operation: all four are mapped to the
// fault should one arrive. Any other error is the fault too (RFC 21 sections
// 10 and 11.5).
//
// rows are the program's own beyond those. Only the NFSv3 family's tables
// (NFSv3, MOUNT, NLM and NSM) are built here; NFSv4 keeps its own two, since
// its security refusal is NFS4ERR_WRONGSEC, not AUTH_TOOWEAK (RFC 21 section
// 11.5).
func ErrorTable(fault core.Status, rows ...core.ErrorRow) *core.ErrorTable {
	return core.NewErrorTable(fault, append([]core.ErrorRow{
		{
			Err:    core.ErrNotSupported,
			Status: core.Status(wire.StatusAccept | wire.AcceptProcUnavail),
		},
		{Err: core.ErrDecode, Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs)},
		{Err: core.ErrMoved, NoReply: true},
		{Err: core.ErrDraining, NoReply: true},
		// decision: a flavour the share refuses is answered AUTH_ERROR
		// AUTH_TOOWEAK, not knfsd's NFS3ERR_ACCES: applications see EACCES
		// either way, and the difference is the line Linux logs, which names
		// the flavour as the cause rather than the file's permissions, and
		// that diagnostic is the whole reason for the choice (RFC 21 section
		// 10). Revisit if a supported NFSv3 client is shown to handle
		// AUTH_TOOWEAK worse than NFS3ERR_ACCES.
		{Err: vfs.ErrWrongSecurity, Status: core.Status(wire.StatusAuth | wire.AuthTooWeak)},
		{Err: core.ErrNotLast, Status: fault},
		{Err: core.ErrTooManyPending, Status: fault},
		{Err: core.ErrCancelled, Status: fault},
		{Err: vfs.ErrLogonFailure, Status: fault},
	}, rows...)...)
}
