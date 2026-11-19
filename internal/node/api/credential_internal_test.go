package api

import (
	"bytes"
	"encoding/json"
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
