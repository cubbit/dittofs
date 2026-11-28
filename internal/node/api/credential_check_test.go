package api_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The access key and the secret of the values the credential check refuses,
// grepped for in every answer, audit record and log line.
const (
	checkedKey    = "AKIDCHECKED"
	checkedSecret = "checked-secret-value"
)

// Each seal of a remote store's credential, POST /stores, PATCH
// /stores/{id} and the credential route, refuses a value the S3 client could
// not use with a field error on the credential before it seals: with no
// sealer, so a seal would answer 503, the value is still refused 400, and
// nothing is written. No answer, audit record or log line holds the key or
// the secret.
func TestRemoteCredentialCheckedBeforeItsSeal(t *testing.T) {
	values := []struct{ name, value string }{
		{"key:secret", checkedKey + ":" + checkedSecret},
		{"an unknown field", `{"access_key_id":"` + checkedKey + `","secret_access_key":"` + checkedSecret + `","token":"t"}`},
		{"the secret missing", `{"access_key_id":"` + checkedKey + `"}`},
		{"a name in another case", `{"Access_Key_Id":"` + checkedKey + `","secret_access_key":"` + checkedSecret + `"}`},
	}
	routes := []struct {
		name, method, path, field string
		body                      func(b64 string) string
	}{
		{"POST /stores", http.MethodPost, "/api/v1/stores", "spec.credential.value", func(b64 string) string {
			return `{"spec":{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b1"},"credential":{"value":"` + b64 + `"}}}`
		}},
		{"PATCH /stores/{id}", http.MethodPatch, "/api/v1/stores/%s", "spec.credential.value", func(b64 string) string {
			return `{"spec":{"credential":{"value":"` + b64 + `"}}}`
		}},
		{"PUT /stores/{id}/credential", http.MethodPut, "/api/v1/stores/%s/credential", "spec.value", func(b64 string) string {
			return `{"spec":{"value":"` + b64 + `"}}`
		}},
	}
	for _, rt := range routes {
		for _, v := range values {
			t.Run(rt.name+"/"+v.name, func(t *testing.T) {
				s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) { d.Settings = writeTable() })
				st := s.remoteStore(t)
				scope := entity.SettingScope{Kind: entity.ScopeRemote, ID: st}
				path := strings.Replace(rt.path, "%s", st.String(), 1)
				hdr := precondition(t, s, rt.method, path)
				before := s.written(t, scope)

				b64 := base64.StdEncoding.EncodeToString([]byte(v.value))
				r := s.do(t, rt.method, path, rt.body(b64), hdr...)
				if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != rt.field {
					t.Fatalf("%s %s: answered %d %s, want 400 naming %s", rt.method, v.name, r.status, r.body, rt.field)
				}
				if after := s.written(t, scope); after != before {
					t.Errorf("the refused credential wrote:\n got %s\nwant %s", after, before)
				}

				recs, _ := json.Marshal(s.audits(t))
				for _, out := range []struct{ what, text string }{{"the answer", string(r.body)}, {"the audit records", string(recs)}, {"the log", s.log.String()}} {
					for _, secret := range []string{checkedKey, checkedSecret, b64} {
						if strings.Contains(out.text, secret) {
							t.Errorf("%s holds %q: %s", out.what, secret, out.text)
						}
					}
				}
			})
		}
	}
}

// A node with no credential check refuses every seal of a remote store's
// credential 503, as one with no sealer does, rather than sealing a value
// nothing judged.
func TestRemoteCredentialRefusedWithoutACheck(t *testing.T) {
	s := writeServe(t, func(d *api.Deps) { d.CheckCredential = nil })
	st := s.remoteStore(t)
	cred := `{"value":"` + base64.StdEncoding.EncodeToString([]byte(`{"access_key_id":"k","secret_access_key":"s"}`)) + `"}`
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/stores", `{"spec":{"settings":{"remote.endpoint":"https://s3.example","remote.bucket":"b1"},"credential":` + cred + `}}`},
		{http.MethodPatch, "/api/v1/stores/" + st.String(), `{"spec":{"credential":` + cred + `}}`},
		{http.MethodPut, "/api/v1/stores/" + st.String() + "/credential", `{"spec":` + cred + `}`},
	} {
		if r := s.do(t, c.method, c.path, c.body, precondition(t, s, c.method, c.path)...); r.status != http.StatusServiceUnavailable {
			t.Errorf("%s %s with no credential check: answered %d %s, want 503", c.method, c.path, r.status, r.body)
		}
	}
}

// precondition is the header a write of method to path carries: a PATCH the
// store's current tag, any other If-None-Match: *.
func precondition(t *testing.T, s *served, method, path string) []string {
	t.Helper()
	if method == http.MethodPatch {
		return []string{"If-Match", s.do(t, http.MethodGet, path, "").header.Get("ETag")}
	}
	return []string{"If-None-Match", "*"}
}
