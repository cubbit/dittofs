package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// A credential's audit change holds its value only as "set", under
// credential.value, whatever the two versions' sealed bytes are.
func TestCredentialAuditHoldsSet(t *testing.T) {
	old := apiv1.Credential{Spec: apiv1.CredentialSpec{Value: []byte("sealed-one-7f3a")}}
	nu := apiv1.Credential{Spec: apiv1.CredentialSpec{Value: []byte("sealed-two-9c1e")}}
	for _, tc := range []struct {
		name     string
		old, new any
	}{{"a first credential", nil, nu}, {"a credential replaced", old, nu}, {"a credential removed", old, nil}} {
		b, err := change(tc.old, tc.new, credentials.auditAs, credentials.secret)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: change %s: %v", tc.name, b, err)
		}
		if len(got) != 1 || got["credential.value"] != "set" {
			t.Errorf("%s: change %s, want credential.value set alone", tc.name, b)
		}
		for _, v := range [][]byte{old.Spec.Value, nu.Spec.Value} {
			enc, _ := json.Marshal(v)
			if bytes.Contains(b, v) || bytes.Contains(b, bytes.Trim(enc, `"`)) {
				t.Errorf("%s: change %s holds a sealed value", tc.name, b)
			}
		}
	}
}

// issuesCredentials lists the rows of rows whose answer carries a credential,
// tokens or an API token, and that are not keyless, so would take an
// Idempotency-Key and record their answer under it.
func issuesCredentials(rows []route) []string {
	var bad []string
	for _, rt := range rows {
		out := rt.serve.out
		if (out == reflect.TypeFor[apiv1.Tokens]() || out == reflect.TypeFor[apiv1.APIToken]()) && !rt.keyless {
			bad = append(bad, rt.op())
		}
	}
	return bad
}

// Every row whose answer carries a credential is keyless, so it refuses an
// Idempotency-Key and records no answer; a planted row issuing an API token
// with a key is caught.
func TestCredentialRowsAreKeyless(t *testing.T) {
	if bad := issuesCredentials(routes); len(bad) != 0 {
		t.Errorf("rows issuing a credential that take an Idempotency-Key: %q, want none", bad)
	}
	planted := route{method: http.MethodPost, pattern: "/api/v1/planted", serve: write(apiTokens, nil, createAPIToken)}
	if bad := issuesCredentials([]route{planted}); len(bad) != 1 {
		t.Errorf("a planted keyed row issuing an API token: caught %q, want it", bad)
	}
}
