package kvstore

import (
	"bytes"
	"context"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// SweepBatch is K, the most records one Sweep call deletes.
//
// ponytail: K counts records, not keys, so a batch's size follows its
// sessions' refresh hashes, about 100 keys for a 12 h session refreshed every
// 15 min, some 6,400 keys at K. Bound the batch by the store's key budget
// instead once sessions live long enough to bring that near it.
const SweepBatch = 64

// Swept is what one Sweep call deleted, by kind, and whether another
// record was already due when it stopped.
type Swept struct {
	Idempotency, Operations, Sessions, Tokens, SignIns, Audit int
	More                                                      bool
}

// Sweep deletes, in one bounded batch, every record whose XP entry is at or
// before the store time, with its index rows, and every audit record older
// than auditRetention, with its AUX rows. Expired records go first, then
// audit records, at most SweepBatch in all; More reports one still due.
//
// Each record is read in the transaction that deletes it, and deleted only
// when its expiry is still the one the row names, so a refresh that moved it
// either commits first, leaving the sweep a moved row it no longer reaches,
// or conflicts with the sweep's read and retries after it.
//
// ponytail: one call sweeps every kind from the oldest due row, so a kind is
// swept once per interval of the caller's, and a backlog of one kind holds
// the others back. Give each kind its own schedule once a profile shows the
// sweep.
func (n Nodes) Sweep(ctx context.Context, auditRetention time.Duration) (sw Swept, err error) {
	// decision: a retention of zero or less is refused rather than read as
	// "keep nothing", since it would delete every audit record at once; a
	// setting that means to keep none needs its own value.
	if auditRetention <= 0 {
		return sw, merrs.NewInvalidArgumentError("audit retention is a positive duration")
	}
	err = n.s.kv.Update(ctx, func(tx kv.Txn) error {
		sw = Swept{}
		now := tx.Now()
		rows, more, err := due(tx, []byte{keys.KindXP}, SweepBatch, func(t time.Time) bool { return !t.After(now) })
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := sweepExpired(tx, row, &sw); err != nil {
				return err
			}
		}
		if !more {
			cut := now.Add(-auditRetention)
			if rows, more, err = due(tx, []byte{keys.KindAU}, SweepBatch-len(rows), cut.After); err != nil {
				return err
			}
			for _, row := range rows {
				if err := sweepAudit(tx, row); err != nil {
					return err
				}
				sw.Audit++
			}
		}
		sw.More = more
		return nil
	})
	return sw, err
}

// due returns up to k keys under prefix, in order, whose time, the eight
// bytes after the prefix, is due; more reports one more due past them.
func due(tx kv.Txn, prefix []byte, k int, isDue func(time.Time) bool) (rows [][]byte, more bool, err error) {
	for kvp, err := range tx.Scan(prefix, nil) {
		if err != nil {
			return nil, false, err
		}
		ns, _, err := keys.ReadUint64(kvp.Key[len(prefix):])
		if err != nil {
			return nil, false, errMalformed
		}
		if !isDue(time.Unix(0, int64(ns))) {
			break
		}
		if len(rows) == k {
			return rows, true, nil
		}
		rows = append(rows, bytes.Clone(kvp.Key))
	}
	return rows, false, nil
}

// sweepExpired deletes the expiry row row and, when its record still
// expires there, the record with all its rows. A row whose record is gone,
// or expires elsewhere, is deleted alone.
func sweepExpired(tx kv.Txn, row []byte, sw *Swept) error {
	if len(row) < 10 {
		return errMalformed
	}
	tag, tail := row[9], row[10:]
	if tag != keys.XPIdempotency && tag != keys.XPSource && tag != keys.XPPair && len(tail) != keys.IDLen {
		return errMalformed
	}
	current, n, del, err := expiringIn(tx, tag, tail, sw)
	if err != nil {
		return err
	}
	if n != nil && bytes.Equal(current, row) {
		*n++
		if err := del(); err != nil {
			return err
		}
	}
	return tx.Delete(row)
}

// expiringIn reads the record an expiry row's tag and tail name: when it is
// there, the expiry row its writer keeps for it, the count in sw it adds to and how to delete it with
// its other rows; a nil count when it is not.
func expiringIn(tx kv.Txn, tag byte, tail []byte, sw *Swept) ([]byte, *int, func() error, error) {
	c := controlTx{tx, new(uint64)}
	switch tag {
	case keys.XPIdempotency:
		key := append([]byte{keys.KindIK}, tail...)
		b, found, err := get(tx, key)
		if err != nil || !found {
			return nil, nil, nil, err
		}
		i, err := decodeIdempotency(entity.Principal{}, "", b)
		return expiryKey(i.Expires, tag, tail), &sw.Idempotency, func() error { return tx.Delete(key) }, err
	case keys.XPOperation:
		op, found, err := operationIn(tx, entity.OperationID(tail))
		if err != nil || !found || !op.State.Ended() {
			return nil, nil, nil, err
		}
		return operationExpiryKey(op), &sw.Operations, func() error { return tx.Delete(operationKey(op.ID)) }, nil
	case keys.XPSession:
		s, found, err := sessionIn(tx, [16]byte(tail))
		if err != nil || !found {
			return nil, nil, nil, err
		}
		return sessionExpiryKey(s), &sw.Sessions, func() error { return c.DeleteSession(s.ID) }, nil
	case keys.XPToken:
		t, found, err := tokenIn(tx, [16]byte(tail))
		if err != nil || !found {
			return nil, nil, nil, err
		}
		return tokenExpiryKey(t), &sw.Tokens, func() error { return c.DeleteAPIToken(t.ID) }, nil
	case keys.XPSource, keys.XPPair:
		sf := keys.SFSource
		if tag == keys.XPPair {
			sf = keys.SFPair
		}
		f, found, err := signInIn(tx, sf, tail)
		if err != nil || !found {
			return nil, nil, nil, err
		}
		return signInExpiryKey(sf, tail, f), &sw.SignIns, func() error { return tx.Delete(signInKey(sf, tail)) }, nil
	}
	return nil, nil, nil, errMalformed
}

// sweepAudit deletes the audit record at key with its three index rows.
func sweepAudit(tx kv.Txn, key []byte) error {
	b, err := tx.Get(key)
	if err != nil {
		return err
	}
	pos := key[1:]
	a, err := decodeAudit(pos, b)
	if err != nil {
		return err
	}
	for _, prefix := range [][]byte{auditActorPrefix(a.Actor), auditTargetPrefix(a.Target), auditNodePrefix(a.Node)} {
		if err := tx.Delete(append(prefix, pos...)); err != nil {
			return err
		}
	}
	return tx.Delete(key)
}
