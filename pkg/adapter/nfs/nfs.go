// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"github.com/cubbit/dittofs/internal/adapter/nfs"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// New builds the NFS adapter, configured: it validates s, refusing a TLS
// mode other than off until StartTLS among the rest, refuses Deps missing a
// required field, naming every one missing, and opens nothing. A refusal is a
// nil adapter with its error.
func New(d adapter.Deps, s settings.NFS) (adapter.Adapter, error) {
	srv, err := nfs.New(d, s)
	if err != nil {
		return nil, err
	}
	return srv, nil
}
