// Package nfs holds the NFS adapter's one public constructor.
package nfs

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the NFS adapter, configured: it validates s, refusing a setting it
// cannot apply, and opens nothing.
func New(d adapter.Deps, s config.NFS) (adapter.Adapter, error) {
	if err := config.Validate(s); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("nfs: the NFS adapter is not built: %w", errors.ErrUnsupported)
}
