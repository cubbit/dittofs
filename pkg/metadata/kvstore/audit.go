package kvstore

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// An audit record is keyed by its store time then its ID, so the records sit
// in time order and a time range is one range. Each also has one empty index
// row for its actor, its target and its taking node, each ending in the
// record's own time and ID. Retention's cut is the time range plus, for each
// record in it, its three index rows; a cut that leaves an index row behind
// leaves one the read skips.
//
// decision: the store time is the transaction's, taken when its attempt
// starts, not when it commits, so a slow write can commit a record behind a
// position a reader has already passed. A listing tolerates that: it is a
// snapshot, and the record shows in the next one. A reader resuming by
// position must re-read from its position minus the longest transaction's
// duration and skip the IDs it has seen; the audit export's cursor owns that.
// Key records by commit order instead if a consumer resuming by position
// cannot re-read.

// auditPos is a record's position: its time then its ID, the tail of its key
// and of each of its index rows.
func auditPos(t time.Time, id [16]byte) []byte {
	return keys.ID(keys.Uint64(nil, uint64(t.UnixNano())), id[:])
}

// AuditPosition is a's position in the audit, what AuditRecords takes to
// resume after a. Its bytes are opaque: a caller keeps and passes them back,
// and never builds or reads them.
func AuditPosition(a entity.AuditRecord) []byte { return auditPos(a.Time, a.ID) }

func auditKey(t time.Time, id [16]byte) []byte {
	return append([]byte{keys.KindAU}, auditPos(t, id)...)
}

func auditActorPrefix(p entity.Principal) []byte {
	return keys.ID([]byte{keys.KindAUX, keys.AUXActor}, p.ID[:])
}

func auditTargetPrefix(r entity.ResourceRef) []byte {
	return keys.String([]byte{keys.KindAUX, keys.AUXTarget, byte(r.Kind)}, r.ID)
}

func auditNodePrefix(n entity.NodeID) []byte {
	return keys.ID([]byte{keys.KindAUX, keys.AUXNode}, n[:])
}

// putAudit appends a: it takes the transaction's store time and a fresh ID,
// so no write lands on an existing record. A target of no known kind is
// refused.
func putAudit(tx kv.Txn, a *entity.AuditRecord) error {
	if a.Target.Kind.String() == "" {
		return merrs.NewInvalidArgumentError("an audit record's target is of no known kind")
	}
	a.Time, a.ID = tx.Now(), uuid.New()
	if err := tx.Set(auditKey(a.Time, a.ID), encodeAudit(*a)); err != nil {
		return err
	}
	for _, prefix := range [][]byte{auditActorPrefix(a.Actor), auditTargetPrefix(a.Target), auditNodePrefix(a.Node)} {
		if err := tx.Set(append(prefix, auditPos(a.Time, a.ID)...), nil); err != nil {
			return err
		}
	}
	return nil
}

// auditMatches reports whether f's actor, target and node select a.
func auditMatches(f entity.AuditFilter, a entity.AuditRecord) bool {
	return (f.Actor == nil || *f.Actor == a.Actor) && (f.Node == nil || *f.Node == a.Node) &&
		(f.Target == nil || f.Target.Kind == a.Target.Kind && bytes.Equal(f.Target.ID, a.Target.ID))
}

// AuditRecords lists the records f selects in store-time order, resuming
// after the position after, the last record read's AuditPosition. A filter
// naming an actor, a target or a node reads that one's index; one naming
// more reads the first and checks the rest on each record. An index row
// whose record retention has already deleted is skipped.
func (c ControlPlane) AuditRecords(ctx context.Context, f entity.AuditFilter, after []byte) iter.Seq2[entity.AuditRecord, error] {
	prefix := []byte{keys.KindAU}
	switch {
	case f.Actor != nil:
		prefix = auditActorPrefix(*f.Actor)
	case f.Target != nil:
		prefix = auditTargetPrefix(*f.Target)
	case f.Node != nil:
		prefix = auditNodePrefix(*f.Node)
	}
	// A key at From's time with no ID sorts before every record at From.
	start := keys.Uint64(nil, uint64(max(f.From.UnixNano(), 0)))
	if f.From.IsZero() || bytes.Compare(after, start) > 0 {
		start = after
	}
	if start != nil {
		start = append(bytes.Clone(prefix), start...)
	}
	type row struct {
		a  entity.AuditRecord
		ok bool
	}
	rows := pages(ctx, c.s.kv, prefix, start, func(r kv.Reader, kvp kv.KeyValue) (row, error) {
		pos := kvp.Key[len(prefix):]
		b := kvp.Value
		if prefix[0] != keys.KindAU {
			var err error
			if b, err = r.Get(append([]byte{keys.KindAU}, pos...)); errors.Is(err, kv.ErrNotFound) {
				return row{}, nil
			} else if err != nil {
				return row{}, err
			}
		}
		a, err := decodeAudit(pos, b)
		return row{a, err == nil && auditMatches(f, a)}, err
	})
	return func(yield func(entity.AuditRecord, error) bool) {
		for r, err := range rows {
			if err != nil {
				yield(entity.AuditRecord{}, err)
				return
			}
			if !f.To.IsZero() && !r.a.Time.Before(f.To) {
				return
			}
			if r.ok && !yield(r.a, nil) {
				return
			}
		}
	}
}

func encodeAudit(a entity.AuditRecord) []byte {
	w := putPrincipal(codec.NewWriter(v1), a.Actor).String(a.ActorName).String(a.Credential).
		Fixed(a.Node[:]).String(a.Source).Bytes(a.Request).Uint8(uint8(a.Target.Kind)).Bytes(a.Target.ID)
	return w.Uint64(a.Before).Uint64(a.After).Bytes(a.Change).Bytes(a.Outcome).Encode()
}

// decodeAudit decodes a record whose position, its time and ID, is pos.
func decodeAudit(pos, b []byte) (entity.AuditRecord, error) {
	ns, rest, err := keys.ReadUint64(pos)
	if err != nil || len(rest) != keys.IDLen {
		return entity.AuditRecord{}, errMalformed
	}
	a := entity.AuditRecord{ID: [16]byte(rest), Time: time.Unix(0, int64(ns)).UTC()}
	r, err := open(b)
	if err != nil {
		return a, err
	}
	a.Actor = readPrincipal(r)
	a.ActorName, a.Credential = r.String(), r.String()
	copy(a.Node[:], r.Fixed(16))
	a.Source, a.Request = r.String(), cloneOrNil(r.Bytes())
	a.Target = entity.ResourceRef{Kind: entity.ResourceKind(r.Uint8()), ID: cloneOrNil(r.Bytes())}
	a.Before, a.After = r.Uint64(), r.Uint64()
	a.Change, a.Outcome = cloneOrNil(r.Bytes()), cloneOrNil(r.Bytes())
	if err := r.Done(); err != nil {
		return a, err
	}
	if a.Target.Kind.String() == "" {
		return a, errMalformed
	}
	return a, nil
}
