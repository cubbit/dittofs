// Package nfs is the NFS adapter: New composes its protocol, its tables and
// core's server, which serves NFS, MOUNT, NLM and NSM on one TCP port (RFC 20
// sections 2.5 and 4.8, RFC 21 section 2.2).
package nfs

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	v4 "github.com/cubbit/dittofs/internal/adapter/nfs/ops/v4"
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
		Protocol: protocol.New(tables(d), d.Budgets),
	})
}

// tables is the five tables RFC 20 section 4.8 names: NFSv4's from its ops
// package, labelled nfs4, and four empty ones, so every procedure of NFSv3,
// MOUNT, NLM and NSM is answered PROC_UNAVAIL, under nfs3, the label of
// NFSv3 and its auxiliary programs (RFC 20 section 10.1).
//
// decision: the four empty tables are built here, not in the ops/ packages
// RFC 20 section 2.5 lays out, since a new package needs a test that can fail
// and a package holding an empty table alone has none. Each table moves to its
// ops package with its first row.
func tables(d adapter.Deps) protocol.Tables {
	aux := errorTable(core.Status(wire.StatusAccept | wire.AcceptSystemErr))
	return protocol.Tables{
		V3:    table("nfs3", errorTable(core.Status(wire.NFS3ErrServerFault))),
		V4:    v4.Table(handlers(d)),
		Mount: table("nfs3", aux),
		NLM:   table("nfs3", aux),
		NSM:   table("nfs3", aux),
	}
}

// hostClock is the pending table's clock: the host's.
type hostClock struct{}

// Now is the host's time.
func (hostClock) Now() time.Time {
	return time.Now()
}

// handlers builds NFSv4's handlers: the service its operations call, the
// constructor of the callbacks value Connect registers per client (RFC 20
// section 3.3), the table of proposals EXCHANGE_ID holds until their confirm
// and what the node is called to a client.
//
// ponytail: nothing sweeps the proposals but the calls that touch the table,
// since core gives a protocol no hook at Stop to end a ticker with; an idle
// node keeps at most the 4 MiB its 4,096 proposals are charged until the next
// EXCHANGE_ID, and after a Stop the charges of the proposals still held never
// come back to the budget. Add the ticker and a release at Stop when core has
// that hook.
func handlers(d adapter.Deps) *v4.Handlers {
	return &v4.Handlers{
		VFS:       d.VFS,
		Callbacks: newCallbacks,
		Sessions:  protocol.NewSessions(d.Budgets, hostClock{}, rand.Reader),
		Server:    v4.Server(d.Server),
	}
}

// errorTable is a program's error table: the two refusals of core
// every program meets, a procedure its table lacks PROC_UNAVAIL and arguments
// that do not decode GARBAGE_ARGS; any other error is fault (RFC 21 section
// 11.5).
func errorTable(fault core.Status) *core.ErrorTable {
	return core.NewErrorTable(
		fault,
		core.ErrorRow{
			Err:    core.ErrNotSupported,
			Status: core.Status(wire.StatusAccept | wire.AcceptProcUnavail),
		},
		core.ErrorRow{
			Err:    core.ErrDecode,
			Status: core.Status(wire.StatusAccept | wire.AcceptGarbageArgs),
		},
	)
}

// table is an empty table labeled label.
func table(label string, errs *core.ErrorTable) *core.OpTable {
	t := core.NewOpTable(errs, false)
	t.Protocol = label
	return t
}
