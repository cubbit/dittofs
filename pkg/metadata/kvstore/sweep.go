package kvstore

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Swept is what one Sweep call deleted, by kind, and whether another
// record was already due when it stopped.
type Swept struct {
	Idempotency, Operations, Sessions, Tokens, SignIns, Audit int
	More                                                      bool
}

// Sweep deletes the control-plane records whose lifetime has ended:
// idempotency records, ended operations, sessions, API tokens and sign-in
// counts, each written with a row in the expiry index (XP) keyed by when it
// expires, and audit records older than auditRetention. Nothing else deletes
// them, so a caller runs Sweep periodically.
//
// One call is one batch within the store's key budget. It deletes every
// record whose expiry row is at or before the store time, with its index
// rows, and every audit record older than auditRetention, with its index
// rows (AUX). Expired
// records go first, then audit records; More reports one still due. A
// session too large for one batch loses as many of its refresh hashes as
// the batch holds, and is deleted by a later call once the rest fit.
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
func (n Nodes) Sweep(ctx context.Context, auditRetention time.Duration) (Swept, error) {
	// decision: a retention of zero or less is refused rather than read as
	// "keep nothing", since it would delete every audit record at once; a
	// setting that means to keep none needs its own value.
	if auditRetention <= 0 {
		return Swept{}, merrs.NewInvalidArgumentError("audit retention is a positive duration")
	}
	var sw Swept
	err := n.s.kv.Update(ctx, func(tx kv.Txn) error {
		sw = Swept{}
		b := &sweepBatch{tx: tx, left: n.s.k}
		now := tx.Now()
		rows, more, err := due(tx, []byte{keys.KindXP}, b.left, func(t time.Time) bool { return xpDue(t, now) })
		if err != nil {
			return err
		}
		for _, row := range rows {
			// decision: a row, or the record it names, that does not decode
			// stops this call and every later one, audit retention included,
			// until it is repaired, rather than being skipped: skipping would
			// leave its record and rows forever with nothing to report them. An
			// audit record that does not decode stops every call the same way.
			// SweepRowUndecodable names both.
			if err := b.expired(row, &sw); err != nil || b.full {
				sw.More = true
				return err
			}
		}
		if !more {
			if rows, more, err = due(tx, []byte{keys.KindAU}, max(b.left, 0)/auditCost, func(t time.Time) bool { return auditDue(t, now, auditRetention) }); err != nil {
				return err
			}
			for _, row := range rows {
				if !b.take(auditCost) {
					sw.More = true
					return nil
				}
				if err := sweepAudit(tx, row); err != nil {
					return err
				}
				sw.Audit++
			}
		}
		sw.More = more
		return nil
	})
	if err != nil {
		return Swept{}, err
	}
	return sw, nil
}

// SweepRowUndecodable reports that err is a Sweep stopped by an expiry row,
// the record it names, or an audit record, that does not decode: every later
// Sweep stops on it the same way until it is repaired.
func SweepRowUndecodable(err error) bool { return Undecodable(err) }

// Undecodable reports that err is a record or key the store read and could
// not decode, which a call meeting it again stops on the same way.
func Undecodable(err error) bool {
	return errors.Is(err, errMalformed) || errors.Is(err, codec.ErrTruncated) ||
		errors.Is(err, codec.ErrTrailing) || errors.Is(err, codec.ErrVersion)
}

// xpDue reports whether an expiry row at t is due at store time now.
func xpDue(t, now time.Time) bool { return !t.After(now) }

// auditDue reports whether an audit record written at t is past retention
// ret at store time now.
func auditDue(t, now time.Time, ret time.Duration) bool { return t.Before(now.Add(-ret)) }

// The keys each kind's deletion takes, its XP row among them: a session
// takes sessionKeys plus two per refresh hash it has had.
const (
	tokenCost   = 4 // AT, ATH, ATA, XP
	countCost   = 2 // the record and its XP row: an idempotency record, an ended operation, a sign-in count
	staleCost   = 1 // the XP row alone
	auditCost   = 4 // AU and its three AUX rows
	sessionKeys = 3 // SS, SSA, XP
)

// sweepBatch is what one Sweep call may still delete.
type sweepBatch struct {
	tx   kv.Txn
	left int  // keys it may still delete
	took bool // whether it has taken a record
	full bool // whether it left a due record for the next call
}

// take reports whether a deletion of cost keys fits, charging it. The
// batch's first deletion always fits, so every call makes progress.
func (b *sweepBatch) take(cost int) bool {
	if b.took && cost > b.left {
		b.full = true
		return false
	}
	b.left, b.took = b.left-cost, true
	return true
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
		if len(rows) >= k {
			return rows, true, nil
		}
		rows = append(rows, bytes.Clone(kvp.Key))
	}
	return rows, false, nil
}

// expired deletes the expiry row row and, when its record still expires
// there, the record with all its rows. A row whose record is gone, or
// expires elsewhere, is deleted alone. A session larger than the whole batch
// loses refresh hashes instead, and keeps its record and row for a later
// call.
func (b *sweepBatch) expired(row []byte, sw *Swept) error {
	if len(row) < 10 {
		return errMalformed
	}
	tag, tail := row[9], row[10:]
	if tag != keys.XPIdempotency && tag != keys.XPSource && tag != keys.XPPair && len(tail) != keys.IDLen {
		return errMalformed
	}
	e, err := expiringIn(b.tx, tag, tail, b.left, sw)
	if err != nil {
		return err
	}
	if e.count == nil || !bytes.Equal(e.row, row) {
		if !b.take(staleCost) {
			return nil
		}
		return b.tx.Delete(row)
	}
	if e.hashes != nil && !b.took && e.cost > b.left {
		b.took, b.full = true, true
		return deleteRows(b.tx, e.hashes[:min(len(e.hashes), 2*max(1, b.left/2))])
	}
	if !b.take(e.cost) {
		return nil
	}
	*e.count++
	if err := e.del(); err != nil {
		return err
	}
	return b.tx.Delete(row)
}

// expiring is a record an expiry row names: the row its writer keeps for it,
// the count in Swept it adds to, the keys its deletion takes and how to
// delete it with its other rows. A session also lists its refresh hash rows,
// each followed by its SSH mirror.
type expiring struct {
	row    []byte
	count  *int
	cost   int
	del    func() error
	hashes [][]byte
}

// expiringIn reads the record an expiry row's tag and tail name; a nil count
// when it is not there, or is an operation still running. A session's hashes
// are read only up to one more than a batch with left keys can trim.
func expiringIn(tx kv.Txn, tag byte, tail []byte, left int, sw *Swept) (expiring, error) {
	c := controlTx{tx, new(uint64)}
	switch tag {
	case keys.XPIdempotency:
		key := append([]byte{keys.KindIK}, tail...)
		b, found, err := get(tx, key)
		if err != nil || !found {
			return expiring{}, err
		}
		i, err := decodeIdempotency(entity.Principal{}, "", b)
		return expiring{expiryKey(i.Expires, tag, tail), &sw.Idempotency, countCost, func() error { return tx.Delete(key) }, nil}, err
	case keys.XPOperation:
		op, found, err := operationIn(tx, entity.OperationID(tail))
		if err != nil || !found || !op.State.Ended() {
			return expiring{}, err
		}
		return expiring{operationExpiryKey(op), &sw.Operations, countCost, func() error { return tx.Delete(operationKey(op.ID)) }, nil}, nil
	case keys.XPSession:
		s, found, err := sessionIn(tx, [16]byte(tail))
		if err != nil || !found {
			return expiring{}, err
		}
		hashes, err := sessionHashRows(tx, s.ID, trimRows(left))
		if err != nil {
			return expiring{}, err
		}
		return expiring{sessionExpiryKey(s), &sw.Sessions, sessionKeys + len(hashes), func() error { return c.DeleteSession(s.ID) }, hashes}, nil
	case keys.XPToken:
		t, found, err := tokenIn(tx, [16]byte(tail))
		if err != nil || !found {
			return expiring{}, err
		}
		return expiring{tokenExpiryKey(t), &sw.Tokens, tokenCost, func() error { return c.DeleteAPIToken(t.ID) }, nil}, nil
	case keys.XPSource, keys.XPPair:
		sf := keys.SFSource
		if tag == keys.XPPair {
			sf = keys.SFPair
		}
		f, found, err := signInIn(tx, sf, tail)
		if err != nil || !found {
			return expiring{}, err
		}
		return expiring{signInExpiryKey(sf, tail, f), &sw.SignIns, countCost, func() error { return tx.Delete(signInKey(sf, tail)) }, nil}, nil
	}
	return expiring{}, errMalformed
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
	return deleteRows(tx, append(auditIndexKeys(a, pos), key))
}
