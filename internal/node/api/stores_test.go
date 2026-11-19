package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

var installation = entity.SettingScope{Kind: entity.ScopeInstallation}

// putSetting writes a setting through the store and returns its generation,
// as a write the settings table admitted would leave it.
func (s *served) putSetting(t *testing.T, scope entity.SettingScope, key, value string) uint64 {
	t.Helper()
	old := uint64(0)
	for st, err := range s.store.Settings(t.Context(), scope) {
		if err != nil {
			t.Fatalf("read settings at %v: %v", scope, err)
		}
		if st.Key == key {
			old = st.Generation
		}
	}
	gen, err := s.store.PutSetting(t.Context(), entity.Setting{Scope: scope, Key: key, Value: []byte(value)}, old)
	if err != nil {
		t.Fatalf("put setting %s at %v: %v", key, scope, err)
	}
	return gen
}

// remoteStore writes a remote store through the store, its endpoint alone,
// as the store routes, which fail closed until the settings table lands,
// would; it returns its ID.
func (s *served) remoteStore(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	s.putSetting(t, entity.SettingScope{Kind: entity.ScopeRemote, ID: id}, "remote.endpoint", `"https://s3.example"`)
	return id
}

// written is what a refused write must leave as it found it: every counter
// a committed write raises, every namespace, and the settings at the scopes
// the test writes at.
func (s *served) written(t *testing.T, scopes ...entity.SettingScope) string {
	t.Helper()
	c, err := s.nodes.Counters(t.Context())
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	out := map[string]any{"counters": c}
	var nss []entity.Namespace
	for n, err := range s.store.Namespaces(t.Context(), entity.NamespaceID{}) {
		if err != nil {
			t.Fatalf("list namespaces: %v", err)
		}
		nss = append(nss, n)
	}
	out["namespaces"] = nss
	for i, sc := range scopes {
		var ss []entity.Setting
		for st, err := range s.store.Settings(t.Context(), sc) {
			if err != nil {
				t.Fatalf("read settings at %v: %v", sc, err)
			}
			ss = append(ss, st)
		}
		out[strconv.Itoa(i)] = ss
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every settings-backed write, of a setting at two scopes, a setting's
// delete, an adapter, a remote store, a namespace and a store's credential,
// is refused 400 naming the first key the empty settings table judges, as
// operator and as admin, and commits nothing: no counter moves, no
// namespace or setting appears or goes, and its audit record is a refusal.
func TestSettingsBackedWritesRefusedByTheEmptyTable(t *testing.T) {
	node := uuid.New()
	for _, role := range []entity.APIRole{entity.RoleOperator, entity.RoleAdmin} {
		t.Run(role.String(), func(t *testing.T) { settingsBackedRefused(t, role, node) })
	}
}

func settingsBackedRefused(t *testing.T, role entity.APIRole, node uuid.UUID) {
	s := serve(t, as(role, false))
	st := s.remoteStore(t)
	stScope := entity.SettingScope{Kind: entity.ScopeRemote, ID: st}
	knob := s.putSetting(t, installation, "gc.schedule", `"daily"`)
	cred := entity.SecretRef(uuid.New())
	s.putSetting(t, stScope, "remote.credential", strconv.Quote(uuid.UUID(cred).String()))
	if err := s.store.PutSecret(t.Context(), entity.Secret{ID: cred, Kind: entity.SecretRemoteCredential, Version: 1,
		Owner: entity.SecretOwner{Scope: stScope, Key: "remote.credential"}, Value: []byte("sealed")}, 0); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	ids := uuid.UUID(st).String()
	if _, err := s.db.RaiseNodeEpoch(t.Context(), entity.NodeID(node)); err != nil {
		t.Fatalf("register node: %v", err)
	}
	emptyNS := s.namespace(t)
	for _, tc := range []struct {
		name, method, path, body, key string
		hdr                           []string
	}{
		{"setting at installation scope", http.MethodPut, "/api/v1/settings/adapters.nfs.port", `{"spec":{"value":12049}}`, "adapters.nfs.port",
			[]string{"If-None-Match", "*"}},
		{"setting at node scope", http.MethodPut, "/api/v1/nodes/" + node.String() + "/settings/adapters.nfs.port", `{"spec":{"value":2049}}`, "adapters.nfs.port",
			[]string{"If-None-Match", "*"}},
		{"setting's delete", http.MethodDelete, "/api/v1/settings/gc.schedule", "", "gc.schedule",
			[]string{"If-Match", strconv.Quote(strconv.FormatUint(knob, 10)), "Idempotency-Key", "del-knob"}},
		{"adapter", http.MethodPut, "/api/v1/adapters/smb", `{"spec":{"enabled":true,"settings":{"port":12445}}}`, "adapters.smb.enabled",
			[]string{"If-Match", `"0"`}},
		{"remote store", http.MethodPost, "/api/v1/stores", `{"spec":{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b"}}}`, "remote.bucket",
			[]string{"Idempotency-Key", "store"}},
		{"namespace", http.MethodPost, "/api/v1/namespaces", `{"spec":{"store":"` + ids + `","settings":{"namespace.prefix":"p/"}}}`, "namespace.prefix",
			[]string{"Idempotency-Key", "ns"}},
		{"namespace delete with no setting at its scope", http.MethodDelete, "/api/v1/namespaces/" + emptyNS, "", "namespace",
			[]string{"If-Match", `"1"`, "Idempotency-Key", "del-ns"}},
		{"store's credential delete", http.MethodDelete, "/api/v1/stores/" + ids + "/credential", "", "remote.credential",
			[]string{"If-Match", `"1"`, "Idempotency-Key", "del-cred"}},
	} {
		before, audits := s.written(t, installation, stScope, entity.SettingScope{Kind: entity.ScopeNode, ID: node}), len(s.audits(t))
		r := s.do(t, tc.method, tc.path, tc.body, tc.hdr...)
		if r.status != http.StatusBadRequest {
			t.Fatalf("%s as %s: answered %d %s, want 400", tc.name, role, r.status, r.body)
		}
		if p := r.problem(t); p.Code != "unknown_key" || p.Field != tc.key {
			t.Errorf("%s as %s: code %q field %q, want unknown_key naming %s", tc.name, role, p.Code, p.Field, tc.key)
		}
		if after := s.written(t, installation, stScope, entity.SettingScope{Kind: entity.ScopeNode, ID: node}); after != before {
			t.Errorf("%s as %s: the store changed:\n got %s\nwant %s", tc.name, role, after, before)
		}
		if as := s.audits(t); len(as) != audits+1 || string(as[len(as)-1].Outcome) == "applied" {
			t.Errorf("%s as %s: audit records %q, want one more, a refusal", tc.name, role, outcomes(as))
		}
	}
}

// A remote store reads back with its settings, its endpoint setting's
// generation as its ETag, and its credential as the secret's reference and
// current version, never its value; a store with no endpoint is 404.
func TestStoreCredentialReadsAsReferenceAndVersion(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	st := s.remoteStore(t)
	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: st}
	gen := s.putSetting(t, scope, "remote.endpoint", `"https://s3.other"`)
	ref := entity.SecretRef(uuid.New())
	s.putSetting(t, scope, "remote.credential", strconv.Quote(uuid.UUID(ref).String()))
	owner := entity.SecretOwner{Scope: scope, Key: "remote.credential"}
	for v := uint64(1); v <= 2; v++ {
		if err := s.store.PutSecret(t.Context(), entity.Secret{ID: ref, Kind: entity.SecretRemoteCredential, Version: v, Owner: owner, Value: []byte("sealed-value-" + strconv.FormatUint(v, 10))}, v-1); err != nil {
			t.Fatalf("put secret version %d: %v", v, err)
		}
	}
	r := s.do(t, http.MethodGet, "/api/v1/stores/"+st.String(), "")
	if r.status != http.StatusOK {
		t.Fatalf("GET store: answered %d %s", r.status, r.body)
	}
	var got apiv1.Store
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("store %s: %v", r.body, err)
	}
	want := &apiv1.SecretRef{Ref: uuid.UUID(ref).String(), Version: 2}
	if got.Spec.Credential == nil || *got.Spec.Credential != *want {
		t.Errorf("store's credential %+v, want %+v", got.Spec.Credential, want)
	}
	if ep := string(got.Spec.Settings["remote.endpoint"]); ep != `"https://s3.other"` || len(got.Spec.Settings) != 1 {
		t.Errorf("store's settings %s, want the endpoint alone", got.Spec.Settings)
	}
	if e := r.header.Get("ETag"); e != strconv.Quote(strconv.FormatUint(gen, 10)) {
		t.Errorf("store's ETag %s, want the endpoint's generation %d", e, gen)
	}
	for _, v := range []string{"sealed-value", "c2VhbGVkLXZhbHVl"} {
		if bytes.Contains(r.body, []byte(v)) {
			t.Errorf("store's body %s holds the sealed value", r.body)
		}
	}
	if r := s.do(t, http.MethodGet, "/api/v1/stores/"+uuid.NewString(), ""); r.status != http.StatusNotFound {
		t.Errorf("GET of a store with no endpoint: answered %d %s, want 404", r.status, r.body)
	}
}

// A remote store a namespace names is refused deletion, 409 not_empty,
// before any of its settings is judged, and keeps its endpoint; once the
// namespace is gone, the same delete reaches the settings table. A
// namespace a share names is refused deletion and kept.
func TestStoreAndNamespaceDeletesRefusedWhileNamed(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	st := s.remoteStore(t)
	ns := entity.NamespaceID(uuid.New())
	gen, err := s.store.PutNamespace(t.Context(), entity.Namespace{ID: ns, Store: st}, 0)
	if err != nil {
		t.Fatalf("put namespace: %v", err)
	}
	path := "/api/v1/stores/" + st.String()
	tag := s.do(t, http.MethodGet, path, "").header.Get("ETag")
	before := s.written(t, entity.SettingScope{Kind: entity.ScopeRemote, ID: st})
	r := s.do(t, http.MethodDelete, path, "", "If-Match", tag, "Idempotency-Key", "del-1")
	if r.status != http.StatusConflict || r.problem(t).Code != "not_empty" {
		t.Fatalf("DELETE of a store a namespace names: answered %d %s, want 409 not_empty", r.status, r.body)
	}
	if after := s.written(t, entity.SettingScope{Kind: entity.ScopeRemote, ID: st}); after != before {
		t.Errorf("a refused store delete changed the store:\n got %s\nwant %s", after, before)
	}

	if err := s.store.DeleteNamespace(t.Context(), ns, gen); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	r = s.do(t, http.MethodDelete, path, "", "If-Match", tag, "Idempotency-Key", "del-2")
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "unknown_key" || p.Field != "remote.endpoint" {
		t.Errorf("DELETE of a store no namespace names: answered %d %s, want the settings table's 400 naming remote.endpoint", r.status, r.body)
	}

	other := s.namespace(t)
	nsPath := "/api/v1/namespaces/" + other
	g := s.do(t, http.MethodGet, nsPath, "")
	if g.status != http.StatusOK {
		t.Fatalf("GET namespace: answered %d %s", g.status, g.body)
	}
	s.createShare(t, "inside", other)
	r = s.do(t, http.MethodDelete, nsPath, "", "If-Match", g.header.Get("ETag"), "Idempotency-Key", "del-ns")
	if p := r.problem(t); r.status != http.StatusConflict || p.Code != "not_empty" || p.Retryable {
		t.Fatalf("DELETE of a namespace a share names: answered %d %s, want 409 not_empty, not retryable", r.status, r.body)
	}
	if g := s.do(t, http.MethodGet, nsPath, ""); g.status != http.StatusOK {
		t.Errorf("a namespace refused deletion: GET answered %d %s, want it kept", g.status, g.body)
	}
}

// A secret's set with no sealer is refused 503 before anything is written:
// no setting names a credential, no counter moves, and the refusal's audit
// record holds no part of the value.
func TestCredentialSetWithoutSealerRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	st := s.remoteStore(t)
	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: st}
	before := s.written(t, scope)
	r := s.do(t, http.MethodPut, "/api/v1/stores/"+st.String()+"/credential", `{"spec":{"value":"aHVudGVyMi1zM2NyM3Q="}}`, "If-None-Match", "*")
	if r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable {
		t.Fatalf("PUT credential with no sealer: answered %d %s, want 503 unavailable", r.status, r.body)
	}
	if after := s.written(t, scope); after != before {
		t.Errorf("a refused credential set changed the store:\n got %s\nwant %s", after, before)
	}
	as := s.audits(t)
	if len(as) == 0 || string(as[len(as)-1].Outcome) == "applied" {
		t.Fatalf("audit records %q, want the refusal", outcomes(as))
	}
	b, _ := json.Marshal(as[len(as)-1])
	for _, v := range []string{"aHVudGVyMi1zM2NyM3Q=", "hunter2-s3cr3t"} {
		if bytes.Contains(b, []byte(v)) {
			t.Errorf("the refusal's audit record %s holds the value", b)
		}
	}
}

// An adapter reads back whether it is enabled, its settings below
// adapters.<protocol>, and, for each node that reports it, its state and
// the settings generation it runs; a node reporting only another adapter is
// left out, and a protocol no adapter serves is 404.
func TestAdapterStatusPerNode(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	gen := s.putSetting(t, installation, "adapters.smb.enabled", "true")
	s.putSetting(t, installation, "adapters.smb.port", "12445")
	s.putSetting(t, installation, "adapters.nfs.port", "12049")
	ids := []entity.NodeID{entity.NodeID(uuid.New()), entity.NodeID(uuid.New()), entity.NodeID(uuid.New())}
	slices.SortFunc(ids, func(a, b entity.NodeID) int { return bytes.Compare(a[:], b[:]) })
	s.putStatus(t, entity.NodeStatus{Node: ids[0], Adapters: []entity.AdapterStatus{{Protocol: "nfs", State: entity.AdapterServing, Generation: 4}, {Protocol: "smb", State: entity.AdapterServing, Generation: 5}}})
	s.putStatus(t, entity.NodeStatus{Node: ids[1], Adapters: []entity.AdapterStatus{{Protocol: "smb", State: entity.AdapterConfigured, Generation: 3}}})
	s.putStatus(t, entity.NodeStatus{Node: ids[2], Adapters: []entity.AdapterStatus{{Protocol: "nfs", State: entity.AdapterStopped, Generation: 5}}})
	r := s.do(t, http.MethodGet, "/api/v1/adapters/smb", "")
	if r.status != http.StatusOK {
		t.Fatalf("GET adapter: answered %d %s", r.status, r.body)
	}
	var got apiv1.Adapter
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatalf("adapter %s: %v", r.body, err)
	}
	wantNodes := []apiv1.AdapterOnNode{
		{Node: uuid.UUID(ids[0]).String(), State: "serving", Generation: 5},
		{Node: uuid.UUID(ids[1]).String(), State: "configured", Generation: 3},
	}
	if !slices.Equal(got.Status.Nodes, wantNodes) {
		t.Errorf("adapter's nodes %+v, want %+v", got.Status.Nodes, wantNodes)
	}
	if !got.Spec.Enabled || string(got.Spec.Settings["port"]) != "12445" || len(got.Spec.Settings) != 1 {
		t.Errorf("adapter's spec %+v, want enabled with port 12445 alone", got.Spec)
	}
	if e := r.header.Get("ETag"); e != strconv.Quote(strconv.FormatUint(gen, 10)) {
		t.Errorf("adapter's ETag %s, want its enabled setting's generation %d", e, gen)
	}
	if r := s.do(t, http.MethodGet, "/api/v1/adapters/ftp", ""); r.status != http.StatusNotFound {
		t.Errorf("GET of an adapter no protocol names: answered %d %s, want 404", r.status, r.body)
	}
}

// A setting reads back at the scope its path names, its generation the
// ETag, and is not found at a scope that holds no record of it: a default
// is the absence of a record.
func TestSettingReadsAtItsScope(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	node := uuid.New()
	nodeScope := entity.SettingScope{Kind: entity.ScopeNode, ID: node}
	gen := s.putSetting(t, nodeScope, "adapters.smb.port", "1445")
	s.putSetting(t, installation, "adapters.smb.port", "12445")
	r := s.do(t, http.MethodGet, "/api/v1/nodes/"+node.String()+"/settings/adapters.smb.port", "")
	var got apiv1.Setting
	if err := json.Unmarshal(r.body, &got); r.status != http.StatusOK || err != nil {
		t.Fatalf("GET node setting: answered %d %s (%v)", r.status, r.body, err)
	}
	if got.ID != "adapters.smb.port" || string(got.Spec.Value) != "1445" || got.Generation != gen {
		t.Errorf("node setting %+v, want adapters.smb.port 1445 at generation %d", got, gen)
	}
	if e := r.header.Get("ETag"); e != strconv.Quote(strconv.FormatUint(gen, 10)) {
		t.Errorf("node setting's ETag %s, want %d", e, gen)
	}
	r = s.do(t, http.MethodGet, "/api/v1/settings/adapters.smb.port", "")
	if err := json.Unmarshal(r.body, &got); r.status != http.StatusOK || err != nil || string(got.Spec.Value) != "12445" {
		t.Errorf("GET installation setting: answered %d %s, want 12445", r.status, r.body)
	}
	if r := s.do(t, http.MethodGet, "/api/v1/nodes/"+uuid.NewString()+"/settings/adapters.smb.port", ""); r.status != http.StatusNotFound {
		t.Errorf("GET of a setting another node holds: answered %d %s, want 404", r.status, r.body)
	}
}

// A credential's set on a store that holds one is refused 409, naming the
// credential, before the sealer is asked for, so a node with none refuses it
// the same way; nothing is written.
func TestCredentialSetOverOneRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	st := s.remoteStore(t)
	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: st}
	ref := entity.SecretRef(uuid.New())
	s.putSetting(t, scope, "remote.credential", strconv.Quote(uuid.UUID(ref).String()))
	if err := s.store.PutSecret(t.Context(), entity.Secret{ID: ref, Kind: entity.SecretRemoteCredential, Version: 1,
		Owner: entity.SecretOwner{Scope: scope, Key: "remote.credential"}, Value: []byte("sealed")}, 0); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	before := s.written(t, scope)
	r := s.do(t, http.MethodPut, "/api/v1/stores/"+st.String()+"/credential", `{"spec":{"value":"bmV3"}}`, "If-Match", `"1"`)
	if p := r.problem(t); r.status != http.StatusConflict || p.Code != "exist" || p.Field != "remote.credential" {
		t.Fatalf("PUT credential over one: answered %d %s, want 409 exist naming remote.credential", r.status, r.body)
	}
	if after := s.written(t, scope); after != before {
		t.Errorf("a refused credential set changed the store:\n got %s\nwant %s", after, before)
	}
	if sec, err := s.store.Secret(t.Context(), ref, 0); err != nil || sec.Version != 1 {
		t.Errorf("the credential after a refused set: version %d, %v; want 1", sec.Version, err)
	}
}

// The generic setting route writes no key a resource's own route writes,
// a store's endpoint and credential and the keytab, 400 naming the key and
// the route; and
// no setting of a node, namespace or store that does not exist, 404. Each
// is refused before the settings table judges it, and writes nothing.
func TestGenericSettingGuards(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	st := s.remoteStore(t)
	for _, tc := range []struct {
		name, path string
		status     int
		field      string
	}{
		{"a store's endpoint", "/api/v1/stores/" + uuid.NewString() + "/settings/remote.endpoint", http.StatusBadRequest, "remote.endpoint"},
		{"a store's credential", "/api/v1/stores/" + st.String() + "/settings/remote.credential", http.StatusBadRequest, "remote.credential"},
		{"the keytab", "/api/v1/settings/identity.keytab", http.StatusBadRequest, "identity.keytab"},
		{"an unknown node's", "/api/v1/nodes/" + uuid.NewString() + "/settings/adapters.nfs.port", http.StatusNotFound, ""},
		{"an unknown namespace's", "/api/v1/namespaces/" + uuid.NewString() + "/settings/namespace.prefix", http.StatusNotFound, ""},
		{"an unknown store's", "/api/v1/stores/" + uuid.NewString() + "/settings/remote.bucket", http.StatusNotFound, ""},
		{"an overlong key", "/api/v1/settings/" + strings.Repeat("k", 256), http.StatusBadRequest, "key"},
	} {
		before := s.written(t, installation)
		r := s.do(t, http.MethodPut, tc.path, `{"spec":{"value":"x"}}`, "If-None-Match", "*")
		if p := r.problem(t); r.status != tc.status || p.Field != tc.field {
			t.Errorf("PUT %s: answered %d %s, want %d naming %q", tc.name, r.status, r.body, tc.status, tc.field)
		}
		if after := s.written(t, installation); after != before {
			t.Errorf("PUT %s: the store changed:\n got %s\nwant %s", tc.name, after, before)
		}
	}
}

// A namespace named on a remote store that does not exist is refused 400
// naming spec.store, and nothing is written.
func TestNamespaceOnMissingStoreRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	before := s.written(t)
	r := s.do(t, http.MethodPost, "/api/v1/namespaces", `{"spec":{"store":"`+uuid.NewString()+`","settings":{"namespace.prefix":"p/"}}}`, "Idempotency-Key", "ns")
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "spec.store" {
		t.Fatalf("POST namespace on a missing store: answered %d %s, want 400 naming spec.store", r.status, r.body)
	}
	if after := s.written(t); after != before {
		t.Errorf("a refused namespace create changed the store:\n got %s\nwant %s", after, before)
	}
}

// An adapter's PUT naming enabled among its settings is refused 400 naming
// that field, which spec.enabled sets.
func TestAdapterEnabledAmongSettingsRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	r := s.do(t, http.MethodPut, "/api/v1/adapters/smb", `{"spec":{"enabled":true,"settings":{"enabled":false}}}`, "If-Match", `"0"`)
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "spec.settings.enabled" {
		t.Errorf("PUT adapter naming enabled in its settings: answered %d %s, want 400 naming spec.settings.enabled", r.status, r.body)
	}
}
