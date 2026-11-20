package node_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
)

// After Start the registry reports both memory_pool series for each of the
// four pools, the ceiling the node built it with and nothing reserved; 64
// KiB reserved on hold reads under hold alone at the next scrape, and 0 once
// released.
func TestBudgetSeries(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	n := h.Node()
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	scrape := func(hold int64) {
		t.Helper()
		want := fmt.Sprintf(`# HELP dittofs_memory_pool_ceiling_bytes The most the memory pool reserves at once.
# TYPE dittofs_memory_pool_ceiling_bytes gauge
dittofs_memory_pool_ceiling_bytes{pool="connection"} 536870912
dittofs_memory_pool_ceiling_bytes{pool="hold"} 536870912
dittofs_memory_pool_ceiling_bytes{pool="receive"} 536870912
dittofs_memory_pool_ceiling_bytes{pool="reply"} 536870912
# HELP dittofs_memory_pool_reserved_bytes Bytes reserved from the memory pool.
# TYPE dittofs_memory_pool_reserved_bytes gauge
dittofs_memory_pool_reserved_bytes{pool="connection"} 0
dittofs_memory_pool_reserved_bytes{pool="hold"} %d
dittofs_memory_pool_reserved_bytes{pool="receive"} 0
dittofs_memory_pool_reserved_bytes{pool="reply"} 0
`, hold)
		err := testutil.GatherAndCompare(h.Metrics().Registry(), strings.NewReader(want),
			"dittofs_memory_pool_ceiling_bytes", "dittofs_memory_pool_reserved_bytes")
		if err != nil {
			t.Errorf("scrape with %d bytes reserved on hold: %v", hold, err)
		}
	}
	scrape(0)
	r, err := n.Reserve(t.Context(), "hold", 64<<10)
	if err != nil {
		t.Fatalf("reserve 64 KiB on hold: %v", err)
	}
	scrape(64 << 10)
	r.Release()
	scrape(0)
	if err := n.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// fourReserved is 1, 2, 3 and 4 KiB on each pool in turn, so each pool's
// record carries bytes no other pool's does.
var fourReserved = map[string]int64{"receive": 1 << 10, "reply": 2 << 10, "connection": 3 << 10, "hold": 4 << 10}

// A node stopped with bytes still reserved logs one error record for each
// pool holding them, naming the pool and its bytes, and none before the
// filesystem's part has stopped; one stopped with nothing reserved logs none.
func TestStopLogsEachBudgetNotAtZero(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reserved map[string]int64
	}{
		{"four reserved", fourReserved},
		{"none reserved", map[string]int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged syncBuffer
			var atFilesystem *string // the log once the filesystem's part stopped
			h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
				cfg.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
				return node.NewStepping(func(_ *node.Node, step string) {
					if step == "stop filesystem" {
						log := logged.String()
						atFilesystem = &log
					}
				})(cfg)
			}))
			n := h.Node()
			if err := n.Start(t.Context()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			for pool, bytes := range tc.reserved {
				if _, err := n.Reserve(t.Context(), pool, bytes); err != nil {
					t.Fatalf("reserve %d bytes on %s: %v", bytes, pool, err)
				}
			}
			if err := n.Stop(t.Context()); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if atFilesystem == nil {
				t.Fatal("Stop: the filesystem's part never stopped")
			}
			if got := notAtZero(t, *atFilesystem); len(got) != 0 {
				t.Errorf("budgets logged by the time the filesystem's part stopped: got %v, want none", got)
			}
			if got := notAtZero(t, logged.String()); !maps.Equal(got, tc.reserved) {
				t.Errorf("budgets logged not at zero after Stop: got %v, want %v", got, tc.reserved)
			}
		})
	}
}

// A node stopped with connections still counted logs one error record for
// the count, naming it as pool conns with its connections, as it does a pool
// holding bytes (RFC 24 §2.7).
func TestStopLogsTheConnectionCountNotAtZero(t *testing.T) {
	var logged syncBuffer
	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
		return node.New(cfg)
	}))
	n := h.Node()
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 2 {
		if err := n.TakeConn(); err != nil {
			t.Fatalf("count a connection: %v", err)
		}
	}
	if err := n.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var got []string
	for line := range strings.Lines(logged.String()) {
		var r struct {
			Level, Msg, Pool string
			Connections      int
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if r.Msg == node.BudgetNotZero {
			got = append(got, fmt.Sprintf("%s %s %d", r.Level, r.Pool, r.Connections))
		}
	}
	if want := []string{"ERROR conns 2"}; !slices.Equal(got, want) {
		t.Errorf("zero-check records after a stop with 2 connections counted: got %q, want %q", got, want)
	}
}

// notAtZero is the pool and bytes of each zero-check record in a JSON log,
// failing t on one not at error or a pool named twice.
func notAtZero(t *testing.T, log string) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for line := range strings.Lines(log) {
		var r struct {
			Level, Msg, Pool string
			Bytes            int64
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if r.Msg != node.BudgetNotZero {
			continue
		}
		if r.Level != slog.LevelError.String() {
			t.Errorf("record for %s: level %s, want %s", r.Pool, r.Level, slog.LevelError)
		}
		if _, twice := got[r.Pool]; twice {
			t.Errorf("record for %s: logged twice, want once", r.Pool)
		}
		got[r.Pool] = r.Bytes
	}
	return got
}

// recorder is a testing.TB that keeps the failures it is handed rather than
// failing the test; everything else reaches the test.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// The harness fails a test whose node stops with budgets not at zero,
// naming each pool and its bytes.
func TestHarnessFailsOnABudgetNotAtZero(t *testing.T) {
	rec := &recorder{TB: t}
	h := nodetest.New(rec)
	n := h.Node()
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for pool, bytes := range fourReserved {
		if _, err := n.Reserve(t.Context(), pool, bytes); err != nil {
			t.Fatalf("reserve %d bytes on %s: %v", bytes, pool, err)
		}
	}
	if err := n.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got := strings.Join(rec.failures, "\n")
	for pool, bytes := range fourReserved {
		if want := fmt.Sprintf("pool=%s bytes=%d", pool, bytes); !strings.Contains(got, want) {
			t.Errorf("the test's failures after a stop with %d bytes on %s: got %q, want one naming %s", bytes, pool, got, want)
		}
	}
}
