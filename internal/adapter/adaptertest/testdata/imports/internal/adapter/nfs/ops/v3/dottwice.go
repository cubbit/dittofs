// A seed of the adapters' import check, read and never built: a command file
// that imports wire twice, with a dot and then by name, so its call of one of
// wire's functions names no package.
package v3

import (
	. "github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	w "github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

func parseDotted(rec []byte) { _, _ = ParseCall(rec) }
