// Package smb holds the SMB adapter's one public constructor.
package smb

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/config"
)

// New builds the SMB adapter, configured: it validates s, refusing a setting it
// cannot apply, and opens nothing.
func New(d adapter.Deps, s config.SMB) (adapter.Adapter, error) {
	if err := config.Validate(s); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("smb: the SMB adapter is not built: %w", errors.ErrUnsupported)
}
