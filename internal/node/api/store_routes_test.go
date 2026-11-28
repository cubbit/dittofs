package api_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/google/uuid"
)

// patchedRemote is a remote store's settings as a settings table declares
// them for its PATCH: how it is reached, its endpoint and its credential;
// its bucket, bound and a location too; a live region; and live tags, an
// object a patch merges into.
type patchedRemote struct {
	Endpoint   string           `json:"endpoint"   class:"restart" location:"store"`
	Bucket     string           `json:"bucket"     class:"bound"   location:"store"`
	Region     string           `json:"region"     class:"live"`
	Tags       map[string]int   `json:"tags"       class:"live"`
	Credential entity.SecretRef `json:"credential" class:"restart" location:"store"`
}

func (patchedRemote) Validate() error { return nil }

// page is one page of a listing at path, failing the test on any answer but
// 200.
func page[T any](t *testing.T, s *served, path string) apiv1.Page[T] {
	t.Helper()
	r := s.do(t, http.MethodGet, path, "")
	if r.status != http.StatusOK {
		t.Fatalf("GET %s: answered %d %s, want 200", path, r.status, r.body)
	}
	var p apiv1.Page[T]
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("GET %s: page %s: %v", path, r.body, err)
	}
	return p
}

func storeAt(id uuid.UUID) entity.SettingScope {
	return entity.SettingScope{Kind: entity.ScopeRemote, ID: id}
}

// GET /stores lists every remote store once, in ID order, page by page,
// each as its GET answers it, and nothing else: no row of another scope, and
// no remote scope that holds settings but no endpoint. The first page ends
// at a store holding a key on each side of its endpoint's, so a cursor that
// resumed inside that store's scope would list it twice.
func TestStoreListPagesTheStoresAlone(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	var want []string
	for range 3 {
		want = append(want, s.remoteStore(t).String())
	}
	slices.Sort(want)
	last := storeAt(uuid.MustParse(want[1])) // the first page's last
	s.putSetting(t, last, "remote.bucket", `"b"`)
	s.putSetting(t, last, "remote.region", `"eu"`)
	s.putSetting(t, storeAt(uuid.New()), "remote.region", `"us"`)
	s.putSetting(t, installation, "gc.schedule", `"daily"`)
	s.putSetting(t, entity.SettingScope{Kind: entity.ScopeNamespace, ID: uuid.New()}, "remote.prefix", `"p/"`)
	s.putSetting(t, entity.SettingScope{Kind: entity.ScopeShare, ID: uuid.New()}, "share.x", `"y"`)

	first := page[apiv1.Store](t, s, "/api/v1/stores?limit=2")
	if first.Next == "" {
		t.Fatalf("GET /stores?limit=2: got %d items and no next, want 2 and a next", len(first.Items))
	}
	rest := page[apiv1.Store](t, s, "/api/v1/stores?limit=2&after="+url.QueryEscape(first.Next))
	if rest.Next != "" {
		t.Errorf("the last page: next %q, want none", rest.Next)
	}
	var got []string
	for _, st := range slices.Concat(first.Items, rest.Items) {
		got = append(got, st.ID)
		if ep := string(st.Spec.Settings["remote.endpoint"]); ep != `"https://s3.example"` {
			t.Errorf("store %s: endpoint %s, want its own", st.ID, ep)
		}
		if r := string(st.Spec.Settings["remote.region"]); (st.ID == want[1]) != (r == `"eu"`) {
			t.Errorf("store %s: region %q, want \"eu\" on %s alone", st.ID, r, want[1])
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("GET /stores: listed %v, want %v", got, want)
	}
	if r := s.do(t, http.MethodGet, "/api/v1/stores?after=AQ", ""); r.status != http.StatusBadRequest {
		t.Errorf("GET /stores after a cursor naming no ID: answered %d %s, want 400", r.status, r.body)
	}
}

// A settings row that does not decode outside every store's scope stops
// nothing the store listing lists: one in a remote scope with no endpoint,
// and one past the remote scopes, are passed over.
func TestStoreListPassesOverABadRowOutsideAStore(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{NodeID: testNode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the KV: %v", err)
		}
	})
	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, as(entity.RoleViewer, false), func(d *api.Deps) { d.Store, d.Nodes = st.ControlPlane(), st.Nodes() })
	want := s.remoteStore(t).String()
	for _, sc := range []entity.SettingScope{storeAt(uuid.New()), {Kind: entity.ScopeShare, ID: uuid.New()}} {
		row := keys.String(keys.ID([]byte{keys.KindCFG, byte(sc.Kind)}, sc.ID[:]), []byte("x.broken"))
		if err := db.Update(t.Context(), func(tx kv.Txn) error { return tx.Set(row, []byte{0xff}) }); err != nil {
			t.Fatalf("plant a bad row at %v: %v", sc, err)
		}
	}
	all := page[apiv1.Store](t, s, "/api/v1/stores")
	if len(all.Items) != 1 || all.Items[0].ID != want {
		t.Errorf("GET /stores beside two bad rows: listed %+v, want store %s alone", all.Items, want)
	}
}

// GET /adapters lists every protocol's adapter in protocol order, page by
// page, each as its GET answers it.
func TestAdapterListListsEveryProtocol(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	s.putSetting(t, installation, "adapters.smb.enabled", `true`)
	all := page[apiv1.Adapter](t, s, "/api/v1/adapters")
	var got []string
	for _, a := range all.Items {
		got = append(got, a.ID)
		if a.Spec.Enabled != (a.ID == "smb") {
			t.Errorf("adapter %s: enabled %v, want true for smb alone", a.ID, a.Spec.Enabled)
		}
	}
	if !slices.Equal(got, []string{"nfs", "smb"}) || all.Next != "" {
		t.Errorf("GET /adapters: listed %v, next %q, want [nfs smb] and no next", got, all.Next)
	}
	one := page[apiv1.Adapter](t, s, "/api/v1/adapters?limit=1")
	rest := page[apiv1.Adapter](t, s, "/api/v1/adapters?limit=1&after="+url.QueryEscape(one.Next))
	if len(one.Items) != 1 || one.Items[0].ID != "nfs" || len(rest.Items) != 1 || rest.Items[0].ID != "smb" || rest.Next != "" {
		t.Errorf("GET /adapters by pages of one: got %+v then %+v, want nfs then smb", one, rest)
	}
}

// PATCH /stores/{id} writes the settings its patch names, merged into the
// value stored, and removes those it names null, each judged by the
// settings table: a live one is written, a bound one is refused 409
// bound_setting while a namespace names the store, and a change of how the
// store is reached, its endpoint, its bucket or its credential, is refused
// 409 unproven, since the capability check that would prove it is not
// built. A patch that removes the endpoint or the credential, or names the
// credential as a setting, is refused 400, and one of a store that does not
// exist 404. A write moves the store's tag and is audited applied on the
// store; every refusal writes nothing and is audited refused on the store,
// with no part of a credential's value.
func TestStorePatchAppliesTheSettingClasses(t *testing.T) {
	const value = "aHVudGVyMi1zM2NyM3Q=" // "hunter2-s3cr3t"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *served, id uuid.UUID)
		held  bool // setup gives the store a credential
		patch string
		// status and, for a refusal, the code and field it names.
		status      int
		code, field string
		// settings is the store's settings after a write, endpoint aside.
		settings map[string]string
	}{
		{name: "a live setting", patch: `{"spec":{"settings":{"remote.region":"eu"}}}`,
			status: http.StatusOK, settings: map[string]string{"remote.region": `"eu"`}},
		{name: "a live setting of a store holding a credential", held: true, patch: `{"spec":{"settings":{"remote.region":"eu"}}}`,
			status: http.StatusOK, settings: map[string]string{"remote.region": `"eu"`}},
		{name: "a setting removed by null", setup: func(t *testing.T, s *served, id uuid.UUID) {
			s.putSetting(t, storeAt(id), "remote.region", `"eu"`)
		}, patch: `{"spec":{"settings":{"remote.region":null}}}`, status: http.StatusOK, settings: map[string]string{}},
		{name: "an object merged into the one stored", setup: func(t *testing.T, s *served, id uuid.UUID) {
			s.putSetting(t, storeAt(id), "remote.tags", `{"a":1,"b":2}`)
		}, patch: `{"spec":{"settings":{"remote.tags":{"b":3,"a":null,"c":4}}}}`, status: http.StatusOK,
			settings: map[string]string{"remote.tags": `{"b":3,"c":4}`}},
		{name: "a bound setting while a namespace names the store", setup: func(t *testing.T, s *served, id uuid.UUID) {
			if _, err := s.store.PutNamespace(t.Context(), entity.Namespace{ID: entity.NamespaceID(uuid.New()), Store: id}, 0); err != nil {
				t.Fatalf("put namespace: %v", err)
			}
		}, patch: `{"spec":{"settings":{"remote.bucket":"other"}}}`, status: http.StatusConflict, code: "bound_setting", field: "remote.bucket"},
		{name: "a bucket no namespace names", patch: `{"spec":{"settings":{"remote.bucket":"other"}}}`,
			status: http.StatusConflict, code: "unproven", field: "remote.bucket"},
		{name: "a new endpoint", patch: `{"spec":{"settings":{"remote.endpoint":"https://s3.other.example"}}}`,
			status: http.StatusConflict, code: "unproven", field: "remote.endpoint"},
		{name: "a credential", patch: `{"spec":{"credential":{"value":"` + value + `"}}}`,
			status: http.StatusConflict, code: "unproven", field: "remote.credential"},
		{name: "a credential over the one the store holds", held: true,
			patch: `{"spec":{"credential":{"value":"` + value + `"}}}`, status: http.StatusConflict, code: "exist", field: "remote.credential"},
		{name: "the credential removed", held: true, patch: `{"spec":{"credential":null}}`,
			status: http.StatusBadRequest, code: "invalid", field: "spec.credential"},
		{name: "the endpoint removed", patch: `{"spec":{"settings":{"remote.endpoint":null}}}`,
			status: http.StatusBadRequest, code: "invalid", field: "spec.settings.remote.endpoint"},
		{name: "every setting removed", patch: `{"spec":{"settings":null}}`,
			status: http.StatusBadRequest, code: "invalid", field: "spec.settings.remote.endpoint"},
		{name: "the credential as a setting", patch: `{"spec":{"settings":{"remote.credential":"x"}}}`,
			status: http.StatusBadRequest, code: "invalid", field: "spec.settings.remote.credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) {
				d.Settings = settings.Table{settings.Of("remote", settings.At(entity.ScopeRemote), patchedRemote{})}
			})
			id := s.remoteStore(t)
			if tc.setup != nil {
				tc.setup(t, s, id)
			}
			var ref entity.SecretRef
			if tc.held {
				ref = holdCredential(t, s, id)
			}
			path := "/api/v1/stores/" + id.String()
			tag := s.do(t, http.MethodGet, path, "").header.Get("ETag")
			before := s.written(t, storeAt(id))
			r := s.do(t, http.MethodPatch, path, tc.patch, "If-Match", tag)
			if r.status != tc.status {
				t.Fatalf("PATCH %s: answered %d %s, want %d", tc.patch, r.status, r.body, tc.status)
			}
			recs := s.audits(t)
			if len(recs) == 0 {
				t.Fatalf("PATCH %s: no audit record", tc.patch)
			}
			rec := recs[len(recs)-1]
			if rec.Target.Kind != entity.ResourceRemoteStore || !bytes.Equal(rec.Target.ID, id[:]) {
				t.Errorf("PATCH %s: audit target %v %x, want the store %s", tc.patch, rec.Target.Kind, rec.Target.ID, id)
			}
			if b, _ := json.Marshal(rec); bytes.Contains(b, []byte(value)) {
				t.Errorf("PATCH %s: its audit record %s holds the credential's value", tc.patch, b)
			}
			if tc.held {
				if sec, err := s.store.Secret(t.Context(), ref, 0); err != nil || sec.Version != 1 {
					t.Errorf("PATCH %s: the held credential reads version %d (%v), want 1", tc.patch, sec.Version, err)
				}
			}
			if tc.status != http.StatusOK {
				if p := r.problem(t); p.Field != tc.field || string(p.Code) != tc.code {
					t.Errorf("PATCH %s: answered code %q naming %q, want %q naming %q", tc.patch, p.Code, p.Field, tc.code, tc.field)
				}
				if want := "refused: " + tc.code; string(rec.Outcome) != want {
					t.Errorf("PATCH %s: audit outcome %q, want %q", tc.patch, rec.Outcome, want)
				}
				if after := s.written(t, storeAt(id)); after != before {
					t.Errorf("a refused PATCH changed the store:\n got %s\nwant %s", after, before)
				}
				return
			}
			if string(rec.Outcome) != "applied" {
				t.Errorf("PATCH %s: audit outcome %q, want applied", tc.patch, rec.Outcome)
			}
			var got apiv1.Store
			if err := json.Unmarshal(r.body, &got); err != nil {
				t.Fatalf("PATCH answer %s: %v", r.body, err)
			}
			now := s.do(t, http.MethodGet, path, "").header.Get("ETag")
			if et := r.header.Get("ETag"); et == tag || et != now || `"`+strconv.FormatUint(got.Generation, 10)+`"` != now {
				t.Errorf("PATCH %s: answered ETag %s, generation %d, read %s before and %s after, want the tag moved and answered", tc.patch, et, got.Generation, tag, now)
			}
			if c := got.Spec.Credential; tc.held != (c != nil) || tc.held && (c.Ref != uuid.UUID(ref).String() || c.Version != 1 || c.Value != nil) {
				t.Errorf("PATCH %s: answered credential %+v, want the held one's reference at version 1: %v", tc.patch, c, tc.held)
			}
			stored := map[string]string{}
			for st, err := range s.store.Settings(t.Context(), storeAt(id)) {
				if err != nil {
					t.Fatal(err)
				}
				if st.Key != "remote.endpoint" && st.Key != "remote.credential" {
					stored[st.Key] = string(st.Value)
				}
			}
			answered := map[string]string{}
			for k, v := range got.Spec.Settings {
				if k != "remote.endpoint" {
					answered[k] = string(v)
				}
			}
			if !maps.Equal(stored, tc.settings) || !maps.Equal(answered, tc.settings) {
				t.Errorf("PATCH %s: stored %v, answered %v, want %v", tc.patch, stored, answered, tc.settings)
			}
		})
	}
	t.Run("a store that does not exist", func(t *testing.T) {
		s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) {
			d.Settings = settings.Table{settings.Of("remote", settings.At(entity.ScopeRemote), patchedRemote{})}
		})
		r := s.do(t, http.MethodPatch, "/api/v1/stores/"+uuid.NewString(), `{"spec":{"settings":{"remote.region":"eu"}}}`, "If-Match", `"1"`)
		if r.status != http.StatusNotFound {
			t.Errorf("PATCH of a store that does not exist: answered %d %s, want 404", r.status, r.body)
		}
	})
}

// Two patches read at one tag cannot both commit, whichever settings they
// change: the second is refused 412 and writes nothing.
func TestStorePatchesAtOneTagConflict(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) {
		d.Settings = settings.Table{settings.Of("remote", settings.At(entity.ScopeRemote), patchedRemote{})}
	})
	id := s.remoteStore(t)
	path := "/api/v1/stores/" + id.String()
	tag := s.do(t, http.MethodGet, path, "").header.Get("ETag")
	if r := s.do(t, http.MethodPatch, path, `{"spec":{"settings":{"remote.region":"eu"}}}`, "If-Match", tag); r.status != http.StatusOK {
		t.Fatalf("the first PATCH: answered %d %s, want 200", r.status, r.body)
	}
	before := s.written(t, storeAt(id))
	if r := s.do(t, http.MethodPatch, path, `{"spec":{"settings":{"remote.tags":{"a":1}}}}`, "If-Match", tag); r.status != http.StatusPreconditionFailed {
		t.Errorf("a second PATCH at the first's tag %s: answered %d %s, want 412", tag, r.status, r.body)
	}
	if after := s.written(t, storeAt(id)); after != before {
		t.Errorf("the refused PATCH changed the store:\n got %s\nwant %s", after, before)
	}
}

// holdCredential gives the store id names a credential, written through
// the store, and returns its reference.
func holdCredential(t *testing.T, s *served, id uuid.UUID) entity.SecretRef {
	t.Helper()
	ref := entity.SecretRef(uuid.New())
	s.putSetting(t, storeAt(id), "remote.credential", refRow(t, ref))
	if err := s.store.PutSecret(t.Context(), entity.Secret{ID: ref, Kind: entity.SecretRemoteCredential, Version: 1,
		Owner: entity.SecretOwner{Scope: storeAt(id), Key: "remote.credential"}, Value: []byte("sealed")}, 0); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	return ref
}
