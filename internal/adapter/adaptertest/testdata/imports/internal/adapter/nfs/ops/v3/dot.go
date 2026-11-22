// A seed of the adapters' import check, read and never built: a command file
// that imports wire with a dot, so its call of one of wire's functions names
// no package.
package v3

import . "github.com/cubbit/dittofs/internal/adapter/nfs/wire"

func parseBare(rec []byte) { _, _ = ParseCall(rec) }
