// A seed of the adapters' import check: its imports are read, nothing here is built.
// A generated codec does no I/O, as wire does none.
package v4

import (
	_ "os"

	_ "github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	_ "github.com/cubbit/dittofs/pkg/resources"
)
