package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace prefixes every DittoFS metric: dittofs_<subsystem>_<name>_<unit>.
const Namespace = "dittofs"

// Metrics owns the Prometheus registry and the instruments registered against
// it. It is created once at startup and injected where instrumentation is
// needed. Using an owned registry (not the global default) keeps the surface
// testable and isolated.
type Metrics struct {
	registry *prometheus.Registry

	specsMu sync.Mutex
	specs   map[string]Spec // the first spec registered under each name, for register's comparisons
}

// ponytail: an alias of Metrics, which has RFC 25 §8.2's Counter, Gauge and
// Histogram; rename Metrics to Registry, turning the alias around, when RFC
// 25's registry replaces it.
type Registry = Metrics

// New creates a Metrics with an owned registry pre-populated with the Go
// runtime collector, the process collector, and a dittofs_build_info gauge
// carrying version/commit as labels.
func New(version, commit string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   Namespace,
		Name:        "build_info",
		Help:        "Build information for the running DittoFS server (always 1).",
		ConstLabels: prometheus.Labels{"version": version, "commit": commit},
	})
	buildInfo.Set(1)
	reg.MustRegister(buildInfo)

	return &Metrics{registry: reg, specs: map[string]Spec{}}
}

// RegisterProvider wires a read-through collector backed by p. The collector
// calls p.MetricsSnapshot once per scrape and emits the result as ConstMetrics.
// Safe to call once after New.
func (m *Metrics) RegisterProvider(p Provider) {
	m.registry.MustRegister(newRuntimeCollector(p))
}

// Registry returns the owned registry (for tests and for advanced wiring).
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns the HTTP handler that serves the registry in Prometheus
// text/exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}
