package kvstore

import (
	"context"
	"errors"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// idempotencyLife is how long a request's answer is kept for its retries.
const idempotencyLife = 24 * time.Hour

// Write runs fn over the control plane's writes and tracked reads in one
// transaction, then writes meta's audit record and, when meta names a key,
// its idempotency record, so the change and both records commit together or
// none of them does. Before fn, the key's record is read tracked: one with
// the same digest answers *entity.IdempotentReplay with the recorded answer,
// one with another digest entity.ErrIdempotencyMismatch, and fn does not
// run. Two requests that both miss the record conflict on its key, and the
// retried one then finds it.
//
// A conflict reruns the whole transaction, the key's read included, so fn
// keeps no state between attempts; each attempt starts from meta's records
// as they were passed in. An error from fn, entity.ErrGenerationMismatch
// among them, commits nothing and is not retried.
func (c ControlPlane) Write(ctx context.Context, meta entity.WriteMeta, fn func(entity.ControlPlaneTx) error) error {
	audit, idem := reset(meta.Audit), reset(meta.Idempotency)
	return c.s.kv.Update(ctx, func(tx kv.Txn) error {
		audit()
		idem()
		if i := meta.Idempotency; i != nil {
			if err := replay(tx, *i); err != nil {
				return err
			}
		}
		if err := fn(controlTx{tx}); err != nil {
			return err
		}
		if meta.Audit != nil {
			if err := putAudit(tx, meta.Audit); err != nil {
				return err
			}
		}
		if i := meta.Idempotency; i != nil {
			i.Expires = tx.Now().Add(idempotencyLife)
			if err := tx.Set(idempotencyKey(i.Account, i.Key), encodeIdempotency(*i)); err != nil {
				return err
			}
			return tx.Set(idempotencyExpiryKey(*i), nil)
		}
		return nil
	})
}

// reset returns a function that puts *p back as it is now; a nil p has
// nothing to put back.
func reset[T any](p *T) func() {
	if p == nil {
		return func() {}
	}
	v := *p
	return func() { *p = v }
}

// update runs fn as one Write recording nothing: the view's own writes.
func (c ControlPlane) update(ctx context.Context, fn func(entity.ControlPlaneTx) error) error {
	return c.Write(ctx, entity.WriteMeta{}, fn)
}

// replay refuses a request whose key its account already used: with the
// answer it was given, or as a mismatch when the digest differs.
func replay(tx kv.Txn, i entity.IdempotencyRecord) error {
	b, ok, err := get(tx, idempotencyKey(i.Account, i.Key))
	if err != nil || !ok {
		return err
	}
	rec, err := decodeIdempotency(i.Account, i.Key, b)
	if err != nil {
		return err
	}
	if rec.Digest != i.Digest {
		return entity.ErrIdempotencyMismatch
	}
	return &entity.IdempotentReplay{Answer: rec.Answer}
}

func idempotencyKey(account entity.Principal, key string) []byte {
	return keys.String(keys.ID([]byte{keys.KindIK}, account.ID[:]), []byte(key))
}

// idempotencyExpiryKey is the record's row in the expiry index: its expiry,
// then its tag, account and key.
func idempotencyExpiryKey(i entity.IdempotencyRecord) []byte {
	k := keys.Uint64([]byte{keys.KindXP}, uint64(i.Expires.UnixNano()))
	return keys.String(keys.ID(append(k, keys.XPIdempotency), i.Account.ID[:]), []byte(i.Key))
}

func encodeIdempotency(i entity.IdempotencyRecord) []byte {
	return putTime(codec.NewWriter(v1).Fixed(i.Digest[:]).Bytes(i.Answer), i.Expires).Encode()
}

func decodeIdempotency(account entity.Principal, key string, b []byte) (entity.IdempotencyRecord, error) {
	r, err := open(b)
	if err != nil {
		return entity.IdempotencyRecord{}, err
	}
	i := entity.IdempotencyRecord{Account: account, Key: key}
	copy(i.Digest[:], r.Fixed(32))
	i.Answer, i.Expires = copied(r.Bytes()), readTime(r)
	return i, r.Done()
}

// controlTx is entity.ControlPlaneTx over one transaction: each write is the
// same function the view's method runs.
type controlTx struct{ tx kv.Txn }

var _ entity.ControlPlaneTx = controlTx{}

func (c controlTx) Setting(scope entity.SettingScope, key string) (entity.Setting, bool, error) {
	b, ok, err := get(c.tx, settingKey(scope, key))
	if err != nil || !ok {
		return entity.Setting{}, false, err
	}
	s, err := decodeSetting(scope, key, b)
	return s, err == nil, err
}

func (c controlTx) User(p entity.Principal) (entity.User, bool, error) {
	if p.Kind != entity.PrincipalUser {
		return entity.User{}, false, nil
	}
	b, ok, err := get(c.tx, userKey(p.ID))
	if err != nil || !ok {
		return entity.User{}, false, err
	}
	u, err := decodeUser(p.ID, b)
	return u, err == nil, err
}

func (c controlTx) Share(id entity.ShareID) (entity.Share, bool, error) {
	s, err := shareIn(c.tx, id)
	if errors.Is(err, kv.ErrNotFound) {
		return entity.Share{}, false, nil
	}
	return s, err == nil, err
}

func (c controlTx) EnabledAdmins() (int, error) {
	prefix := []byte{keys.KindUA}
	if err := c.tx.GuardRange(prefix); err != nil {
		return 0, err
	}
	n := 0
	for _, err := range c.tx.Scan(prefix, nil) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

func (c controlTx) Installation() (entity.Installation, error) {
	b, err := c.tx.Get(installationKey)
	if err != nil {
		return entity.Installation{}, err
	}
	return decodeInstallation(b)
}

// setInstallation rewrites the installation record with change applied to
// it, read tracked in the same transaction.
func (c controlTx) setInstallation(change func(*entity.Installation)) error {
	in, err := c.Installation()
	if err != nil {
		return err
	}
	change(&in)
	return c.tx.Set(installationKey, encodeInstallation(in))
}

func (c controlTx) SetNTLMOff(gen uint64) error {
	return c.setInstallation(func(in *entity.Installation) {
		in.NTLMOff.Off, in.NTLMOff.Deleted = gen, entity.OperationID{}
	})
}

func (c controlTx) SetNTHashDeleted(op entity.OperationID) error {
	return c.setInstallation(func(in *entity.Installation) { in.NTLMOff.Deleted = op })
}

func (c controlTx) SetMachineJoin(j entity.MachineJoin) error {
	return c.setInstallation(func(in *entity.Installation) { in.Join = j })
}

func (c controlTx) Now() time.Time { return c.tx.Now() }

func (c controlTx) DeleteUser(p entity.Principal, expect uint64) error {
	return deleteUser(c.tx, p, expect)
}

func (c controlTx) DeleteGroup(p entity.Principal, expect uint64) error {
	return deleteGroup(c.tx, p, expect)
}

func (c controlTx) DeleteShare(share entity.ShareID, snapshots entity.SnapshotDisposition, expect uint64) error {
	return deleteShare(c.tx, share, snapshots, expect)
}

func (c controlTx) PutUser(u entity.User, expect uint64, a entity.Alloc) error {
	return putUser(c.tx, u, expect, a)
}

func (c controlTx) PutGroup(g entity.Group, expect uint64, a entity.Alloc) error {
	return putGroup(c.tx, g, expect, a)
}

func (c controlTx) PutShare(s entity.Share, expect uint64) error { return putShare(c.tx, s, expect) }

func (c controlTx) SetShareState(share entity.ShareID, from, to entity.ShareState) error {
	return setShareState(c.tx, share, from, to)
}

func (c controlTx) SetGrant(g entity.ShareGrant, expect uint64) error {
	return setGrant(c.tx, g, expect)
}

func (c controlTx) DeleteGrant(share entity.ShareID, p entity.Principal, expect uint64) error {
	return deleteGrant(c.tx, share, p, expect)
}

func (c controlTx) PutExportPolicy(p entity.ExportPolicy, expect uint64) error {
	return putExportPolicy(c.tx, p, expect)
}

func (c controlTx) SetQuota(share entity.ShareID, q entity.Quota, expect uint64) error {
	return setQuota(c.tx, share, q, expect)
}

func (c controlTx) PutNetgroup(n entity.Netgroup, expect uint64) error {
	return putNetgroup(c.tx, n, expect)
}

func (c controlTx) DeleteNetgroup(name string, expect uint64) error {
	return deleteNetgroup(c.tx, name, expect)
}

func (c controlTx) PutSetting(s entity.Setting, expect uint64) error {
	return putSetting(c.tx, s, expect)
}

func (c controlTx) AddMember(m entity.Membership) error    { return addMember(c.tx, m) }
func (c controlTx) RemoveMember(m entity.Membership) error { return removeMember(c.tx, m) }
