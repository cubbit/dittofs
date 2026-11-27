package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A remote store's two settings the API reads by key: its endpoint, which
// every store has, and the reference to its credential's secret.
//
// ponytail: a remote store exists, and is tagged, by its endpoint setting,
// and a write of it audits that setting and its credential alone, so the
// audit record of a PATCH of only its other settings names no change, and a
// write of one of them through the setting resource moves no ETag, since
// the store keeps no
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

	if err := credentialValue(c); err != nil {
		return sealedStore{}, err
	}

	v := c.Value

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

// credentialValue refuses a store's credential a create or a patch states
// by anything but a value.
func credentialValue(c *apiv1.StoreCredential) error {
	switch {
	case c.Ref != "" || c.Version != 0:
		return &fieldError{field: "spec.credential", detail: "a write states its value, never its reference or version"}
	case len(c.Value) == 0:
		return &fieldError{field: "spec.credential.value", detail: "a credential states its value"}
	}
	return nil
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

// storeList is GET /stores: every remote store in ID order, each as its GET
// answers it, found by its endpoint setting among every scope's settings.
//
// The settings are read in the reader's order, scope kind, scope ID, then
// key, so the listing starts at the remote scopes, after the store the
// cursor names, and ends at the first setting of a later kind. A row that
// does not decode outside a store's scope is passed over, as RFC 13 §6 has a
// bad record stop only what it configures; one in a store's scope fails the
// listing, as it fails that store's GET.
//
// ponytail: each store is read again whole, its settings and its
// credential's version, so a page costs one read per store beside the
// listing's. Read a store from the listing's own rows once a page's reads
// show in a profile.
var storeList = listing[apiv1.Store, apiv1.Store]{
	lo: make([]byte, 16), // a cursor is a store's ID
	records: func(ctx context.Context, d Deps, after []byte, _ map[string]string) iter.Seq2[apiv1.Store, error] {
		return func(yield func(apiv1.Store, error) bool) {
			from := entity.SettingKey{Scope: storeScope([16]byte{})}
			if after != nil {
				var id [16]byte
				copy(id[:], after)
				from = entity.SettingKey{Scope: storeScope(id), Key: storeEndpoint}
			}
			for s, err := range d.Nodes.AllSettings(ctx, from) {
				if err != nil && !errors.Is(err, entity.ErrBadRow) {
					yield(apiv1.Store{}, err)
					return
				}
				switch {
				case s.Scope.Kind > entity.ScopeRemote:
					return
				case s.Scope.Kind != entity.ScopeRemote, s.Key != storeEndpoint:
					continue // a bad row whose key does not parse, or a store's other setting
				}
				var v apiv1.Store
				if err == nil {
					v, err = store.read(ctx, d, request[none]{ID: s.Scope.ID})
					if errors.Is(err, errNotFound) {
						continue // deleted since the listing read its endpoint
					}
				}
				if !yield(v, err) || err != nil {
					return
				}
			}
		}
	},
	key: func(s apiv1.Store) []byte {
		id, _ := entity.ParseShareID(s.ID)
		return bytes.Clone(id[:])
	},
	item: func(s apiv1.Store, _ perPage) apiv1.Store { return s },
}

// storePatch is PATCH /stores/{id}'s pre-write step's result: the settings
// the patch names, a JSON null for one it removes; the store's settings as
// the step read them, which the answer is built from; and the credential the
// patch states, sealed.
type storePatch struct {
	settings map[string]json.RawMessage
	had      map[string]entity.Setting
	cred     *sealedSecret
}

// prepareStorePatch is PATCH /stores/{id}'s pre-write step, which sees the
// patch: a credential named as a setting is refused 400, a store with no
// endpoint 404, and a credential the patch states goes
// through the credential's own set, its capability check among its
// refusals, and is sealed, as PUT /stores/{id}/credential seals it.
func prepareStorePatch(ctx context.Context, rq request[storeDoc]) (storePatch, error) {
	spec := rq.Body.Spec
	if c := spec.Credential; c != nil {
		defer clear(c.Value) // on every path out, a refusal's among them
	}
	if _, ok := spec.Settings[storeCredential]; ok {
		return storePatch{}, &fieldError{field: "spec.settings." + storeCredential, detail: "a patch states its credential as spec.credential"}
	}
	had, err := scopeSettings(ctx, rq.d.Store, storeScope(rq.ID))
	if err != nil {
		return storePatch{}, err
	}
	if _, ok := had[storeEndpoint]; !ok {
		return storePatch{}, errNotFound
	}
	out := storePatch{settings: spec.Settings, had: had}
	if c := spec.Credential; c != nil {
		if err := credentialValue(c); err != nil {
			return storePatch{}, err
		}
		in, err := storeCredentials.seal(ctx, request[apiv1.Credential]{ID: rq.ID, d: rq.d, Body: apiv1.Credential{Spec: apiv1.CredentialSpec{Value: c.Value}}})
		if err != nil {
			return storePatch{}, err
		}
		out.cred = &in
	}
	return out, nil
}

// patchStore is PATCH /stores/{id}: the credential the step sealed written
// as its own set writes it, then each setting the patch names written, or
// removed for a null, at the store's scope, a value merged into the one
// stored as RFC 7396 merges it, each judged by the settings
// table, which refuses a bound one 409 bound_setting while a namespace names
// the store and a change of how the store is reached 409 unproven until its
// capability check is built (RFC 13 §5.1, §5.2). A patch that removes the
// credential, or the endpoint, which a null settings removes too, is refused
// 400: the credential's DELETE removes it. A patch that writes anything
// writes the endpoint again, unchanged, so the store's tag moves and a
// second patch read at the same tag is refused 412. Its answer
// is the store's settings as the step read them with the patch applied,
// and its credential as reference and version.
//
// ponytail: a setting written at the store's scope between the step's read
// and the commit is missing from the answer, though not from the store;
// build the answer in the transaction once it can list a scope.
func patchStore(ctx context.Context, rq request[storeDoc], in storePatch, tx entity.ControlPlaneTx) (apiv1.Store, [16]byte, error) {
	scope := storeScope(rq.ID)
	if rq.Body.Spec.Settings[storeEndpoint] == nil {
		return apiv1.Store{}, rq.ID, &fieldError{field: "spec.settings." + storeEndpoint, detail: "a remote store states its endpoint"}
	}
	var cred *apiv1.StoreCredential
	switch {
	case in.cred != nil:
		ref, _, err := storeCredentials.put(ctx, request[apiv1.Credential]{ID: rq.ID, d: rq.d}, *in.cred, tx)
		if err != nil {
			return apiv1.Store{}, rq.ID, err
		}
		cred = &apiv1.StoreCredential{Ref: ref.Ref, Version: ref.Version}
	case rq.Body.Spec.Credential == nil:
		// The merged spec lost the credential the store holds: the patch
		// removed it.
		held, err := credentialIn(tx, scope)
		if err != nil {
			return apiv1.Store{}, rq.ID, err
		}
		if held != nil {
			return apiv1.Store{}, rq.ID, &fieldError{field: "spec.credential", detail: "removed by DELETE /api/v1/stores/{id}/credential"}
		}
	}
	puts, gone := map[string]json.RawMessage{}, []string{}
	for k, v := range in.settings {
		if string(v) == "null" {
			gone = append(gone, k)
			continue
		}
		old, _, err := tx.Setting(scope, k)
		if err != nil {
			return apiv1.Store{}, rq.ID, err
		}
		if puts[k], err = mergeValue(old.Value, v); err != nil {
			return apiv1.Store{}, rq.ID, err
		}
	}
	slices.Sort(gone)
	if err := putSettings(ctx, rq.d, tx, scope, puts); err != nil {
		return apiv1.Store{}, rq.ID, err
	}
	if err := deleteSettings(ctx, rq.d, tx, scope, gone); err != nil {
		return apiv1.Store{}, rq.ID, err
	}
	if _, named := puts[storeEndpoint]; !named && (len(puts) > 0 || len(gone) > 0 || in.cred != nil) {
		ep, _, err := tx.Setting(scope, storeEndpoint)
		if err != nil {
			return apiv1.Store{}, rq.ID, err
		}
		if _, err := putSetting(ctx, rq.d, tx, ep, ep.Generation); err != nil {
			return apiv1.Store{}, rq.ID, err
		}
	}
	out := apiv1.StoreSpec{Settings: map[string]json.RawMessage{}}
	for k, s := range in.had {
		if k != storeCredential {
			out.Settings[k] = s.Value
		}
	}
	maps.Copy(out.Settings, puts)
	for _, k := range gone {
		delete(out.Settings, k)
	}
	out.Credential = cred
	if c, ok := in.had[storeCredential]; ok && cred == nil {
		var err error
		if out.Credential, err = secretRefOf(ctx, rq.d, c); err != nil {
			return apiv1.Store{}, rq.ID, err
		}
	}
	_, gen, _, err := stores.read(tx, rq.ID, "")
	return apiv1.Store{ID: idString(rq.ID), Generation: gen, Spec: out}, rq.ID, err
}

// mergeValue is old, a setting's stored value, nil for none, with patch
// merged into it as RFC 7396 merges a member's value.
func mergeValue(old, patch json.RawMessage) (json.RawMessage, error) {
	if old == nil {
		old = json.RawMessage("null")
	}
	o, err := decodeAny(old)
	if err != nil {
		return nil, fmt.Errorf("api: a stored setting: %w", err)
	}
	p, err := decodeAny(patch)
	if err != nil {
		return nil, &fieldError{detail: "patch: " + err.Error()}
	}
	return json.Marshal(merge(o, p))
}
