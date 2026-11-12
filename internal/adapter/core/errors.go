package core

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Status is a protocol's wire status: an NFS stat, an RPC accept or reject
// state, an NTSTATUS.
type Status uint32

// ErrorTable maps a service error to a Status. Map takes the most specific
// row that matches, in a total order: a row whose Err is a typed value before
// one whose Err is a sentinel; then, among those, a row naming Ops and
// Versions, Ops alone, Versions alone, neither. NewErrorTable refuses two rows
// equal in that order that can match the same error, operation and version.
type ErrorTable struct {
	fault Status
	rows  []ErrorRow // most specific first
}

// ErrorRow is one mapping of an ErrorTable.
type ErrorRow struct {
	Err      error    // matched with errors.Is: a sentinel matches its whole class, a typed value its own kind only
	Ops      []uint32 // nil: every operation
	Versions *Range   // nil: every version
	// NoReply is no final reply (ErrDraining, and ErrMoved where no move
	// signal fits); a table that Holds sends an interim granting the
	// request's credits. Status is unused.
	NoReply bool
	// Hold, only in a table that Holds (NewOpTable refuses it elsewhere): a
	// request whose latest attempt maps here is held and re-driven until the
	// deadline Hold returns, counted from the hold's start, and is then
	// answered Status; a deadline at or before the attempt's time answers
	// Status now. nil: answered Status at once. The protocol's New builds it
	// as a closure over the adapter's live settings, so a held request
	// follows the latest settings.
	Hold   func(err error, start time.Time) time.Time
	Status Status
}

// NewErrorTable builds a table answering an error no row matches with fault,
// the protocol's server-fault status.
//
// decision: a table that fails its check panics, naming both rows, since a
// table is program text and NewErrorTable returns no error; the first
// request a protocol serves builds it, so a test of the protocol finds it.
// Overturned if a table is ever built from configuration.
func NewErrorTable(fault Status, rows ...ErrorRow) *ErrorTable {
	t := &ErrorTable{fault: fault, rows: slices.Clone(rows)}
	// decision: a typed row is one whose Err has an Is method, as the
	// service's typed values do, matching their sentinel and their own kind;
	// a sentinel is a plain value. The rows are ranked once here and Map
	// scans them in order: an error is the slow path, so no index is kept.
	slices.SortStableFunc(t.rows, func(a, b ErrorRow) int { return rank(a) - rank(b) })
	for i, a := range t.rows {
		for _, b := range t.rows[i+1:] {
			if rank(a) == rank(b) && overlap(a, b) {
				panic(fmt.Sprintf("core: error table: rows %s and %s can both match one error, operation and version", describe(a), describe(b)))
			}
		}
	}
	return t
}

// Map is err's status for op at v; reply false is a NoReply row.
func (t *ErrorTable) Map(err error, op *Op, v Version) (s Status, reply bool) {
	s, reply, _ = t.mapped(err, op, v)
	return s, reply
}

// mapped is Map, and whether a row matched: an error no row matches is
// answered the fault status, with a reply.
func (t *ErrorTable) mapped(err error, op *Op, v Version) (s Status, reply, matched bool) {
	for _, r := range t.rows {
		if r.matches(err, op, v) {
			return r.Status, !r.NoReply, true
		}
	}
	return t.fault, true, false
}

// matches reports whether r maps err for op at v. A nil op matches only a
// row naming no operation.
func (r ErrorRow) matches(err error, op *Op, v Version) bool {
	if r.Ops != nil && (op == nil || !slices.Contains(r.Ops, op.Code)) {
		return false
	}
	if r.Versions != nil && (v < r.Versions.First || v > r.Versions.Last) {
		return false
	}
	return errors.Is(err, r.Err)
}

// rank is r's place in Map's order, lowest first: a typed row before a
// sentinel's, then Ops and Versions, Ops, Versions, neither.
func rank(r ErrorRow) int {
	n := 0
	if _, typed := r.Err.(interface{ Is(error) bool }); !typed {
		n = 4
	}
	switch {
	case r.Ops != nil && r.Versions != nil:
	case r.Ops != nil:
		n++
	case r.Versions != nil:
		n += 2
	default:
		n += 3
	}
	return n
}

// overlap reports whether two rows of one rank can match one error,
// operation and version: either's Err matches the other's, and their
// operations and versions meet.
func overlap(a, b ErrorRow) bool {
	if !errors.Is(a.Err, b.Err) && !errors.Is(b.Err, a.Err) {
		return false
	}
	if a.Ops != nil && !slices.ContainsFunc(a.Ops, func(c uint32) bool { return slices.Contains(b.Ops, c) }) {
		return false
	}
	return a.Versions == nil || (a.Versions.First <= b.Versions.Last && b.Versions.First <= a.Versions.Last)
}

// describe names r in a build's panic.
func describe(r ErrorRow) string {
	s := fmt.Sprintf("{%v", r.Err)
	if r.Ops != nil {
		s += fmt.Sprintf(" ops %v", r.Ops)
	}
	if r.Versions != nil {
		s += fmt.Sprintf(" versions %d-%d", r.Versions.First, r.Versions.Last)
	}
	return s + "}"
}

// core's own refusals. Each table maps every one, and the tables' totality
// test takes them from Errors beside the service's errors.
var (
	ErrNotSupported   = errors.New("core: operation not supported")        // absent from the table, or switched off
	ErrVersion        = errors.New("core: version not served")             // outside Op.Versions
	ErrNotNegotiated  = errors.New("core: not negotiated")                 // Needs: negotiated
	ErrNoSession      = errors.New("core: no session")                     // Needs: session
	ErrNoTree         = errors.New("core: no tree")                        // Needs: tree
	ErrNoFH           = errors.New("core: no current filehandle")          // Needs: NeedsFH
	ErrTooLong        = errors.New("core: compound past its cap")          // the compound cap
	ErrExpired        = errors.New("core: credential expired")             // an expired credential, Op.Expired false
	ErrNotLast        = errors.New("core: not the last member of a chain") // Park
	ErrTooManyPending = errors.New("core: too many pending requests")      // Stages.MaxPending
	// ErrMoved and ErrDraining are what a request the drain does not serve
	// gets while the adapter drains: ErrMoved where the service names a
	// gateway node for the request's share (the move signal), ErrDraining
	// where it names none (no final reply, ErrorRow.NoReply).
	ErrMoved    = errors.New("core: moved")
	ErrDraining = errors.New("core: draining")
)

// Errors lists core's refusals above, in their order, for the tables'
// totality test. ErrParked is no refusal: a table never maps it.
func Errors() []error {
	return []error{
		ErrNotSupported, ErrVersion, ErrNotNegotiated, ErrNoSession, ErrNoTree, ErrNoFH,
		ErrTooLong, ErrExpired, ErrNotLast, ErrTooManyPending, ErrMoved, ErrDraining,
	}
}
