package content

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metrics"
)

// The values RFC 4 names, restated: §4.12's ops, §4.8's results with a
// denial's causes, and C.1's features.
var (
	rfcOps      = []string{"put", "put_control", "get", "get_control", "delete", "list", "health"}
	rfcResults  = []string{"ok", "not_found", "invalid", "throttled", "transient", "corrupt", "local"}
	rfcCauses   = []string{"access", "quota", "size"}
	rfcFeatures = []string{"put_integrity", "read_after_write", "storage_class", "range_get", "list_bucket",
		"batch_delete", "list", "conditional_put", "versioning", "lifecycle", "tiering", "encryption"}
	rfcFold = map[string]string{"put": "put", "put_control": "other", "get": "get", "get_control": "other",
		"delete": "delete", "list": "list", "health": "health"}
)

// scrape is every dittofs_remote_ sample the registry exports, keyed by
// name{label=value,...} with the labels sorted; a histogram gives its
// sample count under name_count. labels holds each family's label names.
func scrape(t *testing.T, m *metrics.Metrics) (samples map[string]float64, labels map[string][]string) {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	samples, labels = map[string]float64{}, map[string][]string{}
	for _, mf := range mfs {
		name := mf.GetName()
		if !strings.HasPrefix(name, "dittofs_remote_") {
			continue
		}
		for _, s := range mf.GetMetric() {
			var kv, names []string
			for _, l := range s.GetLabel() {
				kv = append(kv, l.GetName()+"="+l.GetValue())
				names = append(names, l.GetName())
			}
			sort.Strings(kv)
			sort.Strings(names)
			labels[name] = names
			key := name + "{" + strings.Join(kv, ",") + "}"
			switch {
			case s.Counter != nil:
				samples[key] = s.GetCounter().GetValue()
			case s.Gauge != nil:
				samples[key] = s.GetGauge().GetValue()
			case s.Histogram != nil:
				samples[strings.Replace(key, "{", "_count{", 1)] = float64(s.GetHistogram().GetSampleCount())
			}
		}
	}
	return samples, labels
}

func reqKey(store, op, result, cause string) string {
	return fmt.Sprintf("dittofs_remote_requests_total{cause=%s,op=%s,result=%s,store=%s}", cause, op, result, store)
}

func secondsKey(store, op string) string {
	return fmt.Sprintf("dittofs_remote_request_seconds_count{op=%s,store=%s}", op, store)
}

func bytesKey(store, op string) string {
	return fmt.Sprintf("dittofs_remote_bytes_total{op=%s,store=%s}", op, store)
}

func capKey(store, namespace, feature string) string {
	return fmt.Sprintf("dittofs_remote_capability{feature=%s,namespace=%s,store=%s}", feature, namespace, store)
}

// want is every series a recorder for store exports before any call, at 0.
func want(store string) map[string]float64 {
	w := map[string]float64{
		"dittofs_remote_connections_opened_total{store=" + store + "}": 0,
		"dittofs_remote_list_foreign_total{store=" + store + "}":       0,
	}
	for _, op := range rfcOps {
		for _, res := range rfcResults {
			w[reqKey(store, op, res, "")] = 0
		}
		for _, c := range rfcCauses {
			w[reqKey(store, op, "denied", c)] = 0
		}
		w[bytesKey(store, op)] = 0
		w[secondsKey(store, rfcFold[op])] = 0
	}
	return w
}

func diff(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: %s missing from the scrape, want %v", what, k, v)
		} else if g != v {
			t.Errorf("%s: %s = %v, want %v", what, k, g, v)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %s = %v in the scrape, want no such series", what, k, g)
		}
	}
}

// A new recorder exports every combination of the closed sets at 0, under
// RFC 4's names and RFC 4's labels, and no remote_capability series.
func TestRemoteRecorderExportsEveryCombinationAtZero(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	newRemoteRecorder(m, "s1")
	got, _ := scrape(t, m)
	diff(t, "after construction", got, want("s1"))

	r := newRemoteRecorder(m, "s1")
	r.bind("a/", "A")
	r.Capability("a/", "conditional_put", true)
	_, labels := scrape(t, m)
	wantLabels := map[string][]string{
		"dittofs_remote_requests_total":           {"cause", "op", "result", "store"},
		"dittofs_remote_request_seconds":          {"op", "store"},
		"dittofs_remote_bytes_total":              {"op", "store"},
		"dittofs_remote_connections_opened_total": {"store"},
		"dittofs_remote_list_foreign_total":       {"store"},
		"dittofs_remote_capability":               {"feature", "namespace", "store"},
	}
	for name, w := range wantLabels {
		if !slices.Equal(labels[name], w) {
			t.Errorf("%s labels = %v, want %v", name, labels[name], w)
		}
	}
	for name := range labels {
		if _, ok := wantLabels[name]; !ok {
			t.Errorf("%s exported, want no remote_ series RFC 4 §4.12 does not name", name)
		}
	}
}

// Each Call adds to exactly its series: one call per op and result lands
// each requests series at 1, the bytes on the op's own series, and the time
// on the op's histogram fold, put_control and get_control under other.
func TestRemoteRecorderCallAddsToItsSeries(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	r := newRemoteRecorder(m, "s1")
	w := want("s1")
	for i, op := range rfcOps {
		n := int64(1) << i
		call := func(result, cause string) {
			r.Call(op, result, cause, time.Millisecond, n)
			w[reqKey("s1", op, result, cause)]++
			w[bytesKey("s1", op)] += float64(n)
			w[secondsKey("s1", rfcFold[op])]++
		}
		for _, res := range rfcResults {
			call(res, "")
		}
		for _, c := range rfcCauses {
			call("denied", c)
		}
	}
	r.ConnOpened()
	r.ForeignKey()
	r.ForeignKey()
	w["dittofs_remote_connections_opened_total{store=s1}"] = 1
	w["dittofs_remote_list_foreign_total{store=s1}"] = 2
	got, _ := scrape(t, m)
	diff(t, "after a call per combination", got, w)
}

// A value outside the closed sets, and a Capability for a prefix no bind
// named, record nothing and do not panic.
func TestRemoteRecorderRecordsNothingOutsideItsSets(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	r := newRemoteRecorder(m, "s1")
	r.bind("a/", "A")
	r.Call("recheck", "ok", "", time.Second, 10)
	r.Call("put", "denied", "", time.Second, 10)
	r.Call("put", "ok", "access", time.Second, 10)
	r.Call("put", "refused", "", time.Second, 10)
	r.Capability("b/", "conditional_put", true)
	r.Capability("a/", "object_lock", true)
	got, _ := scrape(t, m)
	diff(t, "after calls outside the sets", got, want("s1"))
}

// A bound Capability sets its {store, namespace, feature} gauge to its
// verdict, and a bound namespace with no Capability yet exports none.
func TestRemoteRecorderCapability(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	r := newRemoteRecorder(m, "s1")
	r.bind("a/", "A")
	r.bind("b/", "B")
	w := want("s1")
	for i, f := range rfcFeatures {
		present := i%2 == 0
		r.Capability("a/", f, present)
		w[capKey("s1", "A", f)] = map[bool]float64{true: 1, false: 0}[present]
	}
	got, _ := scrape(t, m)
	diff(t, "after A's check", got, w)

	r.Capability("a/", "conditional_put", true)
	w[capKey("s1", "A", "conditional_put")] = 1
	got, _ = scrape(t, m)
	diff(t, "after A's second check", got, w)
}

// A second holder of a store's recorder, or of a namespace's binding, keeps
// its series exported and recording when the first releases.
func TestRemoteRecorderSharedHoldersKeepTheirSeries(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	first := newRemoteRecorder(m, "s1")
	second := newRemoteRecorder(m, "s1")
	unbindFirst := first.bind("a/", "A")
	second.bind("a/", "A")
	first.Capability("a/", "list", true)

	unbindFirst()
	unbindFirst()
	first.close()
	second.Call("put", "ok", "", time.Millisecond, 7)
	second.Capability("a/", "list", false)

	w := want("s1")
	w[reqKey("s1", "put", "ok", "")] = 1
	w[bytesKey("s1", "put")] = 7
	w[secondsKey("s1", "put")] = 1
	w[capKey("s1", "A", "list")] = 0
	got, _ := scrape(t, m)
	diff(t, "after the first holders released", got, w)
}

// close deletes every series carrying its store, the other store's intact,
// and unbind deletes its namespace's capability series, the other's intact.
func TestRemoteRecorderReleaseDeletesOnlyItsSeries(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	s1, s2 := newRemoteRecorder(m, "s1"), newRemoteRecorder(m, "s2")
	unbindA := s1.bind("a/", "A")
	s1.bind("b/", "B")
	s2.bind("a/", "A")
	for _, r := range []*remoteRecorder{s1, s2} {
		r.Capability("a/", "list", true)
	}
	s1.Capability("b/", "list", true)

	unbindA()
	s1.Capability("a/", "tiering", true) // unbound now: records nothing
	w := want("s1")
	maps.Copy(w, want("s2"))
	w[capKey("s1", "B", "list")] = 1
	w[capKey("s2", "A", "list")] = 1
	got, _ := scrape(t, m)
	diff(t, "after unbinding s1's A", got, w)

	s1.close()
	w = want("s2")
	w[capKey("s2", "A", "list")] = 1
	got, _ = scrape(t, m)
	diff(t, "after closing s1", got, w)

	again := newRemoteRecorder(m, "s1")
	again.Call("get", "ok", "", time.Millisecond, 0)
	s1.close() // a stale release past the last holder deletes nothing of the rebuilt recorder
	again.Call("get", "ok", "", time.Millisecond, 0)
	maps.Copy(w, want("s1"))
	w[reqKey("s1", "get", "ok", "")] = 2
	w[secondsKey("s1", "get")] = 2
	got, _ = scrape(t, m)
	diff(t, "after rebuilding s1 and a stale close", got, w)
}

// A Capability that loaded a binding before its unbind, and resolves its
// gauge after it, makes no series: one made then would outlive the delete.
func TestRemoteRecorderStaleBindingResolvesNothing(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	r := newRemoteRecorder(m, "s1")
	r.bind("a/", "A")()
	stale := &remoteBinding{namespace: "A"}
	if g := r.resolve("a/", stale, 0); g != nil {
		t.Errorf("resolve of an unbound binding = %v, want nil", g)
	}
	unbind := r.bind("a/", "A")
	if g := r.resolve("a/", stale, 0); g != nil {
		t.Errorf("resolve of a binding replaced since = %v, want nil", g)
	}
	unbind()
	got, _ := scrape(t, m)
	diff(t, "after resolving stale bindings", got, want("s1"))
}

// Namespaces binding and unbinding beside a running check race nothing:
// run under -race.
func TestRemoteRecorderBindBesideCapability(t *testing.T) {
	t.Parallel()
	m := metrics.New("test", "test")
	r := newRemoteRecorder(m, "s1")
	r.bind("a/", "A")
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			for _, f := range rfcFeatures {
				r.Capability("a/", f, true)
			}
		}
	})
	wg.Go(func() {
		for i := range 200 {
			r.bind(fmt.Sprintf("b%d/", i), fmt.Sprintf("B%d", i))()
		}
	})
	wg.Go(func() {
		for range 200 {
			r.bind("a/", "A")() // a second holder of A, released beside its check
		}
	})
	wg.Wait()
	got, _ := scrape(t, m)
	for _, f := range rfcFeatures {
		if v, ok := got[capKey("s1", "A", f)]; !ok || v != 1 {
			t.Errorf("%s = %v (present %v), want 1", capKey("s1", "A", f), v, ok)
		}
	}
}

// Recording indexes resolved series: Call, ConnOpened, ForeignKey and a
// bound Capability allocate nothing (RFC 25 §2.9).
func TestAllocsRemoteRecorder(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	m := metrics.New("test", "test")
	r := newRemoteRecorder(m, "s1")
	r.bind("a/", "A")
	for name, f := range map[string]func(){
		"Call":       func() { r.Call("get_control", "denied", "quota", time.Millisecond, 4096) },
		"ConnOpened": r.ConnOpened,
		"ForeignKey": r.ForeignKey,
		"Capability": func() { r.Capability("a/", "conditional_put", true) },
	} {
		if n := testing.AllocsPerRun(100, f); n != 0 {
			t.Errorf("%s allocates %v per call, want 0", name, n)
		}
	}
}
