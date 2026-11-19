package api

import (
	"context"
	"encoding/json"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// sealer seals a secret's value as the next version of its reference,
// unwritten, under the wrapping key of the role its kind is sealed under; a
// new version keeps its reference's kind and owner.
type sealer interface {
	Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte) (entity.Secret, error)
}

// refValue is a secret's reference as the setting naming it holds it: the
// reference's ID as a JSON string. refFrom reads it back.
func refValue(ref entity.SecretRef) []byte {
	b, _ := json.Marshal(idString(ref)) // a string always encodes
	return b
}

func refFrom(v []byte) (entity.SecretRef, error) {
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return entity.SecretRef{}, err
	}
	id, err := entity.ParseShareID(s)
	return entity.SecretRef(id), err
}

// secretRefOf is the secret the setting s names, as a read answers it: its
// reference and its current version, never its value.
func secretRefOf(ctx context.Context, d Deps, s entity.Setting) (*apiv1.SecretRef, error) {
	ref, err := refFrom(s.Value)
	if err != nil {
		return nil, err
	}
	sec, err := d.Store.Secret(ctx, ref, 0)
	if err != nil {
		return nil, err
	}
	return &apiv1.SecretRef{Ref: idString(ref), Version: sec.Version}, nil
}

// credentialOwner is the owner a remote store's credential is sealed for:
// the setting at the store's scope that names it.
func credentialOwner(id [16]byte) entity.SecretOwner {
	return entity.SecretOwner{Scope: storeScope(id), Key: storeCredential}
}

// errRotation refuses a credential's set on a store that holds one.
//
// ponytail: a store's credential is set once, and its rotation, an
// operation that writes the new version and waits for every node to report
// it, is not built, so a set over an existing credential is refused rather
// than writing a version no node is waited on for. Build the rotation
// operation and route the set through it when an operator must change a
// store's credential without deleting it first.
var errRotation = &componentError{field: storeCredential, detail: "rotating a credential is an operation not built yet", err: vfserr.ErrExist}

// credentials is what the write adapter knows of a remote store's
// credential: its current version, sealed, as the body a PUT sends, with
// the version as its tag, so a first PUT sets it under If-None-Match: *. Its
// audit record names the store, and holds the value only as "set": what it
// compares is the sealed bytes, which no answer carries.
var credentials = writing[apiv1.Credential]{kind: entity.ResourceRemoteStore, raises: true, auditAs: "credential",
	secret: func(p string) bool { return p == "credential.value" },
	read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (apiv1.Credential, uint64, bool, error) {
		s, ok, err := tx.Setting(storeScope(id), storeCredential)
		if err != nil || !ok {
			return apiv1.Credential{}, 0, false, err
		}
		ref, err := refFrom(s.Value)
		if err != nil {
			return apiv1.Credential{}, 0, false, err
		}
		sec, ok, err := tx.Secret(ref, 0)
		return apiv1.Credential{Spec: apiv1.CredentialSpec{Value: sec.Value}}, sec.Version, ok, err
	}}

// sealCredential is the credential's pre-write step: the value sealed as
// the first version of a new reference, under the storage role's key. A
// store that holds a credential is refused 409 first, then, with no sealer,
// the set is refused 503, both before anything is written.
//
// ponytail: only the refusals run, since the node hands in no sealer and no
// settings table yet, so no credential is sealed or written through this
// route, no test reads a sealed version back raw, and no test reaches the
// table's check of the set inside the write (the missing sealer refuses
// first). Test the set end to end once the node hands in both: the table's
// refusal of it, and its version read raw holding no value.
func sealCredential(ctx context.Context, rq request[apiv1.Credential]) (entity.Secret, error) {
	if rq.d.Store == nil {
		return entity.Secret{}, errNoStore
	}
	all, err := scopeSettings(ctx, rq.d.Store, storeScope(rq.ID))
	if err != nil {
		return entity.Secret{}, err
	}
	if _, ok := all[storeCredential]; ok {
		return entity.Secret{}, errRotation
	}
	if rq.d.Sealer == nil {
		return entity.Secret{}, &refusal{code: apiv1.CodeUnavailable, detail: "the node holds no secret provider to seal the value with"}
	}
	if len(rq.Body.Spec.Value) == 0 {
		return entity.Secret{}, &fieldError{field: "spec.value", detail: "a credential states its value"}
	}
	ref := entity.SecretRef(entity.NewShareID())
	return rq.d.Sealer.Seal(ctx, entity.SecretRemoteCredential, credentialOwner(rq.ID), ref, rq.Body.Spec.Value)
}

// putCredential is PUT /stores/{id}/credential: the sealed version written
// beside the setting naming it, judged by the settings table. A store gone
// since the step is 404, and one given a credential since is refused 409.
func putCredential(ctx context.Context, rq request[apiv1.Credential], sealed entity.Secret, tx entity.ControlPlaneTx) (apiv1.SecretRef, [16]byte, error) {
	scope := storeScope(rq.ID)
	if _, ok, err := tx.Setting(scope, storeEndpoint); err != nil || !ok {
		if err == nil {
			err = errNotFound
		}
		return apiv1.SecretRef{}, rq.ID, err
	}
	if _, ok, err := tx.Setting(scope, storeCredential); err != nil || ok {
		if err == nil {
			err = errRotation
		}
		return apiv1.SecretRef{}, rq.ID, err
	}
	if _, err := putSetting(ctx, rq.d, tx, entity.Setting{Scope: scope, Key: storeCredential, Value: refValue(sealed.ID)}, 0); err != nil {
		return apiv1.SecretRef{}, rq.ID, err
	}
	if err := tx.PutSecret(sealed, sealed.Version-1); err != nil {
		return apiv1.SecretRef{}, rq.ID, err
	}
	return apiv1.SecretRef{Ref: idString(sealed.ID), Version: sealed.Version}, rq.ID, nil
}

// deleteCredential is DELETE /stores/{id}/credential: the setting naming
// the credential removed, judged by the settings table.
//
// ponytail: the credential's sealed versions outlive the setting, since the
// secret provider's Delete takes no transaction; delete them in this one
// once it does.
func deleteCredential(ctx context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	scope := storeScope(rq.ID)
	s, ok, err := tx.Setting(scope, storeCredential)
	if err != nil {
		return none{}, rq.ID, err
	}
	if !ok {
		return none{}, rq.ID, errNotFound
	}
	return none{}, rq.ID, deleteSetting(ctx, rq.d, tx, scope, storeCredential, s.Generation)
}
