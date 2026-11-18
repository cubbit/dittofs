// A seed of the adapters' import check: its imports are read, nothing here is built.
// A generated codec imports its protocol's wire, and nothing past what wire may.
package ops

import (
	_ "encoding/binary"

	_ "github.com/cubbit/dittofs/internal/adapter/smb/wire"
	_ "github.com/cubbit/dittofs/pkg/resources"
	_ "github.com/cubbit/dittofs/pkg/vfs"
)
