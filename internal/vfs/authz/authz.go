// Package authz is the filesystem's one caller of the metadata store's
// authorisation chokepoint: every Authorize and Granted the filesystem makes
// is made here, through Decide and Rights, and here the filesystem builds the
// access event the share's audit policy selects, since the chokepoint emits
// nothing.
package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Caller is one call's caller as admission resolved it on one share, and
// what an access event names of it.
type Caller struct {
	entity.Identity               // as the chokepoint takes it: mapped, squashed, its groups and flags set
	Mapped          vfs.Principal // who the caller proved or claimed to be, before any squash; zero where it maps to none
	Share           vfs.ShareID   // the share it was admitted to
	Proved          vfs.Identity  // what the adapter proved
	Client          vfs.ClientID  // zero where the call names no client
	Protocol        vfs.Protocol  // zero where the call names no client
}

// Acting is c as the chokepoint takes it: its principal, groups and flags,
// held to c's Access, with a read narrowed by the share or a client rule
// kept as the cause the chokepoint answers ErrReadOnly for. A read grant is
// no such cause: Access alone holds the caller to it, refused ErrAccess.
//
// decision: a caller holding a deny-only principal is refused here, at
// admission, because entity.Identity has no deny-only field: dropping them
// would let a file's deny entry naming one miss, which fails open. Withdraw
// the refusal once the chokepoint matches deny-only principals against deny
// entries (RFC 19 §2.3).
//
// decision: a caller whose Access is none is refused here, and so is not
// audited, because the chokepoint cannot be handed a none: it reads a zero
// Access as "read the grants", and a call that never reaches it, a lock, a
// watch or a share listing, would act on a share no grant admits it to.
// Overturned once the chokepoint takes a grant of none in Access, or
// admission's refusals are audited.
//
// decision: with releases, a call that only releases state, a caller whose
// Access is none is admitted with Access zero, as admission exempts such a
// call from the client rules' refusal, so a caller whose grant was removed
// still gives up the locks and opens it holds. Resolution refuses a
// squashed proven caller granted none itself, so the grants the chokepoint
// would read are the ones resolution read, which name none, and open state
// still refuses a caller that does not own the state.
func Acting(c *identity.Caller, releases bool) (entity.Identity, error) {
	if len(c.DenyOnly) > 0 {
		return entity.Identity{}, fmt.Errorf("%w: the caller holds a deny-only principal", vfs.ErrAccess)
	}
	if c.Access == entity.AccessNone && !releases {
		return entity.Identity{}, fmt.Errorf("%w: the share grants the caller none", vfs.ErrAccess)
	}
	ro := entity.NotNarrowed
	switch c.ReadOnly {
	case identity.ReadOnlyShare:
		ro = entity.NarrowedByShare
	case identity.ReadOnlyClient:
		ro = entity.NarrowedByClient
	}
	return entity.Identity{
		Principal: c.Principal, Group: c.Group, Groups: c.Groups,
		Privileged: c.Privileged, Authenticated: c.Authenticated, Admin: c.Admin,
		ReadOnly: ro, Access: c.Access,
	}, nil
}

// Policy is what a share's audit policy selects.
type Policy uint8

const (
	AuditOff     Policy = iota // nothing
	AuditDenials               // every refusal
	AuditChanges               // every refusal, and every decision on a call that may change state
	AuditAll                   // every decision
)

// Authz decides through the store and emits what the audit policy selects.
type Authz struct {
	Files  kvstore.Files
	Policy func(vfs.ShareID) Policy               // nil: every share's is AuditOff
	Emit   func(context.Context, vfs.AccessEvent) // never waits
}

// Decide is the chokepoint's decision on c doing want to the file h names for
// op; via is the directory h was resolved through, or nil, which can grant
// delete; name is the entry acted on, for a namespace operation. It answers
// the store's error, which the filesystem maps. A refusal or a grant is
// audited as the share's policy selects; an error that decides nothing, such
// as a stale handle, is not.
//
// ponytail: an access event is selected by the share's policy only. The
// store's Authorize does not report an audit or alarm entry of the file's
// ACL selecting the decision, so SelectedByEntry is never emitted; emit it
// once the chokepoint reports one.
func (a Authz) Decide(ctx context.Context, c Caller, op vfs.Op, h, via vfs.Handle, name []byte, want vfs.Access) error {
	err := a.Files.Authorize(ctx, c.Identity, h, via, want)
	allowed := err == nil
	if !allowed && !Refused(err) {
		return err
	}
	if a.selects(c.Share, op, allowed) {
		_, file, _ := h.Parse()
		a.Emit(ctx, vfs.AccessEvent{
			Time: time.Now(), Share: c.Share, Client: c.Client, Addr: c.Proved.Client, Identity: c.Proved,
			Protocol: c.Protocol, Op: op, File: file, Name: name, Want: want, Allowed: allowed,
			SelectedBy: vfs.SelectedByPolicy,
		})
	}
	return err
}

// Rights is the part of want the chokepoint would grant c on the file h
// names, delete through via included.
//
// decision: Rights emits no access event. It answers a question, such as a
// tree connect's maximal access, and refuses nothing; the decision an open
// or a call makes on what it answers goes through Decide, which is
// audited. Audit it too if an audit policy is asked to record what a caller
// was told it may do.
func (a Authz) Rights(ctx context.Context, c Caller, h, via vfs.Handle, want vfs.Access) (vfs.Access, error) {
	return a.Files.Granted(ctx, c.Identity, h, via, want)
}

func (a Authz) selects(share vfs.ShareID, op vfs.Op, allowed bool) bool {
	if a.Policy == nil {
		return false
	}
	switch a.Policy(share) {
	case AuditAll:
		return true
	case AuditChanges:
		return !allowed || vfs.Mutates(op)
	case AuditDenials:
		return !allowed
	}
	return false
}

// Refused reports whether err is the chokepoint refusing the access asked:
// the grant, a read-only narrowing, the mode or the ACL, or the immutable
// and append-only flags.
func Refused(err error) bool {
	var e *merrs.StoreError
	return errors.As(err, &e) &&
		(e.Code == merrs.ErrAccessDenied || e.Code == merrs.ErrPermissionDenied || e.Code == merrs.ErrReadOnly)
}
