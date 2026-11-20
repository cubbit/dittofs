package api

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"net/mail"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cubbit/dittofs/internal/node/api/auth"
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
func createUser(ctx context.Context, rq request[userDoc], link krbLink, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
	sp := rq.Body.Spec
	ids, err := withNumber(nil, entity.SchemeUID, sp.UID, "spec.uid")
	if err != nil {
		return apiv1.User{}, rq.ID, err
	}
	if ids, err = withKerberos(tx, rq.start, ids, sp.Kerberos, link); err != nil {
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
func putUser(ctx context.Context, rq request[userDoc], link krbLink, tx entity.ControlPlaneTx) (apiv1.User, [16]byte, error) {
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
	if u.IDs, err = withKerberos(tx, rq.start, u.IDs, sp.Kerberos, link); err != nil {
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

// krbLink is a user write's pre-write step's result: the krb spelling, linked,
// the spec's Kerberos name is stored as, zero for none, and whether it is a
// machine's, which the write takes from it.
type krbLink struct {
	id      entity.ProtocolID
	machine bool
}

// kerberosLink is the pre-write step of a user's write: the Kerberos name the
// spec states, in its stored form (krbName), with the user SID it links for.
// A name the user already holds keeps its link, and one a machine holds is
// moved with the machine's link, each asking nothing; any other is linked
// for the SID the directory holds under it, asked before the transaction. A
// name the directory cannot answer for is refused 503 and one it holds no
// entry for 400 naming it, each before anything is written; with no
// directory, a new name is refused 503. A name another user holds is refused
// 409, unless that user is hidden from the caller, whose name is treated as
// held by no one, so the refusal confirms no hidden account; the store
// refuses the write if it is still held. A machine's name is moved only by a
// caller holding the installation right, and refused 409 to any other.
func kerberosLink(ctx context.Context, rq request[userDoc]) (krbLink, error) {
	if rq.Body.Spec.Kerberos == "" {
		return krbLink{}, nil
	}
	name, err := krbName(rq.Body.Spec.Kerberos)
	if err != nil {
		return krbLink{}, err
	}
	h, held, err := rq.d.Store.Holder(ctx, entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
	switch {
	case err != nil:
		return krbLink{}, err
	case held && h.Principal == userAt(rq.ID):
		// decision: a write restating the name the user holds keeps its
		// stored link and asks the directory nothing, so an unrelated edit
		// of the user never waits on the directory; linking the name to
		// another directory account takes removing it, then writing it
		// again. Overturned if a restated name must follow a directory
		// account that changed under it.
		id, _ := krbOf(h.IDs)
		return krbLink{id: id}, nil
	case held && h.Principal.Kind == entity.PrincipalMachine:
		// decision: only a caller holding the installation right, the
		// join's own, moves a machine's Kerberos name, since the move
		// hands the computer account's stored SID to a principal that may
		// sign in by password, and deletes the machine. Overturned if a
		// machine's move is ever limited to users holding no password.
		if c, _ := auth.CallerFrom(ctx); !holds(c, auth.Installation) {
			return krbLink{}, &componentError{field: "spec.kerberos", detail: "a machine holds " + name + "; only an admin moves it to a user", err: vfserr.ErrExist}
		}
		id, _ := krbOf(h.IDs)
		return krbLink{id: id, machine: true}, nil
	case held && !hiddenFrom(ctx, h):
		return krbLink{}, &componentError{field: "spec.kerberos", detail: "another user holds " + name, err: vfserr.ErrExist}
	case rq.d.Directory == nil:
		return krbLink{}, errNoDirectory
	}
	s, err := rq.d.Directory.KerberosLinkSID(ctx, name)
	switch {
	case errors.Is(err, vfserr.ErrUnknownPrincipal):
		return krbLink{}, &componentError{field: "spec.kerberos", detail: "the directory holds no user " + name, err: err}
	case err != nil:
		return krbLink{}, err
	}
	id := entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name}
	if s.Revision != 0 {
		var b bytes.Buffer
		sid.EncodeSID(&b, &s)
		id.Linked = b.String()
	}
	return krbLink{id: id}, nil
}

// holds reports whether c may hold r.
func holds(c auth.Caller, r auth.Right) bool {
	_, hold := auth.Allowed(c, r)
	return hold
}

// krbName is name, user@REALM, as a krb spelling is stored and identity
// finds it: refused 400 unless it is valid UTF-8 of at most 1024 bytes with
// exactly one @, a user part and a realm, and no space or control
// character; the realm's ASCII letters upper-cased, as a directory's realm
// is spelled; the user part kept byte for byte, as identity compares it.
//
// decision: the realm is upper-cased because an ad directory's realm is
// upper case, which its record's validation enforces. An rfc2307 record may
// name a realm with lower-case letters, and identity matches a name's realm
// against the directory's case and all, so a name under such a realm is
// stored in a spelling that never matches it. Overturned by the first
// rfc2307 deployment that uses Kerberos: keep the realm as given there.
func krbName(name string) (string, error) {
	if len(name) > maxDirectoryName {
		return "", tooLong("spec.kerberos")
	}
	user, realm, ok := strings.Cut(name, "@")
	if !ok || user == "" || realm == "" || strings.Contains(realm, "@") || !utf8.ValidString(name) ||
		strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", &fieldError{field: "spec.kerberos", detail: "a Kerberos name is user@REALM: one @, and no space or control character"}
	}
	return user + "@" + strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, realm), nil
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

// withKerberos is ids with its krb spelling the one the spec names, in its
// stored form: removed when it names none, kept, link and all, when it names
// the one held, and otherwise link's, which the pre-write step took for that
// name. A name a machine holds, read tracked in tx, is taken from it with the
// machine's link: the machine is marked deleted in tx, which frees the name
// for the user's put after it, and a user deletion of the machine's ID is
// started through start, so the write answers 202 naming it and its audit
// record's outcome is the operation's ID. A link taken from a machine that
// no longer holds the name, or whose spelling has changed since, and one
// asked of the directory for a name a machine has taken since, or for
// another name, are refused 503, as a directory that could not answer is.
func withKerberos(tx entity.ControlPlaneTx, start func(apiv1.OpKind, entity.ResourceRef) error, ids entity.ProtocolIDs, name string, link krbLink) (entity.ProtocolIDs, error) {
	old, held := krbOf(ids)
	out := make(entity.ProtocolIDs, 0, len(ids)+1)
	for _, id := range ids {
		if id.Scheme != entity.SchemeKerberos {
			out = append(out, id)
		}
	}
	if name == "" {
		return out, nil
	}
	name, err := krbName(name)
	if err != nil {
		return nil, err
	}
	if held && old.Value == name {
		return append(out, old), nil
	}
	stale := &refusal{code: apiv1.CodeUnavailable, detail: "the user's Kerberos name changed while its link was asked; retry"}
	if link.id.Value != name {
		return nil, stale // the step took no link for this name
	}
	h, ok, err := tx.Holder(entity.ProtocolID{Scheme: entity.SchemeKerberos, Value: name})
	if err != nil {
		return nil, err
	}
	machine := ok && h.Principal.Kind == entity.PrincipalMachine
	if machine != link.machine {
		return nil, stale
	}
	if machine {
		if mk, _ := krbOf(h.IDs); mk != link.id {
			return nil, stale
		}
		if err := tx.DeleteUser(h.Principal, h.Generation); err != nil {
			return nil, err
		}
		if err := start(apiv1.OpUserDelete, entity.ResourceRef{Kind: entity.ResourceUser, ID: h.Principal.ID[:]}); err != nil {
			return nil, err
		}
	}
	return append(out, link.id), nil
}
