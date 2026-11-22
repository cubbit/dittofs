package kvstore

import (
	"bytes"
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

// RegisteredStatuses is every registered node's status, in node ID order,
// read as the node view reads it: a node that has reported none reads as the
// zero status, and a status whose node has no record is left out.
func (c ControlPlane) RegisteredStatuses(ctx context.Context) ([]entity.NodeStatus, error) {
	return c.s.Nodes().RegisteredStatuses(ctx)
}

// Installation is the installation's identity record, as the store's own
// read answers it.
func (c ControlPlane) Installation(ctx context.Context) (entity.Installation, error) {
	return c.s.Installation(ctx)
}

// Holder is the user or machine whose record holds the spelling id; false
// when none does or a group does.
func (c ControlPlane) Holder(ctx context.Context, id entity.ProtocolID) (u entity.User, ok bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		u, ok, err = holderIn(r, id)
		return err
	})
	return u, ok, err
}

func (c controlTx) Holder(id entity.ProtocolID) (entity.User, bool, error) { return holderIn(c.tx, id) }

// holderIn reads the spelling id's row and the user or machine record it
// names.
func holderIn(r kv.Reader, id entity.ProtocolID) (entity.User, bool, error) {
	b, ok, err := get(r, protocolKey(id))
	if err != nil || !ok {
		return entity.User{}, false, err
	}
	p, _, err := decodeProtocolRow(id.Scheme, b)
	if err != nil || p.Kind == entity.PrincipalGroup {
		return entity.User{}, false, err
	}
	return userIn(r, p)
}

// Users lists users in name order, resuming after the name after. It walks the
// name index, so a user with no name, a machine among them, is not listed:
// such a record is read by its ID or a protocol ID.
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

// Groups lists groups in name order, resuming after the name after. It walks
// the name index, so a group with no name is not listed.
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

// User is the user or machine record of p, as a write's transaction reads
// it; false for one marked deleted, or of another kind than p's.
func (c ControlPlane) User(ctx context.Context, p entity.Principal) (u entity.User, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		if u, found, err = userIn(r, p); err != nil || !found {
			return err
		}
		u.Primary, err = primaryIn(r, u)
		return err
	})
	return u, found, err
}

// Group is the group record of p; false for one marked deleted, or a
// principal of another kind.
func (c ControlPlane) Group(ctx context.Context, p entity.Principal) (g entity.Group, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		g, found, err = groupIn(r, p)
		return err
	})
	return g, found, err
}

// UserNamed is the user the name names, its case folded as sign-in folds
// it; false when none holds it.
func (c ControlPlane) UserNamed(ctx context.Context, name string) (u entity.User, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, nameKey(keys.NXUser, name))
		if err != nil || !ok {
			return err
		}
		id, err := decodeNameRow(b)
		if err != nil {
			return err
		}
		if u, found, err = userIn(r, entity.Principal{Kind: entity.PrincipalUser, ID: id}); err != nil || !found {
			return err
		}
		u.Primary, err = primaryIn(r, u)
		return err
	})
	return u, found, err
}

// GroupNamed is the group the name names; false when none holds it.
func (c ControlPlane) GroupNamed(ctx context.Context, name string) (g entity.Group, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, nameKey(keys.NXGroup, name))
		if err != nil || !ok {
			return err
		}
		id, err := decodeNameRow(b)
		if err != nil {
			return err
		}
		g, found, err = groupIn(r, entity.Principal{Kind: entity.PrincipalGroup, ID: id})
		return err
	})
	return g, found, err
}

// groupIn reads p's group record, tracked in a transaction. A record marked
// deleted, or a principal of another kind, reads as absent.
func groupIn(r kv.Reader, p entity.Principal) (entity.Group, bool, error) {
	if p.Kind != entity.PrincipalGroup {
		return entity.Group{}, false, nil
	}
	b, ok, err := get(r, groupKey(p.ID))
	if err != nil || !ok {
		return entity.Group{}, false, err
	}
	g, err := decodeGroup(p.ID, b)
	if err != nil || g.Deleted {
		return entity.Group{}, false, err
	}
	return g, true, nil
}

func nameCursor(kind byte, after string) []byte {
	if after == "" {
		return nil
	}
	return nameKey(kind, after)
}

// byIndex follows a name index row to the record it names.
func byIndex(r kv.Reader, row kv.KeyValue, key func(entity.PrincipalID) []byte) (entity.PrincipalID, []byte, error) {
	id, err := decodeNameRow(row.Value)
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
// and a put never marks one: the mark is DeleteUser's alone. A reference to
// a secret sealed for another holder or as another kind is refused, as
// ownSecrets has it.
func (c ControlPlane) PutUser(ctx context.Context, u entity.User, expect uint64, a entity.Alloc) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutUser(u, expect, a) })
}

func (c controlTx) PutUser(u entity.User, expect uint64, a entity.Alloc) (uint64, error) {
	if err := userShape(u); err != nil {
		return 0, err
	}
	if !u.Primary.IsZero() && u.Primary.Kind != entity.PrincipalGroup {
		return 0, merrs.NewInvalidArgumentError("a user's primary group must be a group")
	}
	if u.Role.String() == "" {
		return 0, merrs.NewInvalidArgumentError("unknown API role")
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
		if old.Deleted || old.Principal.Kind != u.Principal.Kind {
			return 0, notFound(nil, u.Principal)
		}
	}
	if err := localName(u.Name, existed, old.Name); err != nil {
		return 0, err
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	if err := c.ownSecrets(u); err != nil {
		return 0, err
	}
	if u.IDs, err = c.allocate(u.Principal.Kind, u.IDs, existed, a); err != nil {
		return 0, err
	}
	if err := fileSpellings(u.Principal.Kind, u.IDs); err != nil {
		return 0, err
	}
	if err := keepSID(old.IDs, u.IDs); err != nil {
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
	if g.Principal.Kind != entity.PrincipalGroup || fixedID(g.Principal.ID) {
		return 0, merrs.NewInvalidArgumentError("a group needs a minted group principal")
	}
	if err := validIDs(g.IDs, entity.SchemeGID, entity.SchemeSID, entity.SchemeDir); err != nil {
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
	if err := localName(g.Name, existed, old.Name); err != nil {
		return 0, err
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	if g.IDs, err = c.allocate(g.Principal.Kind, g.IDs, existed, a); err != nil {
		return 0, err
	}
	if err := fileSpellings(g.Principal.Kind, g.IDs); err != nil {
		return 0, err
	}
	if err := keepSID(old.IDs, g.IDs); err != nil {
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

// userShape refuses a user record of a shape no principal has: a
// well-known or unminted principal, a password hash and NT hash naming one
// secret, whose kind fits only one of the two fields once sealed, so every
// deletion of the user would refuse it, and a machine with anything but its
// one Kerberos spelling. A user or group may have no name, as one minted from a
// directory whose name another principal holds: no name row finds it.
//
// decision: the shape is checked on every write, not only where a record is
// first made, so no path stores a well-known principal or a machine holding
// a name, a file spelling or a credential. Withdraw a part of it only if the
// principal kinds themselves change.
func userShape(u entity.User) error {
	if fixedID(u.Principal.ID) {
		return merrs.NewInvalidArgumentError("a user needs a minted principal")
	}
	if u.Password == u.NTHash && u.Password != (entity.SecretRef{}) {
		return merrs.NewInvalidArgumentError("a user's password hash and NT hash are two secrets")
	}
	switch u.Principal.Kind {
	case entity.PrincipalUser:
		return validIDs(u.IDs, entity.SchemeUID, entity.SchemeSID, entity.SchemeKerberos, entity.SchemeDir)
	case entity.PrincipalMachine:
		if u.Name != "" || len(u.IDs) != 1 || !u.Primary.IsZero() || u.Password != (entity.SecretRef{}) || u.NTHash != (entity.SecretRef{}) || u.Role != entity.RoleNone {
			return merrs.NewInvalidArgumentError("a machine holds its Kerberos name only: no name, file spelling, primary group, credential or API role")
		}
		return validIDs(u.IDs, entity.SchemeKerberos)
	}
	return merrs.NewInvalidArgumentError("a user record is a user or a machine")
}

// ownSecrets refuses u when a secret its record names is held, at its
// current version, sealed for another holder or as another kind than the
// field's. PutUser runs it on every write, DeleteUser before the mark, and a
// user's deletion again in the batch that deletes the record, which takes
// every version of each secret the record names, so none reaches another
// holder's. The current version is read tracked, guarding every version,
// so a concurrent seal conflicts.
//
// decision: only the current version is checked, since PutSecret refuses a
// version whose kind or owner differs from the current one's, so every
// version carries the current one's. A reference no secret holds yet is
// accepted, since a user's reference is named before its first version is
// sealed; a first version then sealed under it for another holder is
// refused by the deletion instead. A user naming one already is refused at
// DeleteUser with nothing touched; one sealed after the mark is refused in
// the record's batch, which fails the deletion closed: the batch is refused
// as an invalid argument, the deletion's operation fails, and the record
// stays marked with every version of its secrets, its grants, memberships
// and tokens already dropped, rather than taking another holder's secret or
// leaving the user's own password-equivalent behind. Nothing resumes such a
// deletion yet. Withdraw the acceptance if every caller comes to seal before
// it names.
func (c controlTx) ownSecrets(u entity.User) error {
	for _, f := range []struct {
		ref  entity.SecretRef
		kind entity.SecretKind
		key  string
	}{
		{u.Password, entity.SecretPassword, entity.UserPasswordKey},
		{u.NTHash, entity.SecretNTHash, entity.UserNTHashKey},
	} {
		if f.ref == (entity.SecretRef{}) {
			continue
		}
		s, ok, err := c.Secret(f.ref, 0)
		if err != nil {
			return err
		}
		if ok && (s.Kind != f.kind || !s.Owner.Equal(entity.UserSecretOwner(u.Principal.ID, f.key))) {
			return merrs.NewInvalidArgumentError("a user's " + f.key + " names a secret sealed for another holder")
		}
	}
	return nil
}

// localName refuses, as an invalid argument wrapping entity.ErrLocalName, a
// created or renamed user or group name entity.CheckPrincipalName refuses.
// Every write of a name, the API's, a mint's and the server's own, reaches
// it here. A stored name is kept as it is, so a record written before the
// rule stays writable on its other fields.
func localName(name string, existed bool, oldName string) error {
	if existed && name == oldName {
		return nil
	}
	if err := entity.CheckPrincipalName(name); err != nil {
		return &merrs.StoreError{Code: merrs.ErrInvalidArgument, Message: err.Error() + ": " + strconv.Quote(name), Cause: err}
	}
	return nil
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
// spelling, a UID or GID not in canonical decimal or reserved to root (0) or
// anonymous (65534), a linked SID on any spelling but krb or one that is
// not a domain account's, and a second ID of one scheme: a principal has at most one
// spelling per scheme, so fileSpellings' check that a scheme is held and
// keepSID's that the old SID is kept each pin the one spelling.
func validIDs(ids entity.ProtocolIDs, schemes ...entity.ProtocolScheme) error {
	for i, id := range ids {
		if !slices.Contains(schemes, id.Scheme) || id.Value == "" {
			return merrs.NewInvalidArgumentError("protocol ID " + id.Scheme.String() + " not valid here")
		}
		if id.Linked != "" && (id.Scheme != entity.SchemeKerberos || !linkedSID(id.Linked)) {
			return merrs.NewInvalidArgumentError("protocol ID " + id.Scheme.String() + ": a linked SID is a domain account SID on a krb spelling")
		}
		if id.Scheme == entity.SchemeUID || id.Scheme == entity.SchemeGID {
			n, err := strconv.ParseUint(id.Value, 10, 32)
			if err != nil || strconv.FormatUint(n, 10) != id.Value {
				return merrs.NewInvalidArgumentError(id.Scheme.String() + " " + id.Value + " is not a canonical decimal")
			}
			if n == 0 || n == 65534 {
				return merrs.NewInvalidArgumentError(id.Scheme.String() + " " + id.Value + " is root's or anonymous's, held by rule")
			}
		}
		if slices.ContainsFunc(ids[:i], func(o entity.ProtocolID) bool { return o.Scheme == id.Scheme }) {
			return merrs.NewInvalidArgumentError("one " + id.Scheme.String() + " per principal")
		}
	}
	return nil
}

// reindex moves p's name and protocol-ID rows from the old record's to the
// new one's, refusing a name or ID another principal holds. Each row is read
// before it is claimed, so of two principals racing for one row, the second
// to commit conflicts and then finds it taken. A row p already holds is left
// unwritten, but for a krb row whose linked SID changed, rewritten in place.
// An empty name has no row.
func reindex(tx kv.Txn, p entity.Principal, existed bool, oldName, newName string, oldIDs, newIDs entity.ProtocolIDs) error {
	kind := nameKind(p.Kind)
	switch {
	case existed && oldName == newName:
	case existed && newName != "" && bytes.Equal(nameKey(kind, oldName), nameKey(kind, newName)):
		// a change of case only: the row stays, holding the new spelling
		if err := tx.Set(nameKey(kind, newName), encodeNameRow(p.ID, newName)); err != nil {
			return err
		}
	default:
		if newName != "" {
			if err := claim(tx, nameKey(kind, newName), "name "+newName, p, encodeNameRow(p.ID, newName), decodeIDAs(p.Kind)); err != nil {
				return err
			}
		}
		if existed && oldName != "" {
			if err := tx.Delete(nameKey(kind, oldName)); err != nil {
				return err
			}
		}
	}
	for _, id := range newIDs {
		i := slices.IndexFunc(oldIDs, sameRow(id))
		switch {
		case i < 0:
			decode := func(b []byte) (entity.Principal, error) {
				holder, _, err := decodeProtocolRow(id.Scheme, b)
				return holder, err
			}
			if err := claim(tx, protocolKey(id), id.Scheme.String()+" "+id.Value, p, encodeProtocolRow(p, id), decode); err != nil {
				return err
			}
		case oldIDs[i].Linked != id.Linked:
			if err := tx.Set(protocolKey(id), encodeProtocolRow(p, id)); err != nil {
				return err
			}
		}
	}
	for _, id := range oldIDs {
		if !slices.ContainsFunc(newIDs, sameRow(id)) {
			if err := tx.Delete(protocolKey(id)); err != nil {
				return err
			}
		}
	}
	return nil
}

// sameRow matches the spellings sharing id's index row: its scheme and
// value, whatever their Linked.
func sameRow(id entity.ProtocolID) func(entity.ProtocolID) bool {
	return func(o entity.ProtocolID) bool { return o.Scheme == id.Scheme && o.Value == id.Value }
}

func decodeIDAs(kind entity.PrincipalKind) func([]byte) (entity.Principal, error) {
	return func(b []byte) (entity.Principal, error) {
		id, err := decodeNameRow(b)
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
	if p.Kind != entity.PrincipalUser && p.Kind != entity.PrincipalMachine {
		return merrs.NewInvalidArgumentError("not a user or a machine")
	}
	u, err := c.liveUser(p)
	if err != nil {
		return err
	}
	if err := expected(u.Generation, expect); err != nil {
		return err
	}
	if err := c.ownSecrets(u); err != nil {
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
	u, ok, err := userIn(c.tx, p)
	if err != nil || !ok {
		return entity.User{}, notFound(err, p)
	}
	return u, nil
}

// userIn reads p's user record, tracked in a transaction. A record marked
// deleted, or one of another kind than p's, reads as absent.
func userIn(r kv.Reader, p entity.Principal) (entity.User, bool, error) {
	b, ok, err := get(r, userKey(p.ID))
	if err != nil || !ok {
		return entity.User{}, false, err
	}
	u, err := decodeUser(p.ID, b)
	if err != nil || u.Deleted || u.Principal.Kind != p.Kind {
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

// live reports whether p's user or group record exists, of p's kind, and is
// not marked deleted, a user record as userIn reads it. A transaction's read
// is tracked, so a mark committed meanwhile conflicts with the caller.
func live(r kv.Reader, p entity.Principal) (bool, error) {
	if p.Kind != entity.PrincipalGroup {
		_, ok, err := userIn(r, p)
		return ok, err
	}
	b, ok, err := get(r, groupKey(p.ID))
	if err != nil || !ok {
		return false, err
	}
	g, err := decodeGroup(p.ID, b)
	return !g.Deleted, err
}

func notFound(err error, p entity.Principal) error {
	if err != nil {
		return err
	}
	return merrs.NewNotFoundError(p.String(), "principal")
}

// unindex deletes p's name and protocol-ID rows, all but a dir spelling's.
func unindex(tx kv.Txn, p entity.Principal, name string, ids entity.ProtocolIDs) error {
	if err := tx.Delete(nameKey(nameKind(p.Kind), name)); err != nil {
		return err
	}
	for _, id := range ids {
		if id.Scheme == entity.SchemeDir {
			continue // kept until the record goes: see protocolRow
		}
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
