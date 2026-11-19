package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// sealer seals a secret's value as the next version of its reference,
// unwritten, under the wrapping key of the role its kind is sealed under; a
// new version keeps its reference's kind and owner. handedOver marks a
// machine password an offline join was handed, bound into the sealed value.
type sealer interface {
	Seal(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef, value []byte, handedOver bool) (entity.Secret, error)
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

// secretSetting is a write-only secret that a setting names by reference:
// the scope the path's ID names, the setting's key, the secret's kind, the
// field its audit records name the value by, exists, which refuses a set on
// a record that does not exist, read in the write's transaction, nil for
// none, and check, which refuses a value before it is sealed, nil for none.
type secretSetting struct {
	scope   func([16]byte) entity.SettingScope
	key     string
	kind    entity.SecretKind
	auditAs string
	exists  func(tx entity.ControlPlaneTx, id [16]byte) error
	check   func(ctx context.Context, d Deps, value []byte) error
}

// owner is the owner the secret is sealed for: the setting naming it.
func (ss secretSetting) owner(id [16]byte) entity.SecretOwner {
	return entity.SecretOwner{Scope: ss.scope(id), Key: ss.key}
}

// rotation refuses a set on a record that holds the secret already.
//
// ponytail: a secret named by a setting is set once, and its rotation, an
// operation that writes the new version and waits for every node to report
// it, is not built, so a set over an existing one is refused rather than
// writing a version no node is waited on for. Build the rotation operation
// and route the set through it when an operator must change a store's
// credential or the keytab without deleting it first.
func (ss secretSetting) rotation() error {
	return &componentError{field: ss.key, detail: "rotating a secret is an operation not built yet", err: vfserr.ErrExist}
}

// writing is what the write adapter knows of the secret: its current
// version, sealed, as the body a PUT sends, with the version as its tag, so
// a first PUT sets it under If-None-Match: *. Its audit record names it as
// auditAs and holds the value only as "set": what it compares is the sealed
// bytes, which no answer carries.
func (ss secretSetting) writing(kind entity.ResourceKind) writing[apiv1.Credential] {
	return writing[apiv1.Credential]{kind: kind, raises: true, auditAs: ss.auditAs,
		secret: func(p string) bool { return p == ss.auditAs+".value" },
		read: func(tx entity.ControlPlaneTx, id [16]byte, _ string) (apiv1.Credential, uint64, bool, error) {
			s, ok, err := tx.Setting(ss.scope(id), ss.key)
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
}

// seal is the secret's pre-write step: the value sealed as the first version
// of a new reference, under its kind's role, then cleared. A record that
// holds the secret is refused 409 first, then a value check refuses, then,
// with no sealer, the set is refused 503, each before anything is written.
func (ss secretSetting) seal(ctx context.Context, rq request[apiv1.Credential]) (entity.Secret, error) {
	all, err := scopeSettings(ctx, rq.d.Store, ss.scope(rq.ID))
	if err != nil {
		return entity.Secret{}, err
	}
	if _, ok := all[ss.key]; ok {
		return entity.Secret{}, ss.rotation()
	}
	v := rq.Body.Spec.Value
	if len(v) == 0 {
		return entity.Secret{}, &fieldError{field: "spec.value", detail: "a secret states its value"}
	}
	if ss.check != nil {
		if err := ss.check(ctx, rq.d, v); err != nil {
			return entity.Secret{}, err
		}
	}
	if rq.d.Sealer == nil {
		return entity.Secret{}, errNoSealer
	}
	ref := entity.SecretRef(entity.NewShareID())
	sealed, err := rq.d.Sealer.Seal(ctx, ss.kind, ss.owner(rq.ID), ref, v, false)
	clear(v)
	return sealed, err
}

// put writes the sealed version beside the setting naming it, judged by the
// settings table. A record gone since the step is 404, and one given the
// secret since is refused 409.
func (ss secretSetting) put(ctx context.Context, rq request[apiv1.Credential], sealed entity.Secret, tx entity.ControlPlaneTx) (apiv1.SecretRef, [16]byte, error) {
	scope := ss.scope(rq.ID)
	if ss.exists != nil {
		if err := ss.exists(tx, rq.ID); err != nil {
			return apiv1.SecretRef{}, rq.ID, err
		}
	}
	if _, ok, err := tx.Setting(scope, ss.key); err != nil || ok {
		if err == nil {
			err = ss.rotation()
		}
		return apiv1.SecretRef{}, rq.ID, err
	}
	if _, err := putSetting(ctx, rq.d, tx, entity.Setting{Scope: scope, Key: ss.key, Value: refValue(sealed.ID)}, 0); err != nil {
		return apiv1.SecretRef{}, rq.ID, err
	}
	if err := tx.PutSecret(sealed, sealed.Version-1); err != nil {
		return apiv1.SecretRef{}, rq.ID, err
	}
	return apiv1.SecretRef{Ref: idString(sealed.ID), Version: sealed.Version}, rq.ID, nil
}

// remove is the secret's DELETE: the setting naming it removed, judged by
// the settings table.
//
// ponytail: the secret's sealed versions outlive the setting, since the
// secret provider's Delete takes no transaction; delete them in this one
// once it does.
func (ss secretSetting) remove(ctx context.Context, rq request[none], _ struct{}, tx entity.ControlPlaneTx) (none, [16]byte, error) {
	scope := ss.scope(rq.ID)
	s, ok, err := tx.Setting(scope, ss.key)
	if err != nil {
		return none{}, rq.ID, err
	}
	if !ok {
		return none{}, rq.ID, errNotFound
	}
	return none{}, rq.ID, deleteSetting(ctx, rq.d, tx, scope, ss.key, s.Generation)
}

// storeCredentials is a remote store's credential, sealed under the storage
// role, set only on a store that exists.
var storeCredentials = secretSetting{scope: storeScope, key: storeCredential, kind: entity.SecretRemoteCredential, auditAs: "credential",
	exists: func(tx entity.ControlPlaneTx, id [16]byte) error {
		_, ok, err := tx.Setting(storeScope(id), storeEndpoint)
		if err == nil && !ok {
			err = errNotFound
		}
		return err
	}}

// credentials is what the write adapter knows of a remote store's
// credential.
var credentials = storeCredentials.writing(entity.ResourceRemoteStore)

// keytabKey is the installation setting naming the keytab.
const keytabKey = "identity.keytab"

// keytabs is the installation's keytab, identity's, sealed under the
// protocol role, refused before the seal when identity's check refuses it or
// it holds a key for a name the computer account holds, which the join
// derives and no keytab may replace.
var keytabs = secretSetting{scope: func([16]byte) entity.SettingScope { return entity.SettingScope{Kind: entity.ScopeInstallation} }, key: keytabKey, kind: entity.SecretKeytab, auditAs: "keytab",
	check: func(ctx context.Context, d Deps, v []byte) error {
		names, err := identity.KeytabNames(v)
		if err != nil {
			return &componentError{field: "spec.value", detail: "not a keytab of AES keys alone", err: err}
		}
		in, err := d.Store.Installation(ctx)
		if err != nil {
			return err
		}
		for _, n := range names {
			for _, j := range in.Join.Names {
				if strings.EqualFold(n, j) {
					return &fieldError{field: "spec.value", detail: "holds a key for " + n + ", a name of the computer account the domain join derives keys for"}
				}
			}
		}
		return nil
	}}

// keytabWrites is what the write adapter knows of the keytab: a secret of
// the installation, whose audit records name it by its ID.
var keytabWrites = func() writing[apiv1.Credential] {
	w := keytabs.writing(entity.ResourceInstallation)
	w.self = installationID
	return w
}()
