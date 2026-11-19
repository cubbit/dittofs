package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A remote store's two settings the API reads by key: its endpoint, which
// every store has, and the reference to its credential's secret.
//
// ponytail: a remote store exists, and is tagged, by its endpoint setting,
// and a write of it audits that setting alone, since the store keeps no
// record of a remote store but its settings and a write's transaction reads a
// setting by its key only. Read the store's own record once an RFC 16
// amendment adds one, and take the two keys from the settings table once it
// names them.
const (
	storeEndpoint   = "remote.endpoint"
	storeCredential = "remote.credential"
)

func storeScope(id [16]byte) entity.SettingScope {
	return entity.SettingScope{Kind: entity.ScopeRemote, ID: id}
}

// storeDoc is a remote store as a write's transaction reads it: its
// endpoint setting.
type storeDoc struct {
	Spec apiv1.StoreSpec `json:"spec"`
}

// stores is what the write adapter knows of a remote store: its endpoint
// setting, whose generation is its tag.
var stores = writing[storeDoc]{kind: entity.ResourceRemoteStore, raises: true, read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (storeDoc, uint64, bool, error) {
	s, ok, err := tx.Setting(storeScope(id), storeEndpoint)
	return storeDoc{apiv1.StoreSpec{Settings: map[string]json.RawMessage{storeEndpoint: s.Value}}}, s.Generation, ok, err
}}

// createStore is POST /stores: a new remote store, its settings written at
// its scope, each judged by the settings table, its endpoint among them. Its
// credential is set by its own call.
func createStore(ctx context.Context, rq request[storeDoc], _ struct{}, tx entity.ControlPlaneTx) (apiv1.Store, [16]byte, error) {
	id := entity.NewShareID()
	spec := rq.Body.Spec
	switch {
	case spec.Credential != nil:
		return apiv1.Store{}, id, &fieldError{field: "spec.credential", detail: "set by PUT /stores/{id}/credential"}
	case spec.Settings[storeEndpoint] == nil:
		return apiv1.Store{}, id, &fieldError{field: "spec.settings." + storeEndpoint, detail: "a remote store states its endpoint"}
	case spec.Settings[storeCredential] != nil:
		return apiv1.Store{}, id, &fieldError{field: "spec.settings." + storeCredential, detail: "set by PUT /stores/{id}/credential"}
	}
	if err := putSettings(ctx, rq.d, tx, storeScope(id), spec.Settings); err != nil {
		return apiv1.Store{}, id, err
	}
	_, gen, _, err := stores.read(tx, id, "")
	return apiv1.Store{ID: idString(id), Generation: gen, Spec: spec}, id, err
}

// deleteStore is DELETE /stores/{id}: refused while any namespace names the
// store, one range read, before anything else; then every setting at its
// scope the pre-write step listed, each judged by the settings table.
//
// ponytail: a setting written at the store's scope between the listing and
// the commit outlives the store, and so do its credential's sealed versions,
// since the secret provider's Delete takes no transaction. Delete both in
// the transaction once it can list a scope and delete a secret.
//
// ponytail: the table judges each setting's delete on its own, and RFC 13
// refuses a delete that changes the effective value of a store's bucket,
// endpoint or credential reference even with no content. So once a table
// is handed in, no store can be deleted. The table must judge a resource's
// delete as the removal of its whole scope, which RFC 13's Check does not
// state yet.
func deleteStore(ctx context.Context, rq request[none], keys []string, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	named, err := tx.StoreNamed(rq.ID)
	switch {
	case err != nil:
		return none{}, rq.ID, err
	case named:
		return none{}, rq.ID, fmt.Errorf("remote store %s: a namespace names it: %w", idString(rq.ID), vfserr.ErrNotEmpty)
	}
	return none{}, rq.ID, deleteSettings(ctx, rq.d, tx, storeScope(rq.ID), keys)
}

// store is GET /stores/{id}: its settings at its scope, its credential as
// the secret's reference and current version, never its value, its
// endpoint setting's generation the ETag, 404 for a store with no endpoint;
// ?wait= returns once that generation changes.
var store = watching[apiv1.Store]{
	read: func(ctx context.Context, d Deps, rq request[none]) (apiv1.Store, error) {
		all, err := scopeSettings(ctx, d.Store, storeScope(rq.ID))
		if err != nil {
			return apiv1.Store{}, err
		}
		ep, ok := all[storeEndpoint]
		if !ok {
			return apiv1.Store{}, errNotFound
		}
		v := apiv1.Store{ID: idString(rq.ID), Generation: ep.Generation, Spec: apiv1.StoreSpec{Settings: map[string]json.RawMessage{}}}
		for k, s := range all {
			if k != storeCredential {
				v.Spec.Settings[k] = s.Value
			}
		}
		if c, ok := all[storeCredential]; ok {
			if v.Spec.Credential, err = secretRefOf(ctx, d, c); err != nil {
				return apiv1.Store{}, err
			}
		}
		return v, nil
	},
	tag: func(s apiv1.Store) uint64 { return s.Generation },
	until: changes(func(first, now apiv1.Store) bool {
		return first.Generation != now.Generation
	}),
}
