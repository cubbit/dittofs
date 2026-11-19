package api

import (
	"context"
	"iter"
	"net/mail"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// userDoc is a user as its writes carry it: its spec alone.
type userDoc struct {
	Spec apiv1.UserSpec `json:"spec"`
}

// emailFor refuses an e-mail address u may not hold: one that is not a
// bare, syntactically valid address, and any address on a directory
// principal, one holding a directory entry's ID, whose address the
// directory supplies. An empty address is none.
//
// decision: the directory principal's refusal is inert while no record holds
// a dir spelling, which the store refuses on a user today; it starts to
// refuse once identity's directory work mints directory principals with
// one. Withdraw it only if a directory stops supplying addresses.
func emailFor(u entity.User, email string) error {
	if email == "" {
		return nil
	}
	if _, dir := spelling(u.IDs, entity.SchemeDir); dir {
		return &fieldError{field: "spec.email", detail: "a directory principal's address is the directory's"}
	}
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return &fieldError{field: "spec.email", detail: "not an e-mail address: " + email}
	}
	return nil
}

// named refuses a name a write may not give: none on a create, and none
// in place of one on an edit, since the API's principals are found by
// name. A principal minted with no name keeps none.
func named(old, name string) error {
	if name == "" && old != "" {
		return &fieldError{field: "spec.name", detail: "a user or group keeps its name"}
	}
	return nil
}

// newName refuses a create naming no one.
func newName(name string) error {
	if name == "" {
		return &fieldError{field: "spec.name", detail: "a user or group is created with a name"}
	}
	return nil
}

func userSpecOf(u entity.User) apiv1.UserSpec {
	return apiv1.UserSpec{Name: u.Name, UID: numberOf(u.IDs, entity.SchemeUID), Email: u.Email, Disabled: u.Disabled}
}

func userOf(u entity.User) apiv1.User {
	return apiv1.User{ID: idString(u.Principal.ID), Generation: u.Generation, Spec: userSpecOf(u)}
}

// userAt is the user principal a path's ID names.
func userAt(id [16]byte) entity.Principal {
	return entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID(id)}
}

// users is what the write adapter knows of a user: its record's generation
// is its tag, and its writes raise the principals generation, so each waits
// for the node to apply it.
var users = writing[userDoc]{kind: entity.ResourceUser, raises: true, read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (userDoc, uint64, bool, error) {
	u, ok, err := tx.User(userAt(id))
	return userDoc{userSpecOf(u)}, u.Generation, ok, err
}}

// createUser is POST /users: a user of the spec's name, its uid the one the
// spec names or, when it names none, one allocated from identity.id_range,
// and its SID allocated, in the create's transaction. A name or uid another
// principal holds is refused by the store.
func createUser(_ context.Context, rq request[userDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
	sp := rq.Body.Spec
	u := entity.User{Principal: entity.NewUser(), Name: sp.Name, Email: sp.Email, Disabled: sp.Disabled,
		IDs: withNumber(nil, entity.SchemeUID, sp.UID)}
	if err := newName(sp.Name); err != nil {
		return apiv1.User{}, u.Principal.ID, err
	}
	if err := emailFor(u, sp.Email); err != nil {
		return apiv1.User{}, u.Principal.ID, err
	}
	a := entity.Alloc{UID: sp.UID == 0, RID: true}
	if a.UID {
		var err error
		if a.Range, err = idRange(tx); err != nil {
			return apiv1.User{}, u.Principal.ID, err
		}
	}
	if _, err := tx.PutUser(u, 0, a); err != nil {
		return apiv1.User{}, u.Principal.ID, err
	}
	u, _, err := tx.User(u.Principal)
	return userOf(u), u.Principal.ID, err
}

// putUser is PUT and PATCH /users/{id}: the user's name, uid, address and
// disabled flag as the spec states them, under the generation If-Match
// names, every other field and spelling kept. Its uid is removed only with
// its SID, which never changes, so the store refuses a spec naming none. A
// user that does not exist is 404, If-None-Match: * included: a user is
// created only by POST.
func putUser(ctx context.Context, rq request[userDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
	sp := rq.Body.Spec
	u, ok, err := tx.User(userAt(rq.ID))
	switch {
	case err != nil:
		return apiv1.User{}, rq.ID, err
	case !ok:
		return apiv1.User{}, rq.ID, errNotFound
	}
	if err := mayWrite(ctx, u); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	if err := named(u.Name, sp.Name); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	if err := emailFor(u, sp.Email); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	u.Name, u.Email, u.Disabled, u.IDs = sp.Name, sp.Email, sp.Disabled, withNumber(u.IDs, entity.SchemeUID, sp.UID)
	if u.Generation, err = tx.PutUser(u, rq.Expect, entity.Alloc{}); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	return userOf(u), rq.ID, nil
}

// deleteUser is DELETE /users/{id}: DeleteUser's mark under the generation
// If-Match names, from which the user is found by no name or ID and
// admitted nowhere; the operation the row starts drops the rest.
func deleteUser(ctx context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	u, ok, err := tx.User(userAt(rq.ID))
	if err != nil {
		return none{}, rq.ID, err
	}
	if ok {
		if err := mayWrite(ctx, u); err != nil {
			return none{}, rq.ID, err
		}
	}
	return none{}, rq.ID, tx.DeleteUser(userAt(rq.ID), rq.Expect)
}

// usersList is GET /users, every named user in name order; ?name= answers
// the one user of that name, or none.
var usersList = listing[entity.User, apiv1.User]{
	filters: []string{"name"},
	records: func(ctx context.Context, d Deps, after []byte, f map[string]string) iter.Seq2[entity.User, error] {
		return func(yield func(entity.User, error) bool) {
			if name, ok := f["name"]; ok {
				u, found, err := d.Store.UserNamed(ctx, name)
				if err != nil || found {
					yield(shown(ctx, u), err)
				}
				return
			}
			for u, err := range d.Store.Users(ctx, string(after)) {
				if !yield(shown(ctx, u), err) || err != nil {
					return
				}
			}
		}
	},
	key:  func(u entity.User) []byte { return []byte(u.Name) },
	item: func(u entity.User, _ perPage) apiv1.User { return userOf(u) },
}

// user is GET /users/{id}, 404 for one that does not exist or is marked
// deleted; ?wait= returns once its generation changes.
var user = watching[apiv1.User]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.User, error) {
		u, ok, err := d.Store.User(ctx, userAt(rq.ID))
		if err == nil && !ok {
			err = errNotFound
		}
		return userOf(shown(ctx, u)), err
	},
	tag:   func(u apiv1.User) uint64 { return u.Generation },
	until: changes(func(first, now apiv1.User) bool { return first.Generation != now.Generation }),
}
