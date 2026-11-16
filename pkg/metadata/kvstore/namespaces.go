package kvstore

import (
	"bytes"
	"context"
	"iter"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Namespaces lists the namespaces whose ID sorts after after, in ID order.
//
// ponytail: every page reads every namespace in one transaction, through
// the store index, which holds one key per namespace where the namespace's
// own prefix holds all its content records: a page costs O(N) reads in the
// namespace count. Page the index by ID once that cost shows in production,
// where an installation holds namespaces in the ten thousands.
func (c ControlPlane) Namespaces(ctx context.Context, after entity.NamespaceID) iter.Seq2[entity.Namespace, error] {
	return func(yield func(entity.Namespace, error) bool) {
		var list []entity.Namespace
		err := c.s.kv.View(ctx, func(r kv.Reader) error {
			list = list[:0]
			for row, err := range r.Scan([]byte{keys.KindNSR}, nil) {
				if err != nil {
					return err
				}
				id := entity.NamespaceID(row.Key[len(row.Key)-keys.IDLen:])
				if bytes.Compare(id[:], after[:]) <= 0 {
					continue
				}
				n, ok, err := namespaceIn(r, id)
				if err != nil {
					return err
				}
				if ok {
					list = append(list, n)
				}
			}
			return nil
		})
		if err != nil {
			yield(entity.Namespace{}, err)
			return
		}
		slices.SortFunc(list, func(a, b entity.Namespace) int { return bytes.Compare(a.ID[:], b.ID[:]) })
		for _, n := range list {
			if !yield(n, nil) {
				return
			}
		}
	}
}

// PutNamespace creates a namespace, expect zero, or replaces the one whose
// generation expect names, raising its generation. The record and its key
// in the store index are written together. A namespace naming no remote
// store is refused, and so is a change to an existing one's store or bound
// fields: both are fixed at its creation, and moving a namespace to another
// store is a move, never a put.
//
// decision: the remote store's existence is not read here, since no record
// of a remote store exists for it to read; the caller creating a namespace
// reads the store in the same transaction, so the store's deletion and the
// namespace's create conflict. Read the store's record here, tracked, once
// one is written.
func (c ControlPlane) PutNamespace(ctx context.Context, n entity.Namespace, expect uint64) (uint64, error) {
	return c.updateGen(ctx, func(tx entity.ControlPlaneTx) (uint64, error) { return tx.PutNamespace(n, expect) })
}

func (c controlTx) PutNamespace(n entity.Namespace, expect uint64) (uint64, error) {
	if n.ID.IsZero() {
		return 0, merrs.NewInvalidArgumentError("a namespace needs an ID")
	}
	if n.Store == ([16]byte{}) {
		return 0, merrs.NewInvalidArgumentError("namespace " + n.ID.String() + " names no remote store")
	}
	old, existed, err := c.Namespace(n.ID)
	if err != nil {
		return 0, err
	}
	if err := expected(old.Generation, expect); err != nil {
		return 0, err
	}
	if existed && (old.Store != n.Store || !bytes.Equal(old.Bound, n.Bound)) {
		return 0, merrs.NewInvalidArgumentError("namespace " + n.ID.String() + "'s store and bound fields never change")
	}
	n.Generation = old.Generation + 1
	if err := c.tx.Set(namespaceInfoKey(n.ID), encodeNamespace(n)); err != nil {
		return 0, err
	}
	return n.Generation, c.tx.Set(storeIndexKey(n.Store, n.ID), nil)
}

// DeleteNamespace deletes the namespace whose generation expect names,
// with its key in the store index, and refuses one a share still names.
//
// decision: the record goes once no share names the namespace, and its
// content is drained after, by the collector, rather than the record
// outliving the drain: nothing drains a namespace yet, so a record kept
// until its drain would never go. Keep the record until its drain has run
// once the drain lands.
func (c ControlPlane) DeleteNamespace(ctx context.Context, id entity.NamespaceID, expect uint64) error {
	return c.update(ctx, func(tx entity.ControlPlaneTx) error { return tx.DeleteNamespace(id, expect) })
}

func (c controlTx) DeleteNamespace(id entity.NamespaceID, expect uint64) error {
	old, existed, err := c.Namespace(id)
	if err != nil {
		return err
	}
	if !existed {
		return merrs.NewNotFoundError(id.String(), "namespace")
	}
	if err := expected(old.Generation, expect); err != nil {
		return err
	}
	if named, err := c.NamespaceNamed(id); err != nil || named {
		if err == nil {
			err = merrs.NewConflictError("delete namespace", "a share names namespace "+id.String())
		}
		return err
	}
	if err := c.tx.Delete(namespaceInfoKey(id)); err != nil {
		return err
	}
	return c.tx.Delete(storeIndexKey(old.Store, id))
}

// Namespace reads a namespace's record, tracked.
func (c controlTx) Namespace(id entity.NamespaceID) (entity.Namespace, bool, error) {
	return namespaceIn(c.tx, id)
}

// StoreNamed reports whether any namespace names the remote store, guarding
// the store's whole index range, so a namespace created on the store
// concurrently conflicts with the caller.
func (c controlTx) StoreNamed(store [16]byte) (bool, error) {
	return guardedAny(c.tx, keys.ID([]byte{keys.KindNSR}, store[:]))
}

// NamespaceNamed reports whether any share names the namespace, guarding
// the namespace's share index range, so a share created into it
// concurrently conflicts with the caller.
func (c controlTx) NamespaceNamed(ns entity.NamespaceID) (bool, error) {
	return guardedAny(c.tx, namespaceSharesPrefix(ns))
}

// shareNamespace is the namespace a new share is created in: the one its
// Namespaces names, read tracked so a deletion of it conflicts with the
// create, and refused when it has no record.
func shareNamespace(tx controlTx, s entity.Share) (entity.NamespaceID, error) {
	if len(s.Namespaces) != 1 {
		return entity.NamespaceID{}, merrs.NewInvalidArgumentError("a new share names exactly one namespace")
	}
	id := s.Namespaces[0].Namespace
	if _, ok, err := tx.Namespace(id); err != nil || !ok {
		if err == nil {
			err = merrs.NewInvalidArgumentError("namespace " + id.String() + " of share " + s.Name + " does not exist")
		}
		return id, err
	}
	return id, nil
}

// putShareNamespaces writes share's namespaces record and moves the share's
// keys in the namespaces' share indexes from old's namespaces to next's, so
// the index never drifts from the record. Every write of a share's
// namespaces goes through it: a share's create, and its removal and a
// re-home once they write them.
func putShareNamespaces(tx kv.Txn, share entity.ShareID, old, next shareNamespaces) error {
	in := func(gs []entity.NamespaceGen, ns entity.NamespaceID) bool {
		return slices.ContainsFunc(gs, func(g entity.NamespaceGen) bool { return g.Namespace == ns })
	}
	for _, g := range old.Namespaces {
		if !in(next.Namespaces, g.Namespace) {
			if err := tx.Delete(namespaceShareKey(g.Namespace, share)); err != nil {
				return err
			}
		}
	}
	for _, g := range next.Namespaces {
		if !in(old.Namespaces, g.Namespace) {
			if err := tx.Set(namespaceShareKey(g.Namespace, share), nil); err != nil {
				return err
			}
		}
	}
	return tx.Set(namespacesKey(share), encodeNamespaces(next))
}

func namespaceIn(r kv.Reader, id entity.NamespaceID) (entity.Namespace, bool, error) {
	b, ok, err := get(r, namespaceInfoKey(id))
	if err != nil || !ok {
		return entity.Namespace{}, false, err
	}
	n, err := decodeNamespace(id, b)
	return n, err == nil, err
}

func namespaceInfoKey(id entity.NamespaceID) []byte {
	return append(keys.ID([]byte{keys.KindNS}, id[:]), keys.NSInfo)
}

func namespaceSharesPrefix(id entity.NamespaceID) []byte {
	return append(keys.ID([]byte{keys.KindNS}, id[:]), keys.NSShare)
}

func namespaceShareKey(id entity.NamespaceID, share entity.ShareID) []byte {
	return keys.ID(namespaceSharesPrefix(id), share[:])
}

func storeIndexKey(store [16]byte, id entity.NamespaceID) []byte {
	return keys.ID(keys.ID([]byte{keys.KindNSR}, store[:]), id[:])
}

func encodeNamespace(n entity.Namespace) []byte {
	return codec.NewWriter(v1).Fixed(n.Store[:]).Bytes(n.Bound).Uint64(n.Generation).Encode()
}

func decodeNamespace(id entity.NamespaceID, b []byte) (entity.Namespace, error) {
	r, err := open(b)
	if err != nil {
		return entity.Namespace{}, err
	}
	n := entity.Namespace{ID: id}
	copy(n.Store[:], r.Fixed(16))
	n.Bound, n.Generation = cloneOrNil(r.Bytes()), r.Uint64()
	return n, r.Done()
}
