// A seed of the adapters' import check, read and never built: a command file
// names wire's constants and calls the service, and touches no byte.
package ops

import (
	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
	_ "github.com/cubbit/dittofs/pkg/vfs"
)

const size = wire.HeaderLen
