package journal_test

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
)

// Each repack pass the caller asks for is one span, ended with the pass's own
// error, carrying the pass's own figures — the segments it retired, the bytes
// its copies moved and the storage its retirements freed. The drive makes its
// dead bytes the way the node does, an overwrite in place killing the record
// just written, and asks for one pass after another until one frees what the
// dead record holds — the idle seal that makes the segment a pass's
// candidate lands in the background, so the passes poll the passes' own
// freed figure rather than assuming the first or second.
func TestARepackPassIsOneSpan(t *testing.T) {
	var mu sync.Mutex
	var spans []tracedSpan
	tracer := func(ctx context.Context, op journal.SpanOp, attrs ...slog.Attr) (context.Context, func(error)) {
		mu.Lock()
		spans = append(spans, tracedSpan{op: op, attrs: slices.Clone(attrs)})
		i := len(spans) - 1
		mu.Unlock()
		return ctx, func(err error) {
			mu.Lock()
			spans[i].err = err
			mu.Unlock()
		}
	}
	f := journaltest.NewWithOptions(t, nil, journal.WithTracer(tracer))

	const size = 128 << 10
	h, id := f.Share(0), journaltest.File(0)
	write := func(tag string) {
		t.Helper()
		if err := journaltest.RetrySpare(func() error {
			_, err := h.WriteAt(t.Context(), id, 0, payload(tag, size), 0)
			return err
		}); err != nil {
			t.Fatalf("write %s at 0: %v", tag, err)
		}
	}
	write("first")
	write("second") // the overwrite in place kills the first record's bytes

	if _, err := h.Sync(t.Context(), id); err != nil {
		t.Fatalf("sync the file: %v", err)
	}
	f.Advance(2 * journaltest.IdleSealAge)

	// The idle seal that turns the dead record's segment into a pass's
	// candidate lands in the background of the clock advance, so the passes
	// poll for the one that finds it — one repack call per attempt, a
	// millisecond's pause between them as the suite's own waits do — never
	// a sleep the check does not observe.
	var freed int64
	var calls int
	for range 4096 {
		one, err := f.J.Repack(t.Context(), journaltest.FixtureMaxBytes, journal.RepackScope{})
		if err != nil {
			t.Fatalf("repack pass %d: %v", calls+1, err)
		}
		calls++
		freed += one
		if freed > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if freed <= 0 {
		t.Fatalf("no pass of the poll freed nothing of the dead record's storage across %d calls, want the one that found the sealed segment", calls)
	}

	mu.Lock()
	defer mu.Unlock()
	var repacks []tracedSpan
	for _, s := range spans {
		if string(s.op) == "repack_pass" {
			repacks = append(repacks, s)
		}
	}
	if len(repacks) != calls {
		t.Fatalf("%d repack calls opened %d repack spans, want exactly one per call", calls, len(repacks))
	}
	var spanFreed int64
	for _, s := range repacks {
		if s.err != nil {
			t.Errorf("a repack span ended with %v, want nil: the pass's own answer was clean", s.err)
		}
		attr := func(name string) (int64, bool) {
			for _, a := range s.attrs {
				if a.Key != name {
					continue
				}
				switch a.Value.Kind() {
				case slog.KindInt64:
					return a.Value.Int64(), true
				case slog.KindUint64:
					return int64(a.Value.Uint64()), true
				}
			}
			return 0, false
		}
		if got, ok := attr("segments"); !ok || got < 0 {
			t.Errorf("a repack span carries segments %d (present %v), want the figure of the pass's retired segments", got, ok)
		}
		if got, ok := attr("bytes"); !ok || got < 0 {
			t.Errorf("a repack span carries bytes %d (present %v), want the figure of the payload its copies moved", got, ok)
		}
		if got, ok := attr("freed"); ok {
			spanFreed += got
		}
	}
	if spanFreed != freed {
		t.Errorf("the repack spans sum their freed figures to %d, want the passes' own total %d", spanFreed, freed)
	}
}

// payload returns n bytes no two tags of the same length share a run of.
func payload(tag string, n int) []byte {
	var h uint64
	for _, c := range tag {
		h = h*131 + uint64(c)
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i%251) ^ byte(h)
	}
	return out
}

// tracedSpan is one span a tracer kept: its op, the attributes it started
// with and the error it ended with.
type tracedSpan struct {
	op    journal.SpanOp
	attrs []slog.Attr
	err   error
}
