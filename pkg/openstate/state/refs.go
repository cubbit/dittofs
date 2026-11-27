package state

import (
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// StateType is what a state reference names.
type StateType uint8

const (
	StateOpen StateType = iota + 1
	StateLock
	StateGrant
	StateWatch
)

// StateRef names one piece of state without a file handle: its type, its
// shard, the low bits of the grace instance it was minted under, and a token
// of 64 random bits unique within the shard. It resolves by shard, then token.
// A client that sees one cannot derive another's, so it is a bearer ID with
// room: it carries no counter. Seq is the sequence presented with it, judged
// against the state's own: zero is the current one, an older one ErrOldState,
// a newer one ErrBadState.
type StateRef struct {
	Type     StateType
	Shard    entity.ShardID
	Instance uint8
	Token    uint64
	Seq      uint32
}

// AppInstance names the application instance an open belongs to.
type AppInstance struct {
	ID      [16]byte
	Version [2]uint64 // high, low; zero: unversioned
}

// LockSequence is a lock request's place in its open's lock replay: an index
// from 1 to 64, any other naming none, and a 4-bit sequence.
type LockSequence struct {
	Index uint8
	Seq   uint8
}

// Seqs is the owner sequence numbers a request presents; zero where the
// protocol has none. A reference's own sequence travels in OpenRef.
type Seqs struct {
	Owner    *uint32 // the owner's number: the owner of locks', or the owner of opens' for an open, a downgrade, a close or a lock introducing a new owner of locks
	NewOwner uint32  // a lock introducing a new owner of locks: that owner's first number
}

// OpenRef names what a call acts through, by Kind: an open, a lock state, a
// delegation, or the anonymous open of NFSv3 I/O and of NFSv4's anonymous
// stateid. An NFS reference names it by State, with the sequence presented;
// an SMB one by Open, its FileId's ID, at its current sequence. Handle is
// the file handle the request carried, whose file must be the state's.
type OpenRef struct {
	Kind   RefKind
	State  StateRef      // NFS: the open, lock state or delegation; SMB: only the shard
	Open   entity.OpenID // SMB: the open; zero for NFS
	Handle entity.Handle // nil: none; an anonymous open's file is the FileKey's
}

// RefKind is what an OpenRef names.
type RefKind uint8

const (
	RefOpened RefKind = iota + 1
	RefLocked
	RefDelegated
	RefAnonymous
)

// LeaseKey is an SMB lease's key, the client's own: one grant per client,
// key and file covers every open of the file under it.
type LeaseKey [16]byte

// GrantID names a caching grant by its shard, grace instance bits and token,
// as a StateRef does, so a call naming it with no file handle resolves from
// the ID alone, and one naming a grant a loss took is stale. An NFS
// delegation's stateid is its reference, of type StateGrant.
type GrantID struct {
	Shard    entity.ShardID
	Instance uint8
	Token    uint64
}

// WatchID names a watch by its shard, grace instance bits and token, as a
// StateRef does, so Unwatch resolves it from the ID alone.
type WatchID struct {
	Shard    entity.ShardID
	Instance uint8
	Token    uint64
}

// GraceRef names one grace instance of one shard.
type GraceRef struct {
	Shard    entity.ShardID
	Instance uint64
}
