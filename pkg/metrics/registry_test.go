package metrics

import (
	"fmt"
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

// A spec registered twice returns the vector registered first, so two
// components on one registry record into one series; the same name with
// other labels, or as another kind, panics.
func TestRegistryCallsReturnTheVectorAlreadyRegistered(t *testing.T) {
	m := New("v", "c")
	spec := Spec{Name: "t_events_total", Help: "h", Labels: []string{"protocol"}}
	a, b := m.Counter(spec), m.Counter(spec)
	if a != b {
		t.Error("a counter registered twice gave two vectors")
	}
	a.WithLabelValues("smb").Inc()
	b.WithLabelValues("smb").Inc()
	if !strings.Contains(scrape(t, m), `dittofs_t_events_total{protocol="smb"} 2`+"\n") {
		t.Error("two records through the two calls are not one series of 2")
	}
	gauge := Spec{Name: "t_depth", Help: "h", Labels: []string{"protocol"}}
	hist := Spec{Name: "t_seconds", Help: "h", Labels: []string{"stage"}, Buckets: Request}
	g1, g2 := m.Gauge(gauge), m.Gauge(gauge)
	h1, h2 := m.Histogram(hist), m.Histogram(hist)
	if g1 != g2 || h1 != h2 {
		t.Error("a gauge or histogram registered twice gave two vectors")
	}
	for name, register := range map[string]func(){
		"other labels": func() { m.Counter(Spec{Name: "t_events_total", Help: "h", Labels: []string{"stage"}}) },
		"another kind": func() { m.Gauge(spec) },
		"another order": func() {
			m.Counter(Spec{Name: "t_events_total", Help: "h", Labels: []string{"stage", "protocol"}})
		},
	} {
		func() {
			defer func() {
				p := recover()
				if p == nil {
					t.Errorf("t_events_total registered again with %s: no panic", name)
					return
				}
				if !strings.Contains(fmt.Sprint(p), "t_events_total") {
					t.Errorf("t_events_total registered again with %s: %v does not name it", name, p)
				}
			}()
			register()
		}()
	}
}

// The same label names in another order are another spec: the library sorts
// them before hashing, so they would otherwise share one vector and record
// under the first order's labels, with no error.
func TestRegistryRefusesALabelOrderPermutation(t *testing.T) {
	m := New("v", "c")
	m.Counter(Spec{Name: "t_permuted_total", Help: "h", Labels: []string{"protocol", "stage"}})
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("the same labels in another order registered without a panic")
		}
		msg := fmt.Sprint(p)
		if !strings.Contains(msg, "t_permuted_total") || !strings.Contains(msg, "[protocol stage]") || !strings.Contains(msg, "[stage protocol]") {
			t.Errorf("the panic is %q, want both orders and the name", msg)
		}
	}()
	m.Counter(Spec{Name: "t_permuted_total", Help: "h", Labels: []string{"stage", "protocol"}})
}
