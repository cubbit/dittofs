package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

func namespaceScope(id [16]byte) entity.SettingScope {
	return entity.SettingScope{Kind: entity.ScopeNamespace, ID: id}
}

// namespaceDoc is a namespace as a write's transaction reads it: the store
// its record names.
type namespaceDoc struct {
	Spec apiv1.NamespaceSpec `json:"spec"`
}

// namespaces is what the write adapter knows of a namespace: its record,
// whose generation is its tag.
var namespaces = writing[namespaceDoc]{kind: entity.ResourceNamespace, raises: true, read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (namespaceDoc, uint64, bool, error) {
	n, ok, err := tx.Namespace(entity.NamespaceID(id))
	return namespaceDoc{apiv1.NamespaceSpec{Store: idString(n.Store)}}, n.Generation, ok, err
}}

// createNamespace is POST /namespaces: a new namespace on the remote store
// its spec names, which must exist, with its settings, the bound ones
// among them, written at its scope, the record judged as a whole once every
// row is in the transaction. The record's Bound holds every bound key the
// table declares, the ones the create sends with the value sent and the
// others at their default, encoded by the settings table, beside the rows
// that carry the effective values.
func createNamespace(ctx context.Context, rq request[namespaceDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Namespace, [16]byte, error) {
	id := entity.NewNamespaceID()
	spec := rq.Body.Spec
	st, err := entity.ParseShareID(spec.Store)
	if err != nil {
		return apiv1.Namespace{}, id, &fieldError{field: "spec.store", detail: "not a remote store ID"}
	}
	if _, ok, err := tx.Setting(storeScope(st), storeEndpoint); err != nil || !ok {
		if err == nil {
			err = &fieldError{field: "spec.store", detail: "names no remote store"}
		}
		return apiv1.Namespace{}, id, err
	}
	if len(spec.Settings) == 0 {
		return apiv1.Namespace{}, id, &fieldError{
			field: "spec.settings", detail: "a namespace states its bound settings at its creation",
		}
	}

	if err := createSettings(ctx, rq.d, tx, namespaceScope(id), spec.Settings); err != nil {
		return apiv1.Namespace{}, id, err
	}

	rows := make([]entity.Setting, 0, len(spec.Settings))
	for _, k := range slices.Sorted(maps.Keys(spec.Settings)) {
		rows = append(rows, entity.Setting{
			Scope: namespaceScope(id),
			Key:   k,
			Value: spec.Settings[k],
		})
	}

	b, err := rq.d.Settings.Bound(rows)
	if err != nil {
		return apiv1.Namespace{}, id, err
	}

	gen, err := tx.PutNamespace(entity.Namespace{ID: id, Store: st, Bound: b}, 0)

	return apiv1.Namespace{ID: idString(id), Generation: gen, Spec: spec}, id, err
}

// deleteNamespace is DELETE /namespaces/{id}: refused 409 not_empty while a
// share names the namespace, one range read, before anything else; then
// refused while the node hands in no settings table, even when no setting
// is left at its scope; then the record under the generation If-Match
// names, and every setting at its scope the pre-write step listed, removed
// with the scope, not judged as a change.
//
// ponytail: a setting written at the namespace's scope between the listing
// and the commit outlives it; delete it in the transaction once it can list
// a scope.
func deleteNamespace(ctx context.Context, rq request[none], keys []string, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	id := entity.NamespaceID(rq.ID)
	named, err := tx.NamespaceNamed(id)
	switch {
	case err != nil:
		return none{}, rq.ID, err
	case named:
		return none{}, rq.ID, fmt.Errorf("namespace %s: a share names it: %w", id, vfserr.ErrNotEmpty)
	case rq.d.Settings == nil:
		return none{}, rq.ID, errNoTable
	}
	// ponytail: the record and its settings go at once, while RFC 9 §7.6
	// drains a namespace no share names under its drain mark, and GC's
	// first start finds one left undrained by its record. Keep the record
	// and write the mark instead once GC's drain lands.
	if err := tx.DeleteNamespace(id, rq.Expect); err != nil {
		return none{}, rq.ID, err
	}

	return none{}, rq.ID, removeScope(tx, namespaceScope(rq.ID), keys)
}

// namespaceOn is n on the wire, with its settings.
func namespaceOn(ctx context.Context, d Deps, n entity.Namespace) (apiv1.Namespace, error) {
	all, err := scopeSettings(ctx, d.Store, namespaceScope(n.ID))
	v := apiv1.Namespace{ID: idString(n.ID), Generation: n.Generation, Spec: apiv1.NamespaceSpec{Store: idString(n.Store), Settings: map[string]json.RawMessage{}}}
	for k, s := range all {
		v.Spec.Settings[k] = s.Value
	}
	return v, err
}

// namespaceList is GET /namespaces: every namespace in ID order, with its
// settings.
var namespaceList = listing[apiv1.Namespace, apiv1.Namespace]{
	records: func(ctx context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[apiv1.Namespace, error] {
		return func(yield func(apiv1.Namespace, error) bool) {
			var from entity.NamespaceID
			copy(from[:], after)
			for n, err := range d.Store.Namespaces(ctx, from) {
				var v apiv1.Namespace
				if err == nil {
					v, err = namespaceOn(ctx, d, n)
				}
				if !yield(v, err) || err != nil {
					return
				}
			}
		}
	},
	key: func(n apiv1.Namespace) []byte {
		id, _ := entity.ParseShareID(n.ID)
		return bytes.Clone(id[:])
	},
	item: func(n apiv1.Namespace, _ perPage) apiv1.Namespace { return n },
}

// namespace is GET /namespaces/{id}, its generation the ETag; ?wait=
// returns once it changes.
var namespace = watching[apiv1.Namespace]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Namespace, error) {
		n, ok, err := d.Store.Namespace(ctx, entity.NamespaceID(rq.ID))
		switch {
		case err != nil:
			return apiv1.Namespace{}, err
		case !ok:
			return apiv1.Namespace{}, errNotFound
		}
		return namespaceOn(ctx, d, n)
	},
	tag: func(n apiv1.Namespace) uint64 { return n.Generation },
	until: changes(func(first, now apiv1.Namespace) bool {
		return first.Generation != now.Generation
	}),
}
