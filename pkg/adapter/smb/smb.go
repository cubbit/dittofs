// Package smb holds the SMB adapter's one public constructor.
package smb

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the SMB adapter, configured: it validates s and refuses Deps
// missing a required field, naming every one missing, and opens nothing.
func New(d adapter.Deps, s config.SMB) (adapter.Adapter, error) {
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
	return nil, fmt.Errorf("smb: the SMB adapter is not built: %w", errors.ErrUnsupported)
}
