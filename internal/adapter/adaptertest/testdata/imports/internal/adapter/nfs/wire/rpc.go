// A seed of the adapters' import check, read and never built: one of wire's
// functions, which reads bytes, and one of its types.
package wire

type Call struct{}

func ParseCall(rec []byte) (Call, error) { return Call{}, nil }
