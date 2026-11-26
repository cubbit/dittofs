package openstate

import "github.com/cubbit/dittofs/pkg/openstate/state"

// Open state's contract values, declared in package state so the filesystem
// contract re-exports them without importing the table.
type (
	ByteRange    = state.ByteRange
	ChangeMask   = state.ChangeMask
	Client       = state.Client
	Pending      = state.Pending
	Proposal     = state.Proposal
	ClientStatus = state.ClientStatus
	SessionEnd   = state.SessionEnd
	LockedError  = state.LockedError
	GrantKind    = state.GrantKind
	LeaseKey     = state.LeaseKey
	GrantID      = state.GrantID
	CachingGrant = state.CachingGrant
	Recall       = state.Recall
	Lock         = state.Lock
	LockOwner    = state.LockOwner
	LockRange    = state.LockRange
	WaitKind     = state.WaitKind
	LockWaiter   = state.LockWaiter
	AppInstance  = state.AppInstance
	LockSequence = state.LockSequence
	Seqs         = state.Seqs
	OpenRef      = state.OpenRef
	RefKind      = state.RefKind
	GraceRef     = state.GraceRef
	RevokeReason = state.RevokeReason
	Revocation   = state.Revocation
	StateType    = state.StateType
	StateRef     = state.StateRef
	WatchID      = state.WatchID
	Change       = state.Change
)

// The values of open state's enumerations.
const (
	ChangeRemove    = state.ChangeRemove
	ChangeRename    = state.ChangeRename
	ChangeAddEntry  = state.ChangeAddEntry
	ChangeAttrs     = state.ChangeAttrs
	ChangeSetID     = state.ChangeSetID
	ChangeSize      = state.ChangeSize
	ChangeTimes     = state.ChangeTimes
	ChangeSecurity  = state.ChangeSecurity
	ChangeStreams   = state.ChangeStreams
	ChangeEnumerate = state.ChangeEnumerate
	ChangeReplace   = state.ChangeReplace

	GrantNone   = state.GrantNone
	GrantRead   = state.GrantRead
	GrantWrite  = state.GrantWrite
	GrantHandle = state.GrantHandle

	WaitNone     = state.WaitNone
	WaitPoll     = state.WaitPoll
	WaitCallback = state.WaitCallback

	RefOpened    = state.RefOpened
	RefLocked    = state.RefLocked
	RefDelegated = state.RefDelegated
	RefAnonymous = state.RefAnonymous

	RevokedLeaseExpired = state.RevokedLeaseExpired
	RevokedByAdmin      = state.RevokedByAdmin
	RevokedGrant        = state.RevokedGrant
	RevokedShardState   = state.RevokedShardState
	RevokedShare        = state.RevokedShare

	StateOpen  = state.StateOpen
	StateLock  = state.StateLock
	StateGrant = state.StateGrant
	StateWatch = state.StateWatch
)
