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
// deletes the record and, for a user, every version of each secret it names,
// refusing one sealed for another holder or as another kind as ownSecrets
// says.
// Files keep naming p, as an unknown principal. A user's deletion of a
// machine's ID deletes the machine; a record of any other kind than p's
// refuses.
//
// Each batch finds what is left by the rows themselves, so a restart resumes
// from the mark with no cursor, and a batch lost to a crash is run again. An
// absent record answers done: a deletion whose end was not recorded ends
// when resumed.
func (n Nodes) ContinueDelete(ctx context.Context, p entity.Principal) (done bool, err error) {
	if p.Kind != entity.PrincipalUser && p.Kind != entity.PrincipalMachine && p.Kind != entity.PrincipalGroup {
		return false, merrs.NewInvalidArgumentError("only a user, a machine or a group is deleted in batches")
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
	var u entity.User
	var ids entity.ProtocolIDs
	if p.Kind != entity.PrincipalGroup {
		if u, err = decodeUser(p.ID, b); err != nil {
			return false, err
		}
		if u.Principal.Kind != p.Kind {
			// A user's deletion names its record by ID alone, so a machine
			// DeleteUser marked is deleted as one; any other kind refuses.
			if p.Kind != entity.PrincipalUser || u.Principal.Kind != entity.PrincipalMachine {
				return false, merrs.NewInvalidArgumentError(p.String() + " names a " + u.Principal.Kind.String() + " record")
			}
			p = u.Principal
		}
		marked, ids = u.Deleted, u.IDs
	} else {
		g, err := decodeGroup(p.ID, b)
		if err != nil {
			return false, err
		}
		marked, ids = g.Deleted, g.IDs
	}
	if !marked {
		return false, merrs.NewInvalidArgumentError(p.String() + " is not being deleted")
	}
	// One key is kept for the principals generation a membership drop raises.
	d := &batch{tx: tx, left: k - 1}
	ctl := controlTx{tx: tx}
	inGroups := func([16]byte) error { _, err := raisePrincipals(tx, &d.pgen); return err }
	steps := []func() error{
		func() error {
			return d.dropPairs(grantsOfPrefix(p.ID), 3, func(share [16]byte) []byte { return grantKey(entity.ShareID(share), p.ID) },
				func(share [16]byte) error { return moveGrantVersion(tx, entity.ShareID(share)) })
		},
		func() error {
			return d.dropPairs(memberOfPrefix(p.ID), 2, func(g [16]byte) []byte { return memberKey(entity.PrincipalID(g), p.ID) }, inGroups)
		},
	}
	if p.Kind == entity.PrincipalGroup {
		steps = append(steps, func() error {
			return d.dropPairs(membersPrefix(p.ID), 2, func(m [16]byte) []byte { return memberRevKey(entity.PrincipalID(m), p.ID) }, inGroups)
		})
	}
	if p.Kind != entity.PrincipalMachine { // a quota never limits a machine
		steps = append(steps, func() error { return d.dropQuotas(p) })
	}
	if p.Kind != entity.PrincipalGroup {
		steps = append(steps,
			func() error { return d.dropPairs(accountTokensPrefix(p), 4, nil, ctl.DeleteAPIToken) },
			func() error { return d.dropSessions(p) },
			func() error { return d.dropSignInCount(p) })
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return false, err
		}
	}
	if d.took {
		return false, nil
	}
	if err := ctl.deleteRecord(p, ids, u); err != nil { // a group's zero u names no secret
		return false, err
	}
	return true, nil
}

// batch is what one batch of a deletion may still delete. A take it refuses
// leaves that item to the next batch, and the steps after it may still take
// smaller ones: the record goes only in a batch that took nothing.
type batch struct {
	tx   kv.Txn
	left int  // keys it may still delete
	took bool // whether it has taken an item
	pgen uint64
}

// take reports whether an item deleting cost keys fits, charging it. The
// batch's first item always fits.
func (b *batch) take(cost int) bool {
	if b.took && cost > b.left {
		return false
	}
	b.left, b.took = b.left-cost, true
	return true
}

// rows returns the keys of the rows under prefix the batch may reach.
//
// decision: the range is not guarded. Every write adding a row under these
// prefixes (a grant, a membership, a token, a session) first reads its
// principal tracked and refuses one marked, so it either commits before the
// mark, and every batch sees its row, or conflicts with the mark and is
// refused on retry. Guard the range if a writer ever adds such a row without
// that read.
func (b *batch) rows(prefix []byte) ([][]byte, error) {
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

// dropPairs deletes the rows under prefix, each ending in an ID and costing
// cost keys: the row, the row twin names for its ID unless twin is nil, and
// what then deletes or raises for it.
func (b *batch) dropPairs(prefix []byte, cost int, twin func(id [16]byte) []byte, then func(id [16]byte) error) error {
	rows, err := b.rows(prefix)
	if err != nil {
		return err
	}
	for _, k := range rows {
		id, _, err := keys.ReadID(k[len(prefix):], keys.IDLen)
		if err != nil {
			return err
		}
		if !b.take(cost) {
			return nil
		}
		del := [][]byte{k}
		if twin != nil {
			del = append(del, twin([16]byte(id)))
		}
		if err := deleteRows(b.tx, del); err != nil {
			return err
		}
		if err := then([16]byte(id)); err != nil {
			return err
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
func (b *batch) dropQuotas(p entity.Principal) error {
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

// dropSessions deletes p's sessions, each costing its record, its index and
// expiry rows and two rows per refresh hash it has had. A session that is
// its batch's first item and outgrows the batch loses refresh hashes
// instead, as many as fit, and keeps its record for a later batch, so no
// session is too large to delete however often it was refreshed.
func (b *batch) dropSessions(p entity.Principal) error {
	prefix := accountSessionsPrefix(p)
	rows, err := b.rows(prefix)
	if err != nil {
		return err
	}
	for _, k := range rows {
		id := [16]byte(k[len(prefix):])
		hashes, err := sessionHashRows(b.tx, id, trimRows(b.left))
		if err != nil {
			return err
		}
		cost := 3 + len(hashes)
		if hashes != nil && !b.took && cost > b.left {
			n := min(len(hashes), 2*max(1, b.left/2))
			b.left, b.took = b.left-n, true
			return deleteRows(b.tx, hashes[:n])
		}
		if !b.take(cost) {
			return nil
		}
		if err := (controlTx{tx: b.tx}).DeleteSession(id); err != nil {
			return err
		}
		// The index row goes even when it names no session, so the batches end.
		if err := b.tx.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// dropSignInCount deletes p's account count and its expiry row. Its counts
// with a source expire on their own.
func (b *batch) dropSignInCount(p entity.Principal) error {
	k := entity.SignInKey{Account: p}
	ctl := controlTx{tx: b.tx}
	if _, ok, err := ctl.SignInFailures(k); err != nil || !ok || !b.take(2) {
		return err
	}
	return ctl.DeleteSignInFailures(k)
}

// deleteRecord ends p's deletion: its record goes, with the dir spelling
// row its mark kept and every version of each secret u, p's user record,
// names, its password hash and its NT hash, once ownSecrets has accepted
// them.
//
// decision: the secret is deleted by the reference the owner's own record
// holds, its owner checked by ownSecrets in the same transaction, so the
// store deletes its versions itself rather than through the secrets
// service. A reference read from anywhere but its owner's record goes
// through that service. Every secret change raises the settings generation,
// so every node stops resolving the secret.
//
// ponytail: every version goes in this one transaction, which holds as long
// as a secret keeps a current version and the few it replaced. Delete the
// versions in batches if retention ever keeps many.
func (c controlTx) deleteRecord(p entity.Principal, ids entity.ProtocolIDs, u entity.User) error {
	tx := c.tx
	if err := c.ownSecrets(u, false); err != nil {
		return err
	}
	if _, err := raise(tx, pgenKey); err != nil {
		return err
	}
	for _, id := range ids {
		if id.Scheme == entity.SchemeDir {
			if err := tx.Delete(protocolKey(id)); err != nil {
				return err
			}
		}
	}
	var versions [][]byte
	named := false
	for _, secret := range []entity.SecretRef{u.Password, u.NTHash} {
		if secret == (entity.SecretRef{}) {
			continue
		}
		named = true
		for row, err := range tx.Scan(secretPrefix(secret), nil) {
			if err != nil {
				return err
			}
			versions = append(versions, slices.Clone(row.Key))
		}
	}
	if named {
		if err := deleteRows(tx, versions); err != nil {
			return err
		}
		if _, err := raise(tx, cfgGenKey); err != nil {
			return err
		}
	}
	return tx.Delete(principalKey(p))
}
