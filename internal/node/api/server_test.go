package api_test

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/prometheus/client_golang/prometheus"
)

// node is a started server over a real watchdog on a test clock, with one
// watched loop, and a registry holding a series labelled with a share's name.
type node struct {
	clock         *sim.TestClock
	loop          *health.Loop
	ready         atomic.Value // the node's readiness word
	addr, metrics string
}

func start(t *testing.T) *node {
	t.Helper()
	n := &node{clock: sim.NewTestClock(time.Unix(0, 0))}
	n.ready.Store("ready")
	wd := health.NewWatchdog(n.clock)
	n.loop = wd.Loop("reconcile")
	reg := metrics.New("test", "test")
	share := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dittofs_engine_test_bytes", Help: "a share's figure"}, []string{"share"})
	share.WithLabelValues("payroll").Set(1)
	reg.Registry().MustRegister(share)
	s := api.New(api.Deps{
		Ready:       func() string { return n.ready.Load().(string) },
		Live:        wd.Live,
		Addr:        "127.0.0.1:0",
		MetricsAddr: "127.0.0.1:0",
		Metrics:     reg,
		Logger:      slog.New(slog.NewTextHandler(t.Output(), nil)),
	})
	a, m, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Stop(t.Context()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	n.addr, n.metrics = a.String(), m.String()
	return n
}

// raw sends a bare HTTP/1.0 GET for path to addr and returns every byte of the
// answer, which the server ends by closing the connection.
func raw(t *testing.T, addr, path string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// A backstop against a server that never closes, not a synchronisation.
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read the answer to %s: %v", path, err)
	}
	return string(b)
}

func answer(status, word string) string {
	return "HTTP/1.0 " + status + "\r\nContent-Length: " + strconv.Itoa(len(word)) +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + word
}

// Each probe answers its status with one word and no header but its type and
// length, byte for byte, while the registry the server also serves names a
// share; every other path on the API listener is 404.
func TestHealthAnswers(t *testing.T) {
	prof.Watch(t, prof.Options{})
	n := start(t)
	for _, tc := range []struct {
		ready, path, want string
	}{
		{"ready", "/health/live", answer("200 OK", "live")},
		{"ready", "/health/ready", answer("200 OK", "ready")},
		{"starting", "/health/ready", answer("503 Service Unavailable", "starting")},
		{"draining", "/health/ready", answer("503 Service Unavailable", "draining")},
		{"not_ready", "/health/ready", answer("503 Service Unavailable", "not_ready")},
		{"starting", "/health/live", answer("200 OK", "live")},
		{"ready", "/", answer("404 Not Found", "not_found")},
		{"ready", "/health", answer("404 Not Found", "not_found")},
		{"ready", "/health/ready/payroll", answer("404 Not Found", "not_found")},
		{"ready", "/metrics", answer("404 Not Found", "not_found")},
		{"ready", "/api/v1/health", answer("404 Not Found", "not_found")},
	} {
		n.ready.Store(tc.ready)
		if got := raw(t, n.addr, tc.path); got != tc.want {
			t.Errorf("%s while %s: got %q, want %q", tc.path, tc.ready, got, tc.want)
		}
	}
}

// A watched loop silent for 61 s turns live to 503 and ready to not_ready,
// whatever the node's own word; one reporting idle every 30 s keeps both.
func TestStalledWatchdog(t *testing.T) {
	n := start(t)
	for range 4 {
		n.clock.Advance(health.IdleEvery)
		n.loop.Idle()
	}
	if got := raw(t, n.addr, "/health/ready"); got != answer("200 OK", "ready") {
		t.Errorf("ready with the loop idle every 30 s: got %q, want ready", got)
	}
	n.clock.Advance(61 * time.Second)
	for path, want := range map[string]string{
		"/health/live":  answer("503 Service Unavailable", "stalled"),
		"/health/ready": answer("503 Service Unavailable", "not_ready"),
	} {
		if got := raw(t, n.addr, path); got != want {
			t.Errorf("%s with the loop silent 61 s: got %q, want %q", path, got, want)
		}
	}
}

// The metrics listener has its own address, answers a scrape with no token,
// and serves no health probe.
func TestMetricsListener(t *testing.T) {
	n := start(t)
	if n.metrics == n.addr {
		t.Fatalf("metrics address %s: want one of its own, not the API's", n.metrics)
	}
	resp, err := http.Get("http://" + n.metrics + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `dittofs_engine_test_bytes{share="payroll"} 1`) {
		t.Errorf("scrape with no token: got %d %q, want 200 with the registry's series", resp.StatusCode, b)
	}
	if got := raw(t, n.metrics, "/health/ready"); !strings.HasPrefix(got, "HTTP/1.0 404") {
		t.Errorf("/health/ready on the metrics listener: got %q, want 404", got)
	}
}

// A metrics address that cannot be bound fails Start and leaves the API
// listener closed: the watch finds no descriptor left open.
func TestStartLeavesNothingOpen(t *testing.T) {
	prof.Watch(t, prof.Options{})
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	s := api.New(api.Deps{Addr: "127.0.0.1:0", MetricsAddr: held.Addr().String(), Metrics: metrics.New("test", "test")})
	if _, _, err := s.Start(); err == nil {
		t.Fatal("Start on a held metrics address: got nil, want an error")
	}
}
