// Package nfs is the NFS adapter: New composes its protocol, its tables and
// core's server, which serves NFS, MOUNT, NLM and NSM on one TCP port (RFC 20
// sections 2.5 and 4.8, RFC 21 section 2.2).
package nfs

import (
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/ops/mount"
	"github.com/cubbit/dittofs/internal/adapter/nfs/ops/nlm"
	"github.com/cubbit/dittofs/internal/adapter/nfs/ops/nsm"
	v3 "github.com/cubbit/dittofs/internal/adapter/nfs/ops/v3"
	v4 "github.com/cubbit/dittofs/internal/adapter/nfs/ops/v4"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
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
		Protocol: protocol.New(tables(d)),
	})
}

// tables is the five tables RFC 20 section 4.8 names: NFSv4's and NFSv3's
// from their ops packages, labelled nfs4 and nfs3, and the auxiliary
// programs' under nfs3, the label of NFSv3 and its auxiliary programs (RFC 20
// section 10.1).
func tables(d adapter.Deps) protocol.Tables {
	return protocol.Tables{
		V3:    v3.Table(),
		V4:    v4.Table(&v4.Handlers{VFS: d.VFS}),
		Mount: mount.Table(),
		NLM:   nlm.Table(),
		NSM:   nsm.Table(),
	}
}
