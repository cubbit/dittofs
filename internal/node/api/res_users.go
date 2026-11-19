package api

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"net/mail"
	"strconv"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
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
	krb, _ := krbOf(u.IDs)
	return apiv1.UserSpec{Name: u.Name, UID: numberOf(u.IDs, entity.SchemeUID), Email: u.Email, Disabled: u.Disabled, Kerberos: krb.Value}
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
// principal holds is refused by the store, and a uid of 0 here. Its
// Kerberos name is stored linked as the pre-write step asked the directory.
func createUser(_ context.Context, rq request[userDoc], link entity.ProtocolID, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
	sp := rq.Body.Spec
	ids, err := withNumber(nil, entity.SchemeUID, sp.UID, "spec.uid")
	if err != nil {
		return apiv1.User{}, rq.ID, err
	}
	if ids, err = withKerberos(tx, ids, sp.Kerberos, link); err != nil {
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
// last enabled admin is refused. A Kerberos name it keeps keeps its link; a
// new one is stored linked as the pre-write step asked the directory.
func putUser(_ context.Context, rq request[userDoc], link entity.ProtocolID, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
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
	if u.IDs, err = withKerberos(tx, u.IDs, sp.Kerberos, link); err != nil {
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

// kerberosLink is the pre-write step of a user's write: the Kerberos name the
// spec states, with the user SID it links for. A name the user already holds
// keeps its link, and one a machine holds is moved with the machine's link,
// each asking nothing; any other is linked for the SID the directory holds
// under it, asked before the transaction. A name the directory cannot answer
// for is refused 503 and one it holds no entry for 400 naming it, each before
// anything is written; with no directory, a new name is refused 503. A name
// another user holds is refused 409.
//
// ponytail: no directory implements KerberosLinkSID yet, so the paths that
// ask one (an answer, an outage, no entry, another realm, a stale answer
// within the grace) run in no test, and the SID's encoding into Linked is
// held only by the store's own refusal of a malformed one. Test them over
// identitytest's in-memory directory once identity's directory grows the
// call, as the RFC 18 plan's T6.9 adds it.
func kerberosLink(ctx context.Context, rq request[userDoc]) (entity.ProtocolID, error) {
	name := rq.Body.Spec.Kerberos
	if name == "" {
		return entity.ProtocolID{}, nil
	}
	if len(name) > maxDirectoryName {
		return entity.ProtocolID{}, tooLong("spec.kerberos")
	}
	h, held, err := rq.d.Store.Holder(ctx, entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
	switch {
	case err != nil:
		return entity.ProtocolID{}, err
	case held && (h.Principal.Kind == entity.PrincipalMachine || h.Principal.ID == entity.PrincipalID(rq.ID)):
		id, _ := krbOf(h.IDs)
		return id, nil
	case held:
		return entity.ProtocolID{}, &componentError{field: "spec.kerberos", detail: "another user holds " + name, err: vfserr.ErrExist}
	case rq.d.Directory == nil:
		return entity.ProtocolID{}, errNoDirectory
	}
	s, err := rq.d.Directory.KerberosLinkSID(ctx, name)
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		return entity.ProtocolID{}, &componentError{field: "spec.kerberos", detail: "the directory holds no user " + name, err: err}
	case err != nil:
		return entity.ProtocolID{}, err
	}
	id := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name}
	if s.Revision != 0 {
		var b bytes.Buffer
		sid.EncodeSID(&b, &s)
		id.Linked = b.String()
	}
	return id, nil
}

// krbOf is the krb spelling ids hold, false for none.
func krbOf(ids entity.ProtocolIDs) (entity.ProtocolID, bool) {
	for _, id := range ids {
		if id.Scheme == entity.SchemeKerberos {
			return id, true
		}
	}
	return entity.ProtocolID{}, false
}

// withKerberos is ids with its krb spelling the one the spec names: removed
// when it names none, kept, link and all, when it names the one held, and
// otherwise link, which the pre-write step took for that name. A name a
// machine holds, read tracked in tx, is taken from it with the machine's
// link: the machine is marked deleted in tx, which frees the name for the
// user's put after it. A link taken for another name than the spec's, or
// from a machine whose spelling has changed since, is refused 503, as a
// directory that could not answer is.
//
// decision: moving the name marks the machine deleted, since a machine holds
// its one Kerberos spelling and nothing else, which the store keeps it to,
// so a machine without it is no principal. Its marked record is left as a
// deletion's first transaction leaves one, and no operation drops it, since
// no runner step continues a machine's deletion. Start one once a step
// does, or once a machine can hold anything a deletion must drop.
func withKerberos(tx entity.ControlPlaneTx, ids entity.ProtocolIDs, name string, link entity.ProtocolID) (entity.ProtocolIDs, error) {
	old, held := krbOf(ids)
	out := make(entity.ProtocolIDs, 0, len(ids)+1)
	for _, id := range ids {
		if id.Scheme != entity.SchemeKerberos {
			out = append(out, id)
		}
	}
	stale := &refusal{code: apiv1.CodeUnavailable, detail: "the user's Kerberos name changed while its link was asked; retry"}
	switch {
	case name == "":
		return out, nil
	case held && old.Value == name:
		return append(out, old), nil
	case link.Value != name:
		return nil, stale
	}
	h, ok, err := tx.Holder(entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
	if err != nil {
		return nil, err
	}
	if ok && h.Principal.Kind == entity.PrincipalMachine {
		if mk, _ := krbOf(h.IDs); mk != link {
			return nil, stale
		}
		if err := tx.DeleteUser(h.Principal, h.Generation); err != nil {
			return nil, err
		}
	}
	return append(out, link), nil
}
