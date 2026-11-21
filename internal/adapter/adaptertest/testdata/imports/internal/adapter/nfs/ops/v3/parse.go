// A seed of the adapters' import check, read and never built: a command file
// that parses a call with one of wire's functions, beside a use of one of
// wire's types, which it may name.
package v3

import "github.com/cubbit/dittofs/internal/adapter/nfs/wire"

var _ wire.Call

func parse(rec []byte) { _, _ = wire.ParseCall(rec) }
