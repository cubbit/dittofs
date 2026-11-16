package kvstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"iter"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Shares lists every share with its state, in folded-name order.
func (c ControlPlane) Shares(ctx context.Context) iter.Seq2[entity.Share, error] {
	return pages(ctx, c.s.kv, []byte{keys.KindNX, keys.NXShare}, nil,
		func(r kv.Reader, row kv.KeyValue) (entity.Share, error) {
			id, err := decodeID(row.Value)
			if err != nil {
				return entity.Share{}, err
			}
			return shareIn(r, id)
		})
}

func shareIn(r kv.Reader, id entity.ShareID) (entity.Share, error) {
	b, err := r.Get(shareKey(id, keys.SInfo))
	if err != nil {
		return entity.Share{}, err
	}
	s, err := decodeShareInfo(id, b)
	if err != nil {
		return s, err
	}
	if b, err = r.Get(shareKey(id, keys.SSt)); err != nil {
		return s, err
	}
	st, err := decodeState(b)
	if err != nil {
		return s, err
	}
	s.State = st.State
	// ponytail: every share read scans the share's shard rows, one iterator
	// on top of its point reads, though only a serve reads Shard; about 1µs
	// and 23 allocations on in-memory badger. Read the shard where serve
	// uses it, through a reader of its own, once a remote backend shows the
	// extra read on every call's admission.
	for row, err := range r.Scan(shardsPrefix(id), nil) {
		if err != nil {
			return s, err
		}
		shard, rest, err := keys.ReadUint64(row.Key[len(shardsPrefix(id)):])
		if err != nil || len(rest) != 0 || shard >= uint64(entity.ShardLimit) {
			return s, merrs.NewInvalidArgumentError("a share's shard row does not decode")
		}
		s.Shard = entity.ShardID(shard)
		break
	}
	if s.GrantVersion, err = counterIn(r, shareKey(id, keys.SGv)); err != nil {
		return s, err
	}
	if b, ok, err := get(r, namespacesKey(id)); err != nil || !ok {
		return s, err
	} else if nss, err := decodeNamespaces(b); err != nil {
		return s, err
	} else {
		s.Namespaces, s.Write = nss.Namespaces, nss.Write
	}
	return s, nil
}

// PutShare creates a share, expect zero, or changes the name or path of the
// one whose generation expect names; its root never changes. It writes the
// share's info record, raising its generation, and its name and path rows,
// and raises ShareList when either moves. State is not its to write: a new
// share starts enabled, and only SetShareState changes it after. A new
// share is created in the one namespace its Namespaces names, whose record
// is read tracked and must exist, so a share never lands in a namespace that
// names no remote store; a change ignores Namespaces, which only a re-home
// and an import write.
//
// No share's path is an ancestor of another's. An ancestor is found by a
// point read of each of the path's prefixes, a descendant by a guarded scan
// of the rows below it, so two shares created at once on nested paths cannot
// both commit, in either order.
func (c ControlPlane) PutShare(ctx context.Context, s entity.Share, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutShare(s, expect) })
}

func (c controlTx) PutShare(s entity.Share, expect uint64) (uint64, error) {
	if s.ID.IsZero() || s.Root.IsZero() {
		return 0, merrs.NewInvalidArgumentError("a share needs an ID and a root")
	}
	if !entity.ValidShareName(s.Name) {
		return 0, merrs.NewInvalidArgumentError("share name " + s.Name + " not valid")
	}
	if !entity.ValidSharePath(s.Path) {
		return 0, merrs.NewInvalidArgumentError("share path " + s.Path + " not valid")
	}
	if s.Fold != 0 && !slices.Contains(entity.FoldRules, s.Fold) || s.Names != 0 && s.Names.String() == "" {
		return 0, merrs.NewInvalidArgumentError("unknown fold or name rule")
	}
	info := shareKey(s.ID, keys.SInfo)
	b, existed, err := get(c.tx, info)
	if err != nil {
		return 0, err
	}
	var old entity.Share
	if existed {
		if old, err = decodeShareInfo(s.ID, b); err != nil {
			return 0, err
		}
		if err := liveShare(c.tx, s.ID); err != nil {
			return 0, err
		}
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	if existed {
		if old.Root != s.Root {
			return 0, merrs.NewInvalidArgumentError("a share's root never changes")
		}
		if err := keepBound(&s, old); err != nil {
			return 0, err
		}
	} else {
		bindNew(&s)
	}
	moved := !existed || old.Name != s.Name || old.Path != s.Path
	if err := claimShareName(c.tx, s, existed, old.Name); err != nil {
		return 0, err
	}
	if err := claimSharePath(c.tx, s, existed, old.Path); err != nil {
		return 0, err
	}
	if !existed {
		ns, err := shareNamespace(c, s)
		if err != nil {
			return 0, err
		}
		if err := createShareRecords(c.tx, &s, ns); err != nil {
			return 0, err
		}
	}
	s.Generation, s.Position = old.Generation+1, old.Position
	if moved {
		if s.Position, err = raise(c.tx, shareListKey); err != nil {
			return 0, err
		}
	}
	return s.Generation, c.tx.Set(info, encodeShareInfo(s))
}

// createShareRecords writes what a new share starts with besides its info:
// its state, enabled; its one shard, a number no share of the installation
// has had, with its shard record and its row in the share's shard list; its
// root directory, in that shard; its numeric file id allocator, above the
// root's number; and the namespace ns, generation 1, that its content is
// counted in, with the share's key in that namespace's share index. The
// root's key must be free, so a root ID that already names a file is refused
// rather than overwritten.
//
// ponytail: a share has exactly one shard, made with it. Write a row and a
// record per shard, in the transaction that creates each, once a share can
// have more than one.
func createShareRecords(tx kv.Txn, s *entity.Share, ns entity.NamespaceID) error {
	tag, err := nextTag(tx)
	if err != nil {
		return err
	}
	s.Tag = tag
	if s.Shard, err = nextShard(tx); err != nil {
		return err
	}
	root := fileKey(s.ID, s.Root)
	rf := newRoot(tx, *s)
	if _, ok, err := get(tx, root); err != nil {
		return err
	} else if ok {
		return merrs.NewAlreadyExistsError("root " + s.Root.String())
	}
	for _, kvs := range [][2][]byte{
		{shareKey(s.ID, keys.SSt), encodeState(shareState{State: entity.ShareEnabled})},
		{keys.Uint64(shardsPrefix(s.ID), uint64(s.Shard)), encodeEmpty()},
		{shardKey(uint64(s.Shard)), encodeShard(entity.Shard{ID: s.Shard})},
		{root, encodeFile(rf)},
		{floorKey(s.ID, s.Root, rf.Version), encodeEmpty()},
		{numberKey(s.ID), encodeCounter(rootNumber + 1)},
	} {
		if err := tx.Set(kvs[0], kvs[1]); err != nil {
			return err
		}
	}
	if err := putShareNamespaces(tx, s.ID, shareNamespaces{}, shareNamespaces{
		Namespaces: []entity.NamespaceGen{{Generation: 1, Namespace: ns}}, Write: 1}); err != nil {
		return err
	}
	return chargeFile(tx, s.ID, nil, &rf)
}

// ErrTagsExhausted is an installation that has given every journal tag.
var ErrTagsExhausted = errors.New("kvstore: installation has no share tags left")

// nextTag mints a journal tag no share of the installation has had: the
// tag counter's next value, from 1, since a zero tag names no share.
func nextTag(tx kv.Txn) (uint32, error) {
	v, err := counterIn(tx, tagKey)
	if err != nil {
		return 0, err
	}
	if v >= math.MaxUint32 {
		return 0, ErrTagsExhausted
	}
	return uint32(v + 1), tx.Set(tagKey, encodeCounter(v+1))
}

// nextShard draws a shard number no shard of the installation has had: the
// shard counter's next value, from 0, read and raised in the caller's
// transaction. A number past the 24-bit shard space is refused, as a state
// reference could not carry it.
func nextShard(tx kv.Txn) (entity.ShardID, error) {
	key := idKey(idShard)
	b, ok, err := get(tx, key)
	if err != nil {
		return 0, err
	}
	var v uint64
	if ok {
		if v, err = decodeCounter(b); err != nil {
			return 0, err
		}
	}
	if v >= uint64(entity.ShardLimit) {
		return 0, &merrs.StoreError{Code: merrs.ErrNoSpace, Message: "the installation has no shard numbers left"}
	}
	return entity.ShardID(v), tx.Set(key, encodeCounter(v+1))
}

// liveShare refuses a share being removed or retired, which is stale to
// every caller. The read is tracked, so a write that passed it conflicts with
// a removal that marks the share before it commits.
func liveShare(r kv.Reader, share entity.ShareID) error {
	b, ok, err := get(r, shareKey(share, keys.SSt))
	if err != nil || !ok {
		return err
	}
	st, err := decodeState(b)
	if err != nil {
		return err
	}
	if st.State == entity.ShareRemoving || st.State == entity.ShareRetired {
		return entity.ErrStale
	}
	return nil
}

// bindNew gives a new share its bound settings: the identity fold unless
// one is named, the name rule that goes with its fold unless one is named,
// and a digest key drawn at random unless one is given.
func bindNew(s *entity.Share) {
	if s.Fold == 0 {
		s.Fold = entity.FoldIdentity
	}
	if s.Names == 0 {
		s.Names = entity.NamesPOSIX
		if s.Fold == entity.FoldUpcase {
			s.Names = entity.NamesWindows
		}
	}
	if s.DigestKey == ([entity.DigestKeyLen]byte{}) {
		rand.Read(s.DigestKey[:])
	}
}

// keepBound carries an existing share's bound settings into a put, a zero
// field meaning "unchanged", and refuses any other value.
func keepBound(s *entity.Share, old entity.Share) error {
	if s.Fold != 0 && s.Fold != old.Fold || s.Names != 0 && s.Names != old.Names ||
		s.DigestKey != ([entity.DigestKeyLen]byte{}) && s.DigestKey != old.DigestKey {
		return merrs.NewInvalidArgumentError("a share's fold rule, name rule and digest key never change")
	}
	s.Fold, s.Names, s.DigestKey, s.Tag = old.Fold, old.Names, old.DigestKey, old.Tag
	return nil
}

func claimShareName(tx kv.Txn, s entity.Share, existed bool, oldName string) error {
	key := nameKey(keys.NXShare, s.Name)
	if existed && bytes.Equal(key, nameKey(keys.NXShare, oldName)) {
		return nil // a change of case only: the row stays
	}
	if err := claimShare(tx, key, s.ID, "share name "+s.Name+" taken"); err != nil {
		return err
	}
	if existed {
		return tx.Delete(nameKey(keys.NXShare, oldName))
	}
	return nil
}

func claimSharePath(tx kv.Txn, s entity.Share, existed bool, oldPath string) error {
	if existed && s.Path == oldPath {
		return nil
	}
	if err := claimShare(tx, pathKey(s.Path), s.ID, "share path "+s.Path+" taken"); err != nil {
		return err
	}
	for _, a := range entity.PathAncestors(s.Path) {
		if b, ok, err := get(tx, pathKey(a)); err != nil {
			return err
		} else if ok && !holds(b, s.ID) {
			return merrs.NewAlreadyExistsError("share path " + s.Path + " is inside share path " + a)
		}
	}
	// Every share put also raises ShareList, which already orders two puts;
	// the guard keeps this rule from resting on that counter.
	below := pathsBelow(s.Path)
	if err := tx.GuardRange(below); err != nil {
		return err
	}
	for row, err := range tx.Scan(below, nil) {
		if err != nil {
			return err
		}
		if !holds(row.Value, s.ID) {
			return merrs.NewAlreadyExistsError("share path " + s.Path + " contains another share")
		}
	}
	if existed {
		return tx.Delete(pathKey(oldPath))
	}
	return nil
}

// holds reports whether an index row's value names id. A row that does not
// decode names nobody this caller can be.
func holds(b []byte, id entity.ShareID) bool {
	got, err := decodeID(b)
	return err == nil && got == [16]byte(id)
}

func claimShare(tx kv.Txn, key []byte, id entity.ShareID, taken string) error {
	b, ok, err := get(tx, key)
	if err != nil {
		return err
	}
	if ok {
		if !holds(b, id) {
			return merrs.NewAlreadyExistsError(taken)
		}
		return nil
	}
	return tx.Set(key, encodeID(id))
}

// SetShareState moves a share from one state to another, refusing when it is
// not in from: the read is tracked, so of two changes racing from one state,
// one conflicts and then finds the state moved. It raises ShareList, and the
// share's position with it, which its info record holds.
//
// Removal is one-way and not this call's: only DeleteShare moves a share to
// being removed, and a share being removed or retired is stale to it.
func (c ControlPlane) SetShareState(ctx context.Context, share entity.ShareID, from, to entity.ShareState) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.SetShareState(share, from, to) })
}

func (c controlTx) SetShareState(share entity.ShareID, from, to entity.ShareState) error {
	switch {
	case to.String() == "":
		return merrs.NewInvalidArgumentError("unknown share state")
	case to == entity.ShareRemoving || to == entity.ShareRetired:
		return merrs.NewInvalidArgumentError("only DeleteShare removes a share")
	}
	key := shareKey(share, keys.SSt)
	b, ok, err := get(c.tx, key)
	if err != nil {
		return err
	}
	if !ok {
		return merrs.NewNotFoundError(share.String(), "share")
	}
	st, err := decodeState(b)
	if err != nil {
		return err
	}
	cur := st.State
	if cur == entity.ShareRemoving || cur == entity.ShareRetired {
		return entity.ErrStale
	}
	if cur != from {
		return merrs.NewConflictError("set share state", "share is "+cur.String()+", not "+from.String())
	}
	if from == to {
		return nil
	}
	if err := c.tx.Set(key, encodeState(shareState{State: to})); err != nil {
		return err
	}
	info := shareKey(share, keys.SInfo)
	if b, err = c.tx.Get(info); err != nil {
		return err
	}
	s, err := decodeShareInfo(share, b)
	if err != nil {
		return err
	}
	if s.Position, err = raise(c.tx, shareListKey); err != nil {
		return err
	}
	return c.tx.Set(info, encodeShareInfo(s))
}

// SetGrant sets a principal's access to a share, the grant's version
// expect naming the one it replaces, zero for none. The grant's version and
// the share's grant-list version both move to the writing commit's. A grant
// of none is stored like any other and blocks the principal: only DeleteGrant
// lets its groups, then the defaults, decide again. A grant names a user, a
// group, or the share's defaults, authenticated users or everyone, and a
// default grants at most read-write. Share and principal must exist, read so
// that a deletion of either conflicts; a default always exists.
func (c ControlPlane) SetGrant(ctx context.Context, g entity.ShareGrant, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.SetGrant(g, expect) })
}

func (c controlTx) SetGrant(g entity.ShareGrant, expect uint64) (uint64, error) {
	isDefault := g.Principal == entity.AuthenticatedUsers || g.Principal == entity.Everyone
	if g.Access > entity.AccessAdmin || (g.Principal.Kind != entity.PrincipalUser && g.Principal.Kind != entity.PrincipalGroup && !isDefault) {
		return 0, merrs.NewInvalidArgumentError("a grant names a user, a group, authenticated users or everyone, and a known access")
	}
	if isDefault && g.Access > entity.AccessReadWrite {
		return 0, merrs.NewInvalidArgumentError("a default grant is at most read-write")
	}
	if _, ok, err := get(c.tx, shareKey(g.Share, keys.SInfo)); err != nil || !ok {
		if err == nil {
			err = merrs.NewNotFoundError(g.Share.String(), "share")
		}
		return 0, err
	}
	if err := liveShare(c.tx, g.Share); err != nil {
		return 0, err
	}
	if _, ok, err := get(c.tx, principalKey(g.Principal)); !isDefault && (err != nil || !ok) {
		return 0, notFound(err, g.Principal)
	}
	old, _, err := grantIn(c.tx, g.Share, g.Principal.ID)
	if err != nil {
		return 0, err
	}
	if err := expected(old.Version, expect); err != nil {
		return 0, err
	}
	g.Version = nextVersion(c.tx, old.Version)
	if err := c.tx.Set(grantKey(g.Share, g.Principal.ID), encodeGrant(g)); err != nil {
		return 0, err
	}
	if err := c.tx.Set(grantRevKey(g.Principal.ID, g.Share), nil); err != nil {
		return 0, err
	}
	return g.Version, moveGrantVersion(c.tx, g.Share)
}

// DeleteGrant removes a principal's grant on a share, its version expect,
// with its reverse row, and moves the share's grant-list version: its
// groups, then the defaults, decide its access again. Deleting a grant that
// does not exist, expect zero, changes nothing.
func (c ControlPlane) DeleteGrant(ctx context.Context, share entity.ShareID, p entity.Principal, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteGrant(share, p, expect) })
}

func (c controlTx) DeleteGrant(share entity.ShareID, p entity.Principal, expect uint64) error {
	if err := liveShare(c.tx, share); err != nil {
		return err
	}
	old, ok, err := grantIn(c.tx, share, p.ID)
	if err != nil {
		return err
	}
	if err := expected(old.Version, expect); err != nil || !ok {
		return err
	}
	if err := c.tx.Delete(grantKey(share, p.ID)); err != nil {
		return err
	}
	if err := c.tx.Delete(grantRevKey(p.ID, share)); err != nil {
		return err
	}
	return moveGrantVersion(c.tx, share)
}

// moveGrantVersion sets a share's grant-list version to the writing commit's,
// above the one it replaces, read tracked.
func moveGrantVersion(tx kv.Txn, share entity.ShareID) error {
	key := shareKey(share, keys.SGv)
	v, err := counterIn(tx, key)
	if err != nil {
		return err
	}
	return tx.Set(key, encodeCounter(nextVersion(tx, v)))
}

// ShareGrant is the stored grant naming p on a share, with the version a
// write replacing or deleting it passes as expect; false when none does. It
// is looked up by p's ID alone, so p's Kind is not checked.
func (c ControlPlane) ShareGrant(ctx context.Context, share entity.ShareID, p entity.Principal) (g entity.ShareGrant, found bool, err error) {
	err = c.s.kv.View(ctx, func(r kv.Reader) error {
		g, found, err = grantIn(r, share, p.ID)
		return err
	})
	return g, found, err
}

// ExportPolicy reads a share's export policy; a share without one has
// defaultPolicy's.
func (c ControlPlane) ExportPolicy(ctx context.Context, share entity.ShareID) (entity.ExportPolicy, error) {
	p := defaultPolicy(share)
	err := c.s.kv.View(ctx, func(r kv.Reader) error {
		b, ok, err := get(r, shareKey(share, keys.SXp))
		if err != nil || !ok {
			return err
		}
		p, err = decodePolicy(share, b)
		return err
	})
	return p, err
}

// defaultPolicy is the export policy of a share none is stored for: root
// squashed to the anonymous principal, every client admitted, no flavour
// named.
func defaultPolicy(share entity.ShareID) entity.ExportPolicy {
	return entity.ExportPolicy{Share: share, Squash: entity.SquashRoot, Anonymous: entity.AnonymousPrincipal}
}

// PutExportPolicy replaces a share's export policy, the version expect names
// or zero for none stored, raising its version. A
// policy naming no Anonymous names the anonymous principal: it is stored as
// given and read back as naming it.
func (c ControlPlane) PutExportPolicy(ctx context.Context, p entity.ExportPolicy, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutExportPolicy(p, expect) })
}

func (c controlTx) PutExportPolicy(p entity.ExportPolicy, expect uint64) (uint64, error) {
	for _, f := range p.Flavors {
		if f.String() == "" {
			return 0, merrs.NewInvalidArgumentError("unknown auth flavor")
		}
	}
	for _, r := range p.Clients {
		if !r.Valid() {
			return 0, merrs.NewInvalidArgumentError("a client rule names one CIDR or netgroup, and at most read-write")
		}
	}
	if !p.Valid() {
		return 0, merrs.NewInvalidArgumentError("an export policy's protection floor needs a verified proof, and both are known values")
	}
	if p.Squash.String() == "" {
		return 0, merrs.NewInvalidArgumentError("unknown squash")
	}
	if _, ok, err := get(c.tx, shareKey(p.Share, keys.SInfo)); err != nil || !ok {
		if err == nil {
			err = merrs.NewNotFoundError(p.Share.String(), "share")
		}
		return 0, err
	}
	if err := liveShare(c.tx, p.Share); err != nil {
		return 0, err
	}
	key := shareKey(p.Share, keys.SXp)
	b, ok, err := get(c.tx, key)
	if err != nil {
		return 0, err
	}
	var old entity.ExportPolicy
	if ok {
		if old, err = decodePolicy(p.Share, b); err != nil {
			return 0, err
		}
	}
	if err := expected(old.Version, expect); err != nil {
		return 0, err
	}
	p.Version = nextVersion(c.tx, old.Version)
	return p.Version, c.tx.Set(key, encodePolicy(p))
}
