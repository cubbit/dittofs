package syncer_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/syncer"
)

type kind int

const (
	gauge kind = iota
	counter
	histogram
)

func (k kind) String() string { return [...]string{"gauge", "counter", "histogram"}[k] }

// series is one labelled series the syncer must make observable, typed from
// the syncer's metric list, with the path of the field that carries it in a
// Half. A path element naming a slice steps into its element type. The half
// label is the Half itself and the store label StoreStats.Store, so neither
// appears here.
type series struct {
	name string
	kind kind
	path string
}

var allSeries = []series{
	// Pools.
	{"syncer_workers", gauge, "Workers"},
	{"syncer_workers_busy", gauge, "Stores.WorkersBusy"},
	{"syncer_inflight_bytes", gauge, "Stores.InflightBytes"},
	{`syncer_queue_depth{class="demand"}`, gauge, "Stores.QueueDepth.Demand"},
	{`syncer_queue_depth{class="background"}`, gauge, "Stores.QueueDepth.Background"},
	{`syncer_queue_depth{class="speculation"}`, gauge, "Stores.QueueDepth.Speculation"},
	{`syncer_queue_depth{flow,class="demand"}`, gauge, "Flows.QueueDepth.Demand"},
	{`syncer_queue_depth{flow,class="background"}`, gauge, "Flows.QueueDepth.Background"},
	{`syncer_queue_depth{flow,class="speculation"}`, gauge, "Flows.QueueDepth.Speculation"},
	{`syncer_queue_wait_seconds{class="demand"}`, histogram, "Stores.QueueWait.Demand"},
	{`syncer_queue_wait_seconds{class="background"}`, histogram, "Stores.QueueWait.Background"},
	{`syncer_queue_wait_seconds{class="speculation"}`, histogram, "Stores.QueueWait.Speculation"},
	{`syncer_refused_total{reason="unhealthy"}`, counter, "Stores.Refused.Unhealthy"},
	{`syncer_refused_total{reason="queue_full"}`, counter, "Stores.Refused.QueueFull"},
	{`syncer_refused_total{reason="waiters_full"}`, counter, "Stores.Refused.WaitersFull"},
	{`syncer_refused_total{reason="displaced"}`, counter, "Stores.Refused.Displaced"},
	{`syncer_refused_total{reason="evicted"}`, counter, "Stores.Refused.Evicted"},
	{`syncer_refused_total{reason="closed"}`, counter, "Stores.Refused.Closed"},
	{"syncer_detached_buffers", gauge, "DetachedBuffers"},
	{`syncer_waiters{class="demand"}`, gauge, "Stores.Waiters.Demand"},
	{`syncer_waiters{class="background"}`, gauge, "Stores.Waiters.Background"},
	{`syncer_waiters{class="speculation"}`, gauge, "Stores.Waiters.Speculation"},
	{`syncer_waiters{flow,class="demand"}`, gauge, "Flows.Waiters.Demand"},
	{`syncer_waiters{flow,class="background"}`, gauge, "Flows.Waiters.Background"},
	{`syncer_waiters{flow,class="speculation"}`, gauge, "Flows.Waiters.Speculation"},

	// Transfers.
	{`syncer_transfers_total{outcome="ok"}`, counter, "Stores.Transfers.OK"},
	{`syncer_transfers_total{outcome="failed"}`, counter, "Stores.Transfers.Failed"},
	{`syncer_transfers_total{outcome="unknown"}`, counter, "Stores.Transfers.Unknown"},
	{"syncer_bytes_total", counter, "Stores.Bytes"},
	{"syncer_transfer_duration_seconds", histogram, "Stores.TransferDuration"},
	{"syncer_first_byte_seconds", histogram, "Stores.FirstByte"},
	{`syncer_retries_total{reason="transient"}`, counter, "Stores.Retries.Transient"},
	{`syncer_retries_total{reason="throttled"}`, counter, "Stores.Retries.Throttled"},
	{`syncer_retries_total{reason="slow_tail"}`, counter, "Stores.Retries.SlowTail"},
	{`syncer_retries_total{reason="verify"}`, counter, "Stores.Retries.Verify"},
	{`syncer_retries_total{reason="floor"}`, counter, "Stores.Retries.Floor"},
	{`syncer_retries_total{reason="stalled"}`, counter, "Stores.Retries.Stalled"},
	{`syncer_retries_total{reason="trickle"}`, counter, "Stores.Retries.Trickle"},
	{"syncer_retry_budget_exhausted_total", counter, "Stores.RetryBudgetExhausted"},
	{"syncer_throttle_limit", gauge, "Stores.ThrottleLimit"},
	{"syncer_throttled_seconds_total", counter, "Stores.Throttled"},
	{"syncer_store_throughput_bytes_per_second", gauge, "Stores.Throughput"},

	// Fetches.
	{"syncer_fetch_joined_total", counter, "Stores.Joined"},
	{`syncer_fetch_detached_total{reason="behind"}`, counter, "Stores.Detached.Behind"},
	{`syncer_fetch_detached_total{reason="flow_closed"}`, counter, "Stores.Detached.FlowClosed"},
	{"syncer_fetch_rehomed_total", counter, "Stores.Rehomed"},
	{"syncer_speculation_preempted_total", counter, "Stores.SpeculationPreempted"},

	// Health.
	{"syncer_store_healthy", gauge, "Stores.Healthy"},
	{"syncer_store_unhealthy_seconds_total", counter, "Stores.Unhealthy"},
	{"syncer_store_transitions_total", counter, "Stores.Transitions"},
	{"syncer_probe_duration_seconds", histogram, "Stores.ProbeDuration"},
}

// labelFields are the fields that carry a series' labels rather than a value.
var labelFields = map[string]bool{
	"Stores.Store": true,
	"Stores.Name":  true,
	"Flows.Flow":   true,
	"Flows.Store":  true,
}

var (
	histogramType = reflect.TypeFor[syncer.Histogram]()
	durationType  = reflect.TypeFor[time.Duration]()
)

// resolve walks path from t, stepping into a slice's element type before
// each field, and returns the type the path ends at.
func resolve(t reflect.Type, path string) (reflect.Type, bool) {
	for _, name := range strings.Split(path, ".") {
		if t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		f, ok := t.FieldByName(name)
		if !ok {
			return nil, false
		}
		t = f.Type
	}
	return t, true
}

// fits reports whether a field of type t can carry a series of kind k: a
// gauge is a count, a size or a 0/1 state; a counter only grows, so it is
// unsigned or a duration; a histogram is a Histogram.
func fits(t reflect.Type, k kind) bool {
	switch k {
	case gauge:
		return t.Kind() == reflect.Int || t.Kind() == reflect.Int64 || t.Kind() == reflect.Bool
	case counter:
		return t.Kind() == reflect.Uint64 || t == durationType
	default:
		return t == histogramType
	}
}

// TestEverySeriesHasAField resolves every series of the syncer's metric tables
// in a Half, the value the node's collector reads, and checks the field can
// carry the series' kind.
func TestEverySeriesHasAField(t *testing.T) {
	prof.Watch(t, prof.Options{})
	half := reflect.TypeOf(syncer.Stats{}.Upload)
	for _, s := range allSeries {
		ft, ok := resolve(half, s.path)
		if !ok {
			t.Errorf("%s: no field at Half.%s", s.name, s.path)
			continue
		}
		if !fits(ft, s.kind) {
			t.Errorf("%s: Half.%s is %v, which cannot carry a %v", s.name, s.path, ft, s.kind)
		}
	}
}

// leaves lists the paths of t's value fields, descending into structs other
// than Histogram and into slices' element types.
func leaves(t reflect.Type, prefix string) []string {
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == histogramType {
		return []string{prefix}
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		p := f.Name
		if prefix != "" {
			p = prefix + "." + f.Name
		}
		out = append(out, leaves(f.Type, p)...)
	}
	return out
}

// TestEveryFieldIsASeries checks the converse: every value field of a Half
// carries a series of the tables, or a label, so no field reaches the
// collector without a series to export it under.
func TestEveryFieldIsASeries(t *testing.T) {
	prof.Watch(t, prof.Options{})
	claimed := map[string]bool{}
	for _, s := range allSeries {
		claimed[s.path] = true
	}
	for _, p := range leaves(reflect.TypeFor[syncer.Half](), "") {
		if !claimed[p] && !labelFields[p] {
			t.Errorf("Half.%s carries no series of the syncer's metric tables", p)
		}
	}
}

// TestHistogramBoundsAreTheRequestSet checks the buckets against the request
// bucket set, typed here bound by bound, and that a
// histogram holds one count per bound plus the overflow.
func TestHistogramBoundsAreTheRequestSet(t *testing.T) {
	prof.Watch(t, prof.Options{})
	want := []string{
		"100µs", "250µs", "500µs", "1ms", "2.5ms", "5ms", "10ms", "25ms", "50ms",
		"100ms", "250ms", "500ms", "1s", "2.5s", "5s", "10s", "30s", "60s",
	}
	got := syncer.Histogram{}.Bounds()
	if len(got) != len(want) {
		t.Fatalf("Bounds has %d buckets, want %d", len(got), len(want))
	}
	for i, w := range want {
		d, err := time.ParseDuration(w)
		if err != nil {
			t.Fatalf("parse %q: %v", w, err)
		}
		if got[i] != d {
			t.Errorf("Bounds()[%d] = %v, want %v", i, got[i], d)
		}
	}
	if n := len(syncer.Histogram{}.Counts); n != len(got)+1 {
		t.Errorf("Histogram has %d counts, want %d: one per bound and the overflow", n, len(got)+1)
	}
}
