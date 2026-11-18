package core_test

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
)

// The pipeline samples a trace ID as the tracing API's ratio sampler does,
// at every ratio, on random IDs and on those either side of each bound.
func TestSampledIsTheTracingAPIsRatioRule(t *testing.T) {
	src := rand.New(rand.NewChaCha8([32]byte{1}))
	var ids []trace.TraceID
	for range 4096 {
		var id trace.TraceID
		binary.BigEndian.PutUint64(id[:8], src.Uint64())
		binary.BigEndian.PutUint64(id[8:], src.Uint64())
		ids = append(ids, id)
	}
	ratios := []float64{-1, 0, 1e-9, 0.01, 0.25, 0.5, 0.999, 1, 2}
	for _, r := range ratios {
		if r <= 0 || r >= 1 {
			continue
		}
		bound := uint64(r * (1 << 63))
		for _, low := range []uint64{(bound - 1) << 1, (bound-1)<<1 | 1, bound << 1, bound<<1 | 1} {
			var id trace.TraceID
			id[0] = 1
			binary.BigEndian.PutUint64(id[8:], low)
			ids = append(ids, id)
		}
	}
	var edge trace.TraceID
	edge[0] = 1
	binary.BigEndian.PutUint64(edge[8:], math.MaxUint64)
	ids = append(ids, edge)
	for _, r := range ratios {
		sdk := sdktrace.TraceIDRatioBased(r)
		for _, id := range ids {
			want := sdk.ShouldSample(sdktrace.SamplingParameters{TraceID: id}).Decision == sdktrace.RecordAndSample
			if got := core.Sampled(id, r); got != want {
				t.Fatalf("ratio %v, trace ID %s: sampled %v, the tracing API's ratio sampler %v", r, id, got, want)
			}
		}
	}
}

// rootIDs is the ID generator RFC 25 §8.1's root gives its provider: a
// request's trace ID from the context where the adapter set it, and a
// random one where none is. It counts the roots it gave each.
type rootIDs struct {
	mu            sync.Mutex
	src           *rand.ChaCha8
	given, random int
}

func (g *rootIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	id, ok := adapter.TraceIDFromContext(ctx)
	g.mu.Lock()
	if ok {
		g.given++
	} else {
		g.random++
		_, _ = g.src.Read(id[:])
	}
	g.mu.Unlock()
	return id, g.NewSpanID(ctx, id)
}

func (g *rootIDs) NewSpanID(context.Context, trace.TraceID) (id trace.SpanID) {
	g.mu.Lock()
	_, _ = g.src.Read(id[:])
	g.mu.Unlock()
	return id
}

// seeded is a Deps.TraceIDs drawing from a ChaCha8 seeded with seed, safe
// for concurrent use: two built on one seed draw the same IDs in turn.
func seeded(seed byte) func() trace.TraceID {
	var mu sync.Mutex
	src := rand.NewChaCha8([32]byte{seed})
	return func() (id trace.TraceID) {
		mu.Lock()
		defer mu.Unlock()
		binary.BigEndian.PutUint64(id[:8], src.Uint64())
		binary.BigEndian.PutUint64(id[8:], src.Uint64())
		return id
	}
}

// Each request's trace ID is the one Deps.TraceIDs draws for it: the IDs its
// lines log are, in turn, those a source on the same seed draws.
func TestEachRequestDrawsItsTraceIDFromTraceIDs(t *testing.T) {
	logs := &logSink{}
	s := newSeamWith(t, testproto.New(adapter.Deps{}), 1<<20, adapter.Deps{TraceIDs: seeded(3), Logger: logs.logger()})
	for id := range uint64(8) {
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 3, ID: id + 1}) // refused at route, logged at debug
		s.read()
	}
	s.finish()
	lines, want := logs.at(t, "DEBUG"), seeded(3)
	if len(lines) != 8 {
		t.Fatalf("8 requests refused at route logged %d debug lines", len(lines))
	}
	for i, rec := range lines {
		if id := want().String(); rec["trace_id"] != id {
			t.Errorf("request %d logs trace ID %v, want %s, the one Deps.TraceIDs drew", i+1, rec["trace_id"], id)
		}
	}
}

// StartRequestSpan gives the span it opens the ID it is handed, through the
// root's ID generator, even under a context already carrying a span.
func TestStartRequestSpanOpensARootWithItsID(t *testing.T) {
	provider := sdktrace.NewTracerProvider(sdktrace.WithIDGenerator(&rootIDs{src: rand.NewChaCha8([32]byte{4})}))
	tracer := provider.Tracer("test")
	ctx, parent := tracer.Start(context.Background(), "parent")
	defer parent.End()
	id := seeded(5)()
	_, span := adapter.StartRequestSpan(ctx, tracer, id, "adapter.test")
	defer span.End()
	if got := span.SpanContext().TraceID(); got != id {
		t.Errorf("the span opened under another's context has trace ID %s, want %s, the one handed in (the parent's is %s)", got, id, parent.SpanContext().TraceID())
	}
}

// The context StartRequestSpan returns carries its span and not the ID, so
// a root opened under it draws an ID of its own.
func TestStartRequestSpanHandsOnTheSpanAlone(t *testing.T) {
	tracer := sdktrace.NewTracerProvider().Tracer("test")
	ctx, span := adapter.StartRequestSpan(context.Background(), tracer, seeded(6)(), "adapter.test")
	defer span.End()
	if trace.SpanFromContext(ctx) != span {
		t.Error("the context StartRequestSpan returns does not carry its span")
	}
	if id, ok := adapter.TraceIDFromContext(ctx); ok {
		t.Errorf("the context StartRequestSpan returns gives TraceIDFromContext %s, want none", id)
	}
}

// Under a provider built as the root builds it, sampling at the same ratio,
// only the requests whose logged trace ID the ratio rule samples call the
// tracer, each handing it that ID, and each has a span carrying it.
func TestASampledRequestsSpanCarriesItsLoggedTraceID(t *testing.T) {
	const ratio = 0.5
	spans, ids := tracetest.NewSpanRecorder(), &rootIDs{src: rand.NewChaCha8([32]byte{2})}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithIDGenerator(ids),
		sdktrace.WithSpanProcessor(spans),
	)
	logs := &logSink{}
	s := newSeamWith(t, testproto.New(adapter.Deps{}), 1<<20, adapter.Deps{Tracer: provider.Tracer("test"), SampleRatio: ratio, Logger: logs.logger()})
	for id := range uint64(64) {
		s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 3, ID: id + 1}) // refused at route, logged at debug
		s.read()
	}
	s.finish()
	want := map[string]bool{}
	lines := logs.at(t, "DEBUG")
	for _, rec := range lines {
		id, err := trace.TraceIDFromHex(rec["trace_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if core.Sampled(id, ratio) {
			want[id.String()] = true
		}
	}
	got := map[string]bool{}
	for _, sp := range spans.Ended() {
		got[sp.SpanContext().TraceID().String()] = true
	}
	if len(lines) != 64 || len(want) == 0 || len(want) == 64 {
		t.Fatalf("%d lines logged, %d of them sampled: want 64 lines, the ratio splitting them", len(lines), len(want))
	}
	if ids.given != len(want) || ids.random != 0 {
		t.Errorf("the tracer was handed %d trace IDs and drew %d itself, want the %d sampled requests' IDs and none of its own", ids.given, ids.random, len(want))
	}
	if len(got) != len(want) {
		t.Errorf("%d requests have spans, want the %d whose logged trace ID the ratio samples", len(got), len(want))
	}
	for id := range want {
		if !got[id] {
			t.Errorf("a sampled request's logged trace ID %s has no span", id)
		}
	}
}

// A request the ratio leaves out allocates nothing for tracing: tracking it
// under a provider that would trace every request, at ratio 0, allocates as
// it does with no tracer (RFC 25 §6.3).
func TestAnUnsampledRequestAllocatesNothingForTracing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	tracer := sdktrace.NewTracerProvider().Tracer("test")
	rec := &resources.Buf{B: make([]byte, 64)}
	allocs := map[string]float64{}
	for name, d := range map[string]adapter.Deps{"no tracer": {}, "a tracer at ratio 0": {Tracer: tracer}} {
		s := newSeamWith(t, testproto.New(adapter.Deps{}), 1<<20, d)
		allocs[name] = testing.AllocsPerRun(100, func() { s.p.Track(rec) })
	}
	if allocs["a tracer at ratio 0"] != allocs["no tracer"] {
		t.Errorf("an unsampled request allocates %v times with a tracer, %v with none; want the same", allocs["a tracer at ratio 0"], allocs["no tracer"])
	}
}

// A server with no tracer samples nothing, whatever its ratio: its requests
// are served, with no span to open.
func TestNoTracerAtAnyRatio(t *testing.T) {
	s := newSeamWith(t, testproto.New(adapter.Deps{}), 1<<20, adapter.Deps{SampleRatio: 1})
	s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1, Payload: []byte("hi")})
	if r := s.read(); r.Status != testproto.StatusOK {
		t.Errorf("an Echo on a server with no tracer at ratio 1 is answered %+v", r)
	}
}
