package api

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// grantDoc is a grant as its PUT carries it: its spec alone.
type grantDoc struct {
	Spec apiv1.GrantSpec `json:"spec"`
}

// shareRef is the reference the audit records of a share's grant or quota
// name it by: the share's ID, then the principal's.
func shareRef(id [16]byte, sub string) []byte {
	p, _ := entity.ParsePrincipalID(sub)
	return append(id[:], p[:]...)
}

// grantOf is g on the wire, named by the principal's ID.
func grantOf(g entity.ShareGrant) apiv1.Grant {
	return apiv1.Grant{ID: idString(g.Principal.ID), Generation: g.Version, Spec: apiv1.GrantSpec{Access: g.Access.String()}}
}

// grants is what the write adapter knows of a grant: its tag is its
// version, read by the principal's ID alone, as the store keys it. A grant
// raises neither generation, so its write answers at its commit.
var grants = writing[grantDoc]{kind: entity.ResourceGrant, ref: shareRef, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (grantDoc, uint64, bool, error) {
	p, err := entity.ParsePrincipalID(sub)
	if err != nil {
		return grantDoc{}, 0, false, nil
	}
	g, ok, err := tx.Grant(entity.ShareID(id), entity.Principal{ID: p})
	return grantDoc{grantOf(g).Spec}, g.Version, ok, err
}}

// grantTags is grants for a delete, which carries no body.
var grantTags = writing[none]{kind: entity.ResourceGrant, ref: shareRef, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (none, uint64, bool, error) {
	_, tag, ok, err := grants.read(tx, id, sub)
	return none{}, tag, ok, err
}}

// putGrant is PUT /shares/{id}/grants/{sub}: the access the spec names for
// the user, group or share default sub names by ID, none included, which
// is stored and refuses the principal. A machine principal is refused: it
// signs in for itself and holds no file access of its own.
func putGrant(_ context.Context, rq request[grantDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Grant, [16]byte, error) {
	var a entity.ShareAccess
	if a.UnmarshalText([]byte(rq.Body.Spec.Access)) != nil {
		return apiv1.Grant{}, rq.ID, &fieldError{field: "spec.access", detail: "not an access: none, read, read-write or admin"}
	}
	p, ok, err := principalIn(tx, rq.Sub)
	switch {
	case err != nil:
		return apiv1.Grant{}, rq.ID, err
	case !ok:
		return apiv1.Grant{}, rq.ID, errNotFound
	case p.Kind == entity.PrincipalMachine:
		return apiv1.Grant{}, rq.ID, &fieldError{field: "principal", detail: "a machine principal holds no grant"}
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
	p, err := entity.ParsePrincipalID(rq.Sub)
	if err != nil {
		return none{}, rq.ID, errNotFound
	}
	return none{}, rq.ID, tx.DeleteGrant(entity.ShareID(rq.ID), entity.Principal{ID: p}, rq.Expect)
}

// grant is GET /shares/{id}/grants/{sub}, its version the ETag, 404 when
// the principal holds no grant; ?wait= returns once the version changes.
var grant = watching[apiv1.Grant]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Grant, error) {
		p, err := entity.ParsePrincipalID(rq.Sub)
		if err != nil {
			return apiv1.Grant{}, errNotFound
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
