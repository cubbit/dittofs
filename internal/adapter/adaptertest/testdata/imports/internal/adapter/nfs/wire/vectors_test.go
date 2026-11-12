// A seed of the adapters' import check: its imports are read, nothing here is built.
package wire

import (
	_ "os"

	_ "github.com/cubbit/dittofs/internal/adapter/adaptertest"
	_ "github.com/cubbit/dittofs/internal/prof"
	_ "github.com/cubbit/dittofs/pkg/resources"
	_ "github.com/cubbit/dittofs/pkg/vfs/vfstest"
)
