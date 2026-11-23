package node_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/iana/etypeID"
	"github.com/jcmturner/gokrb5/v8/keytab"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	vfs "github.com/cubbit/dittofs/pkg/vfs"
)

// bindCredentialKey is the setting naming the directory's bind password.
const bindCredentialKey = "identity.directory.bind_credential"

// The directory section's address row names a host the record is valid
// with, sealed under the node's own provider.
const directoryAddress = `{"spec":{"value":"ldaps://dc.corp.example"}}`

// A setting the table declares a secret's reference seals its value under
// the API's sealer, the node's own provider: the answer and the row name
// the reference, the secret is owned by the setting, at version 1 of the
// declared kind, and the protocol role resolves it back to the value.
func TestNodeSealsASettingSecret(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	started(t, h)
	addr, _ := h.Node().Addrs()

	posted := send(t.Context(), addr, http.MethodPut, "/api/v1/settings/identity.directory.address",
		directoryAddress, "If-None-Match", "*")
	if r := answeredIn(t, "PUT the address", posted); r.status != http.StatusOK {
		t.Fatalf("PUT the address: answered %d %s, want 200", r.status, r.body)
	}

	const canary = "node-bind-password-canary"

	r := answeredIn(t, "PUT the bind credential", send(t.Context(), addr, http.MethodPut,
		"/api/v1/settings/"+bindCredentialKey,
		`{"spec":{"value":"`+canary+`"}}`, "If-None-Match", "*"))

	if r.status != http.StatusOK {
		t.Fatalf("PUT the bind credential: answered %d %s, want 200", r.status, r.body)
	}

	var got apiv1.Setting
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("the set's answer %s: %v", r.body, err)
	}

	var ref apiv1.SecretRef
	if err := json.Unmarshal(got.Spec.Value, &ref); err != nil || ref.Version != 1 ||
		ref.Ref == "" {
		t.Fatalf(
			"the answer's value %s: want the reference at version 1, err %v",
			got.Spec.Value,
			err,
		)
	}

	id, err := entity.ParseShareID(ref.Ref)
	if err != nil {
		t.Fatalf("the answered reference %s: %v", ref.Ref, err)
	}

	owner := entity.SecretOwner{
		Scope: entity.SettingScope{Kind: entity.ScopeInstallation},
		Key:   bindCredentialKey,
	}

	cp := h.Store().ControlPlane()
	sec, err := cp.Secret(t.Context(), entity.SecretRef(id), 0)

	if err != nil || sec.Kind != entity.SecretDirectoryCredential || sec.Version != 1 ||
		!sec.Owner.Equal(owner) {
		t.Fatalf(
			"the sealed credential: %+v err %v, "+
				"want version 1 of the directory kind, owned by the setting",
			sec,
			err,
		)
	}

	kp, err := keyprovider.NewProvider(
		t.Context(),
		keyprovider.Config{Kind: keyprovider.KindLocal, File: h.ProtocolKeyFile()},
	)
	if err != nil {
		t.Fatalf("open the protocol key: %v", err)
	}
	defer func() {
		_ = kp.Close()
	}()

	p, err := secrets.New(cp, map[secrets.Role]keyprovider.KeyProvider{secrets.Protocol: kp})
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}

	value, err := p.Resolve(t.Context(), secrets.Protocol, sec.Kind, sec.Owner, sec.ID)
	if err != nil {
		t.Fatalf("resolve the sealed credential: %v", err)
	}

	if string(value) != canary {
		t.Errorf("the protocol role resolved %q, want the written value %q", value, canary)
	}
}

// A directory record written only through the settings route reaches the
// directory: the bind credential the route sealed opens in identity, under
// the owner identity reads it by, and the dial binds with it. The test takes
// no owner from the sealed record: a wrong owner would fail the Find below.
func TestADirectoryRecordWrittenThroughTheRouteDials(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	h := nodetest.New(t, nodetest.Watch(), nodetest.InMemory(),
		nodetest.WithNew(node.NewWithDirectory(d.Dial, admin)))
	d.AddUser(t, "dave", nil)
	started(t, h)
	addr, _ := h.Node().Addrs()

	name, password := d.Account()
	rows := []struct{ key, value string }{
		{"identity.directory.address", "ldaps://dc.corp.example"},
		{"identity.directory.base", "DC=corp,DC=example"},
		{"identity.directory.bind_name", name},
		{"identity.directory.realm", "CORP.EXAMPLE"},
		{"identity.directory.netbios_domain", "KORP"},
		{bindCredentialKey, string(password)},
	}

	for _, row := range rows {
		r := answeredIn(t, "PUT "+row.key, send(t.Context(), addr, http.MethodPut,
			"/api/v1/settings/"+row.key,
			`{"spec":{"value":`+strconv.Quote(row.value)+`}}`, "If-None-Match", "*"))

		if r.status != http.StatusOK {
			t.Fatalf("PUT %s: answered %d %s, want 200", row.key, r.status, r.body)
		}
	}

	caughtUp(t, h)

	proven := vfs.Call{Identity: vfs.Identity{Proof: vfs.ProofSecret}}
	got, err := h.Node().Service().Find(t.Context(), proven,
		vfs.Spelling{Scheme: vfs.SchemeQualified, Text: `KORP\dave`})

	if err != nil || got.IsZero() {
		t.Fatalf(`Find(KORP\dave) once the record was written through the route: `+
			`got %v, %v; want a minted principal, so the sealed bind credential opened`, got, err)
	}
}

// nodeKeytab is one AES keytab holding a key for name at realm.
func nodeKeytab(t *testing.T, name, realm string) []byte {
	t.Helper()

	kt := keytab.New()
	if err := kt.AddEntry(
		name, realm, "pw", time.Unix(1, 0), 1, etypeID.AES256_CTS_HMAC_SHA1_96,
	); err != nil {
		t.Fatalf("build the keytab: %v", err)
	}

	b, err := kt.Marshal()
	if err != nil {
		t.Fatalf("marshal the keytab: %v", err)
	}

	return b
}

// The keytab's own route seals through the same provider the settings route
// hands in, end to end: its 200 answers the reference and the version, the
// row names it and the sealed secret is a keytab.
func TestNodeSealsTheKeytab(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	started(t, h)
	addr, _ := h.Node().Addrs()

	good := nodeKeytab(t, "cifs/nas.ad.example", "AD.EXAMPLE")
	body := `{"spec":{"value":"` + base64.StdEncoding.EncodeToString(good) + `"}}`

	r := answeredIn(t, "PUT the keytab", send(t.Context(), addr, http.MethodPut,
		"/api/v1/installation/keytab", body, "If-None-Match", "*"))

	if r.status != http.StatusOK {
		t.Fatalf("PUT the keytab: answered %d %s, want 200", r.status, r.body)
	}

	var answered apiv1.SecretRef
	if err := json.Unmarshal(r.body, &answered); err != nil || answered.Version != 1 ||
		answered.Ref == "" {
		t.Fatalf("the set's answer %s: want the reference at version 1, err %v", r.body, err)
	}

	ref, err := entity.ParseShareID(answered.Ref)
	if err != nil {
		t.Fatalf("the answered reference %s: %v", answered.Ref, err)
	}

	row, ok := settingRowOf(t, h, "identity.keytab")
	if !ok {
		t.Fatal("no identity.keytab row, want the one naming the sealed keytab")
	}

	var named entity.SecretRef
	if err := json.Unmarshal([]byte(row), &named); err != nil || named != entity.SecretRef(ref) {
		t.Fatalf("the identity.keytab row %s: want the answered reference, err %v", row, err)
	}

	sec, err := h.Store().ControlPlane().Secret(t.Context(), named, 0)

	if err != nil || sec.Kind != entity.SecretKeytab || sec.Version != 1 {
		t.Fatalf("the sealed keytab: %+v err %v, want version 1 of the keytab kind", sec, err)
	}
}

// settingRowOf reads one installation row of key through the harness's
// store: its JSON value, and whether a row is stored at all.
func settingRowOf(t *testing.T, h *nodetest.Harness, key string) (string, bool) {
	t.Helper()

	cp := h.Store().ControlPlane()
	installation := entity.SettingScope{Kind: entity.ScopeInstallation}

	for st, err := range cp.Settings(context.Background(), installation) {
		if err != nil {
			t.Fatalf("read the installation settings: %v", err)
		}

		if st.Key == key {
			return string(st.Value), true
		}
	}

	return "", false
}

// sealedVersions is how many sealed secret versions the harness's raw KV
// holds.
func sealedVersions(t *testing.T, raw kv.KV) int {
	t.Helper()

	n := 0

	if err := raw.View(t.Context(), func(r kv.Reader) error {
		for _, err := range r.Scan([]byte{keys.KindSEC}, nil) {
			if err != nil {
				return err
			}

			n++
		}

		return nil
	}); err != nil {
		t.Fatalf("count the sealed versions: %v", err)
	}

	return n
}

// An operator's write of the bind credential is refused by the table the
// node really composes, the real directory section's admin-only role, and
// nothing is sealed into the store: no sealed version and no row.
func TestOperatorCannotWriteASettingSecretOnTheNode(t *testing.T) {
	var raw kv.KV

	newNode := func(cfg node.Config) (*node.Node, error) {
		raw = cfg.KV

		return node.NewAuthenticating(caller(entity.RoleOperator))(cfg)
	}

	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(newNode))
	putSetting(t, h, "identity.directory.address", `"ldaps://dc.corp.example"`)
	started(t, h)
	addr, _ := h.Node().Addrs()

	before := sealedVersions(t, raw)
	posted := send(t.Context(), addr, http.MethodPut, "/api/v1/settings/"+bindCredentialKey,
		`{"spec":{"value":"operator-node-canary"}}`, "If-None-Match", "*")
	r := answeredIn(t, "PUT the bind credential as an operator", posted)

	var problem apiv1.Problem
	if err := json.Unmarshal(r.body, &problem); err != nil {
		t.Fatalf("the refusal %s: %v", r.body, err)
	}

	if r.status != http.StatusForbidden || problem.Code != "forbidden" ||
		problem.Field != bindCredentialKey {
		t.Fatalf("PUT the bind credential as an operator: answered %d %s, "+
			"want 403 forbidden naming the key",
			r.status,
			r.body)
	}

	if after := sealedVersions(t, raw); after != before {
		t.Errorf("sealed versions after the refused write: %d, "+
			"want the %d there were", after, before)
	}

	if _, ok := settingRowOf(t, h, bindCredentialKey); ok {
		t.Error("the refused write wrote the credential's row")
	}
}
