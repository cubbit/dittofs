package api

import (
	"context"
	"iter"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// groupDoc is a group as its writes carry it: its spec alone.
type groupDoc struct {
	Spec apiv1.GroupSpec `json:"spec"`
}

func groupOf(g entity.Group) apiv1.Group {
	return apiv1.Group{ID: idString(g.Principal.ID), Generation: g.Generation,
		Spec: apiv1.GroupSpec{Name: g.Name, GID: numberOf(g.IDs, entity.SchemeGID)}}
}

// groupAt is the group principal a path's ID names.
func groupAt(id [16]byte) entity.Principal {
	return entity.Principal{Kind: entity.PrincipalGroup, ID: entity.PrincipalID(id)}
}

// groups is what the write adapter knows of a group, as users is of a user.
var groups = writing[groupDoc]{kind: entity.ResourceGroup, raises: true, read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (groupDoc, uint64, bool, error) {
	g, ok, err := tx.Group(groupAt(id))
	return groupDoc{groupOf(g).Spec}, g.Generation, ok, err
}}

// groupTags is groups for a delete, which carries no body.
var groupTags = writing[none]{kind: entity.ResourceGroup, raises: true, read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (none, uint64, bool, error) {
	g, ok, err := tx.Group(groupAt(id))
	return none{}, g.Generation, ok, err
}}

// createGroup is POST /groups: a group of the spec's name, its gid the one
// the spec names or, when it names none, one allocated from
// identity.id_range, and its SID allocated, in the create's transaction.
func createGroup(_ context.Context, rq request[groupDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Group, [16]byte, error) {
	sp := rq.Body.Spec
	g := entity.Group{Principal: entity.NewGroup(), Name: sp.Name, IDs: withNumber(nil, entity.SchemeGID, sp.GID)}
	a := entity.Alloc{GID: sp.GID == 0, RID: true}
	if a.GID {
		var err error
		if a.Range, err = idRange(tx); err != nil {
			return apiv1.Group{}, g.Principal.ID, err
		}
	}
	if _, err := tx.PutGroup(g, 0, a); err != nil {
		return apiv1.Group{}, g.Principal.ID, err
	}
	g, _, err := tx.Group(g.Principal)
	return groupOf(g), g.Principal.ID, err
}

// putGroup is PUT and PATCH /groups/{id}: the group's name and gid as the
// spec states them, under the generation If-Match names, its SID kept.
func putGroup(_ context.Context, rq request[groupDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Group, [16]byte, error) {
	g, _, err := tx.Group(groupAt(rq.ID))
	if err != nil {
		return apiv1.Group{}, rq.ID, err
	}
	g.Name, g.IDs = rq.Body.Spec.Name, withNumber(g.IDs, entity.SchemeGID, rq.Body.Spec.GID)
	if g.Generation, err = tx.PutGroup(g, rq.Expect, entity.Alloc{}); err != nil {
		return apiv1.Group{}, rq.ID, err
	}
	return groupOf(g), rq.ID, nil
}

// deleteGroup is DELETE /groups/{id}: DeleteGroup's mark under the
// generation If-Match names, from which its memberships grant nothing; the
// operation the row starts drops the rest.
func deleteGroup(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	return none{}, rq.ID, tx.DeleteGroup(groupAt(rq.ID), rq.Expect)
}

// groupsList is GET /groups, every named group in name order.
var groupsList = listing[entity.Group, apiv1.Group]{
	records: func(ctx context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[entity.Group, error] {
		return d.Store.Groups(ctx, string(after))
	},
	key:  func(g entity.Group) []byte { return []byte(g.Name) },
	item: func(g entity.Group, _ perPage) apiv1.Group { return groupOf(g) },
}

// group is GET /groups/{id}, as user is a user's.
var group = watching[apiv1.Group]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Group, error) {
		g, ok, err := d.Store.Group(ctx, groupAt(rq.ID))
		if err == nil && !ok {
			err = errNotFound
		}
		return groupOf(g), err
	},
	tag:   func(g apiv1.Group) uint64 { return g.Generation },
	until: changes(func(first, now apiv1.Group) bool { return first.Generation != now.Generation }),
}

// principalIn is the principal whose ID sub names, as a user, a machine, a
// group or a share's default, read tracked in tx; false for an ID that does
// not parse or names none of them.
func principalIn(tx entity.ControlPlaneTx, sub string) (entity.Principal, bool, error) {
	id, err := entity.ParsePrincipalID(sub)
	if err != nil {
		return entity.Principal{}, false, nil
	}
	for _, p := range []entity.Principal{entity.AuthenticatedUsers, entity.Everyone} {
		if p.ID == id {
			return p, true, nil
		}
	}
	for _, k := range []entity.PrincipalKind{entity.PrincipalUser, entity.PrincipalMachine} {
		p := entity.Principal{Kind: k, ID: id}
		if _, ok, err := tx.User(p); err != nil || ok {
			return p, ok, err
		}
	}
	p := entity.Principal{Kind: entity.PrincipalGroup, ID: id}
	_, ok, err := tx.Group(p)
	return p, ok, err
}

// membershipDoc is a membership as its PUT carries it: a spec declaring
// nothing.
type membershipDoc struct {
	Spec struct{} `json:"spec"`
}

// memberRef is the reference the audit records of a membership name it by:
// the group's ID, then the member's.
func memberRef(id [16]byte, sub string) []byte {
	m, _ := entity.ParsePrincipalID(sub)
	return append(id[:], m[:]...)
}

// memberships is what the write adapter knows of one member of a group.
//
// decision: a membership has no version, as it never changes: it exists or
// it does not. Its tag is 1 while it exists, so If-None-Match: * adds it
// once and If-Match: "1" removes it once. Overturned if a membership ever
// carries a field a write can change.
var memberships = writing[membershipDoc]{kind: entity.ResourceMembership, raises: true, ref: memberRef, read: readMember}

// membershipTags is memberships for a delete, which carries no body.
var membershipTags = writing[none]{kind: entity.ResourceMembership, raises: true, ref: memberRef, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (none, uint64, bool, error) {
	_, tag, ok, err := readMember(tx, id, sub)
	return none{}, tag, ok, err
}}

func readMember(tx entity.ControlPlaneTx, id [16]byte, sub string) (membershipDoc, uint64, bool, error) {
	m, found, err := membershipIn(tx, id, sub)
	if err != nil || !found {
		return membershipDoc{}, 0, false, err
	}
	ok, err := tx.Member(m)
	if !ok {
		return membershipDoc{}, 0, false, err
	}
	return membershipDoc{}, 1, true, err
}

// membershipIn is the membership of the principal sub names in the group id
// names; false when sub names none.
func membershipIn(tx entity.ControlPlaneTx, id [16]byte, sub string) (entity.Membership, bool, error) {
	p, ok, err := principalIn(tx, sub)
	return entity.Membership{Group: groupAt(id), Member: p}, ok, err
}

func membershipOf(m entity.Membership) apiv1.Membership {
	return apiv1.Membership{ID: idString(m.Member.ID), Generation: 1}
}

// addMember is PUT /groups/{id}/members/{sub}: the user or group sub names
// added to the group. The store refuses a member or a group that does not
// exist, and a member that is neither a user nor a group.
func addMember(_ context.Context, rq request[membershipDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Membership, [16]byte, error) {
	m, ok, err := membershipIn(tx, rq.ID, rq.Sub)
	switch {
	case err != nil:
		return apiv1.Membership{}, rq.ID, err
	case !ok:
		return apiv1.Membership{}, rq.ID, errNotFound
	}
	return membershipOf(m), rq.ID, tx.AddMember(m)
}

// removeMember is DELETE /groups/{id}/members/{sub}.
func removeMember(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	m, _, err := membershipIn(tx, rq.ID, rq.Sub)
	if err != nil {
		return none{}, rq.ID, err
	}
	return none{}, rq.ID, tx.RemoveMember(m)
}
