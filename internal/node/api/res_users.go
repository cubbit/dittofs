package api

import (
	"context"
	"iter"
	"net/mail"
	"strconv"

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
// decision: a directory principal's address is refused here because the
// directory supplies it, and RFC 18 §5.1's later answers update it.
// Withdraw it only if a directory stops supplying addresses.
func emailFor(u entity.User, email string) error {
	if email == "" {
		return nil
	}
	if len(email) > maxEmail {
		return &fieldError{field: "spec.email", detail: "longer than " + strconv.Itoa(maxEmail) + " bytes"}
	}
	if _, dir := spelling(u.IDs, entity.SchemeDir); dir {
		return &fieldError{field: "spec.email", detail: "a directory principal's address is the directory's"}
	}
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return &fieldError{field: "spec.email", detail: "not an e-mail address: " + email}
	}
	return nil
}

// named refuses an edit giving no name in place of one, since the API's
// principals are found by name. A principal minted with no name keeps none;
// a new name is the store's to refuse (nameField).
func named(old, name string) error {
	if name == "" && old != "" {
		return &fieldError{field: "spec.name", detail: "a user or group keeps its name"}
	}
	return nil
}

// newName refuses a create naming no one; the name itself is the store's
// to refuse (nameField).
func newName(name string) error {
	if name == "" {
		return &fieldError{field: "spec.name", detail: "a user or group is created with a name"}
	}
	return nil
}

// maxEmail bounds an address, in bytes, as SMTP's path limit does.
const maxEmail = 254

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
}, hidden: func(ctx context.Context, tx entity.ControlPlaneTx, id [16]byte) (bool, error) {
	u, ok, err := tx.User(userAt(id))
	return !ok || hiddenFrom(ctx, u), err
}}

// createUser is POST /users: a user of the spec's name, its uid the one the
// spec names or, when it names none, one allocated from identity.id_range,
// and its SID allocated, in the create's transaction. A name or uid another
// principal holds is refused by the store, and a uid of 0 here.
func createUser(_ context.Context, rq request[userDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
	sp := rq.Body.Spec
	ids, err := withNumber(nil, entity.SchemeUID, sp.UID, "spec.uid")
	if err != nil {
		return apiv1.User{}, rq.ID, err
	}
	u := entity.User{Principal: entity.NewUser(), Name: sp.Name, Email: sp.Email, Disabled: sp.Disabled, IDs: ids}
	if err := newName(sp.Name); err != nil {
		return apiv1.User{}, u.Principal.ID, err
	}
	if err := emailFor(u, sp.Email); err != nil {
		return apiv1.User{}, u.Principal.ID, err
	}
	a := entity.Alloc{UID: sp.UID == nil, RID: true}
	if a.UID {
		if a.Range, err = idRange(tx); err != nil {
			return apiv1.User{}, u.Principal.ID, err
		}
	}
	if _, err := tx.PutUser(u, 0, a); err != nil {
		return apiv1.User{}, u.Principal.ID, nameField("spec.name", err)
	}
	u, _, err = tx.User(u.Principal)
	return userOf(u), u.Principal.ID, err
}

// putUser is PUT and PATCH /users/{id}: the user's name, uid, address and
// disabled flag as the spec states them, under the generation If-Match
// names, every other field and spelling kept. Its uid is removed only with
// its SID, which never changes, so the store refuses a spec naming none. A
// user that does not exist, or that the caller may not know of, is 404
// before its precondition: a user is created only by POST. Disabling the
// last enabled admin is refused.
func putUser(_ context.Context, rq request[userDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
	sp := rq.Body.Spec
	u, _, err := tx.User(userAt(rq.ID))
	if err != nil {
		return apiv1.User{}, rq.ID, err
	}
	was := u
	if err := named(u.Name, sp.Name); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	if err := emailFor(u, sp.Email); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	if u.IDs, err = withNumber(u.IDs, entity.SchemeUID, sp.UID, "spec.uid"); err != nil {
		return apiv1.User{}, rq.ID, err
	}
	u.Name, u.Email, u.Disabled = sp.Name, sp.Email, sp.Disabled
	if u.Generation, err = tx.PutUser(u, rq.Expect, entity.Alloc{}); err != nil {
		return apiv1.User{}, rq.ID, nameField("spec.name", err)
	}
	return userOf(u), rq.ID, lastAdmin(tx, was)
}

// deleteUser is DELETE /users/{id}: DeleteUser's mark under the generation
// If-Match names, from which the user is found by no name or ID and
// admitted nowhere; the operation the row starts drops the rest. Deleting
// the last enabled admin is refused.
func deleteUser(_ context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	u, _, err := tx.User(userAt(rq.ID))
	if err != nil {
		return none{}, rq.ID, err
	}
	if err := tx.DeleteUser(userAt(rq.ID), rq.Expect); err != nil {
		return none{}, rq.ID, err
	}
	return none{}, rq.ID, lastAdmin(tx, u)
}

// usersList is GET /users, every named user in name order; ?name= answers
// the one user of that name, or none. A user the caller may not know of is
// left out.
var usersList = listing[entity.User, apiv1.User]{
	filters: []string{"name"},
	records: func(ctx context.Context, d Deps, after []byte, f map[string]string) iter.Seq2[entity.User, error] {
		return func(yield func(entity.User, error) bool) {
			if name, ok := f["name"]; ok {
				u, found, err := d.Store.UserNamed(ctx, name)
				if err != nil || found && !hiddenFrom(ctx, u) {
					yield(shown(ctx, u), err)
				}
				return
			}
			for u, err := range d.Store.Users(ctx, string(after)) {
				if err == nil && hiddenFrom(ctx, u) {
					continue
				}
				if !yield(shown(ctx, u), err) || err != nil {
					return
				}
			}
		}
	},
	key:  func(u entity.User) []byte { return []byte(u.Name) },
	item: func(u entity.User, _ perPage) apiv1.User { return userOf(u) },
}

// user is GET /users/{id}, 404 for one that does not exist, is marked
// deleted or that the caller may not know of; ?wait= returns once its
// generation changes.
var user = watching[apiv1.User]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.User, error) {
		u, ok, err := d.Store.User(ctx, userAt(rq.ID))
		if err == nil && (!ok || hiddenFrom(ctx, u)) {
			return apiv1.User{}, errNotFound
		}
		return userOf(shown(ctx, u)), err
	},
	tag:   func(u apiv1.User) uint64 { return u.Generation },
	until: changes(func(first, now apiv1.User) bool { return first.Generation != now.Generation }),
}
