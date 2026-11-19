package api

import (
	"context"
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

func quotaOf(q entity.Quota) apiv1.Quota {
	return apiv1.Quota{ID: idString(q.Principal.ID), Generation: q.Version, Spec: quotaSpecOf(q)}
}

// quotaTarget is the user or group sub names, read tracked in tx; false
// when it names neither.
func quotaTarget(tx entity.ControlPlaneTx, sub string) (entity.Principal, bool, error) {
	p, ok, err := principalIn(tx, sub)
	return p, ok && (p.Kind == entity.PrincipalUser || p.Kind == entity.PrincipalGroup), err
}

// quotas is what the write adapter knows of a share's quota on a user or
// group: its tag is its version, zero once it limits nothing and so is
// deleted.
var quotas = writing[quotaDoc]{kind: entity.ResourceQuota, ref: shareRef, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (quotaDoc, uint64, bool, error) {
	p, ok, err := quotaTarget(tx, sub)
	if err != nil || !ok {
		return quotaDoc{}, 0, false, err
	}
	q, ok, err := tx.Quota(entity.ShareID(id), p)
	return quotaDoc{quotaSpecOf(q)}, q.Version, ok, err
}}

// putQuota is PUT /shares/{id}/quotas/{sub}: the share's quota on the user
// or group sub names set to the spec's limits, under the version If-Match
// names; one whose limits are all zero limits nothing and is deleted.
func putQuota(_ context.Context, rq request[quotaDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Quota, [16]byte, error) {
	p, ok, err := quotaTarget(tx, rq.Sub)
	switch {
	case err != nil:
		return apiv1.Quota{}, rq.ID, err
	case !ok:
		return apiv1.Quota{}, rq.ID, errNotFound
	}
	sp := rq.Body.Spec
	q := entity.Quota{Share: entity.ShareID(rq.ID), Principal: p, Hard: limitFrom(sp.Hard), Soft: limitFrom(sp.Soft),
		Grace: time.Duration(sp.GraceSeconds) * time.Second, Advisory: limitFrom(sp.Advisory)}
	if q.Version, err = tx.SetQuota(q.Share, q, rq.Expect); err != nil {
		return apiv1.Quota{}, rq.ID, err
	}
	return quotaOf(q), rq.ID, nil
}

// quota is GET /shares/{id}/quotas/{sub}, its version the ETag, 404 when
// the share holds no quota on that principal.
//
// ponytail: it walks the share's quotas to the one named, O(n) in the
// share's quotas. Read the one key once the view reads a quota by its
// target.
var quota = watching[apiv1.Quota]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Quota, error) {
		p, err := entity.ParsePrincipalID(rq.Sub)
		if err != nil {
			return apiv1.Quota{}, errNotFound
		}
		for q, err := range d.Store.Quotas(ctx, entity.ShareID(rq.ID)) {
			if err != nil {
				return apiv1.Quota{}, err
			}
			if !q.Principal.IsZero() && q.Principal.ID == p {
				return quotaOf(q), nil
			}
		}
		return apiv1.Quota{}, errNotFound
	},
	tag:   func(q apiv1.Quota) uint64 { return q.Generation },
	until: changes(func(first, now apiv1.Quota) bool { return first.Generation != now.Generation }),
}
