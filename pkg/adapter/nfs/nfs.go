// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/tlsconfig"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the NFS adapter, configured: it validates s and loads its TLS
// files, refusing a setting it cannot apply, and opens nothing.
func New(d adapter.Deps, s config.NFS) (adapter.Adapter, error) {
	if err := config.Validate(s); err != nil {
		return nil, err
	}
	if s.TLS.CertFile != "" {
		if _, err := tlsconfig.ServerConfig(tlsconfig.Config{
			CertFile:   s.TLS.CertFile,
			KeyFile:    s.TLS.KeyFile,
			ClientCA:   s.TLS.ClientCA,
			MinVersion: s.TLS.MinVersion,
		}); err != nil {
			return nil, fmt.Errorf("nfs: TLS: %w", err)
		}
	}
	return nil, fmt.Errorf("nfs: the NFS adapter is not built: %w", errors.ErrUnsupported)
}
