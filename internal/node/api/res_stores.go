package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

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
// setting, whose generation is its tag, and its credential, read as the
// sealed bytes its secret's reference names. The doc the read returns feeds
// the audit record and the ETag alone: the answer comes from what its
// handler returns, so no answer ever carries the value, and the audit
// record holds the credential's sealed bytes as "set", never a value, which
// is the path the secret closure names.
var stores = writing[storeDoc]{
	kind: entity.ResourceRemoteStore, raises: true,
	secret: func(p string) bool { return p == "spec.credential.value" },
	read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (storeDoc, uint64, bool, error) {
		s, ok, err := tx.Setting(storeScope(id), storeEndpoint)

		spec := apiv1.StoreSpec{Settings: map[string]json.RawMessage{storeEndpoint: s.Value}}

		c, cerr := credentialIn(tx, storeScope(id))
		if cerr != nil {
			return storeDoc{}, 0, false, cerr
		}

		spec.Credential = c
		doc := storeDoc{spec}

		return doc, s.Generation, ok, err
	},
}

// credentialIn is a store's credential as its write's transaction reads
// it: the sealed bytes the setting's reference names, with no reference
// and no version, so the doc it fills names the value alone. A credential
// row that is gone, or whose secret is gone, is nil: the doc has no value
// to hold, never a wrong one. A row too malformed to name a reference
// fails the write it feeds, since the write would judge and audit bytes it
// cannot read.
func credentialIn(
	tx entity.ControlPlaneTx,
	scope entity.SettingScope,
) (*apiv1.StoreCredential, error) {
	s, ok, err := tx.Setting(scope, storeCredential)
	if err != nil || !ok {
		return nil, err
	}

	ref, err := refFrom(s.Value)
	if err != nil {
		return nil, err
	}

	sec, ok, err := tx.Secret(ref, 0)
	if err != nil || !ok {
		return nil, err
	}

	return &apiv1.StoreCredential{Value: sec.Value}, nil
}

// sealedStore is POST /stores' pre-write step: the ID the store will take,
// and its credential's sealed first version, nil when the create carries
// none.
type sealedStore struct {
	id     [16]byte
	sealed *entity.Secret
}

// sealStore is POST /stores' pre-write step: it makes the store's ID, so
// the credential it seals is owned by the store it is written for, and
// seals the value the create carries as the first version of a new
// reference, never passed on as the value, cleared once it is read. A
// create that carries no credential seals none, since the settings table
// judges whether the store's record may go without one; with no sealer, a
// create that carries a value is refused 503 before anything is written.
//
// decision: the create seals without the store's capability check: RFC 13
// §5.2 asks the check for a replacement of what the store already reaches,
// and a store being created has no previous credential for the check to
// hold its claim against, while RFC 4 §4.11 runs the check wherever the
// store is opened. A route set may replace a credential the store held, so
// the route's own decision to check every set names the route alone. Withdraw this once a store can
// hold data before its first credential is written.
func sealStore(ctx context.Context, rq request[storeDoc]) (sealedStore, error) {
	id := entity.NewShareID()
	out := sealedStore{id: id}

	if rq.Body.Spec.Credential == nil {
		return out, nil
	}

	c := rq.Body.Spec.Credential

	// The value clears from here on, on every path out of the step, so a
	// refusal for its reference or its version clears it too.
	defer clear(c.Value)

	if c.Ref != "" || c.Version != 0 {
		refusal := &fieldError{
			field:  "spec.credential",
			detail: "a create states its value, never its reference or version",
		}

		return sealedStore{}, refusal
	}

	v := c.Value

	if len(v) == 0 {
		return sealedStore{}, &fieldError{
			field: "spec.credential.value", detail: "a credential states its value",
		}
	}

	if rq.d.Sealer == nil {
		return sealedStore{}, errNoSealer
	}

	ref := entity.SecretRef(entity.NewShareID())

	s, err := rq.d.Sealer.Seal(ctx, entity.SecretRemoteCredential,
		entity.SecretOwner{Scope: storeScope(id), Key: storeCredential},
		ref, v, false)
	if err != nil {
		return sealedStore{}, err
	}

	out.sealed = &s

	return out, nil
}

// createStore is POST /stores: a new remote store, its settings written at
// its scope, the record judged as a whole once every row is in the
// transaction, and its credential's sealed first version committed beside
// the reference row its settings hold. Its answer holds the credential as
// the reference and the first version, never the value the request carried.
func createStore(
	ctx context.Context,
	rq request[storeDoc],
	in sealedStore,
	tx entity.ControlPlaneTx,
) (apiv1.Store, [16]byte, error) {
	spec := rq.Body.Spec

	switch {
	case spec.Settings[storeEndpoint] == nil:
		return apiv1.Store{}, in.id, &fieldError{
			field: "spec.settings." + storeEndpoint, detail: "a remote store states its endpoint",
		}
	case spec.Settings[storeCredential] != nil:
		return apiv1.Store{}, in.id, &fieldError{
			field:  "spec.settings." + storeCredential,
			detail: "a create states its credential as spec.credential",
		}
	}

	values := spec.Settings

	if in.sealed != nil {
		if err := tx.PutSecret(*in.sealed, in.sealed.Version-1); err != nil {
			return apiv1.Store{}, in.id, err
		}

		values = maps.Clone(spec.Settings)
		values[storeCredential] = refValue(in.sealed.ID)
	}

	if err := createSettings(ctx, rq.d, tx, storeScope(in.id), values); err != nil {
		return apiv1.Store{}, in.id, err
	}

	_, gen, _, err := stores.read(tx, in.id, "")

	out := apiv1.StoreSpec{Settings: spec.Settings}

	if in.sealed != nil {
		cred := &apiv1.StoreCredential{Ref: idString(in.sealed.ID), Version: in.sealed.Version}
		out.Credential = cred
	}

	return apiv1.Store{ID: idString(in.id), Generation: gen, Spec: out}, in.id, err
}

// deleteStore is DELETE /stores/{id}: refused while any namespace names the
// store, one range read, before anything else; then every setting at its
// scope the pre-write step listed, removed with the scope, not judged as a
// change.
//
// ponytail: a setting written at the store's scope between the listing and
// the commit outlives the store, and so do its credential's sealed versions,
// since the secret provider's Delete takes no transaction. Delete both in
// the transaction once it can list a scope and delete a secret.
func deleteStore(
	_ context.Context,
	rq request[none],
	keys []string,
	tx entity.ControlPlaneTx,
) (none, [16]byte, error) {
	named, err := tx.StoreNamed(rq.ID)
	switch {
	case err != nil:
		return none{}, rq.ID, err
	case named:
		return none{}, rq.ID, fmt.Errorf(
			"remote store %s: a namespace names it: %w", idString(rq.ID), vfserr.ErrNotEmpty)
	case rq.d.Settings == nil:
		return none{}, rq.ID, errNoTable
	}

	return none{}, rq.ID, removeScope(tx, storeScope(rq.ID), keys)
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

		spec := apiv1.StoreSpec{Settings: map[string]json.RawMessage{}}
		v := apiv1.Store{ID: idString(rq.ID), Generation: ep.Generation, Spec: spec}

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
