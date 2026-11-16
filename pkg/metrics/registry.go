package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// Spec is one vector: its name without the dittofs_ prefix, its help, its
// label names and, for a histogram, its bucket set (RFC 25 §8.2).
//
// ponytail: unchecked. A name or label that breaks RFC 25 §2.1-§2.4 registers
// as given; check it here, panicking, when RFC 25's registry lands.
type Spec struct {
	Name    string
	Help    string
	Labels  []string
	Buckets Buckets // histograms only
}

// Buckets is a histogram's bucket set (RFC 25 §2.6).
type Buckets uint8

const (
	None    Buckets = iota // not a histogram
	Request                // anything a client request waits on
)

// buckets is each set's upper bounds, in seconds.
var buckets = [...][]float64{
	Request: {100e-6, 250e-6, 500e-6, 1e-3, 2.5e-3, 5e-3, 10e-3, 25e-3, 50e-3, 100e-3, 250e-3, 500e-3, 1, 2.5, 5, 10, 30, 60},
}

// Counter registers s as a counter vector.
func (m *Metrics) Counter(s Spec) *prometheus.CounterVec {
	v := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: s.Name, Help: s.Help}, s.Labels)
	m.registry.MustRegister(v)
	return v
}

// Gauge registers s as a gauge vector.
func (m *Metrics) Gauge(s Spec) *prometheus.GaugeVec {
	v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: Namespace, Name: s.Name, Help: s.Help}, s.Labels)
	m.registry.MustRegister(v)
	return v
}

// Histogram registers s as a histogram vector over its bucket set; a Spec
// with none panics, since the client library would fill in its default set.
func (m *Metrics) Histogram(s Spec) *prometheus.HistogramVec {
	if s.Buckets == None || int(s.Buckets) >= len(buckets) {
		panic(fmt.Sprintf("metrics: histogram %s: no bucket set", s.Name))
	}
	v := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: Namespace, Name: s.Name, Help: s.Help, Buckets: buckets[s.Buckets]}, s.Labels)
	m.registry.MustRegister(v)
	return v
}
