// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/tlsconfig"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the NFS adapter, configured: it validates s and d's required
// fields and loads the TLS files, refusing what it cannot apply, and opens
// nothing.
func New(d adapter.Deps, s config.NFS) (adapter.Adapter, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	switch {
	case d.VFS == nil:
		return nil, errors.New("nfs: Deps.VFS is required")
	case d.Budgets == nil:
		return nil, errors.New("nfs: Deps.Budgets is required")
	}
	if s.TLS.Mode != "off" {
		if _, err := tlsconfig.ServerConfig(tlsconfig.Config{
			CertFile:   s.TLS.CertFile,
			KeyFile:    s.TLS.KeyFile,
			ClientCA:   s.TLS.ClientCA,
			MinVersion: "1.3",
		}); err != nil {
			return nil, fmt.Errorf("nfs: TLS: %w", err)
		}
	}
	return nil, fmt.Errorf("nfs: the NFS adapter is not built: %w", errors.ErrUnsupported)
}
