package kvstore

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
)

// Minter is identity's write path for the principals a sign-in, lookup or
// resolution mints. It never overwrites a record.
type Minter struct{ s *Store }

func (s *Store) Minter() Minter { return Minter{s} }

// Create writes the batch in one transaction: each record through the
// checks PutUser and PutGroup apply, its IDs drawn from the counters its
// Alloc names, and the principals generation raised once, every record
// taking the raised value as its position. A record that exists, or a name
// or protocol ID another principal holds, fails the whole batch ErrExist,
// and nothing is written.
//
// decision: an Unnamed record is stored with no name at all, not with its
// name and no name row: a later put keeping the name would leave the row
// unclaimed, since a write claims a name only when it changes. A rename gives
// it the name once that is free.
//
// decision: a record naming an API role is refused, since the mint writes no
// enabled-admin index row and a principal it mints holds file access only.
// Withdraw it if a minted principal is ever to hold an API role, by having
// the mint keep that index as PutUser does.
func (m Minter) Create(ctx context.Context, b entity.MintBatch) error {
	return m.s.kv.Update(ctx, func(tx kv.Txn) error {
		c := controlTx{tx, new(uint64)}
		for _, r := range b.Users {
			u := r.User
			if u.Role != entity.RoleNone {
				return merrs.NewInvalidArgumentError("a minted user holds no API role")
			}
			if r.Unnamed {
				u.Name = ""
			}
			if err := absent(tx, userKey(u.Principal.ID), u.Principal); err != nil {
				return err
			}
			if _, err := c.PutUser(u, 0, r.Alloc); err != nil {
				return err
			}
		}
		for _, r := range b.Groups {
			g := r.Group
			if r.Unnamed {
				g.Name = ""
			}
			if err := absent(tx, groupKey(g.Principal.ID), g.Principal); err != nil {
				return err
			}
			if _, err := c.PutGroup(g, 0, r.Alloc); err != nil {
				return err
			}
		}
		return nil
	})
}

// absent refuses a record key that exists, marked deleted or not.
func absent(tx kv.Txn, key []byte, p entity.Principal) error {
	_, ok, err := get(tx, key)
	if err == nil && ok {
		err = merrs.NewAlreadyExistsError(p.Kind.String() + " " + p.ID.String())
	}
	return err
}

// Rename applies every renaming one directory answer causes in one
// transaction, so a swap is one call: every listed record's old name is given
// up before any take is written, so a name a listed principal holds never
// blocks a take. Name "" gives the name up. It writes nothing but each listed
// record's Name, its name row, its Generation and its Position, the raised
// principals generation, which a non-empty list raises once even when it
// changes no name. A refusal writes nothing: ErrExist for a take another
// principal of its kind holds, ErrInvalid for a principal listed twice, two
// takes of one folded name within a kind, a principal that is not a user or
// a group, or a name the name rule refuses, and not found for a listed
// principal with no live record. An empty list opens no transaction.
func (m Minter) Rename(ctx context.Context, rs []entity.Renaming) error {
	if len(rs) == 0 {
		return nil
	}
	listed := map[entity.Principal]bool{}
	takes := map[string]bool{}
	for _, r := range rs {
		if r.P.Kind != entity.PrincipalUser && r.P.Kind != entity.PrincipalGroup {
			return merrs.NewInvalidArgumentError("only a user or a group is renamed")
		}
		if listed[r.P] {
			return merrs.NewInvalidArgumentError(r.P.String() + " is listed twice")
		}
		listed[r.P] = true
		if r.Name == "" {
			continue
		}
		key := string(nameKey(nameKind(r.P.Kind), r.Name))
		if takes[key] {
			return merrs.NewInvalidArgumentError("two takes of the name " + r.Name)
		}
		takes[key] = true
	}
	return m.s.kv.Update(ctx, func(tx kv.Txn) error {
		c := controlTx{tx, new(uint64)}
		users := make([]entity.User, len(rs))
		groups := make([]entity.Group, len(rs))
		old := make([]string, len(rs))
		for i, r := range rs {
			var err error
			if r.P.Kind == entity.PrincipalUser {
				users[i], err = c.liveUser(r.P)
				old[i] = users[i].Name
			} else {
				groups[i], err = c.liveGroup(r.P)
				old[i] = groups[i].Name
			}
			if err != nil {
				return err
			}
			if err := localName(r.Name, true, old[i]); r.Name != "" && err != nil {
				return err
			}
			if old[i] != "" {
				if err := tx.Delete(nameKey(nameKind(r.P.Kind), old[i])); err != nil {
					return err
				}
			}
		}
		pos, err := c.principals()
		if err != nil {
			return err
		}
		for i, r := range rs {
			if r.Name != "" {
				if err := claim(tx, nameKey(nameKind(r.P.Kind), r.Name), "name "+r.Name, r.P, encodeNameRow(r.P.ID, r.Name), decodeIDAs(r.P.Kind)); err != nil {
					return err
				}
			}
			if r.P.Kind == entity.PrincipalUser {
				u := users[i]
				u.Name, u.Generation, u.Position = r.Name, nextVersion(tx, u.Generation), pos
				err = tx.Set(userKey(r.P.ID), encodeUser(u))
			} else {
				g := groups[i]
				g.Name, g.Generation, g.Position = r.Name, nextVersion(tx, g.Generation), pos
				err = tx.Set(groupKey(r.P.ID), encodeGroup(g))
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// SetEmail writes a directory user's e-mail address, and nothing of its
// record but its Generation and Position, raising the principals generation
// once. A user with no live record is not found, and a machine or a group,
// which have no address, are refused ErrInvalid.
func (m Minter) SetEmail(ctx context.Context, p entity.Principal, email string) error {
	if p.Kind != entity.PrincipalUser {
		return merrs.NewInvalidArgumentError("only a user has an e-mail address")
	}
	return m.s.kv.Update(ctx, func(tx kv.Txn) error {
		c := controlTx{tx, new(uint64)}
		u, err := c.liveUser(p)
		if err != nil {
			return err
		}
		u.Email, u.Generation = email, nextVersion(tx, u.Generation)
		if u.Position, err = c.principals(); err != nil {
			return err
		}
		return tx.Set(userKey(p.ID), encodeUser(u))
	})
}
