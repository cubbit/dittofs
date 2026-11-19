package api

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// grantDoc is a grant as its PUT carries it: its spec alone.
type grantDoc struct {
	Spec apiv1.GrantSpec `json:"spec"`
}

// grantOf is g on the wire, named by the principal's ID.
func grantOf(g entity.ShareGrant) apiv1.Grant {
	return apiv1.Grant{ID: idString(g.Principal.ID), Generation: g.Version, Spec: apiv1.GrantSpec{Access: g.Access.String()}}
}

// grants is what the write adapter knows of a grant: its tag is its
// version, read by the principal's ID alone, as the store keys it. A grant
// raises neither generation, so its write answers at its commit.
var grants = writing[grantDoc]{kind: entity.ResourceGrant, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (grantDoc, uint64, bool, error) {
	p, err := principalSub(sub)
	if err != nil {
		return grantDoc{}, 0, false, err
	}
	g, ok, err := tx.Grant(entity.ShareID(id), entity.Principal{ID: p})
	return grantDoc{grantOf(g).Spec}, g.Version, ok, err
}}

// putGrant is PUT /shares/{id}/grants/{sub}: the access the spec names for
// the user, group or share default sub names by ID, none included, which
// is stored and refuses the principal. The store refuses a machine
// principal, which holds no file access of its own.
func putGrant(_ context.Context, rq request[grantDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Grant, [16]byte, error) {
	var a entity.ShareAccess
	if a.UnmarshalText([]byte(rq.Body.Spec.Access)) != nil {
		return apiv1.Grant{}, rq.ID, &fieldError{field: "spec.access", detail: "not an access: none, read, read-write or admin"}
	}
	pid, err := principalSub(rq.Sub)
	if err != nil {
		return apiv1.Grant{}, rq.ID, err
	}
	p, ok, err := principalIn(tx, pid)
	switch {
	case err != nil:
		return apiv1.Grant{}, rq.ID, err
	case !ok:
		return apiv1.Grant{}, rq.ID, errNotFound
	}
	g := entity.ShareGrant{Share: entity.ShareID(rq.ID), Principal: p, Access: a}
	if g.Version, err = tx.SetGrant(g, rq.Expect); err != nil {
		return apiv1.Grant{}, rq.ID, err
	}
	return grantOf(g), rq.ID, nil
}

// deleteGrant is DELETE /shares/{id}/grants/{sub}: the grant removed under
// the version If-Match names, so the principal's groups' grants decide its
// access again.
func deleteGrant(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	p, err := principalSub(rq.Sub)
	if err != nil {
		return none{}, rq.ID, err
	}
	return none{}, rq.ID, tx.DeleteGrant(entity.ShareID(rq.ID), entity.Principal{ID: p}, rq.Expect)
}

// grant is GET /shares/{id}/grants/{sub}, its version the ETag, 404 when
// the principal holds no grant; ?wait= returns once the version changes.
var grant = watching[apiv1.Grant]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Grant, error) {
		p, err := principalSub(rq.Sub)
		if err != nil {
			return apiv1.Grant{}, err
		}
		g, ok, err := d.Store.ShareGrant(ctx, entity.ShareID(rq.ID), entity.Principal{ID: p})
		if err == nil && !ok {
			err = errNotFound
		}
		return grantOf(g), err
	},
	tag:   func(g apiv1.Grant) uint64 { return g.Generation },
	until: changes(func(first, now apiv1.Grant) bool { return first.Generation != now.Generation }),
}

// grantsList is GET /shares/{id}/grants, every grant stored on the share,
// none grants included, in principal ID order.
var grantsList = listing[entity.ShareGrant, apiv1.Grant]{
	under: func(ctx context.Context, d Deps, share [16]byte, after []byte, _ map[string]string) iter.Seq2[entity.ShareGrant, error] {
		return d.Store.ShareGrants(ctx, entity.ShareID(share), entity.PrincipalID(after16(after)))
	},
	key:  func(g entity.ShareGrant) []byte { return g.Principal.ID[:] },
	item: func(g entity.ShareGrant, _ perPage) apiv1.Grant { return grantOf(g) },
}
