package core

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
	vShortReads
	vConnections
	vCloses
	vBytes
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
	vShortReads:    {metrics.Spec{Name: "adapter_reads_shortened_total", Help: "Reads in a compound whose count was lowered to fit the protocol's record cap.", Labels: []string{"protocol"}}, false},
	vConnections:   {metrics.Spec{Name: "adapter_connections", Help: "Connections open, in their state.", Labels: []string{"protocol", "state"}}, true},
	vCloses:        {metrics.Spec{Name: "adapter_connection_closes_total", Help: "Connections refused or closed, by the reason of the close.", Labels: []string{"protocol", "reason"}}, false},
	vBytes:         {metrics.Spec{Name: "adapter_bytes_total", Help: "Bytes read and written, framing included.", Labels: []string{"protocol", "direction"}}, false},
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
	{vfs.ErrLogonFailure, "logon_failure"}, {vfs.ErrUnknownPrincipal, "unknown_principal"},
}

// split is why a chain's reply went out in more than one record (§4.7).
type split uint8

const (
	splitUnwrap split = iota
	splitDecode
	splitFlow
	splitReplyPool
	splitQuarter
	splitRecord
	splits
)

var splitReasons = [splits]string{"unwrap", "decode", "flow", "reply_pool", "quarter", "record"}

// The values of the direction label of adapter_bytes_total (§10.1): in, what
// the reader read, and out, what the writer wrote.
const (
	dirIn  = "in"
	dirOut = "out"
)

// connState is a value of the state label of adapter_connections (§10.1):
// handshake from accept until Conn.Established, established from it on.
type connState uint8

const (
	stHandshake connState = iota
	stEstablished
	connStates
)

var stateNames = [connStates]string{"handshake", "established"}

// reason is a value of the reason label of adapter_connection_closes_total
// (§10.1), one per close: at accept pool, limit and per_address; by the client
// client; at a deadline handshake, read_timeout, idle, write_timeout,
// status_wait and reply_wait; by core write, frame, decode, unwrap, walk and
// wiring; by protocol code violation; by the adapter removed, drain and stop.
type reason uint8

const (
	rsnPool reason = iota
	rsnLimit
	rsnPerAddress
	rsnClient
	rsnReadTimeout
	rsnHandshake
	rsnIdle
	rsnWriteTimeout
	rsnStatusWait
	rsnReplyWait
	rsnWrite
	rsnFrame
	rsnDecode
	rsnUnwrap
	rsnWalk
	rsnWiring
	rsnViolation
	rsnDrain
	rsnRemoved
	rsnStop
	reasons
)

var reasonNames = [reasons]string{
	"pool", "limit", "per_address", "client", "read_timeout", "handshake", "idle", "write_timeout",
	"status_wait", "reply_wait", "write", "frame", "decode", "unwrap", "walk", "wiring", "violation",
	"drain", "removed", "stop",
}

// level is where a line of a close under r is logged (§10.3): debug for the
// ends RFC 20 names quiet, the client's, the idle deadline's and the
// adapter's own; error for a wiring fault; warning for every other.
func (r reason) level() slog.Level {
	switch r {
	case rsnClient, rsnIdle, rsnDrain, rsnRemoved, rsnStop:
		return slog.LevelDebug
	case rsnWiring:
		return slog.LevelError
	}
	return slog.LevelWarn
}

// protocolSeries is what a request records under one protocol label: before
// Route under its connection's Stages.Protocol, from it under its table's.
// refusals holds the refusing stages, wire the timed ones.
type protocolSeries struct {
	inFlight, unmapped adder
	refusals           [stages]adder
	wire               [stages]observer
	mapped             [len(serviceErrors)]adder
	splits             [splits]adder
}

// labelSeries is what a request records before Route returns its table, and
// what a connection records before its stage list is built: the series of
// §10.1's connection table, under the label its listener's nil-Conn stages
// named.
type labelSeries struct {
	protocolSeries
	protocol   string
	replayHits adder
	states     [connStates]adder
	closes     [reasons]adder
	in, out    adder
}

// tableSeries is what a request records from Route on, and its rows'
// chains, built with it.
type tableSeries struct {
	protocolSeries
	protocol                              string
	member                                bool            // the table is a compound row's Members
	chains                                map[uint32]Call // by operation code
	heldRequests, heldBytes, holdRefusals adder
	shortened                             adder                   // a compound's members whose bound their room lowered
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

func (t *tableSeries) holdRefused()     { t.holdRefusals.Add(1) }
func (s *protocolSeries) split(r split) { s.splits[r].Add(1) }

// readsShortened counts n members of a compound whose bound their room
// lowered: its reads answered short (§4.3).
func (t *tableSeries) readsShortened(n int) { t.shortened.Add(float64(n)) }

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

// table is the series NewServer resolved for t, which Route returned for a
// header of version v. A table no listener's Tables listed, a compound row's
// Members among them, or one whose Versions do not hold v, is a fault of the
// protocol's wiring (§4.7): the error closes the connection, naming the
// table.
func (sh *shared) table(t *OpTable, v Version) (*tableSeries, error) {
	ts, ok := sh.tables[t]
	if !ok || ts.member {
		return nil, fmt.Errorf("%w: Route returned the table of %s, which no listener's Tables listed", errWiring, opNames(t))
	}
	if r := t.Versions; r != nil && (v < r.First || v > r.Last) {
		return nil, fmt.Errorf("%w: Route returned the table of %s, whose Versions %d to %d do not hold version %d", errWiring, opNames(t), r.First, r.Last, v)
	}
	return ts, nil
}

// resolveTables resolves the series of each of ts, which checkTables
// returned, under its Protocol, and of each of its compound rows' Members
// with it, under that same label. NewServer calls it before any listener
// accepts, so no request writes sh.tables and every request reads it with no
// lock.
func (sh *shared) resolveTables(ts []*OpTable) {
	for _, t := range ts {
		sh.tables[t] = sh.newTable(t.Protocol, t, false)
		for _, op := range t.Ops {
			if op.Members != nil && sh.tables[op.Members] == nil {
				sh.tables[op.Members] = sh.newTable(t.Protocol, op.Members, true)
			}
		}
	}
}

// label is the series a connection whose Stages.Protocol is p records
// before Route, and the connection series under the same label (§10.1):
// read with no lock from the copy-on-write map, and resolved under mu at the
// first accept that names p, which reads the map again there, so two first
// accepts at once resolve p once.
func (sh *shared) label(p string) *labelSeries {
	if ls, ok := (*sh.labels.Load())[p]; ok {
		return ls
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if ls, ok := (*sh.labels.Load())[p]; ok {
		return ls
	}
	next := maps.Clone(*sh.labels.Load())
	ls := &labelSeries{
		protocolSeries: sh.newProtocol(p),
		protocol:       p,
		replayHits:     sh.fam[vReplayHits](p),
		in:             sh.fam[vBytes](p, dirIn),
		out:            sh.fam[vBytes](p, dirOut),
	}
	for st, name := range stateNames {
		ls.states[st] = sh.fam[vConnections](p, name)
	}
	for r, name := range reasonNames {
		ls.closes[r] = sh.fam[vCloses](p, name)
	}
	next[p] = ls
	sh.labels.Store(&next)
	return ls
}

// accepted counts a connection in at accept, in the handshake state;
// established moves it to the established state, once (§10.1).
func (ls *labelSeries) accepted()    { ls.states[stHandshake].Add(1) }
func (ls *labelSeries) established() { ls.states[stEstablished].Add(1); ls.states[stHandshake].Add(-1) }

// closed counts a connection's close under r, its state's gauge lowered from
// established where est, the handshake state otherwise. refusedAtAccept counts
// one closed at accept, whose gauge never rose: pool, limit and per_address's
// closes (§10.1).
func (ls *labelSeries) closed(r reason, est bool) {
	ls.closes[r].Add(1)
	if est {
		ls.states[stEstablished].Add(-1)
		return
	}
	ls.states[stHandshake].Add(-1)
}

func (ls *labelSeries) refusedAtAccept(r reason) { ls.closes[r].Add(1) }

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
	for r, reason := range splitReasons {
		ps.splits[r] = sh.fam[vChainSplits](p, reason)
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
		shortened:      sh.fam[vShortReads](p),
		panics:         make(map[uint32]adder, len(t.Ops)),
		panicLines:     make(map[uint32]*lineLimiter, len(t.Ops)),
		names:          make(map[uint32]string, len(t.Ops)),
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

// opNames names t by its operations, sorted.
func opNames(t *OpTable) string {
	names := make([]string, 0, len(t.Ops))
	for _, op := range t.Ops {
		names = append(names, op.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
