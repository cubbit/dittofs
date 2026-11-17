package core

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/resources"
)

// What the package's external tests reach of core's unexported parts. It is
// the standard library's export_test.go by another name, since
// exports_test.go here is the surface test.

type (
	Shared      = shared
	TableSeries = tableSeries
	LabelSeries = labelSeries
)

var (
	NewShared    = newShared
	SplitReasons = splitReasons
)

// StageNames is the names of the stages a refusal counts under and of those
// adapter_wire_seconds observes, by their index for Refused and Observe.
func StageNames() (refusals, wire map[int]string) {
	refusals, wire = map[int]string{}, map[int]string{}
	for _, st := range refusing {
		refusals[int(st)] = stageNames[st]
	}
	for _, st := range timed {
		wire[int(st)] = stageNames[st]
	}
	return refusals, wire
}

// ServiceErrorNames is the error label's values, by sentinel.
func ServiceErrorNames() map[error]string {
	m := map[error]string{}
	for _, e := range serviceErrors {
		m[e.err] = e.name
	}
	return m
}

func (sh *shared) Table(t *OpTable) (*TableSeries, error) { return sh.table(t) }
func (sh *shared) Label(p string) *LabelSeries            { return sh.label(p) }
func (sh *shared) HoldPoolCharged(n int64)                { sh.holdPoolCharged(n) }

func (s *protocolSeries) Answered()                       { s.answered() }
func (s *protocolSeries) Refused(st int)                  { s.refused(stage(st)) }
func (s *protocolSeries) Observe(st int, d time.Duration) { s.observe(stage(st), d) }

func (s *labelSeries) Admitted()  { s.admitted() }
func (s *labelSeries) ReplayHit() { s.replayHit() }

func (t *tableSeries) Routed(from *LabelSeries) { t.routed(from) }
func (t *tableSeries) UnmappedErr()             { t.unmappedErr() }
func (t *tableSeries) HoldRefused()             { t.holdRefused() }
func (t *tableSeries) Split(reason int)         { t.split(split(reason)) }
func (t *tableSeries) Panicked(op *Op)          { t.panicked(op) }
func (t *tableSeries) StatusMapped(err error)   { t.statusMapped(err) }
func (t *tableSeries) Held(n int64)             { t.held(n) }
func (t *tableSeries) Released(n int64)         { t.released(n) }

// CountLookups makes n count each child sh resolves from here on, each a
// WithLabelValues on a registry.
func CountLookups(sh *Shared, n *atomic.Int64) {
	for i, f := range sh.fam {
		sh.fam[i] = func(lv ...string) adder {
			n.Add(1)
			return f(lv...)
		}
	}
	wire := sh.wire
	sh.wire = func(lv ...string) observer {
		n.Add(1)
		return wire(lv...)
	}
}

// NewCapped is the FlowControl a connection gets where its protocol has none.
func NewCapped() FlowControl { return newCapped() }

var (
	ReserveReply  = reserveReply
	ReserveStatus = reserveStatus
	ErrReplyWait  = errReplyWait
)

// TestReader is one connection's record cut, reached by the reader's tests.
type TestReader struct{ r *reader }

// NewTestReader returns a record cut over nc, its records charged to key from
// recv, with timeout as read_timeout.
func NewTestReader(nc net.Conn, f Framer, recv *resources.BufPool, key netip.Addr, timeout time.Duration) *TestReader {
	return &TestReader{r: newReader(nc, f, recv, key, timeout)}
}

// Read returns the next record.
func (t *TestReader) Read(ctx context.Context) (*resources.Buf, error) { return t.r.Read(ctx) }

// ErrFrame is the framing refusal every record-level error wraps, so a close
// can name its reason.
var ErrFrame = errFrame
