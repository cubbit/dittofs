// Package smb holds the SMB adapter's one public constructor.
package smb

import (
	"github.com/cubbit/dittofs/internal/adapter/smb"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
)

// New builds the SMB adapter, configured: it validates s and refuses Deps
// missing a required field, naming every one missing, and opens nothing. It
// is one call into internal/adapter/smb, whose refusal is a nil adapter.
func New(d adapter.Deps, s settings.SMB) (adapter.Adapter, error) {
	srv, err := smb.New(d, s)
	if err != nil {
		return nil, err
	}
	return srv, nil
}
