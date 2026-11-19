package identity

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sync/singleflight"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ponytail: the TTLs and the slot wait are constants. Make them settings
// when an operator shows a directory that needs others.
const (
	dirFound    = 5 * time.Minute  // how long a positive answer is fresh
	dirNotFound = 30 * time.Second // how long a "no such entry" answer is fresh
	dirSlotWait = 5 * time.Second  // how long a query waits for a slot, the client's query timeout
)

// ponytail: the cache holds 65 536 entries and lets 64 queries run at once,
// fixed bounds rather than a share of RFC 24's connection-state pool. Charge
// both to that pool when it is built, or size them from a measured
// directory's load.
const (
	dirEntries  = 1 << 16
	dirInFlight = 64
)

// dirLookupsSpec counts directory lookups by question and by how each was
// answered.
var dirLookupsSpec = metrics.Spec{
	Name:   "identity_directory_lookups_total",
	Help:   "Directory lookups, by question and result.",
	Labels: []string{"question", "result"},
}

// dirSecondsSpec times each query sent to the directory.
var dirSecondsSpec = metrics.Spec{
	Name:    "identity_directory_seconds",
	Help:    "Directory query latency.",
	Buckets: metrics.Request,
}

// dirEvictionsSpec counts the entries the cache evicted at its bound.
var dirEvictionsSpec = metrics.Spec{
	Name: "identity_directory_evictions_total",
	Help: "Directory cache entries evicted, least recently used first, at the cache's bound.",
}

// dirKey is a question: what a lookup is by and the value it asks, a name
// folded as principal names compare, a SID or an entry ID as its bytes.
type dirKey struct {
	by    directory.Key
	value string
}

// dirEntry is the directory's last answer to one question, positive or "no
// such entry", and when it was given.
type dirEntry struct {
	key   dirKey
	ans   directory.Answer
	found bool
	at    time.Time
}

// fresh reports whether e may answer at now without asking the directory.
func (e *dirEntry) fresh(now time.Time) bool {
	ttl := dirFound
	if !e.found {
		ttl = dirNotFound
	}
	return now.Sub(e.at) < ttl
}

// dirCache is the server's cache of one directory client's answers, built
// with the client and dropped with it, so a changed record starts empty.
//
// ponytail: one directory cache per node, so N nodes ask the directory N
// times. Share answers through the metadata store when the directory's query
// rate shows it.
type dirCache struct {
	client  *directory.Client
	clock   Clock
	tracer  trace.Tracer
	lookups *prometheus.CounterVec
	seconds prometheus.Observer
	evicted prometheus.Counter

	flight  singleflight.Group
	slots   chan struct{}   // the queries running, dirInFlight at most
	waiting atomic.Int32    // the queries waiting for a slot
	out     outage          // directory_unreachable
	life    context.Context // the probe loop's, ended by Close
	end     context.CancelFunc

	mu      sync.Mutex
	entries map[dirKey]*list.Element // of *dirEntry
	recent  *list.List               // most recently used first
}

// newDirCache is a cache of c's answers, observed through d's clock,
// registry and tracer.
func newDirCache(c *directory.Client, d Deps) *dirCache {
	tracer := d.Tracer
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}
	life, end := context.WithCancel(context.Background())
	return &dirCache{
		life:    life,
		end:     end,
		client:  c,
		clock:   d.Clock,
		tracer:  tracer,
		lookups: counter(d.Metrics, dirLookupsSpec),
		seconds: histogram(d.Metrics, dirSecondsSpec).WithLabelValues(),
		evicted: counter(d.Metrics, dirEvictionsSpec).WithLabelValues(),
		slots:   make(chan struct{}, dirInFlight),
		entries: map[dirKey]*list.Element{},
		recent:  list.New(),
	}
}

// histogram is spec registered on reg, or an unregistered one for a nil reg.
func histogram(reg *metrics.Registry, spec metrics.Spec) *prometheus.HistogramVec {
	if reg != nil {
		return reg.Histogram(spec)
	}
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: spec.Name, Help: spec.Help}, spec.Labels)
}

// Close ends the probe loop and waits for it, then closes the client once
// the lookups running on it return.
func (c *dirCache) Close() {
	c.end()
	c.out.close()
	c.client.Close()
}

// question is by's question label.
func question(by directory.Key) string {
	switch by {
	case directory.ByUserName:
		return "name"
	case directory.ByGroupName:
		return "group"
	case directory.BySID:
		return "sid"
	case directory.ByEntryID:
		return "entry_id"
	}
	return "unknown"
}

// directoryLookup answers "who holds this" through the applied State's
// directory cache, under its stale grace: ErrUnavailable with no record or no
// Deps.Directory. fresh asks for a fresh answer only (a query's, or an entry
// within its TTL), never one served stale.
func (s *Server) directoryLookup(ctx context.Context, by directory.Key, value []byte, fresh bool) (directory.Answer, bool, error) {
	st := s.state.Load()
	if st == nil || st.dir == nil {
		return directory.Answer{}, false, fmt.Errorf("%w: identity: no directory", vfserr.ErrUnavailable)
	}
	return st.dir.lookup(ctx, by, value, st.Settings.DirectoryStaleGrace, fresh)
}

// lookup answers from a fresh entry, else from one query, shared by every
// concurrent lookup of the key. A query that fails, or a caller that stops
// waiting for it, leaves the entry as it was and answers from a positive one whose last answer is no older than the
// TTL plus grace, counted stale, unless fresh is set; any other failure is
// ErrUnavailable, counted refused, or ambiguous for an answer more than one
// entry gave. No failure is stored. While directory_unreachable is raised
// only the probe queries; every other lookup is answered as a failed query is.
func (c *dirCache) lookup(ctx context.Context, by directory.Key, value []byte, grace time.Duration, fresh bool) (directory.Answer, bool, error) {
	// The client answers an empty value without the directory, since LDAP
	// holds none, so it is answered here, before the outage is consulted: it
	// is no probe and neither raises nor clears directory_unreachable.
	if len(value) == 0 {
		return directory.Answer{}, false, nil
	}
	q := question(by)
	k := dirKey{by: by, value: string(value)}
	if by == directory.ByUserName || by == directory.ByGroupName {
		k.value = entity.FoldPrincipalName(k.value)
	}
	e := c.get(k)
	now := c.clock.Now()
	if e != nil && e.fresh(now) {
		c.lookups.WithLabelValues(q, "fresh").Inc()
		return e.ans, e.found, nil
	}
	var r singleflight.Result
	// decision: while directory_unreachable is raised, the probe is a
	// lookup, not a goroutine: the first lookup dirProbeEvery after the last
	// probe asks the directory and every other is answered from the grace or
	// refused at once, so an outage costs one query's timeout per 10 seconds
	// rather than one per expired entry or cold sign-in (RFC 18 §5.3). A
	// node no lookup probes on is probed by the cache's own loop instead,
	// dirQuietProbe after the last probe, twice the lookups' interval so it
	// sends only the probes no lookup sent, and the condition, which RFC 25
	// derives and never latches, clears once the directory is back with no
	// sign-in to notice it. Close ends the loop and waits for it. Overturned
	// if a quiet node's 20 seconds to clear ever delays a recovery an
	// operator watches for.
	if c.out.skip(now) {
		r.Err = errDirUnreachable
	} else {
		// The query runs on the first caller's context without its
		// cancellation, so a caller that stops waiting leaves it to the
		// others; the client's query timeout bounds it.
		ch := c.flight.DoChan(strconv.Itoa(int(by))+":"+k.value, func() (any, error) {
			return c.query(context.WithoutCancel(ctx), k, q, value)
		})
		select {
		case r = <-ch:
		case <-ctx.Done():
			r.Err = fmt.Errorf("%w: identity: directory lookup: %w", vfserr.ErrUnavailable, ctx.Err())
		}
	}
	if r.Err == nil {
		n := r.Val.(*dirEntry)
		result := "queried"
		if !n.found {
			result = "not_found"
		}
		c.lookups.WithLabelValues(q, result).Inc()
		return n.ans, n.found, nil
	}
	if errors.Is(r.Err, directory.ErrAmbiguous) {
		c.lookups.WithLabelValues(q, "ambiguous").Inc()
		return directory.Answer{}, false, r.Err
	}
	// decision: serve stale for a bounded grace, then fail closed. Failing
	// closed at once turns every directory blip into a sign-in storm of
	// refusals for users the server knew a minute ago; serving stale without
	// bound lets a fired user in for as long as the directory is down. Only a
	// positive answer is served stale, never "no such entry" (RFC 18 §5.4),
	// and a grace of zero serves none past the TTL. Overturned if an operator
	// needs a disabled directory account stopped sooner than the grace they
	// set, which a shorter grace already gives.
	if !fresh && e != nil && e.found && c.clock.Now().Sub(e.at) <= dirFound+grace {
		c.lookups.WithLabelValues(q, "stale").Inc()
		return e.ans, true, nil
	}
	c.lookups.WithLabelValues(q, "refused").Inc()
	c.out.refuse()
	return directory.Answer{}, false, r.Err
}

// query asks the directory k once, in a slot of its own, under a span of the
// context that opened it, and stores the answer. A query that waits
// dirSlotWait for a slot fails ErrUnavailable unsent, so an outage of many
// distinct names neither parks a goroutine each without bound nor sends their
// queries after every caller has left.
func (c *dirCache) query(ctx context.Context, k dirKey, q string, value []byte) (*dirEntry, error) {
	select {
	case c.slots <- struct{}{}:
	default:
		c.waiting.Add(1)
		got := false
		select {
		case c.slots <- struct{}{}:
			got = true
		case <-c.clock.After(dirSlotWait):
		}
		c.waiting.Add(-1)
		if !got {
			return nil, fmt.Errorf("%w: identity: no directory query slot within %v", vfserr.ErrUnavailable, dirSlotWait)
		}
	}
	defer func() { <-c.slots }()

	ctx, span := c.tracer.Start(ctx, "identity.directory.query", trace.WithAttributes(attribute.String("identity.question", q)))
	defer span.End()
	start := c.clock.Now()
	ans, found, err := c.client.Lookup(ctx, k.by, value)
	c.seconds.Observe(c.clock.Now().Sub(start).Seconds())
	if err != nil {
		span.SetStatus(codes.Error, "directory unavailable")
		if !errors.Is(err, directory.ErrAmbiguous) {
			if c.out.raise(start, err) {
				go c.probe(k, q)
			}
			return nil, err
		}
	}
	// An answer, ambiguous or not, is the directory's.
	c.out.clear(c.clock.Now())
	if err != nil {
		return nil, err
	}
	e := &dirEntry{key: k, ans: ans, found: found, at: c.clock.Now()}
	c.put(e)
	return e, nil
}

// probe is the probe loop raise started: while the outage is raised it asks
// the directory k, the question whose failure raised it, whenever no probe
// was sent for dirQuietProbe, and it ends when the outage clears or Close
// ends it. It asks k's own copy of the value, a name folded, which the
// caller's buffer is not.
func (c *dirCache) probe(k dirKey, q string) {
	defer c.out.loops.Done()
	for {
		wait, ok := c.out.quietWait(c.clock.Now())
		if !ok {
			return
		}
		if wait > 0 {
			select {
			case <-c.clock.After(wait):
			case <-c.life.Done():
				return
			}
		}
		if c.out.quiet(c.clock.Now()) {
			_, _ = c.query(c.life, k, q, []byte(k.value))
		}
	}
}

// get is k's entry, marked most recently used; nil when none is held.
func (c *dirCache) get(k dirKey) *dirEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[k]
	if !ok {
		return nil
	}
	c.recent.MoveToFront(el)
	return el.Value.(*dirEntry)
}

// put stores e in place of its key's entry, evicting the least recently used
// entry past the bound.
func (c *dirCache) put(e *dirEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[e.key]; ok {
		el.Value = e
		c.recent.MoveToFront(el)
		return
	}
	c.entries[e.key] = c.recent.PushFront(e)
	if c.recent.Len() > dirEntries {
		old := c.recent.Back()
		c.recent.Remove(old)
		delete(c.entries, old.Value.(*dirEntry).key)
		c.evicted.Inc()
	}
}
