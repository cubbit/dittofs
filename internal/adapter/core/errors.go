package core

import (
	"errors"
)

// ErrorTable maps a service error to a Status. Map takes the most specific
// row that matches, in a total order: a row whose Err is a typed value before
// one whose Err is a sentinel; then, among those, a row naming Ops and
// Versions, Ops alone, Versions alone, neither. NewErrorTable refuses two rows
// equal in that order that can match the same error, operation and version.
type ErrorTable struct{}

// ErrorRow is one mapping of an ErrorTable.
type ErrorRow struct {
	Err      error    // matched with errors.Is: a sentinel matches its whole class, a typed value its own kind only
	Ops      []uint32 // nil: every operation
	Versions *Range   // nil: every version
	// NoReply is no final reply (ErrDraining, and ErrMoved where no move
	// signal fits); a table that Holds sends an interim granting the
	// request's credits. Status is unused.
	NoReply bool
	Status  Status
}

// NewErrorTable builds a table answering an error no row matches with fault,
// the protocol's server-fault status.
func NewErrorTable(fault Status, rows ...ErrorRow) *ErrorTable {
	panic("not implemented")
}

// Map is err's status for op at v; reply false is a NoReply row.
func (t *ErrorTable) Map(err error, op *Op, v Version) (s Status, reply bool) {
	panic("not implemented")
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

// Errors lists core's refusals above, for the tables' totality test.
func Errors() []error {
	panic("not implemented")
}
