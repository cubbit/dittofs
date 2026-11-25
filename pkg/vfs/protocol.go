package vfs

import "context"

// protocolKey keys the protocol label a call's context carries. A struct
// type, never a string, so no other package's key can collide with it.
type protocolKey struct{}

// WithProtocol labels ctx with the protocol its request came over, for
// observability alone: the adapter sets it once per request, and only the
// access event and the operation metric read it. No rule of the service
// does, so a context without it is as valid as one with it.
func WithProtocol(ctx context.Context, p Protocol) context.Context {
	return context.WithValue(ctx, protocolKey{}, p)
}

// ProtocolFrom returns the label WithProtocol set on ctx and whether it
// carries one. Only an emitter calls it.
func ProtocolFrom(ctx context.Context) (Protocol, bool) {
	p, ok := ctx.Value(protocolKey{}).(Protocol)
	return p, ok
}
