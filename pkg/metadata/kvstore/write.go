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
		if err := fn(controlTx{tx, new(uint64)}); err != nil {
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

// updateGen runs fn as update does and returns the generation fn's write
// returned in the attempt that committed.
func (c ControlPlane) updateGen(ctx context.Context, fn func(entity.ControlPlaneTx) (uint64, error)) (uint64, error) {
	var gen uint64
	err := c.update(ctx, func(tx entity.ControlPlaneTx) (err error) {
		gen, err = fn(tx)
		return err
	})
	return gen, err
}

// replay refuses a request whose key its account already used: with the
// answer it was given, or as a mismatch when the digest differs.
//
// decision: a record past its Expires still replays until the expiry sweep
// deletes it, so a key is held for 24 hours plus the sweep's lag. Honouring
// Expires here would let a new request write a record under the same key,
// which the sweep would then delete on reaching the old record's XP row.
//
// decision: the key is read tracked, as the store's contract asks, but no
// test can observe that: the record's Set already conflicts with any
// concurrent write of the key, since the kv contract conflicts blind writes,
// so the tracked read is redundant today. It decides only on a backend that
// stops conflicting blind writes.
func replay(r kv.Reader, i entity.IdempotencyRecord) error {
	b, ok, err := get(r, idempotencyKey(i.Account, i.Key))
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

// Replay is what Write would answer i before running its function, read
// untracked outside any write: *entity.IdempotentReplay with the recorded
// answer when i's account recorded the same digest under its key,
// entity.ErrIdempotencyMismatch when it recorded another, and nil when the key
// holds no record. A caller checks it before a step it must not repeat; a
// request that misses here may still meet the record inside Write.
func (c ControlPlane) Replay(ctx context.Context, i entity.IdempotencyRecord) error {
	return c.s.kv.View(ctx, func(r kv.Reader) error { return replay(r, i) })
}

func idempotencyKey(account entity.Principal, key string) []byte {
	return keys.String(keys.ID([]byte{keys.KindIK}, account.ID[:]), []byte(key))
}

// expiryKey is a row of the expiry index: at, then the tagged record.
func expiryKey(at time.Time, tag byte, tail []byte) []byte {
	return keys.ID(append(keys.Uint64([]byte{keys.KindXP}, uint64(at.UnixNano())), tag), tail)
}

// idempotencyExpiryKey is the record's row in the expiry index: its expiry,
// then its tag, account and key.
func idempotencyExpiryKey(i entity.IdempotencyRecord) []byte {
	return keys.String(expiryKey(i.Expires, keys.XPIdempotency, i.Account.ID[:]), []byte(i.Key))
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
	i.Answer, i.Expires = cloneOrNil(r.Bytes()), readTime(r)
	return i, r.Done()
}

// controlTx is entity.ControlPlaneTx over one transaction; the view's
// methods run their writes through it. The transaction is a named field, not
// embedded, so a caller holding a ControlPlaneTx cannot assert it back to a
// kv.Txn and write rows no method checks. pgen is the principals generation
// the transaction raised, zero until it raises one.
type controlTx struct {
	tx   kv.Txn
	pgen *uint64
}

var _ entity.ControlPlaneTx = controlTx{}

// principals raises the principals generation for this transaction, once.
func (c controlTx) principals() (uint64, error) { return raisePrincipals(c.tx, c.pgen) }

func (c controlTx) Setting(scope entity.SettingScope, key string) (entity.Setting, bool, error) {
	b, ok, err := get(c.tx, settingKey(scope, key))
	if err != nil || !ok {
		return entity.Setting{}, false, err
	}
	s, err := decodeSetting(scope, key, b)
	return s, err == nil, err
}

func (c controlTx) User(p entity.Principal) (entity.User, bool, error) {
	u, ok, err := userIn(c.tx, p)
	if err != nil || !ok {
		return entity.User{}, false, err
	}
	u.Primary, err = primaryIn(c.tx, u)
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

func (c controlTx) SetMachinePassword(ref entity.SecretRef) error {
	return c.setInstallation(func(in *entity.Installation) { in.MachinePassword = ref })
}

func (c controlTx) Now() time.Time { return c.tx.Now() }
