package api

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets/secreterr"
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
// JSON list of the reference's 16 bytes, the form a settings table's strict
// decode reads a reference field from, so the write's check decodes the row
// it is given. refFrom reads it back; a read answers an operator the
// reference as the API spells it, idString.
func refValue(ref entity.SecretRef) []byte {
	b, _ := json.Marshal(ref) // a [16]byte always encodes
	return b
}

func refFrom(v []byte) (entity.SecretRef, error) {
	var ref entity.SecretRef

	err := json.Unmarshal(v, &ref)

	return ref, err
}

// secretRefOf is the secret the setting s names, as a read answers it: its
// reference and its current version, never its value.
func secretRefOf(ctx context.Context, d Deps, s entity.Setting) (*apiv1.StoreCredential, error) {
	ref, err := refFrom(s.Value)
	if err != nil {
		return nil, err
	}
	sec, err := d.Store.Secret(ctx, ref, 0)
	if err != nil {
		return nil, err
	}

	return &apiv1.StoreCredential{Ref: idString(ref), Version: sec.Version}, nil
}

// secretSetting is a write-only secret that a setting names by reference:
// the scope the path's ID names, the setting's key, the secret's kind, the
// field its audit records name the value by, record, the key of the record
// the secret belongs to, and check, which refuses a value before it is
// sealed and returns what to check again in the write's transaction, read
// tracked there, nil for none.
type secretSetting struct {
	scope   func([16]byte) entity.SettingScope
	key     string
	kind    entity.SecretKind
	auditAs string
	// record, when set, is the key whose setting at the scope is the record
	// the secret belongs to: a set on a scope without it is 404.
	record string
	check  func(ctx context.Context, d Deps, value []byte) (inTx func(entity.ControlPlaneTx) error, err error)
}

// sealedSecret is a secret's pre-write step's result: the sealed version,
// and what its check runs again in the write's transaction, nil for none.
type sealedSecret struct {
	entity.Secret
	inTx func(entity.ControlPlaneTx) error
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
// writing a version no node is waited on for. Until it is, the keytab is
// changed by a DELETE, then a PUT; a store's credential cannot be changed
// at all, since its PUT is refused until the store's capability check is
// built (see capabilityCheck). Build the
// rotation operation (RFC 13 §7, and RFC 18 §3.3's for the keytab) and
// route the set through it when an operator must change one without the
// gap between the two.
func (ss secretSetting) rotation() error {
	return &componentError{field: ss.key, detail: "rotating a secret is an operation not built yet", err: vfserr.ErrExist}
}

// writing is what the write adapter knows of the secret: its current
// version, sealed, as the body a PUT sends, with the version as its tag, so
// a first PUT sets it under If-None-Match: *. Its audit record names it as
// auditAs and holds the value only as "set": what it compares is the sealed
// bytes, which no answer carries.
func (ss secretSetting) writing(kind entity.ResourceKind) writing[apiv1.Credential] {
	return writing[apiv1.Credential]{
		kind: kind, raises: true, auditAs: ss.auditAs,
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
		},
	}
}

// seal is the secret's pre-write step: the value sealed as the first version
// of a new reference, under its kind's role, then cleared. A set on a
// record that does not exist is refused 404 first, then one on a record
// that holds the secret 409, then a value check refuses, then,
// with no sealer, the set is refused 503, each before anything is written.
//
// ponytail: the node hands in no sealer yet, so neither a store's credential
// nor the keytab reaches these routes in production; test the set end to
// end once the node hands in the secret provider.
func (ss secretSetting) seal(ctx context.Context, rq request[apiv1.Credential]) (sealedSecret, error) {
	v := rq.Body.Spec.Value
	defer clear(v)
	all, err := scopeSettings(ctx, rq.d.Store, ss.scope(rq.ID))
	if err != nil {
		return sealedSecret{}, err
	}
	if _, ok := all[ss.record]; ss.record != "" && !ok {
		return sealedSecret{}, errNotFound
	}
	if _, ok := all[ss.key]; ok {
		return sealedSecret{}, ss.rotation()
	}
	if len(v) == 0 {
		return sealedSecret{}, &fieldError{field: "spec.value", detail: "a secret states its value"}
	}
	var out sealedSecret
	if ss.check != nil {
		if out.inTx, err = ss.check(ctx, rq.d, v); err != nil {
			return sealedSecret{}, err
		}
	}
	if rq.d.Sealer == nil {
		return sealedSecret{}, errNoSealer
	}
	ref := entity.SecretRef(entity.NewShareID())
	out.Secret, err = rq.d.Sealer.Seal(ctx, ss.kind, ss.owner(rq.ID), ref, v, false)
	return out, err
}

// put writes the sealed version, then the setting naming it, judged by the
// settings table, so the write's check reads the secret the reference names
// in the same transaction. A record gone since the step is 404, one given
// the secret since is refused 409, and the check is run again, tracked, so
// what it read cannot change under the write.
func (ss secretSetting) put(ctx context.Context, rq request[apiv1.Credential], in sealedSecret, tx entity.ControlPlaneTx) (apiv1.SecretRef, [16]byte, error) {
	sealed, scope := in.Secret, ss.scope(rq.ID)
	if ss.record != "" {
		if _, ok, err := tx.Setting(scope, ss.record); err != nil || !ok {
			if err == nil {
				err = errNotFound
			}
			return apiv1.SecretRef{}, rq.ID, err
		}
	}
	if in.inTx != nil {
		if err := in.inTx(tx); err != nil {
			return apiv1.SecretRef{}, rq.ID, err
		}
	}
	if _, ok, err := tx.Setting(scope, ss.key); err != nil || ok {
		if err == nil {
			err = ss.rotation()
		}
		return apiv1.SecretRef{}, rq.ID, err
	}

	if err := tx.PutSecret(sealed, sealed.Version-1); err != nil {
		return apiv1.SecretRef{}, rq.ID, err
	}

	ref := entity.Setting{Scope: scope, Key: ss.key, Value: refValue(sealed.ID)}

	if _, err := putSetting(ctx, rq.d, tx, ref, 0); err != nil {
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
// role, set only on a store that exists and only once the store's
// capability check admits it.
var storeCredentials = secretSetting{
	scope:   storeScope,
	key:     storeCredential,
	kind:    entity.SecretRemoteCredential,
	auditAs: "credential",
	record:  storeEndpoint,
	check:   capabilityCheck,
}

// capabilityCheck is a remote store's capability check, run on every set of
// its credential.
//
// decision: the check runs on every set, a first one too, because nothing
// records whether the store held a credential before: a DELETE removes the
// setting naming it, and the set after it seals a fresh reference at
// version 1, so the version cannot tell a first credential from a changed
// one, which RFC 13 §5.2 has the check prove. The check and the namespace
// claim it proves against are not built, so it refuses every set, failing
// closed. The ceiling: POST /stores takes no credential, so until the check
// is built no remote store holds a credential set through the API, and the
// plan's requirement that a store can be created and given its first
// credential is not met until then. Withdraw the
// refusal once both are built, and run the claim part only for a store that
// held a credential once the store records that.
func capabilityCheck(context.Context, Deps, []byte) (func(entity.ControlPlaneTx) error, error) {
	return nil, &componentError{field: storeCredential, detail: "the store's capability check is not built yet", err: secreterr.ErrUnproven}
}

// credentials is what the write adapter knows of a remote store's
// credential.
var credentials = storeCredentials.writing(entity.ResourceRemoteStore)

// keytabKey is the installation setting naming the keytab, and its secret's
// owner key, identity's, which the node opens the keytab by.
const keytabKey = identity.KeytabSetting

// keytabs is the installation's keytab, identity's, sealed under the
// protocol role, refused when identity's check refuses it or it holds a key
// for a name the computer account holds, which the join derives and no
// keytab may replace: refused before the seal on the installation and the
// directory's realm read then, and again in the write's transaction on both
// read tracked there, so a join that names the account in between is not
// missed. The keytab's bytes are cleared once its names are read.
var keytabs = secretSetting{
	scope:   func([16]byte) entity.SettingScope { return installation },
	key:     keytabKey,
	kind:    entity.SecretKeytab,
	auditAs: "keytab",
	check: func(ctx context.Context, d Deps, v []byte) (func(entity.ControlPlaneTx) error, error) {
		names, err := identity.KeytabNames(v)
		if err != nil {
			return nil, &componentError{field: "spec.value", detail: "not a keytab of AES keys alone", err: err}
		}
		in, err := d.Store.Installation(ctx)
		if err != nil {
			return nil, err
		}
		all, err := scopeSettings(ctx, d.Store, installation)
		if err != nil {
			return nil, err
		}
		s, ok := all[directoryRealm]
		if err := accountHolds(in.Join, s, ok, names); err != nil {
			return nil, err
		}
		return func(tx entity.ControlPlaneTx) error {
			in, err := tx.Installation()
			if err != nil {
				return err
			}
			s, ok, err := tx.Setting(installation, directoryRealm)
			if err != nil {
				return err
			}
			return accountHolds(in.Join, s, ok, names)
		}, nil
	},
}

// accountHolds refuses a keytab holding a key for a name the computer
// account holds while joined, its own name or one of its service names, in
// the realm of the directory record's realm setting realm. With ok false,
// or the setting empty, no realm is recorded, so a name is refused in every
// realm. Names are
// compared with ASCII letters folded, realms ignoring the case of ASCII
// letters, as identity matches a ticket to the account's names.
func accountHolds(j entity.MachineJoin, realm entity.Setting, ok bool, names []identity.KeytabName) error {
	n, held, err := accountKey(j, realm, ok, names)
	if err != nil || !held {
		return err
	}
	return &fieldError{field: "spec.value", detail: "holds a key for " + n.Name + "@" + n.Realm + ", a name of the computer account the domain join derives keys for"}
}

// accountKey is the first of names accountHolds refuses, false for none.
func accountKey(j entity.MachineJoin, realm entity.Setting, ok bool, names []identity.KeytabName) (identity.KeytabName, bool, error) {
	if !j.Joined {
		return identity.KeytabName{}, false, nil
	}
	var r string
	if ok {
		if err := json.Unmarshal(realm.Value, &r); err != nil {
			return identity.KeytabName{}, false, &componentError{field: directoryRealm, detail: "the stored realm is not a JSON string", err: vfserr.ErrCorrupt}
		}
	}
	for _, n := range names {
		if r != "" && !equalFoldASCII(n.Realm, r) {
			continue
		}
		if equalFoldASCII(n.Name, j.Account) || slices.ContainsFunc(j.Names, func(m string) bool { return equalFoldASCII(n.Name, m) }) {
			return n, true, nil
		}
	}
	return identity.KeytabName{}, false, nil
}

// equalFoldASCII reports whether a and b are equal with ASCII letters
// folded, every other byte compared as is.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// keytabWrites is what the write adapter knows of the keytab: a secret of
// the installation, whose audit records name it by its ID.
var keytabWrites = func() writing[apiv1.Credential] {
	w := keytabs.writing(entity.ResourceInstallation)
	w.self = installationID
	return w
}()
