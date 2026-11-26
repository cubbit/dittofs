package state

import (
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Client is a client's record and what the table holds for it in memory.
type Client struct {
	entity.ClientRecord
	Expires time.Duration // the Deps.Clock.Elapsed reading its lease ends at, at this node; zero: no lease
	Pending *Pending      // a proposed instance awaiting its confirm
}

// Pending is a proposed client instance awaiting its confirm. It is held only
// by the node that answered, and never written.
type Pending struct {
	ID      entity.ClientID  // the ID a confirm makes current: this client's, or a new one for a new Boot
	Boot    [8]byte          // the instance proposed
	Owner   entity.Principal // who proposed it; only it may confirm
	Confirm [8]byte          // what the confirm must present: 64 bits from Deps.Rand
	Expires time.Duration    // Deps.Clock.Elapsed one lease period after the proposal; then it is gone
	Adapter []byte           // becomes the record's Adapter at the confirm; never read here
}

// Proposal is what Propose answers and Connect confirms.
type Proposal struct {
	Client  entity.ClientID // Pending.ID
	Confirm [8]byte         // Pending.Confirm
}

// ClientStatus is what a renewal reports beside the lease.
type ClientStatus struct {
	AdminRevoked      bool       // a reference an administrator revoked is in the client's revoked set
	RecallableRevoked bool       // a grant revoked at its recall deadline is
	ReclaimNeeded     bool       // a shard of its state is in a grace instance it has not finished
	Reclaim           []GraceRef // those instances, which the adapter keeps per session for ReclaimComplete
}

// SessionEnd is how Disconnect ends one session.
type SessionEnd struct {
	Logoff bool   // a logoff or a failed re-authentication's teardown: every open of the session closes
	Last   bool   // the node's last session of the client: the node leaves Reach.Sessions in the same call, at Gen
	Gen    uint64 // with Last: the node's entry's Gen when its last session was decided; a later listing's keeps the entry, and Disconnect returns its Gen
}

// RevokeReason is why a client's state ended without the client asking.
type RevokeReason uint8

const (
	RevokedLeaseExpired RevokeReason = iota // its lease expired, or a reboot replaced it
	RevokedByAdmin                          // an administrator revoked the client
	RevokedGrant                            // one caching grant whose recall missed its deadline
	RevokedShardState                       // a shard's open state was lost: Shard names it
	RevokedShare                            // a share was removed, or moved away to Addr: Share names it
)

// Revocation names state ended without the client asking: Grant for
// RevokedGrant; Shard for RevokedShardState; Share, and Addr for a move
// naming one, for RevokedShare; every piece of the client's state for
// RevokedByAdmin. Adapter is the client record's own bytes, so the
// adapter acts with no read: an NLM host's notice address.
type Revocation struct {
	Client  entity.ClientID
	Grant   GrantID
	Shard   entity.ShardID
	Share   entity.ShareID
	Addr    netip.Addr
	Reason  RevokeReason
	Adapter []byte
}
