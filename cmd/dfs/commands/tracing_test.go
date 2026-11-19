package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// With no DITTOFS_OTLP_ENDPOINT, the start's tracing wiring is off: no tracer
// and nothing to shut down, so the node runs untraced.
func TestDevTracingUnsetIsOff(t *testing.T) {
	t.Setenv("DITTOFS_OTLP_ENDPOINT", "")
	tracer, stop, err := devTracing()
	if err != nil {
		t.Fatalf("devTracing with the endpoint unset: %v", err)
	}
	if tracer != nil {
		t.Errorf("devTracing with the endpoint unset handed a tracer, want nil")
	}
	if stop != nil {
		t.Errorf("devTracing with the endpoint unset handed a shutdown, want nil")
	}
}

// With DITTOFS_OTLP_ENDPOINT set, the wiring yields a real provider: a span
// it traces leaves through the real OTLP/HTTP exporter to the endpoint, one
// export request over the wire — asserted at a real HTTP listener the test
// binds, caught after the provider's shutdown flushed.
func TestDevTracingExportsToTheEndpoint(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the export request's body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, r.Clone(r.Context()))
		bodies = append(bodies, len(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("DITTOFS_OTLP_ENDPOINT", srv.URL)
	tracer, stop, err := devTracing()
	if err != nil {
		t.Fatalf("devTracing with the endpoint set to %s: %v", srv.URL, err)
	}
	if tracer == nil {
		t.Fatal("devTracing with the endpoint set handed no tracer, want the provider's")
	}

	_, span := tracer.Start(t.Context(), "the span the test traces")
	span.End()
	if err := stop(context.Background()); err != nil {
		t.Fatalf("the provider's shutdown: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("the endpoint received no export request after the provider shut down, want the traced span flushed to it")
	}
	for i, n := range bodies {
		if n == 0 {
			t.Errorf("export request %d of %d carried an empty body, want the encoded span", i+1, len(got))
		}
	}
	for _, r := range got {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("the exporter posted to %s, want the OTLP/HTTP traces path /v1/traces", r.URL.Path)
		}
	}
}

// An endpoint that is no http(s) URL is refused rather than let the exporter
// fall back to its own default and trace nowhere.
func TestDevTracingRefusesANonURL(t *testing.T) {
	for _, endpoint := range []string{"localhost:4318", "not a url", "ftp://collector:4318"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("DITTOFS_OTLP_ENDPOINT", endpoint)
			tracer, stop, err := devTracing()
			if err == nil || !strings.Contains(err.Error(), "is no http(s) URL") {
				t.Fatalf("devTracing over %q: err %v, want the refusal naming it no http(s) URL", endpoint, err)
			}
			if tracer != nil || stop != nil {
				t.Errorf("devTracing over %q handed a tracer (%v) and a shutdown (%v) with its refusal", endpoint, tracer != nil, stop != nil)
			}
		})
	}
}

// The provider's shutdown is bounded: a collector that accepts connections
// and never answers cannot hold the unwinding start, so the flush fails and
// the shutdown returns within the bound and its slack.
func TestDevTracingShutsDownWithinItsBound(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block // a collector that takes the connection and never answers
	}))
	t.Cleanup(srv.Close)

	t.Setenv("DITTOFS_OTLP_ENDPOINT", srv.URL)
	tracer, stop, err := devTracing()
	if err != nil {
		t.Fatalf("devTracing over the silent collector: %v", err)
	}
	_, span := tracer.Start(t.Context(), "the span the test traces")
	span.End()

	started := time.Now()
	if err := stop(context.Background()); err == nil {
		t.Error("the shutdown over a collector that never answers: nil, want the flush's failure")
	}
	if took := time.Since(started); took >= 2*shutdownBound {
		t.Errorf("the shutdown waited %v over a collector that never answers, want it bounded at %v", took, shutdownBound)
	}
}

// A refused endpoint stops dfs start before the metadata store opens: node.New
// owns the KV from openKV on, so a start refused on its tracing must refuse
// before any store exists, not leave one behind.
func TestStartRefusesABadOTLPEndpoint(t *testing.T) {
	t.Setenv("DITTOFS_OTLP_ENDPOINT", "localhost:4318")
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	code, out := runChild(t, bootstrapFile{store: store, journal: filepath.Join(dir, "device"), node: nodeUUID}.write(t, dir))
	if code == 0 || !strings.Contains(out, "Error: building the tracing provider: the OTLP endpoint") {
		t.Fatalf("dfs start over localhost:4318 as the OTLP endpoint: exit %d, output:\n%s\nwant non-zero naming the tracing provider", code, out)
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store directory %s after the refused start: %v, want none created", store, err)
	}
}
