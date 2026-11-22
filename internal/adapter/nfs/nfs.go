// Package nfs is the NFS adapter: New composes its protocol, its tables and
// core's server, which serves NFS, MOUNT, NLM and NSM on one TCP port (RFC 20
// sections 2.5 and 4.8, RFC 21 section 2.2).
package nfs

import (
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// New builds the NFS adapter, configured: it validates s, refusing a TLS mode
// other than off until StartTLS among the rest, so no certificate file is
// loaded; refuses Deps missing VFS or Budgets, naming each missing, core
// refusing a nil Budgets.Conns itself; builds every table whatever s's range,
// since each connection's router serves its own (RFC 20 section 4.8); and
// opens nothing. Start binds the one TCP listener on s.Port. No listener reads
// UDP: core has no datagram listener, and NLM and NSM over UDP wait for it
// (RFC 21 section 2.2).
func New(d adapter.Deps, s settings.NFS) (*core.Server, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var missing []string
	if d.VFS == nil {
		missing = append(missing, "Deps.VFS")
	}
	if d.Budgets == nil {
		missing = append(missing, "Deps.Budgets")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("nfs: %s required", strings.Join(missing, ", "))
	}
	return core.NewServer(d, s, core.Listener{
		Services: []string{"nfs", "mount", "nlm", "nsm"},
		Network:  "tcp",
		Port:     s.Port,
		Protocol: protocol.New(tables()),
	})
}

// tables is the five tables RFC 20 section 4.8 names, each empty, so every
// procedure is answered PROC_UNAVAIL, under the label of its program: nfs4
// for NFSv4, nfs3 for NFSv3 and its auxiliary programs (RFC 20 section 10.1).
//
// decision: the empty tables are built here, not in the ops/ packages RFC 20
// section 2.5 lays out, since a new package needs a test that can fail and a
// package holding an empty table alone has none. Each table moves to its ops
// package with its first row.
func tables() protocol.Tables {
	aux := errorTable(core.Status(wire.StatusAccept | wire.AcceptSystemErr))
	return protocol.Tables{
		V3: table("nfs3", errorTable(core.Status(wire.NFS3ErrServerFault))),
		V4: table("nfs4", errorTable(core.Status(wire.NFS4ErrServerFault),
			core.ErrorRow{Err: core.ErrVersion, Status: core.Status(wire.NFS4ErrMinorVersMismatch)})),
		Mount: table("nfs3", aux),
		NLM:   table("nfs3", aux),
		NSM:   table("nfs3", aux),
	}
}

// errorTable is a program's error table: rows, and the two refusals of core
// every program meets, a procedure its table lacks PROC_UNAVAIL and arguments
// that do not decode GARBAGE_ARGS; any other error is fault (RFC 21 section
// 11.5).
func errorTable(fault core.Status, rows ...core.ErrorRow) *core.ErrorTable {
	return core.NewErrorTable(fault, append(rows,
		core.ErrorRow{Err: core.ErrNotSupported, Status: core.Status(wire.StatusAccept | wire.AcceptProcUnavail)},
		core.ErrorRow{Err: core.ErrDecode, Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs)},
	)...)
}

// table is an empty table labeled label.
func table(label string, errs *core.ErrorTable) *core.OpTable {
	t := core.NewOpTable(errs, false)
	t.Protocol = label
	return t
}
