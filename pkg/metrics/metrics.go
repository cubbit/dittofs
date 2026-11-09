package metrics

import (
	"net/http"

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
	in       *instruments
}

// Registry is the name an adapter's dependencies hold the metrics under.
//
// ponytail: an alias of Metrics, whose RegisterProvider reads a snapshot per
// scrape and cannot resolve a counter once, when an operation table is built.
// Grow Metrics with the methods that resolve a series once and return its
// counter, then rename it Registry and turn this alias around, when the first
// caller needs a counter resolved at table build; a separate struct would
// break every holder of a *Metrics.
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

	return &Metrics{registry: reg, in: newInstruments(reg)}
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
