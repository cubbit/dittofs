// A seed of the adapters' import check: its imports are read, nothing here is built.
package nfsharness

import (
	_ "github.com/cubbit/dittofs/internal/adapter/adaptertest"
	_ "github.com/cubbit/dittofs/internal/adapter/core"
	_ "github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
)
