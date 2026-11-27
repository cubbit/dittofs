package kvstore

import (
	"bytes"
	"context"
	"iter"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// QuotaTarget is what a quota limits in a share: a user or group, a project,
// or, with neither set, the share itself.
type QuotaTarget struct {
	Principal entity.Principal
	Project   uint64
}

// The quota target tag that starts a quota key's suffix.
const (
	quotaPrincipal byte = 0x01
	quotaProject   byte = 0x02
	quotaShare     byte = 0x03
)

// quotaSuffix is t's tagged key component: the tag, then a PrincipalID, the
// project number, or nothing for the share's own quota, which keeps that key
// off the prefix of every other.
func quotaSuffix(t QuotaTarget) ([]byte, error) {
	switch {
	case t == QuotaTarget{}:
		return []byte{quotaShare}, nil
	case t.Project == 0 && (t.Principal.Kind == entity.PrincipalUser || t.Principal.Kind == entity.PrincipalGroup):
		return keys.ID([]byte{quotaPrincipal}, t.Principal.ID[:]), nil
	case t.Project != 0 && t.Principal == (entity.Principal{}):
		return keys.Uint64([]byte{quotaProject}, t.Project), nil
	}
	return nil, merrs.NewInvalidArgumentError("a quota limits a user or group, a project, or the share")
}

func quotaKey(share entity.ShareID, sub byte, suffix []byte) []byte {
	return append(shareKey(share, sub), suffix...)
}

// A quota's value: the principal's kind, which its key's ID lacks, then the
// limits. A project quota's kind is zero.
func encodeQuota(q entity.Quota) []byte {
	w := codec.NewWriter(v1).Uint8(uint8(q.Principal.Kind))
	for _, l := range []entity.Limit{q.Hard, q.Soft, q.Advisory} {
		w.Int64(l.Bytes).Int64(l.Files)
	}
	return w.Int64(int64(q.Grace)).Uint64(q.Version).Encode()
}

func decodeQuota(share entity.ShareID, suffix, b []byte) (entity.Quota, error) {
	r, err := open(b)
	if err != nil {
		return entity.Quota{}, err
	}
	q := entity.Quota{Share: share}
	kind := entity.PrincipalKind(r.Uint8())
	limits := []*entity.Limit{&q.Hard, &q.Soft, &q.Advisory}
	for _, l := range limits {
		l.Bytes, l.Files = r.Int64(), r.Int64()
	}
	q.Grace, q.Version = time.Duration(r.Int64()), r.Uint64()
	if len(suffix) == 0 {
		return q, errMalformed
	}
	switch suffix[0] {
	case quotaPrincipal:
		id, _, err := keys.ReadID(suffix[1:], keys.IDLen)
		if err != nil {
			return q, err
		}
		q.Principal = entity.Principal{Kind: kind, ID: entity.PrincipalID(id)}
	case quotaProject:
		if q.Project, _, err = keys.ReadUint64(suffix[1:]); err != nil {
			return q, err
		}
	case quotaShare:
	default:
		return q, errMalformed
	}
	return q, r.Done()
}

// SetQuota writes a share's quota on a user, a group, a project or the share
// itself, the version expect names or zero for none, raising its version; a
// quota that limits nothing deletes it and its crossing time.
// The share must be live, and a user or group must exist: its record is
// read, so a deletion of it conflicts. A changed soft limit restarts its
// grace: the crossing time is rewritten at the change's time when usage is
// above the new soft limit, and deleted otherwise. A change to any other
// limit leaves a running grace alone.
func (c ControlPlane) SetQuota(ctx context.Context, share entity.ShareID, q entity.Quota, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.SetQuota(share, q, expect) })
}

func (c controlTx) SetQuota(share entity.ShareID, q entity.Quota, expect uint64) (uint64, error) {
	q.Share = share
	t := QuotaTarget{q.Principal, q.Project}
	suffix, err := quotaSuffix(t)
	if err != nil {
		return 0, err
	}
	for _, l := range []entity.Limit{q.Hard, q.Soft, q.Advisory} {
		if l.Bytes < 0 || l.Files < 0 {
			return 0, merrs.NewInvalidArgumentError("a quota limit is not negative")
		}
	}
	if q.Grace < 0 {
		return 0, merrs.NewInvalidArgumentError("a quota's grace is not negative")
	}
	if _, ok, err := get(c.tx, shareKey(q.Share, keys.SInfo)); err != nil || !ok {
		if err == nil {
			err = merrs.NewNotFoundError(q.Share.String(), "share")
		}
		return 0, err
	}
	if err := liveShare(c.tx, q.Share); err != nil {
		return 0, err
	}
	if q.Principal != (entity.Principal{}) {
		if ok, err := live(c.tx, q.Principal); err != nil || !ok {
			return 0, notFound(err, q.Principal)
		}
	}
	key, since := quotaKey(q.Share, keys.SQt, suffix), quotaKey(q.Share, keys.SQx, suffix)
	b, ok, err := get(c.tx, key)
	if err != nil {
		return 0, err
	}
	old := entity.Quota{}
	if ok {
		if old, err = decodeQuota(q.Share, suffix, b); err != nil {
			return 0, err
		}
	}
	if err := expected(old.Version, expect); err != nil {
		return 0, err
	}
	if q.Unlimited() {
		if err := c.tx.Delete(key); err != nil {
			return 0, err
		}
		return 0, c.tx.Delete(since)
	}
	if !ok || old.Soft != q.Soft {
		d, err := usageIn(c.tx, q.Share)
		if err != nil {
			return 0, err
		}
		if above(d.of(t), q.Soft) {
			err = c.tx.Set(since, encodeSince(c.tx.Now()))
		} else {
			err = c.tx.Delete(since)
		}
		if err != nil {
			return 0, err
		}
	}
	q.Version = nextVersion(c.tx, old.Version)
	return q.Version, c.tx.Set(key, encodeQuota(q))
}

// above reports usage past a limit in either of its counts; a zero count
// limits nothing.
func above(u entity.Usage, l entity.Limit) bool {
	return l.Bytes > 0 && u.Bytes > l.Bytes || l.Files > 0 && u.Files > l.Files
}

// of is the usage a quota target is charged: a principal's, a project's, or
// the share's.
func (d usageDelta) of(t QuotaTarget) entity.Usage {
	switch {
	case t.Principal != (entity.Principal{}):
		return d.Principals[t.Principal]
	case t.Project != 0:
		return d.Projects[t.Project]
	}
	return d.Share
}

// Quotas lists a share's quotas: the users' and groups', the projects', then
// the share's own.
func (c ControlPlane) Quotas(ctx context.Context, share entity.ShareID) iter.Seq2[entity.Quota, error] {
	pre := shareKey(share, keys.SQt)
	return pages(ctx, c.s.kv, pre, nil, func(_ kv.Reader, row kv.KeyValue) (entity.Quota, error) {
		return decodeQuota(share, row.Key[len(pre):], row.Value)
	})
}

// Quota is the quota on t in a share, the zero Quota when there is none, and
// since, when usage first went above its soft limit, zero when it isn't
// above it.
func (c Capacity) Quota(ctx context.Context, share entity.ShareID, t QuotaTarget) (q entity.Quota, since time.Time, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		var ok bool
		if q, ok, err = quotaIn(r, share, t); err != nil || !ok {
			return err
		}
		suffix, _ := quotaSuffix(t) // quotaIn parsed it
		b, ok, err := get(r, quotaKey(share, keys.SQx, suffix))
		if err != nil || !ok {
			return err
		}
		since, err = decodeSince(b)
		return err
	})
	if err == nil && q.Version == 0 {
		q = entity.Quota{Share: share, Principal: t.Principal, Project: t.Project}
	}
	return q, since, err
}

// Quota is a share's quota on the user or group p, the zero p naming the
// share's own, and false when there is none.
func (c ControlPlane) Quota(ctx context.Context, share entity.ShareID, p entity.Principal) (q entity.Quota, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		q, found, err = quotaIn(r, share, QuotaTarget{Principal: p})
		return err
	})
	return q, found, err
}

// PrincipalQuotas lists a share's quotas on users and groups, in principal
// ID order, resuming after the principal after; the zero ID starts at the
// first.
func (c ControlPlane) PrincipalQuotas(ctx context.Context, share entity.ShareID, after entity.PrincipalID) iter.Seq2[entity.Quota, error] {
	pre := append(shareKey(share, keys.SQt), quotaPrincipal)
	var cursor []byte
	if after != (entity.PrincipalID{}) {
		cursor = keys.ID(bytes.Clone(pre), after[:])
	}
	return pages(ctx, c.s.kv, pre, cursor, func(_ kv.Reader, row kv.KeyValue) (entity.Quota, error) {
		return decodeQuota(share, row.Key[len(pre)-1:], row.Value)
	})
}

// quotaIn reads the quota on t in a share, tracked in a transaction; false
// when there is none.
func quotaIn(r kv.Reader, share entity.ShareID, t QuotaTarget) (entity.Quota, bool, error) {
	suffix, err := quotaSuffix(t)
	if err != nil {
		return entity.Quota{}, false, err
	}
	b, ok, err := get(r, quotaKey(share, keys.SQt, suffix))
	if err != nil || !ok {
		return entity.Quota{}, false, err
	}
	q, err := decodeQuota(share, suffix, b)
	return q, err == nil, err
}

// SoftExceeded records whether usage of t is above its soft limit, as the
// filesystem service finds it: above, it returns when usage first went
// above, the store's time now when nothing was recorded yet; not above, it
// clears the record and returns zero. Each call writes only when the answer
// changes.
func (c Capacity) SoftExceeded(ctx context.Context, share entity.ShareID, t QuotaTarget, above bool) (since time.Time, err error) {
	suffix, err := quotaSuffix(t)
	if err != nil {
		return since, err
	}
	key := quotaKey(share, keys.SQx, suffix)
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		since = time.Time{}
		b, ok, err := get(tx, key)
		switch {
		case err != nil:
			return err
		case ok && above:
			since, err = decodeSince(b)
			return err
		case ok:
			return tx.Delete(key)
		case above:
			since = tx.Now()
			return tx.Set(key, encodeSince(since))
		}
		return nil
	})
	return since, err
}

func encodeSince(t time.Time) []byte { return putTime(codec.NewWriter(v1), t).Encode() }

func decodeSince(b []byte) (time.Time, error) {
	r, err := open(b)
	if err != nil {
		return time.Time{}, err
	}
	t := readTime(r)
	return t, r.Done()
}
