package metrics

import (
	"errors"
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

// Counter registers s as a counter vector, or returns the one registered
// under it already.
func (m *Metrics) Counter(s Spec) *prometheus.CounterVec {
	return register(m, prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: s.Name, Help: s.Help}, s.Labels))
}

// Gauge registers s as a gauge vector, or returns the one registered under
// it already.
func (m *Metrics) Gauge(s Spec) *prometheus.GaugeVec {
	return register(m, prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: Namespace, Name: s.Name, Help: s.Help}, s.Labels))
}

// Histogram registers s as a histogram vector over its bucket set, or
// returns the one registered under it already; a Spec with none panics,
// since the client library would fill in its default set.
//
// ponytail: the bucket set is not compared. The client library's descriptor
// holds the name, help and label names but no buckets, so a second Spec of
// the same name with another set would get the first vector back. Request
// is the only set, so no two Specs can differ there; compare the set here
// when a second one is added.
func (m *Metrics) Histogram(s Spec) *prometheus.HistogramVec {
	if s.Buckets == None || int(s.Buckets) >= len(buckets) {
		panic(fmt.Sprintf("metrics: histogram %s: no bucket set", s.Name))
	}
	return register(m, prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: Namespace, Name: s.Name, Help: s.Help, Buckets: buckets[s.Buckets]}, s.Labels))
}

// register registers v, or returns the vector of v's kind registered under
// its spec already: a node's components share its registry, and each
// registers what it records, as each adapter does. A vector registered
// under the same name with another spec or kind panics, naming it.
func register[V prometheus.Collector](m *Metrics, v V) V {
	err := m.registry.Register(v)
	if err == nil {
		return v
	}
	var dup prometheus.AlreadyRegisteredError
	if errors.As(err, &dup) {
		if existing, ok := dup.ExistingCollector.(V); ok {
			return existing
		}
	}
	panic(fmt.Sprintf("metrics: register: %v", err))
}
