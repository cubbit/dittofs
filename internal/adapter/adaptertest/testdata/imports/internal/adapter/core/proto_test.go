// A seed of the adapters' import check: its imports are read, nothing here is built.
package core

import (
	_ "github.com/cubbit/dittofs/internal/adapter/adaptertest"
	_ "github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	_ "github.com/cubbit/dittofs/internal/prof"
	_ "github.com/pmezard/go-difflib/difflib"
)
