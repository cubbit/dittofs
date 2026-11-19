package adapter

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// traceIDKey is the context key of the trace ID StartRequestSpan opens its
// span under.
type traceIDKey struct{}

// StartRequestSpan opens the span of a request the adapter sampled, with t,
// under ctx carrying id, the trace ID the adapter drew, which the root's ID
// generator gives that span (RFC 20 §10.2, RFC 25 §8.1). It passes
// trace.WithNewRoot(), since the tracing SDK asks the generator only for a
// span with no parent: a span already in ctx would give its own trace ID and
// drop id and the adapter's decision. It returns the span's context built on
// ctx, which does not carry id, so a root opened under it later draws an ID
// of its own.
func StartRequestSpan(ctx context.Context, t trace.Tracer, id trace.TraceID, name string) (context.Context, trace.Span) {
	_, span := t.Start(context.WithValue(ctx, traceIDKey{}, id), name, trace.WithNewRoot())
	return trace.ContextWithSpan(ctx, span), span
}

// TraceIDFromContext is the ID StartRequestSpan opens its span under, for
// the root's ID generator; false in any other context, the one it returns
// included.
func TraceIDFromContext(ctx context.Context) (trace.TraceID, bool) {
	id, ok := ctx.Value(traceIDKey{}).(trace.TraceID)
	return id, ok
}
