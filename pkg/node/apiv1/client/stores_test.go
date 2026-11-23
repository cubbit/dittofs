package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/google/uuid"
)

// The store, namespace, adapter and setting accessors read what the store
// holds, the store's credential as its reference and version; a setting's
// put comes back as the server's problem, naming the refused key.
func TestStoresNamespacesAdaptersSettings(t *testing.T) {
	s := serve(t, admin())
	c := dial(t, s.url, newClock())
	st := uuid.New()
	scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: st}
	ref := entity.SecretRef(uuid.New())

	refRow, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal the reference row: %v", err)
	}
	for _, set := range []entity.Setting{
		{Scope: scope, Key: "remote.endpoint", Value: []byte(`"https://s3.example"`)},
		{Scope: scope, Key: "remote.credential", Value: refRow},
		{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "adapters.nfs.enabled", Value: []byte("true")},
	} {
		if _, err := s.store.PutSetting(t.Context(), set, 0); err != nil {
			t.Fatalf("put setting %s: %v", set.Key, err)
		}
	}
	if err := s.store.PutSecret(t.Context(), entity.Secret{ID: ref, Kind: entity.SecretRemoteCredential, Version: 1,
		Owner: entity.SecretOwner{Scope: scope, Key: "remote.credential"}, Value: []byte("sealed")}, 0); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	ns := entity.NamespaceID(uuid.New())
	if _, err := s.store.PutNamespace(t.Context(), entity.Namespace{ID: ns, Store: st}, 0); err != nil {
		t.Fatalf("put namespace: %v", err)
	}

	got, err := c.Stores().Get(t.Context(), st.String())
	if err != nil || got.Spec.Credential == nil {
		t.Fatalf("Stores().Get: %+v err %v, want its credential", got, err)
	}

	if got.Spec.Credential.Ref != uuid.UUID(ref).String() || got.Spec.Credential.Version != 1 {
		t.Errorf(
			"Stores().Get: credential %+v, want reference %s at version 1",
			got.Spec.Credential, uuid.UUID(ref))
	}
	if n, err := c.Namespaces().Get(t.Context(), uuid.UUID(ns).String()); err != nil || n.Spec.Store != st.String() || n.ETag == "" {
		t.Errorf("Namespaces().Get: %+v err %v, want on store %s with its tag", n, err, st)
	}
	var listed []string
	for n, err := range c.Namespaces().List(t.Context(), nil) {
		if err != nil {
			t.Fatalf("Namespaces().List: %v", err)
		}
		listed = append(listed, n.ID)
	}
	if len(listed) != 1 || listed[0] != uuid.UUID(ns).String() {
		t.Errorf("Namespaces().List: %q, want %s", listed, uuid.UUID(ns))
	}
	if a, err := c.Adapters().Get(t.Context(), "nfs"); err != nil || !a.Spec.Enabled {
		t.Errorf("Adapters().Get(nfs): %+v err %v, want enabled", a, err)
	}
	if v, err := c.Settings(client.ScopeStore, st.String()).Get(t.Context(), "remote.endpoint"); err != nil || string(v.Spec.Value) != `"https://s3.example"` {
		t.Errorf("Settings(stores).Get: %+v err %v, want the endpoint", v, err)
	}

	var p *apiv1.Problem
	_, err = c.Settings(client.ScopeInstallation, "").Put(t.Context(), "gc.schedule", apiv1.SettingSpec{Value: []byte(`"daily"`)}, "*")
	if !errors.As(err, &p) || p.Code != "unknown_key" || p.Field != "gc.schedule" {
		t.Errorf("Settings().Put: %v, want the problem unknown_key naming gc.schedule", err)
	}
	// A credential's set with no sealer answers 503, which the client
	// retries on its clock; the refusal itself is the API's test.
}

// A store's create goes with no idempotency key, since its spec carries its
// credential's value: the request the front recorded holds none. The front
// answers the create itself, since the API behind it hands in no table and
// its refusal, a 503, the client would retry on its clock.
func TestStoresCreateGoesNoKey(t *testing.T) {
	s := serve(t, admin())
	c := dial(t, s.url, newClock())

	s.front.add(fault{
		match: func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == "/api/v1/stores"
		},
		status: http.StatusCreated,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   `{"id":"id","generation":0,"spec":{"settings":{}}}`,
	})

	if _, err := c.Stores().Create(t.Context(),
		apiv1.StoreSpec{Settings: map[string]json.RawMessage{}}); err != nil {
		t.Fatalf("Stores().Create: %v", err)
	}

	for _, req := range s.front.requests(http.MethodPost) {
		if req.uri == "/api/v1/stores" && req.header.Get("Idempotency-Key") != "" {
			t.Errorf("the create carried the idempotency key %q, want none",
				req.header.Get("Idempotency-Key"))
		}
	}
}
