package nfs

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// callbacks is one client's callbacks value: the client's ID as the service
// knows it and the protocol it connected with, and nothing about files (RFC
// 20 section 3.3).
//
// decision: every method but one answers as the client not being reachable,
// the interim answers until the back channel and the NLM rows exist:
// RecallGrant and RecallAny return errNoBackChannel, LockWaitOver the same,
// Notify returns nil because NFS registers no watch. Revoked ends the client's
// sessions when its lease expired, since the lease is what a session lives
// under (RFC 21 section 3.3); its other reasons wait for the state that can
// be revoked. Overturned when a back channel can be called.
type callbacks struct {
	client   vfs.ClientID
	protocol vfs.Protocol
	sessions *protocol.Sessions
}

var _ vfs.Callbacks = (*callbacks)(nil)

// newCallbacks is the constructor of the callbacks value Connect registers
// for a client cl connected with protocol p, over the sessions the node holds.
func newCallbacks(s *protocol.Sessions) func(cl vfs.ClientID, p vfs.Protocol) vfs.Callbacks {
	return func(cl vfs.ClientID, p vfs.Protocol) vfs.Callbacks {
		return &callbacks{
			client:   cl,
			protocol: p,
			sessions: s,
		}
	}
}

// errNoBackChannel reports a client that cannot be reached for a callback:
// the adapter has no back channel to it.
var errNoBackChannel = errors.New("nfs: client has no back channel")

// RecallGrant reports the client as not answering: sent false with an error
// naming the missing back channel, so an unanswered recall ends in the
// service revoking the grant.
func (c *callbacks) RecallGrant(
	ctx context.Context,
	s vfs.SessionRef,
	openers []vfs.Principal,
	g vfs.CachingGrant,
	to vfs.GrantKind,
) (bool, error) {
	return false, errNoBackChannel
}

// Notify reports the changes delivered, since NFS registers no watch yet.
func (c *callbacks) Notify(ctx context.Context, w vfs.WatchID, changes []vfs.Change) error {
	return nil
}

// Revoked ends the client's sessions when its lease expired: they are the
// lease's, and a session left would answer a client the service forgot.
func (c *callbacks) Revoked(ctx context.Context, what vfs.Revocation) {
	if what.Reason == vfs.RevokedLeaseExpired {
		c.sessions.EndClient(c.client)
	}
}

// LockWaitOver reports the client as not answering: refused false with an
// error naming the missing back channel, so the service keeps the lock and
// sends the grant again.
func (c *callbacks) LockWaitOver(
	ctx context.Context,
	w vfs.LockWaiter,
	granted bool,
) (refused bool, err error) {
	return false, errNoBackChannel
}

// RecallAny reports the client as not answering: an error naming the missing
// back channel.
func (c *callbacks) RecallAny(ctx context.Context, keep int) error {
	return errNoBackChannel
}
