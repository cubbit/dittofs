// Package smb builds the SMB adapter: SMB's one table, its error table, and
// the server that serves them on one TCP listener (RFC 20 §2.5, §4.8).
package smb

import (
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/smb/protocol"
	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// New builds the SMB adapter, configured: it validates s, refuses Deps
// missing a required field, naming every one missing, and returns the server
// whose listener, smb over TCP, binds s.Port at Start. It opens nothing
// (RFC 20 §3.4, §3.6).
func New(d adapter.Deps, s settings.SMB) (*core.Server, error) {
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
		return nil, fmt.Errorf("smb: %s required", strings.Join(missing, ", "))
	}
	// core's own refusals that reach the table, as RFC 22 §16.5 maps them.
	errs := core.NewErrorTable(wire.StatusInternalError,
		core.ErrorRow{Err: core.ErrNotSupported, Status: wire.StatusNotSupported},
		core.ErrorRow{Err: core.ErrDecode, Status: wire.StatusInvalidParameter},
		// A CANCEL is answered nothing (MS-SMB2 3.3.5.16).
		core.ErrorRow{Err: protocol.ErrCancel, NoReply: true},
	)
	// decision: SMB's one table, which does not hold, is built here, not in
	// the ops package RFC 20 §2.5 lays out: it has no row yet, and a package
	// of none would be empty. Empty, it answers every command
	// STATUS_NOT_SUPPORTED. It moves to ops with the first command's row.
	t := core.NewOpTable(errs, false)
	t.Protocol = "smb"
	return core.NewServer(d, s, core.Listener{Services: []string{"smb"}, Network: "tcp", Port: s.Port, Protocol: protocol.New(t)})
}
