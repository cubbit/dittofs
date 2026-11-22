package identity

import (
	"context"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Resolver is admission's resolution step: from an Identity to the Caller it
// acts as on one share. *vfs.Filesystem is its only caller. p is the share's
// policy, built from one ShareIndex.Share read, the share with its
// GrantVersion and its export policy; narrowed is why admission held the call
// to read-only before resolution: ReadOnlyNotNarrowed, ReadOnlyShare or
// ReadOnlyClient, never ReadOnlyGrant, which resolution sets itself. Resolve
// answers vfserr.ErrAccess for a disabled or deleted principal, a deleted
// Anonymous, a principal over the group cap, or a Secret identity while NTLM
// is off, and vfserr.ErrUnavailable for
// an undecidable lookup or an incomplete group closure.
type Resolver interface {
	Resolve(ctx context.Context, id Identity, p entity.SharePolicy, narrowed ReadOnlyCause) (*Caller, error)
	// ResolveMark resolves by, the principal that marked a file delete on
	// close, for the unlink the last close runs: as Resolve resolves a
	// Secret identity bound to by, but never refused because NTLM is off.
	ResolveMark(ctx context.Context, by entity.Principal, p entity.SharePolicy, narrowed ReadOnlyCause) (*Caller, error)
	// Current is the serial of the State a Resolve starting now reads, one
	// atomic load of that State: admission stores and serves a cached Caller
	// only while its Built equals it.
	//
	// decision: the serial is read through the Resolver, not handed to
	// admission some other way, because admission has no other path to the
	// identity server's State and a cache that cannot tell a stale caller
	// would serve one. Overturned if admission ever receives the State
	// itself.
	Current() uint64
}

// Caller is what one call acts as on one share. Every field any check reads
// is here.
type Caller struct {
	Principal     entity.Principal   // after squashing: what the permission check and grants read
	Mapped        entity.Principal   // before squashing: what state protection and a client record's creator compare; the permission check never reads it
	Group         entity.Principal   // the credential's primary group, an unsquashed claimed caller's gid: what a create stamps; zero for any other caller
	Groups        []entity.Principal // the closure, sorted
	DenyOnly      []entity.Principal // principals the proof marked deny-only; empty when squashed
	Access        entity.ShareAccess // the grant's level, narrowed to read where admission narrowed it
	ReadOnly      ReadOnlyCause      // why Access is read
	Authenticated bool               // the proof proved a principal
	Admin         bool               // an admin grant to a proven caller
	Privileged    bool               // may bypass file permissions; a protocol may confine it to an open
	Built         uint64             // the serial of the State it was resolved under; no check reads it
	Fresh         time.Time          // no cache serves it after this; no check reads it
}

// ReadOnlyCause is why a Caller's Access is read, so a refused mutation says
// why, and which narrowed it: one the share or a client rule holds read-only
// is vfserr.ErrReadOnly, one a read grant refuses is vfserr.ErrAccess.
type ReadOnlyCause uint8

const (
	ReadOnlyNotNarrowed ReadOnlyCause = iota // Access is not read, or nothing narrowed it
	ReadOnlyGrant                            // the grant's own level is read
	ReadOnlyShare                            // the policy's ReadOnly flag
	ReadOnlyClient                           // a client rule admits the caller's address read-only
)
