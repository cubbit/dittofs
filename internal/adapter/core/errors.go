package core

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Status is a protocol's wire status: an NFS stat, an RPC accept or reject
// state, an NTSTATUS.
type Status uint32

// ErrorTable maps a service error to a Status. Map takes the most specific
// row that matches, in a total order: a row whose Err is a typed value before
// one whose Err is a sentinel; then, among those, a row naming Ops and
// Versions, Ops alone, Versions alone, neither. A NotFailure row is taken
// only where no other row matches. NewErrorTable refuses two rows equal in
// that order that can match the same error, operation and version.
//
// A zero Status is success in every protocol here, and a NotFailure row's
// status is one its protocol counts as no failure for the operations it
// names. Every other status is a failure, which ends a compound there and
// fails the related members of a chain after it (RFC 20 §4.3).
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
	// NotFailure marks Status, for the operations the row names, as one its
	// protocol counts as no failure: a compound runs its next member past
	// it, and a chain its related members (RFC 20 §4.3). Its Err is a value
	// of its pair's own, returned only by that pair's handler. NewErrorTable
	// refuses a row keyed on an error every operation shares; review holds
	// the rest, and each protocol's test pins its table's NotFailure rows.
	NotFailure bool
	// Hold, only in a table that Holds and never beside NoReply (NewOpTable
	// refuses it elsewhere): a request whose latest attempt maps here is held
	// and re-driven until the deadline Hold returns, counted from the hold's
	// start, and is then answered Status; a deadline at or before the
	// attempt's time answers Status now. nil: answered Status at once. The
	// protocol's New builds it as a closure over the adapter's live settings,
	// so a held request follows the latest settings.
	Hold   func(err error, start time.Time) time.Time
	Status Status
}

// NewErrorTable builds a table answering an error no row matches with fault,
// the protocol's server-fault status. It refuses two rows equal in rank that
// can match one error, operation and version, and a table with no row
// answering ErrDecode for every operation and version (one naming neither Ops
// nor Versions that replies at once with a status other than the fault or
// success), or with a row for it, whatever its Ops or Versions, that holds,
// sends no reply or answers the fault or success. It also refuses a
// NotFailure row that names no Ops, whose Status is zero or the fault, that
// sets NoReply or Hold, or whose Err matches, by errors.Is, a value of
// vfs.Errors() or Errors(), which every operation shares: such a row would
// let the members after any failure run.
//
// decision: a table that fails its check panics, naming the rows, since a
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
	shared := slices.Concat(vfs.Errors(), Errors())
	for _, r := range t.rows {
		if r.NotFailure && (len(r.Ops) == 0 || r.Status == 0 || r.Status == fault || r.NoReply || r.Hold != nil ||
			slices.ContainsFunc(shared, func(e error) bool { return errors.Is(e, r.Err) })) {
			panic(fmt.Sprintf("core: error table: NotFailure row %s names no operation, answers success or the fault, sends no reply, holds, or matches an error every operation shares", describe(r)))
		}
	}
	// A decode error is mapped with no operation, and outside any version a
	// row names, so only a row naming neither maps it everywhere. Without
	// one a client's garbage reaches the fault status, which §10.1 alerts on
	// as a bug; a row that holds, sends no reply or answers the fault status
	// would hide it the same way, and one answering success would tell the
	// client it was served, whatever either names, since Map takes a narrower
	// row first.
	if r, ok := t.badRow(ErrDecode); ok {
		panic(fmt.Sprintf("core: error table: row %s matches ErrDecode but holds, sends no reply or answers the fault or success", describe(r)))
	}
	if !t.mapsEverywhere(ErrDecode) {
		panic("core: error table: no row answers ErrDecode for every operation and version")
	}
	return t
}

// mapsEverywhere reports whether a row err matches names neither Ops nor
// Versions, so some row maps err for every operation and version.
func (t *ErrorTable) mapsEverywhere(err error) bool {
	return slices.ContainsFunc(t.rows, func(r ErrorRow) bool {
		return r.Ops == nil && r.Versions == nil && errors.Is(err, r.Err)
	})
}

// badRow returns a row err matches that holds, sends no reply or answers the
// fault or success: Map answers err by it for every operation and version it
// names, so err is not answered at once there, or is answered as served. A
// zero Status is success in every protocol here: NFS3_OK, NFS4_OK, RPC's
// SUCCESS and STATUS_SUCCESS are all 0.
//
// decision: ErrDecode's and ErrCancelled's checks pass the bare sentinel,
// which a protocol's typed row never matches, so that row still wins for its
// own value (§4.5) and goes unchecked: a protocol may hold its own decode or
// cancel value, send no reply for it, or answer it the fault or success, and
// only that protocol's tests would see it. The ceiling is that protocol's own
// value; the catch-all for the sentinel stays checked. Check typed rows too if
// a protocol is found answering its own value that way.
func (t *ErrorTable) badRow(err error) (ErrorRow, bool) {
	for _, r := range t.rows {
		if errors.Is(err, r.Err) && (r.NoReply || r.Hold != nil || r.Status == t.fault || r.Status == 0) {
			return r, true
		}
	}
	return ErrorRow{}, false
}

// typedOnly is t with its typed rows alone, for a check of a typed value that
// also matches a sentinel's row.
func (t *ErrorTable) typedOnly() *ErrorTable {
	rows := slices.DeleteFunc(slices.Clone(t.rows), func(r ErrorRow) bool { return !r.typed() })
	return &ErrorTable{fault: t.fault, rows: rows}
}

// Map is err's status for op at v. reply false is a NoReply row; notFailure
// true is a NotFailure row's status, after which the compound runner runs the
// next member and a chain's related ones (RFC 20 §4.3). An err that also
// matches a row not marked NotFailure maps to that row instead, as an
// errors.Join(pairErr, err) or a pair's error wrapping a service one does, so
// a failure is never answered as none.
func (t *ErrorTable) Map(err error, op *Op, v Version) (s Status, reply, notFailure bool) {
	r, _ := t.row(err, op, v)
	return r.Status, !r.NoReply, r.NotFailure
}

// row is the row Map answers err by for op at v, and whether one matched: an
// error no row matches is answered the fault status, with a reply.
func (t *ErrorTable) row(err error, op *Op, v Version) (ErrorRow, bool) {
	for _, r := range t.rows {
		if r.matches(err, op, v) {
			return r, true
		}
	}
	return ErrorRow{Status: t.fault}, false
}

// hold is the Hold of the row that maps err for op at v: nil where that row
// has none, or no row maps err.
func (t *ErrorTable) hold(err error, op *Op, v Version) func(error, time.Time) time.Time {
	for _, r := range t.rows {
		if r.matches(err, op, v) {
			return r.Hold
		}
	}
	return nil
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

// rank is r's place in Map's order, lowest first: every other row before a
// NotFailure row; then a typed row before a sentinel's; then Ops and
// Versions, Ops, Versions, neither.
func rank(r ErrorRow) int {
	n := 0
	if r.NotFailure {
		n = 8
	}
	if !r.typed() {
		n += 4
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

// typed reports whether r's Err is a typed value: one with an Is method.
func (r ErrorRow) typed() bool {
	_, ok := r.Err.(interface{ Is(error) bool })
	return ok
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
	ErrNotSupported   = errors.New("core: operation not supported")                // absent from the table, or switched off
	ErrVersion        = errors.New("core: version not served")                     // outside Op.Versions
	ErrNotNegotiated  = errors.New("core: not negotiated")                         // Needs: negotiated
	ErrNoSession      = errors.New("core: no session")                             // Needs: session
	ErrNoTree         = errors.New("core: no tree")                                // Needs: tree
	ErrNoFH           = errors.New("core: no current filehandle")                  // Needs: NeedsFH
	ErrTooLong        = errors.New("core: compound past its cap")                  // the compound cap
	ErrReplyTooBig    = errors.New("core: reply past a quarter of the reply pool") // a reply's charge past a quarter of the reply pool's ceiling, or a compound's reply past the record cap, a READ left less than one byte among them (§4.3)
	ErrExpired        = errors.New("core: credential expired")                     // an expired credential, Op.Expired false
	ErrNotLast        = errors.New("core: not the last member of a chain")         // Park
	ErrTooManyPending = errors.New("core: too many pending requests")              // Stages.MaxPending
	// ErrDecode refuses a header, an operation's or a member's arguments, or a
	// step of the record's walk that does not decode (§4.1). The pipeline
	// wraps every error a header's or a row's Decode, or a members' table's
	// ReadOp, returns, once, as
	// fmt.Errorf("%w: %v", ErrDecode, err): the cause's text survives, the
	// cause itself is not matched, so no row for it answers in ErrDecode's
	// place. A codec returns its own error and never ErrDecode. Unwrap's
	// refusals are the stage's to name, since they answer differently (access
	// denied, ErrDecode for a malformed inner header, or close); core passes
	// them unchanged.
	ErrDecode    = errors.New("core: cannot decode")
	ErrCancelled = errors.New("core: cancelled by its client") // a held or parked request its client cancelled (§6.5); wraps nothing
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
		ErrTooLong, ErrReplyTooBig, ErrExpired, ErrNotLast, ErrTooManyPending, ErrDecode, ErrCancelled, ErrMoved,
		ErrDraining,
	}
}
