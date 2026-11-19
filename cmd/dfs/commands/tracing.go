package commands

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// shutdownBound is how long the provider's shutdown waits to flush: a
// collector that will not answer cannot hold the unwinding start longer.
const shutdownBound = 5 * time.Second

// devTracing is the start's tracing wiring: while DITTOFS_OTLP_ENDPOINT is
// set to an http or https URL, a real SDK tracer provider over an OTLP/HTTP
// exporter pointing at it — every span it records leaves as service dittofs
// — and that provider's shutdown, for when the start unwinds, bounded at
// shutdownBound so an unreachable collector cannot hold the exit. An
// endpoint that is no http(s) URL is refused rather than let the exporter
// fall back to its own default silently. Unset, the tracer is nil and the
// shutdown a no-op, and the node runs untraced: Config's nil tracer opens
// every journal without one.
//
// ponytail: the endpoint is an environment gate for development, read outside
// the bootstrap and outside its own tracing settings, with the SDK's own
// defaults for sampling and flushing. Move to the configuration's tracing
// settings, with the sampler they name and a drain-bounded shutdown, once
// configuration reaches tracing.
func devTracing() (trace.Tracer, func(context.Context) error, error) {
	endpoint := os.Getenv("DITTOFS_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, func(context.Context) error { return nil }, nil
	}
	if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, fmt.Errorf("the OTLP endpoint %q is no http(s) URL", endpoint)
	}
	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, nil, fmt.Errorf("an OTLP trace exporter for %s: %w", endpoint, err)
	}
	res, err := sdkresource.Merge(sdkresource.Default(),
		sdkresource.NewSchemaless(semconv.ServiceName("dittofs")))
	if err != nil {
		return nil, nil, fmt.Errorf("the tracing resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return tp.Tracer("dittofs"), func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownBound)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}
