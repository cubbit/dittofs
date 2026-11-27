// A seed of the adapters' import check, read and never built: a command file
// that imports wire twice under two names and calls one of wire's functions
// through the first.
package v3

import (
	a "github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	b "github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

func parseTwice(rec []byte) { _, _ = a.ParseCall(rec) }
