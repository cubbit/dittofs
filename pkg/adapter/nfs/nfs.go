// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/internal/tlsconfig"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the NFS adapter, configured: it validates s, refuses Deps
// missing a required field, naming every one missing, and loads the TLS
// files, refusing what it cannot apply, and opens nothing.
func New(d adapter.Deps, s config.NFS) (adapter.Adapter, error) {
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
