package journal

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/recovery"
)

// The journal's errors, declared in internal/core so the internal packages can
// return them. Each one's contract is documented at its declaration.
var (
	ErrNotImplemented    = core.ErrNotImplemented
	ErrOpenRefused       = core.ErrOpenRefused
	ErrClosed            = core.ErrClosed
	ErrHandleOpen        = core.ErrHandleOpen
	ErrNotEmpty          = core.ErrNotEmpty
	ErrNoJournal         = core.ErrNoJournal
	ErrUnidentified      = core.ErrUnidentified
	ErrIdentityMismatch  = core.ErrIdentityMismatch
	ErrUnknownFormat     = core.ErrUnknownFormat
	ErrCorrupt           = core.ErrCorrupt
	ErrStaleFill         = core.ErrStaleFill
	ErrNotOffloaded      = core.ErrNotOffloaded
	ErrNotOffered        = core.ErrNotOffered
	ErrOfferEnded        = core.ErrOfferEnded
	ErrZeroLength        = core.ErrZeroLength
	ErrInvalidExtent     = core.ErrInvalidExtent
	ErrSyncFailed        = core.ErrSyncFailed
	ErrNoSpace           = core.ErrNoSpace
	ErrOtherShare        = core.ErrOtherShare
	ErrInvalidSetting    = core.ErrInvalidSetting
	ErrNoClock           = core.ErrNoClock
	ErrJournalReadOnly   = core.ErrJournalReadOnly
	ErrUnsuitableStorage = core.ErrUnsuitableStorage
	ErrUnreadable        = core.ErrUnreadable

	// ErrReservationExhausted is a reserved write's refusal when what its
	// reservation still holds does not cover the write. It is not a NoSpaceError:
	// no journal limit was met, the caller asked for less than it then wrote.
	ErrReservationExhausted = core.ErrReservationExhausted
)

type (
	Limit                 = core.Limit
	Unit                  = core.Unit
	NoSpaceError          = core.NoSpaceError
	OtherShareError       = core.OtherShareError
	IdentityError         = core.IdentityError
	SyncError             = core.SyncError
	NotOffloadedError     = core.NotOffloadedError
	ReadOnlyState         = core.ReadOnlyState
	UnreadableRangesError = core.UnreadableRangesError

	// LostPastReserveError is the open's past-reserve refusal, the type
	// Open wraps its ErrOpenRefused with when corrupt losses would run past
	// the records the headroom keeps for the open. Its declaration and
	// contract live with the loss pass that answers it
	// (internal/recovery/seal.go), which never passes a core boundary; the
	// alias hands the error's value to a caller that reads the count it
	// carries, most often to name the operator's remedy.
	LostPastReserveError = recovery.LostPastReserveError
)

// The limits a NoSpaceError names; see Limit.
const (
	LimitShare    = core.LimitShare
	LimitJournal  = core.LimitJournal
	LimitIndex    = core.LimitIndex
	LimitHeadroom = core.LimitHeadroom
	LimitVersion  = core.LimitVersion
	LimitSpare    = core.LimitSpare
)

// The units a NoSpaceError counts in; see Unit.
const (
	UnitBytes    = core.UnitBytes
	UnitEntries  = core.UnitEntries
	UnitVersions = core.UnitVersions
	UnitSegments = core.UnitSegments
)
