// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// New builds the NFS adapter, configured: it validates s, refusing a TLS
// mode other than off until StartTLS among the rest, refuses Deps missing a
// required field, naming every one missing, and opens nothing.
func New(d adapter.Deps, s settings.NFS) (adapter.Adapter, error) {
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
	return nil, fmt.Errorf("nfs: the NFS adapter is not built: %w", errors.ErrUnsupported)
}
