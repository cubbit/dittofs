package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// idRange is the installation's ID range, read tracked in tx, so a create
// racing a change of the range retries under the new one.
//
// ponytail: the setting row is decoded here as JSON rather than through
// the settings table, which the node does not hand the API yet. Read the
// range through the table once it does.
func idRange(tx entity.ControlPlaneTx) (entity.IDRange, error) {
	s, ok, err := tx.Setting(entity.SettingScope{Kind: entity.ScopeInstallation}, entity.IDRangeSetting)
	if err != nil || !ok {
		return entity.DefaultIDRange, err
	}
	var r entity.IDRange
	return r, json.Unmarshal(s.Value, &r)
}

// nameField is err with the store's refusal of a user's or group's name
// answered as a *fieldError naming field, since the store's own refusal
// names no field. The store holds the name rule, so every path that writes
// a name, not the API's alone, is refused it.
func nameField(field string, err error) error {
	var se *merrs.StoreError
	if errors.As(err, &se) && errors.Is(se.Cause, entity.ErrLocalName) {
		return &fieldError{field: field, detail: se.Cause.Error()}
	}
	return err
}

// spelling is the value of ids' spelling in scheme, and false when it has
// none.
func spelling(ids entity.ProtocolIDs, scheme entity.ProtocolScheme) (string, bool) {
	i := slices.IndexFunc(ids, func(id entity.ProtocolID) bool { return id.Scheme == scheme })
	if i < 0 {
		return "", false
	}
	return ids[i].Value, true
}

// numberOf is the uid or gid ids holds in scheme, nil for none.
func numberOf(ids entity.ProtocolIDs, scheme entity.ProtocolScheme) *uint32 {
	v, ok := spelling(ids, scheme)
	if !ok {
		return nil
	}
	n, _ := strconv.ParseUint(v, 10, 32)
	u := uint32(n)
	return &u
}

// withNumber is ids with its spelling in scheme set to n, removed for nil,
// every other spelling kept as it is. A spec naming 0 is refused naming
// field: 0 is root's and no principal's spelling.
func withNumber(ids entity.ProtocolIDs, scheme entity.ProtocolScheme, n *uint32, field string) (entity.ProtocolIDs, error) {
	if n != nil && *n == 0 {
		return nil, &fieldError{field: field, detail: "0 is root's, held by rule; leave it out to allocate one"}
	}
	out := slices.DeleteFunc(slices.Clone(ids), func(id entity.ProtocolID) bool { return id.Scheme == scheme })
	if n != nil {
		out = append(out, entity.ProtocolID{Scheme: scheme, Value: strconv.FormatUint(uint64(*n), 10)})
	}
	return out, nil
}

// principalSub is the principal ID a path's {sub} names, 404 for one that
// does not parse.
func principalSub(sub string) (entity.PrincipalID, error) {
	p, err := entity.ParsePrincipalID(sub)
	if err != nil {
		return p, errNotFound
	}
	return p, nil
}

// pairRef is the target of a row whose {sub} is a principal below {id}: a
// grant, a quota or a membership, named by {id}'s ID, then the principal's.
func pairRef(id [16]byte, sub string) ([]byte, error) {
	p, err := principalSub(sub)
	if err != nil {
		return nil, err
	}
	return append(id[:], p[:]...), nil
}

// principalIn is the principal of ID id, as a user, a machine, a group or a
// share's default, read tracked in tx; false when it names none of them.
func principalIn(tx entity.ControlPlaneTx, id entity.PrincipalID) (entity.Principal, bool, error) {
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

// tagsOf is w for a write that carries no body: a delete, whose record,
// tag and hidden records w reads.
func tagsOf[B any](w writing[B]) writing[none] {
	return writing[none]{kind: w.kind, raises: w.raises, hidden: w.hidden, read: func(tx entity.ControlPlaneTx, id [16]byte, sub string) (none, uint64, bool, error) {
		_, tag, ok, err := w.read(tx, id, sub)
		return none{}, tag, ok, err
	}}
}

// hiddenFrom reports whether the caller ctx carries may not know of u: a
// user holding an API role, to a caller that is not admin, as accounts with
// a role are admin's alone to read and write.
//
// decision: an account with a role is hidden by the role alone: a read
// answers 404 and a listing leaves it out, and a write naming it answers 404
// ahead of its precondition, as one naming no user does, so no answer tells
// a non-admin which IDs are accounts. Epic 5's per-call account check, with
// the rule on raising a role, replaces it once accounts and sign-in land.
func hiddenFrom(ctx context.Context, u entity.User) bool {
	c, _ := auth.CallerFrom(ctx)
	return u.Role != entity.RoleNone && c.Role < entity.RoleAdmin
}

// lastAdmin refuses, 409 last_admin, a write that leaves no enabled admin,
// counted tracked in tx after the write, so two writes each removing one of
// the last two conflict.
func lastAdmin(tx entity.ControlPlaneTx, was entity.User) error {
	if was.Role != entity.RoleAdmin || was.Disabled {
		return nil
	}
	n, err := tx.EnabledAdmins()
	if err == nil && n == 0 {
		err = &refusal{code: apiv1.CodeLastAdmin, detail: "the installation's last enabled admin stays"}
	}
	return err
}

// shown is u as the caller ctx carries may read it: its address left out
// for a viewer reading another user, as only operator, admin and the user
// itself read it.
func shown(ctx context.Context, u entity.User) entity.User {
	if c, _ := auth.CallerFrom(ctx); c.Role < entity.RoleOperator && c.Principal != u.Principal {
		u.Email = ""
	}
	return u
}

// after16 is a principal ID cursor as the listing decoded it, the zero ID
// for none.
func after16(after []byte) (id [16]byte) {
	copy(id[:], after)
	return id
}
