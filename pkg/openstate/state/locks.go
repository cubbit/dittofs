package state

import (
	"errors"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate/ranges"
)

// Lock is one byte-range lock, held against the file.
type Lock struct {
	Owner LockOwner
	Open  uint64 // the token of the open it was taken under, whose close releases it; zero: none
	Range LockRange
}

// LockOwner is who holds a lock. Two owners are one when their Client and
// Name are, so two owners of one client conflict like two clients.
type LockOwner struct {
	Client entity.ClientID
	Name   []byte // the owner the client names, at most 1,024 bytes, compared and never interpreted

	// Stack: its locks stack, an exclusive one conflicts with its own, and
	// they are mandatory; else they merge, never conflict with each other,
	// and are advisory.
	//
	// ponytail: one flag carries two rules that go together in every protocol
	// served, as SMB's locks are the only ones that stack and the only ones
	// that are mandatory. Split it into two flags when a protocol is served
	// whose locks stack but are advisory, or merge but are mandatory.
	Stack bool
}

// LockRange is one range of a lock request; one request may mix shared and
// exclusive ranges.
type LockRange struct {
	ByteRange
	Exclusive bool
}

// WaitKind is how a conflicting lock request waits.
type WaitKind uint8

const (
	WaitNone     WaitKind = iota
	WaitPoll              // the client asks again; a freed range is reserved for its next request
	WaitCallback          // granted in the step that frees the range, and reported through Sink.LockWaitOver
)

// LockWaiter is a lock request that conflicted and asked to wait.
type LockWaiter struct {
	File     entity.FileID
	Owner    LockOwner
	Open     uint64 // the open's token; zero: the lock names no open
	Range    LockRange
	Wait     WaitKind
	Deadline time.Duration // poll only: one lease period after the owner's last poll, or after its range freed
	Reserved bool          // poll only: the range is free and held for this owner until Deadline
	Replay   *LockSequence // the request's place in its open's lock replay, stored when it is granted; nil: none
}

// ByteRange is Length bytes from Offset, ending at Offset+Length-1, a zero
// Length's included; a length of all ones reaches the last byte.
type ByteRange = ranges.Range

// ErrLocked is a lock request refused by a conflicting lock, as a
// *LockedError naming the holder.
var ErrLocked = errors.New("openstate: range locked")

// LockedError is a lock conflict. errors.Is(err, ErrLocked) holds for it, and
// Holder is the conflicting lock, which a protocol's denial reports.
type LockedError struct{ Holder Lock }

func (e *LockedError) Error() string        { return ErrLocked.Error() }
func (e *LockedError) Is(target error) bool { return target == ErrLocked }
