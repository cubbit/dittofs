package v4

import (
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// Every operation code whose ARGUMENTS XDR names CURRENT_FH reads the
// current filehandle and answers NFS4ERR_MOVED to a move, written by hand
// from the RFCs, not from movedOps: the operations' XDR tables (RFC 8881
// section 18, RFC 7530 section 16) write /* CURRENT_FH: ... */ in the
// arguments of each of these, GETFH, LOOKUPP, READLINK and SAVEFH above a void,
// OPEN's and WANT_DELEGATION's claims over the file they name, SECINFO_NO_
// NAME's own style, RECLAIM_COMPLETE's one_fs, and OPEN_CONFIRM, RFC
// 7530's alone, which 4.1 keeps for 4.0 only. The setters and restorers
// (PUTFH, PUTPUBFH, PUTROOTFH, RESTOREFH, which writes SAVED_FH), the
// client- and session-keeping operations and every code the minor versions
// do not define name none and answer a move with silence.
func TestMovedOpsAreEveryOperationThatReadsTheCurrentFilehandle(t *testing.T) {
	reads := map[uint32]string{
		3: "ACCESS", 4: "CLOSE", 5: "COMMIT", 6: "CREATE", 8: "DELEGRETURN",
		9: "GETATTR", 10: "GETFH", 11: "LINK", 12: "LOCK", 13: "LOCKT",
		14: "LOCKU", 15: "LOOKUP", 16: "LOOKUPP", 17: "NVERIFY", 18: "OPEN",
		19: "OPENATTR", 20: "OPEN_CONFIRM", 21: "OPEN_DOWNGRADE",
		25: "READ", 26: "READDIR", 27: "READLINK", 28: "REMOVE",
		29: "RENAME", 32: "SAVEFH", 33: "SECINFO", 34: "SETATTR",
		37: "VERIFY", 38: "WRITE", 46: "GET_DIR_DELEGATION",
		48: "GETDEVICELIST", 49: "LAYOUTCOMMIT", 50: "LAYOUTGET",
		51: "LAYOUTRETURN", 52: "SECINFO_NO_NAME", 56: "WANT_DELEGATION",
		58: "RECLAIM_COMPLETE",
	}

	table := memberErrors()

	for code := uint32(0); code <= 70; code++ {
		s, reply, _ := table.Map(core.ErrMoved, &core.Op{Code: code}, 1)

		name, reading := reads[code]

		if reading {
			if !reply || s != core.Status(wire.NFS4ErrMoved) {
				t.Errorf(
					"op %d (%s) answered %#x, reply %v, want NFS4ERR_MOVED",
					code,
					name,
					uint32(s),
					reply,
				)
			}

			continue
		}

		if reply {
			t.Errorf(
				"op %d, whose XDR names no CURRENT_FH, answered %#x, want no reply",
				code,
				uint32(s),
			)
		}
	}
}
