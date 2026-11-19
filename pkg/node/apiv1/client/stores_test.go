package client_test

import (
	"errors"
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
	for _, set := range []entity.Setting{
		{Scope: scope, Key: "remote.endpoint", Value: []byte(`"https://s3.example"`)},
		{Scope: scope, Key: "remote.credential", Value: []byte(`"` + uuid.UUID(ref).String() + `"`)},
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
	if want := (apiv1.SecretRef{Ref: uuid.UUID(ref).String(), Version: 1}); err != nil || got.Spec.Credential == nil || *got.Spec.Credential != want {
		t.Errorf("Stores().Get: %+v err %v, want credential %+v", got, err, want)
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
