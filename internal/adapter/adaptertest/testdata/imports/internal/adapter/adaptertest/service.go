// A seed of the adapters' import check: its imports are read, nothing here is built.
package adaptertest

import (
	_ "github.com/cubbit/dittofs/internal/prof"
	_ "github.com/cubbit/dittofs/pkg/resources"
	_ "github.com/cubbit/dittofs/pkg/vfs/vfstest"
)
