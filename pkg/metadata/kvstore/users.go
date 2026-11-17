package kvstore

import (
	"context"
	"iter"
	"slices"
	"strconv"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// ControlPlane is the management API's view. Every write names what it
// changes: a user rename writes the user record and its name rows, and
// nothing under any file's prefix.
type ControlPlane struct{ s *Store }

func (s *Store) ControlPlane() ControlPlane { return ControlPlane{s} }

// Users lists users in name order, resuming after the name after.
func (c ControlPlane) Users(ctx context.Context, after string) iter.Seq2[entity.User, error] {
	return pages(ctx, c.s.kv, []byte{keys.KindNX, keys.NXUser}, nameCursor(keys.NXUser, after),
		func(r kv.Reader, row kv.KeyValue) (entity.User, error) {
			id, b, err := byIndex(r, row, userKey)
			if err != nil {
				return entity.User{}, err
			}
			return decodeUser(id, b)
		})
}

// Groups lists groups in name order, resuming after the name after.
func (c ControlPlane) Groups(ctx context.Context, after string) iter.Seq2[entity.Group, error] {
	return pages(ctx, c.s.kv, []byte{keys.KindNX, keys.NXGroup}, nameCursor(keys.NXGroup, after),
		func(r kv.Reader, row kv.KeyValue) (entity.Group, error) {
			id, b, err := byIndex(r, row, groupKey)
			if err != nil {
				return entity.Group{}, err
			}
			return decodeGroup(id, b)
		})
}

func nameCursor(kind byte, after string) []byte {
	if after == "" {
		return nil
	}
	return nameKey(kind, after)
}

// byIndex follows a name index row to the record it names.
func byIndex(r kv.Reader, row kv.KeyValue, key func(entity.PrincipalID) []byte) (entity.PrincipalID, []byte, error) {
	id, err := decodeID(row.Value)
	if err != nil {
		return id, nil, err
	}
	b, err := r.Get(key(id))
	return id, b, err
}

// fixedID reports whether id has the form of a well-known principal's,
// fifteen zero bytes then any byte, the zero ID among them. No user or group
// takes one: a grant row keys on the ID alone, so a group holding everyone's
// ID would hold everyone's grant.
func fixedID(id entity.PrincipalID) bool { return [15]byte(id[:15]) == [15]byte{} }

// PutUser creates a user, expect zero, or changes the one whose generation
// expect names, raising its generation. A user marked deleted is not found,
// and a put never marks one: the mark is DeleteUser's alone.
func (c ControlPlane) PutUser(ctx context.Context, u entity.User, expect uint64, a entity.Alloc) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutUser(u, expect, a) })
}

func (c controlTx) PutUser(u entity.User, expect uint64, a entity.Alloc) (uint64, error) {
	if u.Principal.Kind != entity.PrincipalUser || fixedID(u.Principal.ID) || u.Name == "" {
		return 0, merrs.NewInvalidArgumentError("a user needs a minted user principal and a name")
	}
	if !u.Primary.IsZero() && u.Primary.Kind != entity.PrincipalGroup {
		return 0, merrs.NewInvalidArgumentError("a user's primary group must be a group")
	}
	if u.Role.String() == "" {
		return 0, merrs.NewInvalidArgumentError("unknown API role")
	}
	if err := validIDs(u.IDs, entity.SchemeUID, entity.SchemeSID, entity.SchemeKerberos); err != nil {
		return 0, err
	}
	if err := noAlloc(a); err != nil {
		return 0, err
	}
	key := userKey(u.Principal.ID)
	b, existed, err := get(c.tx, key)
	if err != nil {
		return 0, err
	}
	var old entity.User
	if existed {
		if old, err = decodeUser(u.Principal.ID, b); err != nil {
			return 0, err
		}
		if old.Deleted {
			return 0, notFound(nil, u.Principal)
		}
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	if err := reindex(c.tx, u.Principal, existed, old.Name, u.Name, old.IDs, u.IDs); err != nil {
		return 0, err
	}
	if err := indexAdmin(c.tx, u.Principal.ID, enabledAdmin(old), enabledAdmin(u)); err != nil {
		return 0, err
	}
	u.Deleted, u.Generation = false, nextVersion(c.tx, old.Generation)
	if u.Position, err = c.principals(); err != nil {
		return 0, err
	}
	return u.Generation, c.tx.Set(key, encodeUser(u))
}

// PutGroup creates a group, expect zero, or changes the one whose generation
// expect names, raising its generation. A group marked deleted is not found,
// and a put never marks one: the mark is DeleteGroup's alone.
func (c ControlPlane) PutGroup(ctx context.Context, g entity.Group, expect uint64, a entity.Alloc) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutGroup(g, expect, a) })
}

func (c controlTx) PutGroup(g entity.Group, expect uint64, a entity.Alloc) (uint64, error) {
	if g.Principal.Kind != entity.PrincipalGroup || fixedID(g.Principal.ID) || g.Name == "" {
		return 0, merrs.NewInvalidArgumentError("a group needs a minted group principal and a name")
	}
	if err := validIDs(g.IDs, entity.SchemeGID, entity.SchemeSID); err != nil {
		return 0, err
	}
	if err := noAlloc(a); err != nil {
		return 0, err
	}
	key := groupKey(g.Principal.ID)
	b, existed, err := get(c.tx, key)
	if err != nil {
		return 0, err
	}
	var old entity.Group
	if existed {
		if old, err = decodeGroup(g.Principal.ID, b); err != nil {
			return 0, err
		}
		if old.Deleted {
			return 0, notFound(nil, g.Principal)
		}
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	if err := reindex(c.tx, g.Principal, existed, old.Name, g.Name, old.IDs, g.IDs); err != nil {
		return 0, err
	}
	g.Deleted, g.Generation = false, nextVersion(c.tx, old.Generation)
	if g.Position, err = c.principals(); err != nil {
		return 0, err
	}
	return g.Generation, c.tx.Set(key, encodeGroup(g))
}

func enabledAdmin(u entity.User) bool { return u.Role == entity.RoleAdmin && !u.Disabled }

// indexAdmin keeps the enabled-admin index, one empty row per enabled admin,
// writing it only when the user moves into or out of the set: every write
// under the index conflicts with a concurrent count of it.
func indexAdmin(tx kv.Txn, id entity.PrincipalID, was, is bool) error {
	switch {
	case is && !was:
		return tx.Set(adminKey(id), nil)
	case was && !is:
		return tx.Delete(adminKey(id))
	}
	return nil
}

func adminKey(id entity.PrincipalID) []byte { return keys.ID([]byte{keys.KindUA}, id[:]) }

// noAlloc refuses an Alloc naming any ID.
//
// ponytail: the store keeps no uid, gid or RID counter yet, so a write can
// allocate nothing and takes every ID as the record holds it. Draw the named
// IDs from those counters in the write's transaction once they exist.
func noAlloc(a entity.Alloc) error {
	if a != (entity.Alloc{}) {
		return merrs.NewInvalidArgumentError("the store allocates no uid, gid or RID yet")
	}
	return nil
}

// expected refuses a write whose caller read another generation or version
// than the stored one, zero standing for no record. The stored one is read
// with conflict tracking, so of two writes from one read, the second to
// commit conflicts and then finds it moved. An expect value is opaque and
// belongs to one record kind: a caller passes back what it read from that
// record, never a value it computed or read from another kind.
func expected(stored, expect uint64) error {
	if stored != expect {
		return entity.ErrGenerationMismatch
	}
	return nil
}

// validIDs refuses a scheme the principal's kind may not carry, an empty
// spelling, a UID or GID not in canonical decimal, and a repeated ID: each
// spelling has one index row, so it must have one form.
func validIDs(ids entity.ProtocolIDs, schemes ...entity.ProtocolScheme) error {
	for i, id := range ids {
		if !slices.Contains(schemes, id.Scheme) || id.Value == "" {
			return merrs.NewInvalidArgumentError("protocol ID " + id.Scheme.String() + " not valid here")
		}
		if id.Scheme == entity.SchemeUID || id.Scheme == entity.SchemeGID {
			n, err := strconv.ParseUint(id.Value, 10, 32)
			if err != nil || strconv.FormatUint(n, 10) != id.Value {
				return merrs.NewInvalidArgumentError(id.Scheme.String() + " " + id.Value + " is not a canonical decimal")
			}
		}
		if slices.Contains(ids[:i], id) {
			return merrs.NewInvalidArgumentError("protocol ID repeated")
		}
	}
	return nil
}

// reindex moves p's name and protocol-ID rows from the old record's to the
// new one's, refusing a name or ID another principal holds. Each row is read
// before it is claimed, so of two principals racing for one row, the second
// to commit conflicts and then finds it taken. A row p already holds is left
// unwritten.
func reindex(tx kv.Txn, p entity.Principal, existed bool, oldName, newName string, oldIDs, newIDs entity.ProtocolIDs) error {
	kind := nameKind(p.Kind)
	if !existed || oldName != newName {
		if err := claim(tx, nameKey(kind, newName), "name "+newName, p, encodeID(p.ID), decodeIDAs(p.Kind)); err != nil {
			return err
		}
		if existed {
			if err := tx.Delete(nameKey(kind, oldName)); err != nil {
				return err
			}
		}
	}
	for _, id := range newIDs {
		if !slices.Contains(oldIDs, id) {
			if err := claim(tx, protocolKey(id), id.Scheme.String()+" "+id.Value, p, encodePrincipal(p), decodePrincipal); err != nil {
				return err
			}
		}
	}
	for _, id := range oldIDs {
		if !slices.Contains(newIDs, id) {
			if err := tx.Delete(protocolKey(id)); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeIDAs(kind entity.PrincipalKind) func([]byte) (entity.Principal, error) {
	return func(b []byte) (entity.Principal, error) {
		id, err := decodeID(b)
		return entity.Principal{Kind: kind, ID: id}, err
	}
}

// claim writes an index row for p, refusing one that names another
// principal; what names the row in the refusal.
func claim(tx kv.Txn, key []byte, what string, p entity.Principal, value []byte, decode func([]byte) (entity.Principal, error)) error {
	b, ok, err := get(tx, key)
	if err != nil {
		return err
	}
	if ok {
		holder, err := decode(b)
		if err != nil {
			return err
		}
		if holder != p {
			return merrs.NewAlreadyExistsError(what)
		}
		return nil
	}
	return tx.Set(key, value)
}

// DeleteUser starts the deletion of the user whose generation expect names,
// and returns once the user is marked: found by no name or protocol ID,
// admitted nowhere, and no longer an enabled admin. Nodes.ContinueDelete
// drops the rest in batches.
func (c ControlPlane) DeleteUser(ctx context.Context, p entity.Principal, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteUser(p, expect) })
}

// decision: the mark deletes the user's name index, so a sign-in naming a
// marked user finds no account and fails as an unknown name does, not as a
// disabled account. The specification requires the index deletion at the
// mark, freeing the name at once; telling a deleted account from a wrong
// name would take a change to that specification, not to this method.
func (c controlTx) DeleteUser(p entity.Principal, expect uint64) error {
	if p.Kind != entity.PrincipalUser {
		return merrs.NewInvalidArgumentError("not a user")
	}
	u, err := c.liveUser(p)
	if err != nil {
		return err
	}
	if err := expected(u.Generation, expect); err != nil {
		return err
	}
	if err := indexAdmin(c.tx, p.ID, enabledAdmin(u), false); err != nil {
		return err
	}
	if err := unindex(c.tx, p, u.Name, u.IDs); err != nil {
		return err
	}
	u.Deleted, u.Generation = true, nextVersion(c.tx, u.Generation)
	if u.Position, err = c.principals(); err != nil {
		return err
	}
	return c.tx.Set(userKey(p.ID), encodeUser(u))
}

// DeleteGroup starts the deletion of the group whose generation expect
// names, and returns once the group is marked: found by no name or protocol
// ID, and its memberships grant nothing. Nodes.ContinueDelete drops the rest
// in batches.
func (c ControlPlane) DeleteGroup(ctx context.Context, p entity.Principal, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteGroup(p, expect) })
}

func (c controlTx) DeleteGroup(p entity.Principal, expect uint64) error {
	if p.Kind != entity.PrincipalGroup {
		return merrs.NewInvalidArgumentError("not a group")
	}
	g, err := c.liveGroup(p)
	if err != nil {
		return err
	}
	if err := expected(g.Generation, expect); err != nil {
		return err
	}
	if err := unindex(c.tx, p, g.Name, g.IDs); err != nil {
		return err
	}
	g.Deleted, g.Generation = true, nextVersion(c.tx, g.Generation)
	if g.Position, err = c.principals(); err != nil {
		return err
	}
	return c.tx.Set(groupKey(p.ID), encodeGroup(g))
}

// liveUser reads p's user record tracked, refusing one absent or marked
// deleted as not found.
func (c controlTx) liveUser(p entity.Principal) (entity.User, error) {
	u, ok, err := userIn(c.tx, p.ID)
	if err != nil || !ok {
		return entity.User{}, notFound(err, p)
	}
	return u, nil
}

// userIn reads id's user record, tracked in a transaction. A user marked
// deleted reads as absent.
func userIn(r kv.Reader, id entity.PrincipalID) (entity.User, bool, error) {
	b, ok, err := get(r, userKey(id))
	if err != nil || !ok {
		return entity.User{}, false, err
	}
	u, err := decodeUser(id, b)
	if err != nil || u.Deleted {
		return entity.User{}, false, err
	}
	return u, true, nil
}

// liveGroup is liveUser for a group.
func (c controlTx) liveGroup(p entity.Principal) (entity.Group, error) {
	b, ok, err := get(c.tx, groupKey(p.ID))
	if err != nil || !ok {
		return entity.Group{}, notFound(err, p)
	}
	g, err := decodeGroup(p.ID, b)
	if err == nil && g.Deleted {
		err = notFound(nil, p)
	}
	return g, err
}

// live reports whether p's user or group record exists and is not marked
// deleted. A transaction's read is tracked, so a mark committed meanwhile
// conflicts with the caller.
func live(r kv.Reader, p entity.Principal) (bool, error) {
	b, ok, err := get(r, principalKey(p))
	if err != nil || !ok {
		return false, err
	}
	if p.Kind == entity.PrincipalGroup {
		g, err := decodeGroup(p.ID, b)
		return !g.Deleted, err
	}
	u, err := decodeUser(p.ID, b)
	return !u.Deleted, err
}

func notFound(err error, p entity.Principal) error {
	if err != nil {
		return err
	}
	return merrs.NewNotFoundError(p.String(), "principal")
}

// unindex deletes p's name and protocol-ID rows.
func unindex(tx kv.Txn, p entity.Principal, name string, ids entity.ProtocolIDs) error {
	if err := tx.Delete(nameKey(nameKind(p.Kind), name)); err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.Delete(protocolKey(id)); err != nil {
			return err
		}
	}
	return nil
}

// AddMember adds a user or a nested group to a group. Both must exist,
// unmarked; the reads that check so conflict with a deletion of either.
func (c ControlPlane) AddMember(ctx context.Context, m entity.Membership) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.AddMember(m) })
}

func (c controlTx) AddMember(m entity.Membership) error {
	if m.Group.Kind != entity.PrincipalGroup || (m.Member.Kind != entity.PrincipalUser && m.Member.Kind != entity.PrincipalGroup) || m.Group == m.Member {
		return merrs.NewInvalidArgumentError("a membership is a user or another group in a group")
	}
	for _, p := range []entity.Principal{m.Group, m.Member} {
		if ok, err := live(c.tx, p); err != nil || !ok {
			return notFound(err, p)
		}
	}
	if _, err := c.principals(); err != nil {
		return err
	}
	kind := encodeKind(m.Member.Kind)
	if err := c.tx.Set(memberKey(m.Group.ID, m.Member.ID), kind); err != nil {
		return err
	}
	return c.tx.Set(memberRevKey(m.Member.ID, m.Group.ID), kind)
}

// RemoveMember removes both rows of a membership; removing one that does not
// exist is not an error.
func (c ControlPlane) RemoveMember(ctx context.Context, m entity.Membership) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.RemoveMember(m) })
}

func (c controlTx) RemoveMember(m entity.Membership) error {
	if _, err := c.principals(); err != nil {
		return err
	}
	if err := c.tx.Delete(memberKey(m.Group.ID, m.Member.ID)); err != nil {
		return err
	}
	return c.tx.Delete(memberRevKey(m.Member.ID, m.Group.ID))
}
