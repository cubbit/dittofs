package adapter

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// traceIDKey is the context key of a sampled request's trace ID.
type traceIDKey struct{}

// ContextWithTraceID is ctx carrying id, the trace ID an adapter drew for a
// request it sampled, as it opens the request's span. The root's ID generator
// gives that span id (RFC 25 §8.1), so the provider's sampler reaches the
// adapter's verdict and the span's trace ID is the one the request's log
// lines carry.
func ContextWithTraceID(ctx context.Context, id trace.TraceID) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceIDFromContext is the trace ID ContextWithTraceID put in ctx.
func TraceIDFromContext(ctx context.Context) (trace.TraceID, bool) {
	id, ok := ctx.Value(traceIDKey{}).(trace.TraceID)
	return id, ok
}
