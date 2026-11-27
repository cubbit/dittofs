// A seed of the adapters' import check, read and never built: a handler test
// may read a reply's bytes.
package ops

import (
	"encoding/binary"

	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
)

var _ = binary.LittleEndian
var _ wire.Encoder
