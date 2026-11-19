package api

import (
	"context"
	"iter"
	"math"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// quotaDoc is a quota as its PUT carries it: its spec alone.
type quotaDoc struct {
	Spec apiv1.QuotaSpec `json:"spec"`
}

func limitOf(l entity.Limit) apiv1.Limit { return apiv1.Limit{Bytes: l.Bytes, Files: l.Files} }

func limitFrom(l apiv1.Limit) entity.Limit { return entity.Limit{Bytes: l.Bytes, Files: l.Files} }

func quotaSpecOf(q entity.Quota) apiv1.QuotaSpec {
	return apiv1.QuotaSpec{Hard: limitOf(q.Hard), Soft: limitOf(q.Soft), GraceSeconds: int64(q.Grace / time.Second), Advisory: limitOf(q.Advisory)}
}

// quotaOf is q on the wire, named by its principal's ID, or by the share's
// for the share's own.
func quotaOf(q entity.Quota) apiv1.Quota {
	id := [16]byte(q.Principal.ID)
	if q.Principal.IsZero() {
		id = q.Share
	}
	return apiv1.Quota{ID: idString(id), Generation: q.Version, Spec: quotaSpecOf(q)}
}

// quotaTarget is the user or group sub names, read tracked in tx, the zero
// principal for an empty sub, the share's own quota; false when sub names
// neither.
func quotaTarget(tx entity.ControlPlaneTx, sub string) (entity.Principal, bool, error) {
	if sub == "" {
		return entity.Principal{}, true, nil
	}
	pid, err := principalSub(sub)
	if err != nil {
		return entity.Principal{}, false, err
	}
	p, ok, err := principalIn(tx, pid)
	return p, ok && (p.Kind == entity.PrincipalUser || p.Kind == entity.PrincipalGroup), err
}

// quotas is what the write adapter knows of a share's quota on a user or
// group, or on the share itself: its tag is its version, zero once it limits
// nothing and so is deleted.
var quotas = writing[quotaDoc]{kind: entity.ResourceQuota, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (quotaDoc, uint64, bool, error) {
	p, ok, err := quotaTarget(tx, sub)
	if err != nil || !ok {
		return quotaDoc{}, 0, false, err
	}
	q, ok, err := tx.Quota(entity.ShareID(id), p)
	return quotaDoc{quotaSpecOf(q)}, q.Version, ok, err
}}

// maxGrace is the longest grace a quota's time.Duration holds, in seconds.
const maxGrace = math.MaxInt64 / int64(time.Second)

// putQuota is PUT /shares/{id}/quotas/{sub} and /shares/{id}/quota: the
// share's quota on the user or group sub names, or on the share itself, set
// to the spec's limits under the version If-Match names; one whose limits
// are all zero limits nothing and is deleted.
func putQuota(_ context.Context, rq request[quotaDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Quota, [16]byte, error) {
	sp := rq.Body.Spec
	if sp.GraceSeconds < 0 || sp.GraceSeconds > maxGrace {
		return apiv1.Quota{}, rq.ID, &fieldError{field: "spec.grace_seconds", detail: "a grace from 0 to the longest a duration holds"}
	}
	p, ok, err := quotaTarget(tx, rq.Sub)
	switch {
	case err != nil:
		return apiv1.Quota{}, rq.ID, err
	case !ok:
		return apiv1.Quota{}, rq.ID, errNotFound
	}
	q := entity.Quota{Share: entity.ShareID(rq.ID), Principal: p, Hard: limitFrom(sp.Hard), Soft: limitFrom(sp.Soft),
		Grace: time.Duration(sp.GraceSeconds) * time.Second, Advisory: limitFrom(sp.Advisory)}
	if q.Version, err = tx.SetQuota(q.Share, q, rq.Expect); err != nil {
		return apiv1.Quota{}, rq.ID, err
	}
	return quotaOf(q), rq.ID, nil
}

// quota is GET /shares/{id}/quotas/{sub} and /shares/{id}/quota, its
// version the ETag, 404 when the share holds no such quota.
var quota = watching[apiv1.Quota]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Quota, error) {
		var p entity.Principal
		if rq.Sub != "" {
			pid, err := principalSub(rq.Sub)
			if err != nil {
				return apiv1.Quota{}, err
			}
			p = entity.Principal{Kind: entity.PrincipalUser, ID: pid} // the store keys a quota by ID alone
		}
		q, ok, err := d.Store.Quota(ctx, entity.ShareID(rq.ID), p)
		if err == nil && !ok {
			err = errNotFound
		}
		return quotaOf(q), err
	},
	tag:   func(q apiv1.Quota) uint64 { return q.Generation },
	until: changes(func(first, now apiv1.Quota) bool { return first.Generation != now.Generation }),
}

// quotasList is GET /shares/{id}/quotas, the share's quotas on users and
// groups in principal ID order.
var quotasList = listing[entity.Quota, apiv1.Quota]{
	under: func(ctx context.Context, d Deps, share [16]byte, after []byte, _ map[string]string) iter.Seq2[entity.Quota, error] {
		return d.Store.PrincipalQuotas(ctx, entity.ShareID(share), entity.PrincipalID(after16(after)))
	},
	key:  func(q entity.Quota) []byte { return q.Principal.ID[:] },
	item: func(q entity.Quota, _ perPage) apiv1.Quota { return quotaOf(q) },
}
