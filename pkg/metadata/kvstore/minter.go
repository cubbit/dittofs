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
