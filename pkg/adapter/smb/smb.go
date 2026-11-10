// Package smb holds the SMB adapter's one public constructor.
package smb

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the SMB adapter, configured: it validates s and d's required
// fields, refusing what it cannot apply, and opens nothing.
func New(d adapter.Deps, s config.SMB) (adapter.Adapter, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	switch {
	case d.Service == nil:
		return nil, errors.New("smb: Deps.Service is required")
	case d.Budgets == nil:
		return nil, errors.New("smb: Deps.Budgets is required")
	}
	return nil, fmt.Errorf("smb: the SMB adapter is not built: %w", errors.ErrUnsupported)
}
