// A seed of the adapters' import check: its imports are read, nothing here is built.
package gss

import (
	_ "context"

	_ "github.com/cubbit/dittofs/internal/adapter/gss/gsstest"
	_ "github.com/cubbit/dittofs/pkg/vfs"
	_ "github.com/jcmturner/gokrb5/v8/types"
)
