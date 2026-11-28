package content

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/remote/s3"
)

// The remote store's series (RFC 4 §4.12), labelled by the store
// configuration a recorder records for.
var (
	remoteRequestsSpec = metrics.Spec{
		Name:   "remote_requests_total",
		Help:   "Calls a remote store made, by op and result; cause is a denial's cause, empty otherwise.",
		Labels: []string{"store", "op", "result", "cause"},
	}
	remoteSecondsSpec = metrics.Spec{
		Name:    "remote_request_seconds",
		Help:    "Time per remote store call, by op folded to put, get, delete, list, health and other.",
		Labels:  []string{"store", "op"},
		Buckets: metrics.Request,
	}
	remoteBytesSpec = metrics.Spec{
		Name:   "remote_bytes_total",
		Help:   "Body bytes a remote store's calls moved, by op.",
		Labels: []string{"store", "op"},
	}
	remoteConnsSpec = metrics.Spec{
		Name:   "remote_connections_opened_total",
		Help:   "Connections a remote store's client opened.",
		Labels: []string{"store"},
	}
	remoteForeignSpec = metrics.Spec{
		Name:   "remote_list_foreign_total",
		Help:   "Keys under the block namespace a listing skipped because they are not block names.",
		Labels: []string{"store"},
	}
	remoteCapabilitySpec = metrics.Spec{
		Name:   "remote_capability",
		Help:   "The last capability check's verdict per feature of a namespace's store: 1 if present.",
		Labels: []string{"store", "namespace", "feature"},
	}
)

// decision: the closed sets below are RFC 4's, restated because the S3
// store's constants are unexported: the ops a store records (§4.12), the
// results of §4.8's errors with a denial's causes, and C.1's features. A
// value outside them records nothing, and so does a Capability for a prefix
// no bind named: a series under an op, a result or a namespace nothing
// declared would be worse than none. A new value (recheck, with RFC 9's
// calls) needs an RFC 4 amendment, whose review must reach these lists, since
// no test here can see the store's constants drift.
var (
	remoteOps = [...]string{"put", "put_control", "get", "get_control", "delete", "list", "health"}
	// remoteSecondsOps are the histogram's op values; remoteFold maps each of
	// remoteOps to one of them.
	remoteSecondsOps = [...]string{"put", "get", "delete", "list", "health", "other"}
	remoteFold       = [len(remoteOps)]uint8{0, 5, 1, 5, 2, 3, 4}
	remoteResults    = [...]struct{ result, cause string }{
		{"ok", ""}, {"not_found", ""}, {"invalid", ""}, {"throttled", ""}, {"transient", ""},
		{"corrupt", ""}, {"local", ""}, {"denied", "access"}, {"denied", "quota"}, {"denied", "size"},
	}
	remoteFeatures = [...]string{
		"put_integrity", "read_after_write", "storage_class", "range_get", "list_bucket", "batch_delete",
		"list", "conditional_put", "versioning", "lifecycle", "tiering", "encryption",
	}
)

var _ s3.Recorder = (*remoteRecorder)(nil)

// remoteRecorder implements s3.Recorder for one store configuration (RFC 4
// §4.12). Every series but remote_capability's is resolved at construction,
// so a call indexes and adds (RFC 25 §2.9).
type remoteRecorder struct {
	key  recorderKey
	refs int // under remoteRecorders' lock

	requests [len(remoteOps)][len(remoteResults)]prometheus.Counter
	seconds  [len(remoteSecondsOps)]prometheus.Observer
	bytes    [len(remoteOps)]prometheus.Counter
	conns    prometheus.Counter
	foreign  prometheus.Counter

	capability *prometheus.GaugeVec
	vecs       [6]interface{ DeletePartialMatch(prometheus.Labels) int }

	// bindings maps a prefix to its namespace's binding, copied on write
	// under bindMu so Capability loads it with no lock.
	bindings atomic.Pointer[map[string]*remoteBinding]
	bindMu   sync.Mutex
}

// remoteBinding is one prefix's namespace and its remote_capability
// children, each resolved on its feature's first Capability.
type remoteBinding struct {
	namespace string
	refs      int // under bindMu
	features  [len(remoteFeatures)]atomic.Pointer[prometheus.Gauge]
}

type recorderKey struct {
	m     *metrics.Metrics
	store string
}

// remoteRecorders are the live recorders, one per registry and store label:
// the vectors' children are shared per label value, so two recorders for
// one value would delete each other's series.
var remoteRecorders = struct {
	sync.Mutex
	live map[recorderKey]*remoteRecorder
}{live: map[recorderKey]*remoteRecorder{}}

// newRemoteRecorder returns the recorder for store on m, building and
// resolving it when none is live, and counts the caller among its holders.
// Each holder calls close once.
func newRemoteRecorder(m *metrics.Metrics, store string) *remoteRecorder {
	remoteRecorders.Lock()
	defer remoteRecorders.Unlock()

	key := recorderKey{m, store}
	if r := remoteRecorders.live[key]; r != nil {
		r.refs++
		return r
	}

	requests := m.Counter(remoteRequestsSpec)
	seconds := m.Histogram(remoteSecondsSpec)
	bytes := m.Counter(remoteBytesSpec)
	conns := m.Counter(remoteConnsSpec)
	foreign := m.Counter(remoteForeignSpec)
	r := &remoteRecorder{
		key:        key,
		refs:       1,
		conns:      conns.WithLabelValues(store),
		foreign:    foreign.WithLabelValues(store),
		capability: m.Gauge(remoteCapabilitySpec),
	}
	r.vecs = [...]interface{ DeletePartialMatch(prometheus.Labels) int }{requests, seconds, bytes, conns, foreign, r.capability}
	for i, op := range remoteOps {
		for j, res := range remoteResults {
			r.requests[i][j] = requests.WithLabelValues(store, op, res.result, res.cause)
		}
		r.bytes[i] = bytes.WithLabelValues(store, op)
	}
	for i, op := range remoteSecondsOps {
		r.seconds[i] = seconds.WithLabelValues(store, op)
	}
	r.bindings.Store(&map[string]*remoteBinding{})
	remoteRecorders.live[key] = r

	return r
}

// Call records one call on its op's and result's series.
func (r *remoteRecorder) Call(op, result, cause string, took time.Duration, bytes int64) {
	i := slices.Index(remoteOps[:], op)
	j := remoteResult(result, cause)
	if i < 0 || j < 0 {
		return
	}
	r.requests[i][j].Inc()
	r.seconds[remoteFold[i]].Observe(took.Seconds())
	if bytes > 0 {
		r.bytes[i].Add(float64(bytes))
	}
}

// remoteResult is result's and cause's index in remoteResults, -1 for none.
func remoteResult(result, cause string) int {
	for j, v := range remoteResults {
		if v.result == result && v.cause == cause {
			return j
		}
	}
	return -1
}

// ConnOpened records one connection the client opened.
func (r *remoteRecorder) ConnOpened() { r.conns.Inc() }

// ForeignKey records one key a listing skipped.
func (r *remoteRecorder) ForeignKey() { r.foreign.Inc() }

// Capability sets feature's gauge for the namespace bound to prefix: 1 if
// present, 0 otherwise.
func (r *remoteRecorder) Capability(prefix, feature string, present bool) {
	b := (*r.bindings.Load())[prefix]
	f := slices.Index(remoteFeatures[:], feature)
	if b == nil || f < 0 {
		return
	}
	g := b.features[f].Load()
	if g == nil {
		if g = r.resolve(prefix, b, f); g == nil {
			return
		}
	}
	v := 0.0
	if present {
		v = 1
	}
	(*g).Set(v)
}

// resolve resolves b's child for feature f, unless b was unbound since
// Capability loaded it: a child made then would outlive the delete.
func (r *remoteRecorder) resolve(prefix string, b *remoteBinding, f int) *prometheus.Gauge {
	r.bindMu.Lock()
	defer r.bindMu.Unlock()

	if (*r.bindings.Load())[prefix] != b {
		return nil
	}
	if g := b.features[f].Load(); g != nil {
		return g
	}
	g := r.capability.WithLabelValues(r.key.store, b.namespace, remoteFeatures[f])
	b.features[f].Store(&g)

	return &g
}

// bind names the namespace a prefix's capability series carry, and counts
// the caller among the binding's holders; unbind releases it, deleting the
// namespace's series on the last release. A prefix holds one namespace on a
// store, as the location is the content (RFC 13 §5.1), so a second bind of
// a bound prefix shares the first's binding whatever namespace it names.
func (r *remoteRecorder) bind(prefix, namespace string) (unbind func()) {
	r.bindMu.Lock()
	defer r.bindMu.Unlock()

	cur := *r.bindings.Load()
	b := cur[prefix]
	if b == nil {
		b = &remoteBinding{namespace: namespace}
		next := maps.Clone(cur)
		next[prefix] = b
		r.bindings.Store(&next)
	}
	b.refs++

	var once sync.Once
	return func() { once.Do(func() { r.unbind(prefix, b) }) }
}

func (r *remoteRecorder) unbind(prefix string, b *remoteBinding) {
	r.bindMu.Lock()
	defer r.bindMu.Unlock()

	cur := *r.bindings.Load()
	if b.refs--; b.refs > 0 || cur[prefix] != b {
		return
	}
	next := maps.Clone(cur)
	delete(next, prefix)
	r.bindings.Store(&next)
	r.capability.DeletePartialMatch(prometheus.Labels{"store": r.key.store, "namespace": b.namespace})
}

// close releases the caller's hold; the last release deletes every series
// carrying this store and every binding, so a later newRemoteRecorder for
// the same store builds afresh.
func (r *remoteRecorder) close() {
	remoteRecorders.Lock()
	defer remoteRecorders.Unlock()

	if r.refs == 0 {
		return
	}
	if r.refs--; r.refs > 0 {
		return
	}
	delete(remoteRecorders.live, r.key)

	r.bindMu.Lock()
	r.bindings.Store(&map[string]*remoteBinding{})
	r.bindMu.Unlock()
	for _, v := range r.vecs {
		v.DeletePartialMatch(prometheus.Labels{"store": r.key.store})
	}
}
