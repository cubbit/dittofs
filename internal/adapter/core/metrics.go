package core

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// adder and observer are the one method core calls on a resolved child: a
// counter's or a gauge's Add, a histogram's Observe. core does not import the
// client library, whose children satisfy them.
type (
	adder    interface{ Add(float64) }
	observer interface{ Observe(float64) }
)

// nop is a child of a server with no registry: it records nothing.
type nop struct{}

func (nop) Add(float64)     {}
func (nop) Observe(float64) {}

// family and histFamily resolve a vector's child for its label values.
type (
	family     func(values ...string) adder
	histFamily func(values ...string) observer
)

// vec names a counter or gauge vector of the request path (§10.1) by its
// place in specs; the one histogram, adapter_wire_seconds, is wireSpec.
type vec uint8

const (
	vInFlight vec = iota
	vRefusals
	vReplayHits
	vPanics
	vUnmapped
	vStatusMapped
	vHeldRequests
	vHeldBytes
	vHoldPoolBytes
	vHoldRefusals
	vChainSplits
	vecs
)

// specs is each vector's spec and kind, in vec's order.
var specs = [vecs]struct {
	metrics.Spec
	gauge bool
}{
	vInFlight:      {metrics.Spec{Name: "adapter_requests_in_flight", Help: "Requests admitted and not yet answered.", Labels: []string{"protocol"}}, true},
	vRefusals:      {metrics.Spec{Name: "adapter_refusals_total", Help: "Requests refused before the service, by the stage that refused them.", Labels: []string{"protocol", "stage"}}, false},
	vReplayHits:    {metrics.Spec{Name: "adapter_replay_hits_total", Help: "Requests answered from the replay cache.", Labels: []string{"protocol"}}, false},
	vPanics:        {metrics.Spec{Name: "adapter_handler_panics_total", Help: "Handlers that panicked, by operation.", Labels: []string{"protocol", "op"}}, false},
	vUnmapped:      {metrics.Spec{Name: "adapter_unmapped_errors_total", Help: "Errors no row of the table maps, answered the fault status.", Labels: []string{"protocol"}}, false},
	vStatusMapped:  {metrics.Spec{Name: "adapter_status_mapped_total", Help: "Service errors answered by a row of the table, by error.", Labels: []string{"protocol", "error"}}, false},
	vHeldRequests:  {metrics.Spec{Name: "adapter_held_requests", Help: "Requests held pending.", Labels: []string{"protocol"}}, true},
	vHeldBytes:     {metrics.Spec{Name: "adapter_held_bytes", Help: "Bytes the held requests keep.", Labels: []string{"protocol"}}, true},
	vHoldPoolBytes: {metrics.Spec{Name: "adapter_hold_pool_bytes", Help: "Bytes charged to the hold pool."}, true},
	vHoldRefusals:  {metrics.Spec{Name: "adapter_hold_refusals_total", Help: "Requests refused past their connection's floor with the hold pool full.", Labels: []string{"protocol"}}, false},
	vChainSplits:   {metrics.Spec{Name: "adapter_chain_splits_total", Help: "Chains whose reply went out in more than one record, by reason.", Labels: []string{"protocol", "reason"}}, false},
}

var wireSpec = metrics.Spec{Name: "adapter_wire_seconds", Help: "Time a request spends in each stage around the service call.", Labels: []string{"protocol", "stage"}, Buckets: metrics.Request}

// newFamilies registers each vector once on r, or, with no registry,
// resolves every child to nop.
func newFamilies(r *metrics.Registry) (fs [vecs]family, wire histFamily) {
	if r == nil {
		for i := range fs {
			fs[i] = func(...string) adder { return nop{} }
		}
		return fs, func(...string) observer { return nop{} }
	}
	for i, s := range specs {
		if s.gauge {
			v := r.Gauge(s.Spec)
			fs[i] = func(lv ...string) adder { return v.WithLabelValues(lv...) }
		} else {
			v := r.Counter(s.Spec)
			fs[i] = func(lv ...string) adder { return v.WithLabelValues(lv...) }
		}
	}
	h := r.Histogram(wireSpec)
	return fs, func(lv ...string) observer { return h.WithLabelValues(lv...) }
}

// stage is a value of the stage label, in §10.1's order: §4.1's stages from
// decode_header to send, reply_wait, the request's own reply reservation,
// between decode_args and resolve_ids, and reply_too_big, which only a
// refusal takes, for a reply or §4.3's compound past the reply pool's
// ceiling. frame and call_service are left out: no series records them.
type stage uint8

const (
	stageDecodeHeader stage = iota
	stageUnwrap
	stageAdmit
	stageReplayCheck
	stageRoute
	stageDecodeArgs
	stageReplyWait
	stageResolveIDs
	stageMapError
	stageEncode
	stageWrap
	stageSend
	stageReplyTooBig
	stages
)

var stageNames = [stages]string{
	"decode_header", "unwrap", "admit", "replay_check", "route", "decode_args", "reply_wait",
	"resolve_ids", "map_error", "encode", "wrap", "send", "reply_too_big",
}

// refusing is the stages a refusal counts under, and timed the stages
// adapter_wire_seconds observes (§10.1).
var (
	refusing = []stage{stageDecodeHeader, stageUnwrap, stageAdmit, stageReplayCheck, stageRoute, stageDecodeArgs, stageReplyWait, stageResolveIDs, stageReplyTooBig}
	timed    = []stage{stageDecodeHeader, stageUnwrap, stageAdmit, stageReplayCheck, stageRoute, stageDecodeArgs, stageReplyWait, stageResolveIDs, stageMapError, stageEncode, stageWrap, stageSend}
)

// serviceErrors is the error label's values: each sentinel of the service's
// closed error set and its name without Err, in snake case (§10.1). A typed
// error counts under the sentinel it matches.
var serviceErrors = [...]struct {
	err  error
	name string
}{
	{vfs.ErrNotFound, "not_found"}, {vfs.ErrExist, "exist"}, {vfs.ErrNotEmpty, "not_empty"},
	{vfs.ErrNotDir, "not_dir"}, {vfs.ErrIsDir, "is_dir"}, {vfs.ErrAccess, "access"},
	{vfs.ErrPerm, "perm"}, {vfs.ErrStale, "stale"}, {vfs.ErrReadOnly, "read_only"},
	{vfs.ErrNameTooLong, "name_too_long"}, {vfs.ErrInvalid, "invalid"}, {vfs.ErrNoSpace, "no_space"},
	{vfs.ErrQuota, "quota"}, {vfs.ErrLocked, "locked"}, {vfs.ErrRangeNotLocked, "range_not_locked"},
	{vfs.ErrShareViolation, "share_violation"}, {vfs.ErrDeletePending, "delete_pending"}, {vfs.ErrGrace, "grace"},
	{vfs.ErrDelay, "delay"}, {vfs.ErrBlocked, "blocked"}, {vfs.ErrWrongSecurity, "wrong_security"},
	{vfs.ErrState, "state"}, {vfs.ErrCross, "cross"}, {vfs.ErrIncomplete, "incomplete"},
	{vfs.ErrInconsistent, "inconsistent"}, {vfs.ErrLost, "lost"}, {vfs.ErrUnavailable, "unavailable"},
	{vfs.ErrCorrupt, "corrupt"}, {vfs.ErrIO, "io"}, {vfs.ErrPastEnd, "past_end"},
}

// split is why a chain's reply went out in more than one record (§4.7).
type split uint8

const (
	splitUnwrap split = iota
	splitDecode
	splitFlow
	splitReplyPool
	splitQuarter
	splits
)

var splitReasons = [splits]string{"unwrap", "decode", "flow", "reply_pool", "quarter"}

// protocolSeries is what a request records under one protocol label: before
// Route under its connection's Stages.Protocol, from it under its table's.
// refusals holds the refusing stages, wire the timed ones.
type protocolSeries struct {
	inFlight, unmapped adder
	refusals           [stages]adder
	wire               [stages]observer
	mapped             [len(serviceErrors)]adder
}

// labelSeries is what a request records before Route returns its table.
type labelSeries struct {
	protocolSeries
	replayHits adder
}

// tableSeries is what a request records from Route on, and its rows'
// chains, built with it.
type tableSeries struct {
	protocolSeries
	protocol                              string
	member                                bool            // the table is a compound row's Members
	chains                                map[uint32]Call // by operation code
	heldRequests, heldBytes, holdRefusals adder
	splits                                [splits]adder
	panics                                map[uint32]adder        // by operation code
	panicLines                            map[uint32]*lineLimiter // by operation code
	names                                 map[uint32]string       // the request span's name, by operation code
}

func (s *protocolSeries) answered()                         { s.inFlight.Add(-1) }
func (s *protocolSeries) refused(st stage)                  { s.refusals[st].Add(1) }
func (s *protocolSeries) observe(st stage, d time.Duration) { s.wire[st].Observe(d.Seconds()) }

func (s *labelSeries) admitted()  { s.inFlight.Add(1) }
func (s *labelSeries) replayHit() { s.replayHits.Add(1) }

// routed moves a request's in-flight count from its connection's label to t's.
func (t *tableSeries) routed(from *labelSeries) {
	from.inFlight.Add(-1)
	t.inFlight.Add(1)
}

func (t *tableSeries) holdRefused()  { t.holdRefusals.Add(1) }
func (t *tableSeries) split(r split) { t.splits[r].Add(1) }

// panicked counts op's panic. An op outside the table counts nothing: a
// recovered panic is never turned into a crash.
func (t *tableSeries) panicked(op *Op) {
	if c := t.panics[op.Code]; c != nil {
		c.Add(1)
	}
}

// statusMapped counts a service error a row answered under the sentinel it
// matches; an error of core's own matches none and is not counted.
func (s *protocolSeries) statusMapped(err error) {
	if i := serviceError(err); i >= 0 {
		s.mapped[i].Add(1)
	}
}

// unmappedErr counts an error no row of the table maps.
func (s *protocolSeries) unmappedErr() { s.unmapped.Add(1) }

// serviceError is the index in serviceErrors of the sentinel err matches,
// or -1 for an error outside the service's set.
func serviceError(err error) int {
	for i, e := range serviceErrors {
		if errors.Is(err, e.err) {
			return i
		}
	}
	return -1
}

// held counts a request held, keeping n bytes; released, one no longer held.
func (t *tableSeries) held(n int64) {
	t.heldRequests.Add(1)
	t.heldBytes.Add(float64(n))
}

func (t *tableSeries) released(n int64) {
	t.heldRequests.Add(-1)
	t.heldBytes.Add(-float64(n))
}

// holdPoolCharged moves the hold pool's gauge by n bytes, released as a
// negative n.
func (sh *shared) holdPoolCharged(n int64) { sh.holdPool.Add(float64(n)) }

// table is t's series. A table with no Protocol is refused, naming its
// operations: NewOpTable runs before the field is set, so this is the first
// place core can see it. A compound row's Members are resolved with it,
// under its label.
func (sh *shared) table(t *OpTable) (*tableSeries, error) {
	if ts, ok := (*sh.tables.Load())[t]; ok {
		return ts, nil
	}
	if t.Protocol == "" {
		return nil, fmt.Errorf("core: the table of %s has no Protocol", opNames(t))
	}
	return resolve(&sh.mu, &sh.tables, t, func(m map[*OpTable]*tableSeries) *tableSeries {
		ts := sh.newTable(t.Protocol, t, false)
		m[t] = ts
		for _, op := range t.Ops {
			if op.Members != nil && m[op.Members] == nil {
				m[op.Members] = sh.newTable(t.Protocol, op.Members, true)
			}
		}
		return ts
	}), nil
}

// label is the series a connection whose Stages.Protocol is p records
// before Route.
func (sh *shared) label(p string) *labelSeries {
	if ls, ok := (*sh.labels.Load())[p]; ok {
		return ls
	}
	return resolve(&sh.mu, &sh.labels, p, func(m map[string]*labelSeries) *labelSeries {
		ls := &labelSeries{protocolSeries: sh.newProtocol(p), replayHits: sh.fam[vReplayHits](p)}
		m[p] = ls
		return ls
	})
}

func (sh *shared) newProtocol(p string) protocolSeries {
	ps := protocolSeries{inFlight: sh.fam[vInFlight](p), unmapped: sh.fam[vUnmapped](p)}
	for i, e := range serviceErrors {
		ps.mapped[i] = sh.fam[vStatusMapped](p, e.name)
	}
	for _, st := range refusing {
		ps.refusals[st] = sh.fam[vRefusals](p, stageNames[st])
	}
	for _, st := range timed {
		ps.wire[st] = sh.wire(p, stageNames[st])
	}
	return ps
}

// newTable is the series of t's operations under the label p, and their
// chains; member says t is a compound row's Members.
func (sh *shared) newTable(p string, t *OpTable, member bool) *tableSeries {
	ts := &tableSeries{
		protocolSeries: sh.newProtocol(p),
		protocol:       p,
		member:         member,
		chains:         make(map[uint32]Call, len(t.Ops)),
		heldRequests:   sh.fam[vHeldRequests](p),
		heldBytes:      sh.fam[vHeldBytes](p),
		holdRefusals:   sh.fam[vHoldRefusals](p),
		panics:         make(map[uint32]adder, len(t.Ops)),
		panicLines:     make(map[uint32]*lineLimiter, len(t.Ops)),
		names:          make(map[uint32]string, len(t.Ops)),
	}
	for r, reason := range splitReasons {
		ts.splits[r] = sh.fam[vChainSplits](p, reason)
	}
	for code, op := range t.Ops {
		ts.panics[code] = sh.fam[vPanics](p, strings.ToLower(op.Name))
		ts.panicLines[code] = sh.panicLine(p, op.Name)
		ts.names[code] = "adapter." + p + "." + op.Name
	}
	for code, op := range t.Ops {
		ts.chains[code] = sh.chain(op, ts)
	}
	return ts
}

// resolve returns m's entry for k, which build adds to a copy of m under mu.
// It reads m again under mu first, so two first calls at once build k once.
func resolve[K comparable, V any](mu *sync.Mutex, m *atomic.Pointer[map[K]V], k K, build func(map[K]V) V) V {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := (*m.Load())[k]; ok {
		return v
	}
	next := maps.Clone(*m.Load())
	v := build(next)
	m.Store(&next)
	return v
}

// opNames names t by its operations, sorted.
func opNames(t *OpTable) string {
	names := make([]string, 0, len(t.Ops))
	for _, op := range t.Ops {
		names = append(names, op.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
