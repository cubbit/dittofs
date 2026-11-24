package clisuite

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// storeRows are the rows of the remote store, namespace, adapter and
// setting commands. Until the settings table declares the remote store's,
// namespace's and adapters' sections, the server refuses every write of
// them, so a row reads what the test wrote through the store, and the
// writes are rows of their refusal.
func storeRows() []row {
	return []row{
		{name: "setting set of a key the settings table does not declare prints the refusal and exits non-zero", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			refused(t, run, "unknown_key", "setting", "set", "installation", "clisuite.undeclared", "1")
		}},
		{name: "adapter set prints the refusal until the settings table declares the adapters", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			refused(t, run, "unknown_key", "adapter", "set", "nfs", "--enabled")
		}},
		{name: "store create prints the refusal until the settings table declares the remote store", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			refused(t, run, "unknown_key", "store", "create", "--setting", "remote.endpoint=https://s3.example.com", "--credential-file", secretFile(t, "a password\n"))
		}},
		{name: "namespace create prints the refusal until the settings table declares the namespace", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, _ *client.Client) {
			refused(t, run, "unknown_key", "namespace", "create", "--store", s.putStore(t), "--setting", "namespace.prefix=builds/")
		}},
		{name: "setting get reads a record at the installation's scope and at a node's", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, _ *client.Client) {
			key := "clisuite.k" + uuid.NewString()[:8]
			node := s.Harness.NodeID()
			for scope, at := range map[string]entity.SettingScope{
				"installation":                     {Kind: entity.ScopeInstallation},
				"node:" + uuid.UUID(node).String(): {Kind: entity.ScopeNode, ID: node},
			} {
				value := `"at ` + scope + `"`
				if _, err := s.Harness.Store().ControlPlane().PutSetting(t.Context(), entity.Setting{Scope: at, Key: key, Value: []byte(value)}, 0); err != nil {
					t.Fatalf("put %s at %s: %v", key, scope, err)
				}
				var got apiv1.Setting
				decodeJSON(t, mustRun(t, run, "setting", "get", scope, key, "-o", "json"), &got)
				if got.ID != key || string(got.Spec.Value) != value {
					t.Errorf("setting get %s %s: got %s = %s, want %s", scope, key, got.ID, got.Spec.Value, value)
				}
			}
		}},
		{name: "setting set, get and unset a declared key", harnessOnly: true, run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			const key = "audit.retention"
			for _, v := range []string{"10000h", "10001h"} {
				mustRun(t, run, "setting", "set", "installation", key, v)
				got, err := c.Settings(client.ScopeInstallation, "").Get(t.Context(), key)
				if err != nil || string(got.Spec.Value) != `"`+v+`"` {
					t.Fatalf("setting set installation %s %s: read back %s (%v), want %q", key, v, got.Spec.Value, err, v)
				}
			}
			mustRun(t, run, "setting", "unset", "installation", key)
			if _, err := c.Settings(client.ScopeInstallation, "").Get(t.Context(), key); !isStatus(err, 404) {
				t.Errorf("setting unset installation %s: read back %v, want 404", key, err)
			}
		}},
		{name: "setting get of a scope no kind names is refused naming it", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			_, errOut, status := run(t.Context(), "setting", "get", "cluster:x", "api.session_lifetime")
			if status == 0 || !strings.Contains(errOut, `scope "cluster:x"`) {
				t.Errorf("setting get cluster:x: status %d, stderr %q, want non-zero naming the scope", status, errOut)
			}
		}},
		{name: "store get and delete a store written through the store", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			id := s.putStore(t)
			var got apiv1.Store
			decodeJSON(t, mustRun(t, run, "store", "get", id, "-o", "json"), &got)
			if got.ID != id || string(got.Spec.Settings["remote.endpoint"]) != `"https://s3.example.com"` {
				t.Errorf("store get %s: got %+v, want its endpoint", id, got)
			}
			mustRun(t, run, "store", "delete", id)
			if _, err := c.Stores().Get(t.Context(), id); !isStatus(err, 404) {
				t.Errorf("store delete %s: read back %v, want 404", id, err)
			}
		}},
		{name: "store set-credential prints the server's refusal until the store's capability check is built", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, _ *client.Client) {
			id := s.putStore(t)
			_, errOut, status := run(t.Context(), "store", "set-credential", id, "--credential-file", secretFile(t, "a password\n"))
			if status == 0 || !strings.Contains(errOut, "409 ") || !strings.Contains(errOut, id) {
				t.Errorf("store set-credential %s: status %d, stderr %q, want non-zero, the store and the server's 409", id, status, errOut)
			}
		}},
		{name: "namespace list, get and delete a namespace written through the store", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			id := entity.NamespaceID(uuid.New())
			if _, err := s.Harness.Store().ControlPlane().PutNamespace(t.Context(), entity.Namespace{ID: id, Store: uuid.New()}, 0); err != nil {
				t.Fatalf("put a namespace: %v", err)
			}
			want := uuid.UUID(id).String()
			if out := mustRun(t, run, "namespace", "list"); !strings.Contains(out, want) {
				t.Errorf("namespace list: got %q, want a row for %s", out, want)
			}
			var got apiv1.Namespace
			decodeJSON(t, mustRun(t, run, "namespace", "get", want, "-o", "json"), &got)
			if got.ID != want {
				t.Errorf("namespace get %s: got %+v", want, got)
			}
			mustRun(t, run, "namespace", "delete", want)
			if _, err := c.Namespaces().Get(t.Context(), want); !isStatus(err, 404) {
				t.Errorf("namespace delete %s: read back %v, want 404", want, err)
			}
		}},
		{name: "adapter get prints the adapter its protocol names", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			want, err := c.Adapters().Get(t.Context(), "nfs")
			if err != nil {
				t.Fatalf("read adapter nfs: %v", err)
			}
			var got apiv1.Adapter
			decodeJSON(t, mustRun(t, run, "adapter", "get", "nfs", "-o", "json"), &got)
			if got.ID != "nfs" || got.Spec.Enabled != want.Spec.Enabled {
				t.Errorf("adapter get nfs: got %+v, want %+v", got, want.Resource)
			}
		}},
		{name: "adapter get of a protocol no adapter serves is refused", run: func(t *testing.T, run Runner, _ Server, _ *client.Client) {
			_, errOut, status := run(t.Context(), "adapter", "get", "ftp")
			if status == 0 || !strings.Contains(errOut, "adapter ftp") || !strings.Contains(errOut, "404 ") {
				t.Errorf("adapter get ftp: status %d, stderr %q, want non-zero and the server's 404 naming it", status, errOut)
			}
		}},
	}
}

// putStore writes a remote store through the store, as its endpoint
// setting, and returns its ID.
func (s Server) putStore(t *testing.T) string {
	t.Helper()
	id := uuid.New()
	if _, err := s.Harness.Store().ControlPlane().PutSetting(t.Context(), entity.Setting{
		Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: id}, Key: "remote.endpoint", Value: []byte(`"https://s3.example.com"`),
	}, 0); err != nil {
		t.Fatalf("put a remote store: %v", err)
	}
	return id.String()
}

// refused runs args, which the server refuses 400 with code, and fails the
// test unless the command exits non-zero printing itself and that refusal.
func refused(t *testing.T, run Runner, code string, args ...string) {
	t.Helper()
	_, errOut, status := run(t.Context(), args...)
	if want := "dfsctl " + strings.Join(args[:2], " ") + ": "; status == 0 || !strings.Contains(errOut, want) || !strings.Contains(errOut, "400 "+code+": ") {
		t.Errorf("dfsctl %s: status %d, stderr %q, want non-zero, %q and the server's 400 %s", strings.Join(args, " "), status, errOut, want, code)
	}
}

// decodeJSON decodes out, a resource dfsctl printed as JSON, into v.
func decodeJSON(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
}
