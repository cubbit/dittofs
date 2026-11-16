package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape is what the registry's handler serves.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Counter, Gauge and Histogram each register a vector under dittofs_, which
// a scrape of the handler shows, the histogram over the request set's
// buckets.
func TestRegistryCallsRegisterWhatAScrapeShows(t *testing.T) {
	m := New("v", "c")
	m.Counter(Spec{Name: "t_events_total", Help: "h", Labels: []string{"protocol"}}).WithLabelValues("smb").Add(2)
	m.Gauge(Spec{Name: "t_depth", Help: "h", Labels: []string{"protocol"}}).WithLabelValues("nfs3").Set(3)
	m.Histogram(Spec{Name: "t_seconds", Help: "h", Labels: []string{"stage"}, Buckets: Request}).WithLabelValues("x").Observe(0.0002)
	got := scrape(t, m)
	for _, want := range []string{
		`dittofs_t_events_total{protocol="smb"} 2`,
		`dittofs_t_depth{protocol="nfs3"} 3`,
		`dittofs_t_seconds_bucket{stage="x",le="0.0001"} 0`,
		`dittofs_t_seconds_bucket{stage="x",le="0.00025"} 1`,
		`dittofs_t_seconds_bucket{stage="x",le="60"} 1`,
		`dittofs_t_seconds_count{stage="x"} 1`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("the scrape has no line %q", want)
		}
	}
	if n := strings.Count(got, `dittofs_t_seconds_bucket{`); n != len(buckets[Request])+1 {
		t.Errorf("the histogram has %d buckets, want the request set's %d and +Inf", n, len(buckets[Request]))
	}
}

// A histogram with no bucket set panics, naming it.
func TestHistogramWithoutBucketsPanics(t *testing.T) {
	defer func() {
		if p := recover(); p == nil || !strings.Contains(p.(string), "t_seconds") {
			t.Errorf("Histogram with no bucket set: panic %v, want one naming t_seconds", p)
		}
	}()
	New("v", "c").Histogram(Spec{Name: "t_seconds", Help: "h"})
}
