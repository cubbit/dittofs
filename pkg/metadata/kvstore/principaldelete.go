package kvstore

import (
	"context"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// ContinueDelete runs one bounded batch of the deletion of p, a user or a
// group DeleteUser or DeleteGroup marked, and reports whether the deletion
// is done. A batch drops, in order, p's grants, moving the grant-list
// version of each share whose grant it drops; its memberships, both ways for
// a group; its quota records; and, for a user, its API tokens, its sessions
// and its account's sign-in count. The batch that finds none of them left
// deletes the record and, for a user, every version of the secret it names.
// Files keep naming p, as an unknown principal.
//
// Each batch finds what is left by the rows themselves, so a restart resumes
// from the mark with no cursor, and a batch lost to a crash is run again. An
// absent record answers done: a deletion whose end was not recorded ends
// when resumed.
func (n Nodes) ContinueDelete(ctx context.Context, p entity.Principal) (done bool, err error) {
	if p.Kind != entity.PrincipalUser && p.Kind != entity.PrincipalGroup {
		return false, merrs.NewInvalidArgumentError("only a user or a group is deleted in batches")
	}
	err = n.s.kv.Update(ctx, func(tx kv.Txn) (err error) {
		done, err = deleteBatch(tx, p, n.s.k)
		return err
	})
	return done, err
}

// deleteBatch is one batch of p's deletion, deleting at most k keys but for
// its first item, so every batch makes progress.
func deleteBatch(tx kv.Txn, p entity.Principal, k int) (bool, error) {
	b, ok, err := get(tx, principalKey(p))
	if err != nil || !ok {
		return !ok, err
	}
	var marked bool
	var secret entity.SecretRef
	if p.Kind == entity.PrincipalUser {
		u, err := decodeUser(p.ID, b)
		if err != nil {
			return false, err
		}
		marked, secret = u.Deleted, u.Secret
	} else {
		g, err := decodeGroup(p.ID, b)
		if err != nil {
			return false, err
		}
		marked = g.Deleted
	}
	if !marked {
		return false, merrs.NewInvalidArgumentError(p.String() + " is not being deleted")
	}
	// One key is kept for the principals generation a membership drop raises.
	d := &batch{tx: tx, left: k - 1}
	steps := []func(*batch, entity.Principal) error{dropGrants, dropMemberships, dropQuotas}
	if p.Kind == entity.PrincipalUser {
		steps = append(steps, dropTokens, dropSessions, dropSignInCount)
	}
	for _, step := range steps {
		if err := step(d, p); err != nil || d.full {
			return false, err
		}
	}
	if d.took {
		return false, nil
	}
	return true, deleteRecord(tx, p, secret)
}

// batch is what one batch of a deletion may still delete.
type batch struct {
	tx   kv.Txn
	left int  // keys it may still delete
	took bool // whether it has taken an item
	full bool // whether it left an item for the next batch
	pgen uint64
}

// take reports whether an item deleting cost keys fits, charging it. The
// batch's first item always fits.
//
// ponytail: an item is never split, so a session with more refresh hashes
// than one transaction holds stops its account's deletion with
// kv.ErrTooLarge. A session refreshed every 15 min has about 48; split a
// session's hashes across batches if lifetimes ever grow that far.
func (b *batch) take(cost int) bool {
	if b.took && cost > b.left {
		b.full = true
		return false
	}
	b.left, b.took = b.left-cost, true
	return true
}

// rows guards prefix, so a row added under it meanwhile conflicts with the
// batch, and returns the keys of the rows under it the batch may reach.
func (b *batch) rows(prefix []byte) ([][]byte, error) {
	if err := b.tx.GuardRange(prefix); err != nil {
		return nil, err
	}
	var out [][]byte
	for row, err := range b.tx.Scan(prefix, nil) {
		if err != nil {
			return nil, err
		}
		if len(out) >= max(1, b.left) {
			break
		}
		out = append(out, slices.Clone(row.Key))
	}
	return out, nil
}

// idAfter is the ID that ends key, a row under prefix.
func idAfter(prefix, key []byte) ([16]byte, error) {
	id, _, err := keys.ReadID(key[len(prefix):], keys.IDLen)
	return [16]byte(id), err
}

func dropGrants(b *batch, p entity.Principal) error {
	prefix := grantsOfPrefix(p.ID)
	rows, err := b.rows(prefix)
	if err != nil {
		return err
	}
	for _, k := range rows {
		share, err := idAfter(prefix, k)
		if err != nil {
			return err
		}
		if !b.take(3) {
			return nil
		}
		if err := deletePair(b.tx, k, grantKey(share, p.ID)); err != nil {
			return err
		}
		if err := moveGrantVersion(b.tx, share); err != nil {
			return err
		}
	}
	return nil
}

// twinRows is one direction of a two-way index: the rows under prefix, each
// ending in an ID, and the other direction's row for that ID.
type twinRows struct {
	prefix []byte
	twin   func(other [16]byte) []byte
}

// dropMemberships deletes the groups p is in and, for a group, its members,
// each row with its twin, raising the principals generation once if it
// deletes any.
func dropMemberships(b *batch, p entity.Principal) error {
	ways := []twinRows{{memberOfPrefix(p.ID), func(g [16]byte) []byte { return memberKey(g, p.ID) }}}
	if p.Kind == entity.PrincipalGroup {
		ways = append(ways, twinRows{membersPrefix(p.ID), func(m [16]byte) []byte { return memberRevKey(m, p.ID) }})
	}
	for _, w := range ways {
		rows, err := b.rows(w.prefix)
		if err != nil {
			return err
		}
		for _, k := range rows {
			other, err := idAfter(w.prefix, k)
			if err != nil {
				return err
			}
			if !b.take(2) {
				return nil
			}
			if err := deletePair(b.tx, k, w.twin(other)); err != nil {
				return err
			}
			if _, err := raisePrincipals(b.tx, &b.pgen); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropQuotas deletes p's quota records, each read tracked, so a quota set
// for p meanwhile conflicts.
//
// ponytail: each batch reads two keys in every share, O(shares), since no
// index names the shares holding a principal's quotas. Add a reverse index,
// as grants have, when installations hold enough shares for a principal
// deletion to show it.
func dropQuotas(b *batch, p entity.Principal) error {
	suffix, err := quotaSuffix(QuotaTarget{Principal: p})
	if err != nil {
		return err
	}
	var shares []entity.ShareID
	for row, err := range b.tx.Scan([]byte{keys.KindNX, keys.NXShare}, nil) {
		if err != nil {
			return err
		}
		id, err := decodeID(row.Value)
		if err != nil {
			return err
		}
		shares = append(shares, entity.ShareID(id))
	}
	for _, share := range shares {
		for _, sub := range []byte{keys.SQt, keys.SQx} {
			key := quotaKey(share, sub, suffix)
			if _, ok, err := get(b.tx, key); err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if !b.take(1) {
				return nil
			}
			if err := b.tx.Delete(key); err != nil {
				return err
			}
		}
	}
	return nil
}

func dropTokens(b *batch, p entity.Principal) error {
	prefix := accountTokensPrefix(p)
	rows, err := b.rows(prefix)
	if err != nil {
		return err
	}
	for _, k := range rows {
		if !b.take(4) {
			return nil
		}
		if err := (controlTx{tx: b.tx}).DeleteAPIToken([16]byte(k[len(prefix):])); err != nil {
			return err
		}
		// The index row goes even when it names no token, so the batches end.
		if err := b.tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// dropSessions deletes p's sessions, each costing its record, its index and
// expiry rows and two rows per refresh hash it has had.
func dropSessions(b *batch, p entity.Principal) error {
	prefix := accountSessionsPrefix(p)
	rows, err := b.rows(prefix)
	if err != nil {
		return err
	}
	for _, k := range rows {
		id := [16]byte(k[len(prefix):])
		cost := 3
		for _, err := range b.tx.Scan(sessionHashesPrefix(id), nil) {
			if err != nil {
				return err
			}
			cost += 2
		}
		if !b.take(cost) {
			return nil
		}
		if err := (controlTx{tx: b.tx}).DeleteSession(id); err != nil {
			return err
		}
		if err := b.tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// dropSignInCount deletes p's account count. Its counts with a source
// expire on their own.
func dropSignInCount(b *batch, p entity.Principal) error {
	k := entity.SignInKey{Account: p}
	ctl := controlTx{tx: b.tx}
	if _, ok, err := ctl.SignInFailures(k); err != nil || !ok || !b.take(1) {
		return err
	}
	return ctl.DeleteSignInFailures(k)
}

// deleteRecord ends p's deletion: its record goes, with every version of
// the secret a user's names.
//
// decision: the secret is deleted by the reference the owner's own record
// holds, so the owner check secrets make holds by construction, and the
// store deletes its versions itself rather than through the secrets service.
// A reference read from anywhere but its owner's record goes through that
// service. Every secret change raises the settings generation, so every node
// stops resolving the secret.
//
// ponytail: every version goes in this one transaction, which holds as long
// as a secret keeps a current version and the few it replaced. Delete the
// versions in batches if retention ever keeps many.
func deleteRecord(tx kv.Txn, p entity.Principal, secret entity.SecretRef) error {
	var pgen uint64
	if _, err := raisePrincipals(tx, &pgen); err != nil {
		return err
	}
	if secret != (entity.SecretRef{}) {
		prefix := keys.ID([]byte{keys.KindSEC}, secret[:])
		var versions [][]byte
		for row, err := range tx.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			versions = append(versions, slices.Clone(row.Key))
		}
		for _, k := range versions {
			if err := tx.Delete(k); err != nil {
				return err
			}
		}
		if _, err := raise(tx, cfgGenKey); err != nil {
			return err
		}
	}
	return tx.Delete(principalKey(p))
}

// deletePair deletes a row and its twin.
func deletePair(tx kv.Txn, row, twin []byte) error {
	if err := tx.Delete(row); err != nil {
		return err
	}
	return tx.Delete(twin)
}
