package nfs

import (
	"context"
	"errors"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// callbacks is one client's callbacks value: the client's ID as the service
// knows it and the protocol it connected with, and nothing about files (RFC
// 20 section 3.3).
//
// decision: (D1, RFC 21 epic 8) every method answers as the client not being
// reachable, the interim answers of this subtask: RecallGrant and RecallAny
// return errNoBackChannel until epic 10 builds the back channel, LockWaitOver
// the same until epic 11's NLM rows, Notify returns nil because NFS registers
// no watch, and Revoked holds nothing pending: its lease expiry arm comes with
// T8.4's session registry, its other reasons with epics 9, 10 and 11 (T11.7).
type callbacks struct {
	client   vfs.ClientID
	protocol vfs.Protocol
}

var _ vfs.Callbacks = (*callbacks)(nil)

// newCallbacks builds the callbacks value for client cl connected with
// protocol p: the value Connect registers.
func newCallbacks(cl vfs.ClientID, p vfs.Protocol) vfs.Callbacks {
	return &callbacks{
		client:   cl,
		protocol: p,
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

// Revoked holds nothing pending on the client to answer with.
func (c *callbacks) Revoked(ctx context.Context, what vfs.Revocation) {
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
