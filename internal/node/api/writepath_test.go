package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/bytesize"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/settings"
)

// remoteFields is a remote store's section as these tests declare it: its
// two required location keys, its credential reference, which a create
// states once sealed, and a bound target.
type remoteFields struct {
	Endpoint   string           `json:"endpoint"   class:"live"  location:"store"`
	Bucket     string           `json:"bucket"     class:"bound" location:"store"`
	Credential entity.SecretRef `json:"credential" class:"live"  location:"store"`
	Target     int64            `json:"target"     class:"bound"`
}

func (remoteFields) Validate() error {
	return nil
}

// namespaceFields is a namespace's section as these tests declare it: one
// required bound key, two more at a default, and a next-write key.
type namespaceFields struct {
	Level  int               `json:"level"  class:"bound"`
	Prefix string            `json:"prefix" class:"bound"`
	Target bytesize.ByteSize `json:"target" class:"bound"`
	Codec  string            `json:"codec"  class:"next write"`
}

func (namespaceFields) Validate() error {
	return nil
}

// shareFields is a share-scope section as these tests declare it: one bound
// key, held at the share's creation.
type shareFields struct {
	Digest string `json:"digest" class:"bound"`
}

func (shareFields) Validate() error {
	return nil
}

// writeTable is the table the write path's tests declare: a remote store's
// section, its credential required as the record's whole, a namespace's and
// a share's. No plan's API declares a namespace section yet, so the tests own
// their keys.
func writeTable() settings.Table {
	return settings.Table{
		settings.Of("remote", settings.At(entity.ScopeRemote), remoteFields{Target: 4194304},
			settings.Required("Endpoint", "Bucket", "Credential"),
			settings.SecretRef("Credential", entity.SecretRemoteCredential)),
		settings.Of("ns", settings.At(entity.ScopeNamespace),
			namespaceFields{Level: 3, Target: 4194304, Codec: "none"}, settings.Required("Prefix")),
		settings.Of("sh", settings.At(entity.ScopeShare), shareFields{Digest: "sha256"}),
	}
}

// credSealer is the secret provider as the API seals with it: each kind under
// the role the provider derives from it, the storage role a remote store's
// credential, as the node's own sealer hands it in.
type credSealer struct{ p *secrets.Provider }

func (s credSealer) Seal(
	ctx context.Context,
	kind entity.SecretKind,
	owner entity.SecretOwner,
	ref entity.SecretRef,
	value []byte,
	handedOver bool,
) (entity.Secret, error) {
	role := secrets.Storage

	switch kind {
	case entity.SecretNTHash,
		entity.SecretKeytab,
		entity.SecretMachinePassword,
		entity.SecretDirectoryCredential:
		role = secrets.Protocol
	}

	return s.p.Seal(ctx, role, kind, owner, ref, value, secrets.Meta{HandedOver: handedOver})
}

// storeSealer is a real secret provider over d's store, one key file per
// role, as the node hands its own provider in.
func storeSealer(t *testing.T, d *api.Deps) credSealer {
	t.Helper()

	const passphrase = "write path test passphrase"

	t.Setenv("DITTOFS_ENCRYPTION_PASSPHRASE", passphrase)

	dir := t.TempDir()
	keys := map[secrets.Role]keyprovider.KeyProvider{}

	for _, role := range []secrets.Role{secrets.Protocol, secrets.Storage} {
		b, err := keyprovider.GenerateKeyFile(passphrase)
		if err != nil {
			t.Fatalf("generate the %s key file: %v", role, err)
		}

		path := filepath.Join(dir, role.String()+".key")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatalf("write the %s key file: %v", role, err)
		}

		kp, err := keyprovider.NewProvider(t.Context(), keyprovider.Config{
			Kind: keyprovider.KindLocal,
			File: path,
		})
		if err != nil {
			t.Fatalf("open the %s key file: %v", role, err)
		}

		t.Cleanup(func() { _ = kp.Close() })

		keys[role] = kp
	}

	p, err := secrets.New(d.Store.(kvstore.ControlPlane), keys)
	if err != nil {
		t.Fatal(err)
	}

	return credSealer{p}
}

// writeServe serves the API as an operator over the write path's table and a
// real sealer, with each further edit on top.
func writeServe(t *testing.T, edit ...func(*api.Deps)) *served {
	t.Helper()

	edits := append([]func(*api.Deps){func(d *api.Deps) {
		d.Settings = writeTable()
		d.Sealer = storeSealer(t, d)
	}}, edit...)

	return serve(t, as(entity.RoleOperator, false), edits...)
}

// apiStore creates a remote store through the API under the settings
// settings names and no credential, as a table that declares none or names
// one not required takes it, and returns its ID as the paths take it.
func (s *served) apiStore(t *testing.T, settings string) string {
	t.Helper()

	return s.createStore(t, `{"spec":{"settings":`+settings+`}}`)
}

// apiCredentialStore creates a remote store through the API carrying its
// credential's value, sealed beside the reference row the table requires,
// and returns its ID as the paths take it.
func (s *served) apiCredentialStore(t *testing.T, settings string) string {
	t.Helper()

	body := `{"spec":{"settings":` + settings + `,"credential":{"value":"czMtc2VjcmV0"}}}`

	return s.createStore(t, body)
}

// createStore posts body to /api/v1/stores, whose 201's Location names the
// store's ID, the path's spelling.
func (s *served) createStore(t *testing.T, body string) string {
	t.Helper()

	r := s.do(t, http.MethodPost, "/api/v1/stores", body)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /stores %s: answered %d %s, want 201", body, r.status, r.body)
	}

	return strings.TrimPrefix(r.header.Get("Location"), "/api/v1/stores/")
}

// apiNamespace creates a namespace on the store through the API with the
// settings settings, and returns its ID as the paths take it.
func (s *served) apiNamespace(t *testing.T, store, settings string) string {
	t.Helper()

	body := `{"spec":{"store":"` + store + `","settings":` + settings + `}}`

	r := s.do(t, http.MethodPost, "/api/v1/namespaces", body, "Idempotency-Key", "ns-create")
	if r.status != http.StatusCreated {
		t.Fatalf("POST /namespaces %s: answered %d %s, want 201", settings, r.status, r.body)
	}

	return strings.TrimPrefix(r.header.Get("Location"), "/api/v1/namespaces/")
}

// settingRow reads one row at scope through the store: its JSON value, and
// whether a row is stored at all.
func (s *served) settingRow(t *testing.T, scope entity.SettingScope, key string) (string, bool) {
	t.Helper()

	for st, err := range s.store.Settings(t.Context(), scope) {
		if err != nil {
			t.Fatalf("read the settings at %v: %v", scope, err)
		}

		if st.Key == key {
			return string(st.Value), true
		}
	}

	return "", false
}

// idOf is s as the 16 bytes an ID holds.
func idOf(t *testing.T, s string) [16]byte {
	t.Helper()

	id, err := entity.ParseShareID(s)
	if err != nil {
		t.Fatalf("ID %s: %v", s, err)
	}

	return id
}

// A store's bound key changes on a store no namespace names, and is
// refused once one does, 409 bound_setting, leaving the stored row.
func TestStoreBoundKeyRefusedWhileNamed(t *testing.T) {
	s := writeServe(t)
	st := s.apiCredentialStore(t, `{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`)

	r := s.do(t, http.MethodPut, "/api/v1/stores/"+st+"/settings/remote.target",
		`{"spec":{"value":8388608}}`, "If-None-Match", "*")
	if r.status != http.StatusOK {
		t.Fatalf(
			"PUT remote.target on a store no namespace names: answered %d %s, want 200",
			r.status,
			r.body,
		)
	}

	tag := r.header.Get("ETag")

	id, err := entity.ParseShareID(st)
	if err != nil {
		t.Fatal(err)
	}

	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: id}
	if _, err := s.store.PutNamespace(t.Context(), ns, 0); err != nil {
		t.Fatalf("name the store: %v", err)
	}

	r = s.do(t, http.MethodPut, "/api/v1/stores/"+st+"/settings/remote.target",
		`{"spec":{"value":16777216}}`, "If-Match", tag)
	if p := r.problem(t); r.status != http.StatusConflict || p.Code != "bound_setting" ||
		p.Field != "remote.target" {
		t.Fatalf("PUT remote.target on named store %d %s: want 409 bound_setting", r.status, r.body)
	}

	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: id}
	if v, ok := s.settingRow(t, scope, "remote.target"); !ok || v != "8388608" {
		t.Errorf("remote.target %q after the refusal, want the stored row 8388608", v)
	}
}

// apartStore writes apart on the write it is armed for: on the first run of
// the armed write's function it commits a namespace naming its store in a
// write of its own, so the armed write's commit conflicts with what it read
// and its function runs again.
type apartStore struct {
	kvstore.ControlPlane
	armed   bool
	storeID [16]byte
	runs    *int
}

func (a *apartStore) Write(
	ctx context.Context,
	meta entity.WriteMeta,
	fn func(entity.ControlPlaneTx) error,
) error {
	if !a.armed {
		return a.ControlPlane.Write(ctx, meta, fn)
	}

	a.armed = false

	return a.ControlPlane.Write(ctx, meta, func(tx entity.ControlPlaneTx) error {
		*a.runs++

		if err := fn(tx); err != nil {
			return err
		}

		if *a.runs == 1 {
			_, err := a.PutNamespace(ctx,
				entity.Namespace{ID: entity.NewNamespaceID(), Store: a.storeID}, 0)

			return err
		}

		return nil
	})
}

// A namespace written between a bound change's read and its commit, read
// tracked, makes the change retry, and what the retry finds refuses it.
func TestStoreBoundChangeRetries(t *testing.T) {
	apart := &apartStore{runs: new(int)}
	s := writeServe(t, func(d *api.Deps) {
		apart.ControlPlane = d.Store.(kvstore.ControlPlane)
		d.Store = apart
	})
	st := s.apiCredentialStore(
		t,
		`{"remote.endpoint":"https://s3.example","remote.bucket":"b1","remote.target":8388608}`,
	)

	id := idOf(t, st)
	apart.armed = true
	apart.storeID = id

	tag := s.do(
		t,
		http.MethodGet,
		"/api/v1/stores/"+st+"/settings/remote.target",
		"",
	).header.Get(
		"ETag",
	)
	r := s.do(t, http.MethodPut, "/api/v1/stores/"+st+"/settings/remote.target",
		`{"spec":{"value":16777216}}`, "If-Match", tag)

	p := r.problem(t)

	if r.status != http.StatusConflict || p.Code != "bound_setting" || p.Field != "remote.target" {
		t.Fatalf("PUT remote.target against a namespace written apart: %d %s", r.status, r.body)
	}

	if *apart.runs != 2 {
		t.Errorf("the write's function ran %d times, want exactly 2", *apart.runs)
	}

	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: id}
	if v, ok := s.settingRow(t, scope, "remote.target"); !ok || v != "8388608" {
		t.Errorf("remote.target %q after the refused change, want the stored row 8388608", v)
	}
}

// A namespace's bound key is refused whether or not a share names it, since
// the namespace's blocks may still drain after its last share goes, and no
// row of the key is stored by the refused writes.
func TestNamespaceBoundKeyAlwaysRefused(t *testing.T) {
	s := writeServe(t)
	st := s.apiCredentialStore(t, `{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`)
	ns := s.apiNamespace(t, st, `{"ns.prefix":"p/"}`)

	path := "/api/v1/namespaces/" + ns + "/settings/ns.target"
	r := s.do(t, http.MethodPut, path, `{"spec":{"value":8388608}}`, "If-None-Match", "*")
	p := r.problem(t)

	if r.status != http.StatusConflict || p.Code != "bound_setting" || p.Field != "ns.target" {
		t.Fatalf("PUT ns.target with no share naming it: %d %s, want 409", r.status, r.body)
	}

	if _, loc := s.createShare(t, "data", ns); loc == "" {
		t.Fatal("the share was not created")
	}

	r = s.do(t, http.MethodPut, path, `{"spec":{"value":16777216}}`, "If-None-Match", "*")
	p = r.problem(t)

	if r.status != http.StatusConflict || p.Code != "bound_setting" || p.Field != "ns.target" {
		t.Fatalf("PUT ns.target with a share naming it: %d %s, want 409", r.status, r.body)
	}

	nsScope := entity.SettingScope{Kind: entity.ScopeNamespace, ID: idOf(t, ns)}
	if v, ok := s.settingRow(t, nsScope, "ns.target"); ok {
		t.Errorf("ns.target %q stored by a refused write, want no row", v)
	}
}

// A share-scope bound key is not read as "no content": its scope holds no
// rule anyone can ask, so the write is answered as a store error, 500 with
// no code, and stores nothing.
func TestShareBoundKeyIsAStoreError(t *testing.T) {
	s := writeServe(t)
	ns := s.namespace(t)
	share, _ := s.createShare(t, "data", ns)

	before, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	path := "/api/v1/shares/" + uuid.UUID(share).String() + "/settings/sh.digest"
	r := s.do(t, http.MethodPut, path, `{"spec":{"value":"blake3"}}`, "If-None-Match", "*")

	if r.status != http.StatusInternalServerError {
		t.Fatalf("PUT sh.digest: answered %d %s, want 500 with no code", r.status, r.body)
	}

	if p := r.problem(t); p.Code != "" || p.Detail != "internal error" {
		t.Errorf(
			"PUT sh.digest answered code %q detail %q, want no code and an internal error",
			p.Code,
			p.Detail,
		)
	}

	after, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	if after != before {
		t.Errorf("the refused write moved the settings generation: got %v after %v", after, before)
	}

	shareScope := entity.SettingScope{Kind: entity.ScopeShare, ID: share}
	if v, ok := s.settingRow(t, shareScope, "sh.digest"); ok {
		t.Errorf("sh.digest %q stored by a refused write, want no row", v)
	}
}

// A store's create carrying its credential takes no idempotency key: the
// key is refused 400, and the node records neither the digest nor the
// answer, so no retry under it is answered from a record.
func TestStoreCreateRefusesAnIdempotencyKey(t *testing.T) {
	account := entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}
	s := serve(t, func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: account, Role: entity.RoleOperator}, nil
	}, func(d *api.Deps) { d.Settings = writeTable() })

	before, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	spec := `{"settings":{"remote.endpoint":"https://s3.example"},` +
		`"credential":{"value":"czMtc2VjcmV0"}}`
	r := s.do(t, http.MethodPost, "/api/v1/stores", `{"spec":`+spec+`}`, "Idempotency-Key", "k1")

	if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "Idempotency-Key" {
		t.Fatalf(
			"the keyed create: answered %d %s, want 400 naming Idempotency-Key",
			r.status,
			r.body,
		)
	}

	after, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	if after != before {
		t.Errorf("the refused keyed create wrote: got %v after %v", after, before)
	}

	record := entity.IdempotencyRecord{Account: account, Key: "k1"}
	if err := s.store.Replay(t.Context(), record); err != nil {
		t.Errorf("the refused keyed create kept a record for k1: %v", err)
	}
}

// The create's own step refuses what it must before anything is written: a
// credential naming its reference or its version, one with no value, and a
// value with no sealer to seal it. Each writes nothing: no counter moves.
func TestStoreCreateRefusedBeforeItsWrite(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) { d.Settings = writeTable() })

	for _, tc := range []struct {
		name   string
		spec   string
		status int
		field  string
	}{
		{
			"a credential naming a reference",
			`{"credential":{"ref":"se_1","value":"czMtc2VjcmV0"}}`,
			http.StatusBadRequest,
			"spec.credential",
		},
		{
			"a credential naming a version",
			`{"credential":{"version":1,"value":"czMtc2VjcmV0"}}`,
			http.StatusBadRequest,
			"spec.credential",
		},
		{
			"a credential with no value",
			`{"credential":{}}`,
			http.StatusBadRequest,
			"spec.credential.value",
		},
		{
			"a value with no sealer",
			`{"credential":{"value":"czMtc2VjcmV0"}}`,
			http.StatusServiceUnavailable,
			"",
		},
	} {
		before, err := s.nodes.Counters(t.Context())
		if err != nil {
			t.Fatalf("read the counters: %v", err)
		}

		r := s.do(t, http.MethodPost, "/api/v1/stores", `{"spec":`+tc.spec+`}`)

		if r.status != tc.status {
			t.Fatalf("%s: answered %d %s, want %d", tc.name, r.status, r.body, tc.status)
		}

		if tc.field != "" && r.problem(t).Field != tc.field {
			t.Errorf("%s: field %q, want %s", tc.name, r.problem(t).Field, tc.field)
		}

		after, err := s.nodes.Counters(t.Context())
		if err != nil {
			t.Fatalf("read the counters: %v", err)
		}

		if after != before {
			t.Errorf("%s: wrote something: got %v after %v", tc.name, after, before)
		}
	}
}

// A create is judged as one record: a namespace that sends a required key
// beside another is created with both rows stored, and one that omits the
// required key is refused under the first key judged, writing nothing.
func TestCreateJudgedAsAWholeRecord(t *testing.T) {
	s := writeServe(t)
	st := s.apiCredentialStore(t, `{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`)

	ns := s.apiNamespace(t, st, `{"ns.codec":"zstd","ns.prefix":"p/"}`)

	scope := entity.SettingScope{Kind: entity.ScopeNamespace, ID: idOf(t, ns)}
	rows := map[string]string{"ns.codec": `"zstd"`, "ns.prefix": `"p/"`}

	for key, want := range rows {
		if v, ok := s.settingRow(t, scope, key); !ok || v != want {
			t.Errorf("%s %q after the create, want %s", key, v, want)
		}
	}

	before, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	r := s.do(
		t,
		http.MethodPost,
		"/api/v1/namespaces",
		`{"spec":{"store":"`+st+`","settings":{"ns.codec":"zstd"}}}`,
		"Idempotency-Key",
		"ns-create-2",
	)
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "required" ||
		p.Field != "ns.codec" {
		t.Fatalf(
			"the create without ns.prefix: answered %d %s, want 400 required naming ns.codec",
			r.status,
			r.body,
		)
	}

	after, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	if after != before {
		t.Errorf("the refused create wrote: got %v after %v", after, before)
	}
}

// A namespace and the store it names are each deleted once nothing names
// them, their settings removed with their scope, never refused as a change
// of the bound or location keys they hold.
func TestStoreAndNamespaceDeletedWithTheirSettings(t *testing.T) {
	s := writeServe(t)
	st := s.apiCredentialStore(
		t,
		`{"remote.endpoint":"https://s3.example","remote.bucket":"b1","remote.target":8388608}`,
	)
	ns := s.apiNamespace(t, st, `{"ns.prefix":"p/","ns.codec":"zstd"}`)

	nsTag := s.do(t, http.MethodGet, "/api/v1/namespaces/"+ns, "").header.Get("ETag")
	stTag := s.do(t, http.MethodGet, "/api/v1/stores/"+st, "").header.Get("ETag")
	nsID, stID := idOf(t, ns), idOf(t, st)

	r := s.do(
		t,
		http.MethodDelete,
		"/api/v1/namespaces/"+ns,
		"",
		"If-Match",
		nsTag,
		"Idempotency-Key",
		"del-ns",
	)
	if r.status != http.StatusOK {
		t.Fatalf(
			"DELETE the named-by-nothing namespace: answered %d %s, want 200",
			r.status,
			r.body,
		)
	}

	r = s.do(
		t,
		http.MethodDelete,
		"/api/v1/stores/"+st,
		"",
		"If-Match",
		stTag,
		"Idempotency-Key",
		"del-st",
	)
	if r.status != http.StatusOK {
		t.Fatalf("DELETE store: answered %d %s, want 200", r.status, r.body)
	}

	for _, sc := range []entity.SettingScope{
		{Kind: entity.ScopeNamespace, ID: nsID},
		{Kind: entity.ScopeRemote, ID: stID},
	} {
		for row, err := range s.store.Settings(t.Context(), sc) {
			if err != nil {
				t.Fatalf("read the settings at %v: %v", sc, err)
			}

			t.Errorf("%s at %v outlived its scope's removal", row.Key, sc)
		}
	}

	if n, ok, err := s.store.Namespace(t.Context(), entity.NamespaceID(nsID)); err != nil || ok {
		t.Errorf("the deleted namespace reads as %+v ok %v err %v, want none", n, ok, err)
	}
}

// A namespace's record holds, in its Bound, every bound key its section
// declares, the ones the create sends with the value sent and the others
// at their declared default, the schema its rows carry, and no other key.
func TestNamespaceBoundRecorded(t *testing.T) {
	s := writeServe(t)
	st := s.apiCredentialStore(t, `{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`)
	ns := s.apiNamespace(t, st, `{"ns.prefix":"p/","ns.target":"256KiB","ns.codec":"zstd"}`)

	n, ok, err := s.store.Namespace(t.Context(), entity.NamespaceID(idOf(t, ns)))
	if err != nil || !ok {
		t.Fatalf("read the namespace: ok %v err %v", ok, err)
	}

	var rec struct {
		Schema   uint32                     `json:"schema"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(n.Bound, &rec); err != nil {
		t.Fatalf("the namespace's Bound %s: %v", n.Bound, err)
	}

	if rec.Schema != 0 {
		t.Errorf("the recorded schema %d, want the rows' 0", rec.Schema)
	}

	want := map[string]string{
		"ns.level":  "3",
		"ns.prefix": `"p/"`,
		"ns.target": "262144",
	}
	if len(rec.Settings) != len(want) {
		t.Errorf("the recorded keys %v, want exactly %v", rec.Settings, want)
	}

	for k, v := range want {
		if string(rec.Settings[k]) != v {
			t.Errorf("the recorded %s = %s, want %s", k, rec.Settings[k], v)
		}
	}
}

// Two creates that send the same bound values in another order and another
// spelling record byte-equal Bound, since each value is what the decode
// gives, encoded once.
func TestNamespaceBoundOneEncoding(t *testing.T) {
	s := writeServe(t)
	st := s.apiCredentialStore(t, `{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`)
	first := s.apiNamespace(t, st, `{"ns.prefix":"p/","ns.target":262144}`)
	second := s.apiNamespace(t, st, `{"ns.target":"256KiB","ns.prefix":"p/"}`)

	n, ok, err := s.store.Namespace(t.Context(), entity.NamespaceID(idOf(t, first)))
	if err != nil || !ok {
		t.Fatalf("read the first namespace: ok %v err %v", ok, err)
	}

	m, ok, err := s.store.Namespace(t.Context(), entity.NamespaceID(idOf(t, second)))
	if err != nil || !ok {
		t.Fatalf("read the second namespace: ok %v err %v", ok, err)
	}

	if !bytes.Equal(n.Bound, m.Bound) {
		t.Errorf("the same bound values record\n got %s\nwant %s", m.Bound, n.Bound)
	}
}

// emptySetFields is a namespace section that declares no bound key, only a
// next-write one.
type emptySetFields struct {
	Codec string `json:"codec" class:"next write"`
}

func (emptySetFields) Validate() error {
	return nil
}

// A namespace section that declares no bound key records an empty set,
// never an absent Bound.
func TestNamespaceBoundEmptySet(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) {
		d.Settings = settings.Table{
			settings.Of("remote", settings.At(entity.ScopeRemote), remoteFields{Target: 4194304},
				settings.Required("Endpoint", "Bucket")),
			settings.Of("nb", settings.At(entity.ScopeNamespace), emptySetFields{Codec: "none"}),
		}
	})
	st := s.apiStore(t, `{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}`)
	ns := s.apiNamespace(t, st, `{"nb.codec":"zstd"}`)

	n, ok, err := s.store.Namespace(t.Context(), entity.NamespaceID(idOf(t, ns)))
	if err != nil || !ok {
		t.Fatalf("read the namespace: ok %v err %v", ok, err)
	}

	if string(n.Bound) != `{"schema":0,"settings":{}}` {
		t.Errorf("the empty set records %s, want {\"schema\":0,\"settings\":{}}", n.Bound)
	}
}

// A store's create carrying its credential seals its first version and
// commits it beside the reference row the settings table judges, sealed
// under the store's own ID, which the step makes before it seals: the answer
// carries the credential as the reference and version 1 alone, neither the
// value nor its base64 reaches any answer or any log line, a later set over
// it is refused 409 and writes no second version, and a create whose record
// omits the credential writes nothing.
func TestStoreCreatedWithItsCredential(t *testing.T) {
	s := writeServe(t)
	spec := `{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b1"},` +
		`"credential":{"value":"czMtc2VjcmV0"}}`

	r := s.do(t, http.MethodPost, "/api/v1/stores", `{"spec":`+spec+`}`)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /stores with its credential: answered %d %s, want 201", r.status, r.body)
	}

	var got apiv1.Store
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("the created store %s: %v", r.body, err)
	}

	if got.Spec.Credential == nil || got.Spec.Credential.Value != nil ||
		got.Spec.Credential.Ref == "" || got.Spec.Credential.Version != 1 {
		t.Fatalf(
			"the created store's credential %+v, want its reference alone at version 1",
			got.Spec.Credential,
		)
	}

	st := strings.TrimPrefix(r.header.Get("Location"), "/api/v1/stores/")
	id := idOf(t, st)
	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: id}

	row, ok := s.settingRow(t, scope, "remote.credential")
	if !ok {
		t.Fatal("no credential row stored, want the one naming the sealed secret")
	}

	var ref entity.SecretRef
	if err := json.Unmarshal([]byte(row), &ref); err != nil {
		t.Fatalf("the credential row %s: %v", row, err)
	}

	if uuid.UUID(ref).String() != got.Spec.Credential.Ref {
		t.Errorf(
			"the credential row names %s, want the answered reference %s",
			uuid.UUID(ref).String(),
			got.Spec.Credential.Ref,
		)
	}

	sec, err := s.store.Secret(t.Context(), ref, 0)
	if err != nil {
		t.Fatalf("read the sealed credential: %v", err)
	}

	owner := entity.SecretOwner{Scope: scope, Key: "remote.credential"}
	if sec.Version != 1 || sec.Kind != entity.SecretRemoteCredential || !sec.Owner.Equal(owner) {
		t.Errorf(
			"the sealed credential: version %d kind %s, want version 1, the store's own",
			sec.Version,
			sec.Kind,
		)
	}

	leaked := map[string][]byte{"the answer": r.body, "the log": []byte(s.log.String())}

	for name, body := range leaked {
		if bytes.Contains(body, []byte("s3-secret")) ||
			bytes.Contains(body, []byte("czMtc2VjcmV0")) {
			t.Errorf("the create holds the credential's value in %s", name)
		}
	}

	r = s.do(t, http.MethodPut, "/api/v1/stores/"+st+"/credential",
		`{"spec":{"value":"bmV3"}}`, "If-Match", `"1"`)
	if p := r.problem(t); r.status != http.StatusConflict || p.Code != "exist" ||
		p.Field != "remote.credential" {
		t.Fatalf(
			"PUT the credential over the created one: answered %d %s, want 409 exist",
			r.status, r.body,
		)
	}

	if sec, err := s.store.Secret(t.Context(), ref, 0); err != nil || sec.Version != 1 {
		t.Errorf(
			"the credential after the refused set: version %d err %v, want 1",
			sec.Version,
			err,
		)
	}

	before, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	r = s.do(t, http.MethodPost, "/api/v1/stores",
		`{"spec":{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b1"}}}`)
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "required" ||
		p.Field != "remote.bucket" {
		t.Fatalf(
			"POST /stores with no credential: answered %d %s, want 400 required",
			r.status, r.body,
		)
	}

	after, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read the counters: %v", err)
	}

	if after != before {
		t.Errorf("the create without its credential wrote: got %v after %v", after, before)
	}
}

// A credential's value reaches no answer: the create's 201 and the store's
// own GET answer the credential as its reference and current version, those
// two fields alone, and the create's audit record holds it as "set", never
// the value nor its base64.
func TestStoreCredentialValueNeverAnswered(t *testing.T) {
	s := writeServe(t)

	create := `{"spec":{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b1"},` +
		`"credential":{"value":"czMtc2VjcmV0"}}}`

	r := s.do(t, http.MethodPost, "/api/v1/stores", create)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /stores with its credential: answered %d %s, want 201", r.status, r.body)
	}

	st := strings.TrimPrefix(r.header.Get("Location"), "/api/v1/stores/")

	get := s.do(t, http.MethodGet, "/api/v1/stores/"+st, "")
	if get.status != http.StatusOK {
		t.Fatalf("GET the created store: answered %d %s, want 200", get.status, get.body)
	}

	for _, body := range []struct {
		name string
		body []byte
	}{{"the create's 201", r.body}, {"the store's GET", get.body}} {
		var doc struct {
			Spec struct {
				Credential map[string]json.RawMessage `json:"credential"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(body.body, &doc); err != nil {
			t.Fatalf("%s: %v", body.name, err)
		}

		if len(doc.Spec.Credential) != 2 {
			t.Errorf(
				"%s answers the credential %s, want exactly the reference and the version",
				body.name,
				body.body,
			)
		}

		var version json.Number
		if v, ok := doc.Spec.Credential["version"]; !ok || json.Unmarshal(v, &version) != nil ||
			version.String() != "1" {
			t.Errorf("%s answers the credential's version %s, want 1", body.name, body.body)
		}

		if bytes.Contains(body.body, []byte("s3-secret")) ||
			bytes.Contains(body.body, []byte("czMtc2VjcmV0")) {
			t.Errorf("%s answers the credential's value: %s", body.name, body.body)
		}

		row, ok := s.settingRow(t, entity.SettingScope{Kind: entity.ScopeRemote, ID: idOf(t, st)},
			"remote.credential")
		if !ok {
			t.Fatal("no credential row stored, want the one naming the sealed secret")
		}

		var ref entity.SecretRef
		if err := json.Unmarshal([]byte(row), &ref); err != nil {
			t.Fatalf("the credential row %s: %v", row, err)
		}

		var want string
		if v, ok := doc.Spec.Credential["ref"]; ok {
			_ = json.Unmarshal(v, &want)
		}

		if want != uuid.UUID(ref).String() {
			t.Errorf(
				"%s answers the reference %s, want the row's %s",
				body.name,
				want,
				uuid.UUID(ref).String(),
			)
		}
	}

	as := s.audits(t)
	if len(as) == 0 {
		t.Fatal("the create wrote no audit record")
	}

	change := as[len(as)-1].Change

	if !bytes.Contains(change, []byte(`"spec.credential.value":"set"`)) {
		t.Errorf(
			"the create's audit change %s holds no credential, want spec.credential.value set",
			change,
		)
	}

	if bytes.Contains(change, []byte("s3-secret")) ||
		bytes.Contains(change, []byte("czMtc2VjcmV0")) {
		t.Errorf("the create's audit change %s holds the credential's value", change)
	}
}
