package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// openAPIDoc is the API's OpenAPI document, decoded as a client reads it.
func openAPIDoc(t *testing.T) map[string]any {
	t.Helper()
	b, err := api.OpenAPI()
	if err != nil {
		t.Fatalf("OpenAPI: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// at is the value at the keys under v, nil when one is missing.
func at(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

// deref is v, or what it references when it is a $ref into doc.
func deref(doc map[string]any, v any) map[string]any {
	m, _ := v.(map[string]any)
	if ref, ok := m["$ref"].(string); ok {
		m, _ = at(doc, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...).(map[string]any)
	}
	return m
}

// conforms fails unless r is an answer the document's operation at method
// and path documents: its status is one of its answers, it carries every
// header that answer always carries and the answer documents as always
// carried each of always, in the content type it names, and its body
// validates against that answer's schema.
func conforms(t *testing.T, doc map[string]any, method, path string, r reply, always ...string) {
	t.Helper()
	op := at(doc, "paths", path, strings.ToLower(method))
	if op == nil {
		t.Fatalf("%s: the document has no operation %s %s", r.req, method, path)
	}
	resp := deref(doc, at(op, "responses", strconv.Itoa(r.status)))
	if resp == nil {
		t.Errorf("%s: answered %d %s, which %s %s does not document", r.req, r.status, r.body, method, path)
		return
	}
	headers, _ := at(resp, "headers").(map[string]any)
	for _, name := range always {
		if r.header.Get(name) == "" {
			t.Errorf("%s: answered %d without %s", r.req, r.status, name)
		}
		if at(headers, name, "required") != true {
			t.Errorf("%s: answered %d with %s, which its answer does not document as always sent", r.req, r.status, name)
		}
	}
	for name, h := range headers {
		if at(h, "required") == true && r.header.Get(name) == "" {
			t.Errorf("%s: answered %d without %s, which its answer always carries", r.req, r.status, name)
		}
	}
	content, _ := at(resp, "content").(map[string]any)
	ct := r.header.Get("Content-Type")
	c, ok := content[ct]
	if !ok {
		t.Errorf("%s: answered %d as %q, which its answer does not name among %v", r.req, r.status, ct, content)
		return
	}
	var body any
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("%s: body %s: %v", r.req, r.body, err)
	}
	if err := api.Validate(doc, at(c, "schema"), body); err != nil {
		t.Errorf("%s: answered %d %s, which its answer's schema refuses: %v", r.req, r.status, r.body, err)
	}
}

// Each answer the server gives conforms to the operation that documents it:
// a share's create, read, listing, patch, delete and wait for serving, the
// cancel of the delete's operation and the read of it, and the version.
func TestOpenAPIDescribesAnswers(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	doc := openAPIDoc(t)
	ns := s.namespace(t)
	var share, etag, op string
	for _, tc := range []struct {
		method, docPath string
		path            func() string
		body            string
		hdr             func() []string
		want            int
		always          []string
		then            func(reply)
	}{
		{method: http.MethodGet, docPath: "/api/version", path: func() string { return "/api/version" }, want: 200},
		{method: http.MethodPost, docPath: "/api/v1/shares", path: func() string { return "/api/v1/shares" },
			body: jsonOf(t, newShare("docs", ns)), hdr: func() []string { return []string{"Idempotency-Key", "create"} }, want: 201, always: []string{"ETag", "Location"},
			then: func(r reply) { share, etag = r.header.Get("Location"), r.header.Get("ETag") }},
		{method: http.MethodGet, docPath: "/api/v1/shares/{id}", path: func() string { return share }, want: 200, always: []string{"ETag"}},
		{method: http.MethodGet, docPath: "/api/v1/shares", path: func() string { return "/api/v1/shares" }, want: 200},
		{method: http.MethodPatch, docPath: "/api/v1/shares/{id}", path: func() string { return share },
			body: `{"spec":{"path":"/docs2"}}`, hdr: func() []string { return []string{"If-Match", `"999"`} }, want: 412},
		{method: http.MethodPatch, docPath: "/api/v1/shares/{id}", path: func() string { return share },
			body: `null`, hdr: func() []string { return []string{"If-Match", etag} }, want: 400},
		{method: http.MethodPatch, docPath: "/api/v1/shares/{id}", path: func() string { return share },
			hdr: func() []string { return []string{"If-Match", etag} }, want: 400},
		{method: http.MethodPatch, docPath: "/api/v1/shares/{id}", path: func() string { return share },
			body: `{"spec":{"path":"/docs2"}}`, hdr: func() []string { return []string{"If-Match", etag} }, want: 200, always: []string{"ETag"},
			then: func(r reply) { etag = r.header.Get("ETag") }},
		{method: http.MethodDelete, docPath: "/api/v1/shares/{id}", path: func() string { return share },
			hdr: func() []string { return []string{"If-Match", etag, "Idempotency-Key", "delete"} }, want: 202, always: []string{"Location"},
			then: func(r reply) { op = r.header.Get("Location") }},
		{method: http.MethodGet, docPath: "/api/v1/shares/{id}", path: func() string { return share + "?wait=serving" }, want: 409},
		{method: http.MethodPost, docPath: "/api/v1/operations/{id}:cancel", path: func() string { return op + ":cancel" },
			hdr: func() []string { return []string{"Idempotency-Key", "cancel"} }, want: 409},
		{method: http.MethodGet, docPath: "/api/v1/operations/{id}", path: func() string { return op }, want: 200},
	} {
		var hdr []string
		if tc.hdr != nil {
			hdr = tc.hdr()
		}
		r := s.do(t, tc.method, tc.path(), tc.body, hdr...)
		if r.status != tc.want {
			t.Fatalf("%s: answered %d %s, want %d", r.req, r.status, r.body, tc.want)
		}
		conforms(t, doc, tc.method, tc.docPath, r, tc.always...)
		if tc.then != nil {
			tc.then(r)
		}
	}
}

// operations yields each operation of doc with its method and its path.
func operations(doc map[string]any, yield func(method, path string, op map[string]any)) {
	paths := at(doc, "paths").(map[string]any)
	for _, path := range slices.Sorted(func(y func(string) bool) {
		for p := range paths {
			if !y(p) {
				return
			}
		}
	}) {
		for method, op := range paths[path].(map[string]any) {
			yield(strings.ToUpper(method), path, op.(map[string]any))
		}
	}
}

// params is op's parameters.
func params(op map[string]any) []map[string]any {
	var out []map[string]any
	ps, _ := at(op, "parameters").([]any)
	for _, p := range ps {
		out = append(out, p.(map[string]any))
	}
	return out
}

// filled is op's path with its path parameters filled with a value of each
// one's schema.
func filled(path string, op map[string]any) string {
	for _, p := range params(op) {
		if p["in"] == "path" {
			path = strings.Replace(path, "{"+p["name"].(string)+"}", valueOf(p["schema"].(map[string]any)), 1)
		}
	}
	return path
}

// valueOf is a value of a parameter's schema: its example, its first enum
// value, a value of its format, or any string.
func valueOf(schema map[string]any) string {
	if ex, ok := at(schema, "examples").([]any); ok {
		return ex[0].(string)
	}
	if e, ok := at(schema, "enum").([]any); ok {
		return e[0].(string)
	}
	switch at(schema, "format") {
	case "uuid":
		return uuid.NewString()
	case "date-time":
		return time.Now().UTC().Format(time.RFC3339)
	}
	return "x"
}

// headerValue is a value of header h, the nth sent: a fresh key, or a tag.
func headerValue(h string, n int) string {
	if h == "Idempotency-Key" {
		return "key-" + strconv.Itoa(n)
	}
	return `"1"`
}

// Each filter a listing documents, sent alone with a value of its schema, is
// one the listing takes: it is not refused 400 naming it.
func TestOpenAPIListingFiltersAreTaken(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	doc := openAPIDoc(t)
	sent := 0
	operations(doc, func(method, path string, op map[string]any) {
		ps := params(op)
		if method != http.MethodGet || !slices.ContainsFunc(ps, func(p map[string]any) bool { return p["name"] == "limit" }) {
			return
		}
		for _, p := range ps {
			name := p["name"].(string)
			if p["in"] != "query" || name == "limit" || name == "after" {
				continue
			}
			sent++
			r := s.do(t, method, filled(path, op)+"?"+name+"="+valueOf(p["schema"].(map[string]any)), "")
			if r.status == http.StatusBadRequest && r.problem(t).Field == name {
				t.Errorf("%s: refused the filter %s that %s %s documents: %s", r.req, name, method, path, r.body)
			}
		}
	})
	if sent == 0 {
		t.Error("the document names no listing filter to send")
	}
}

// Each header an operation documents as required, left out with every other
// one it requires sent, is refused 428; and a request carrying every one,
// and If-None-Match: * where it is documented, is not, so no header the
// server requires goes undocumented.
func TestOpenAPIRequiredHeadersAreRequired(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	doc := openAPIDoc(t)
	n := 0
	operations(doc, func(method, path string, op map[string]any) {
		var required []string
		for _, p := range params(op) {
			if p["in"] == "header" && p["required"] == true {
				required = append(required, p["name"].(string))
			}
		}
		all := []string{"If-None-Match", "*"}
		for _, h := range required {
			n++
			all = append(all, h, headerValue(h, n))
		}
		if !slices.ContainsFunc(params(op), func(p map[string]any) bool { return p["name"] == "If-None-Match" }) {
			all = all[2:]
		}
		if r := s.do(t, method, filled(path, op), "{}", all...); r.status == http.StatusPreconditionRequired {
			t.Errorf("%s with every header %s %s documents as required %v: answered 428 %s", r.req, method, path, required, r.body)
		}
		for _, missing := range required {
			var hdr []string
			for _, h := range required {
				if h == missing {
					continue
				}
				n++
				hdr = append(hdr, h, headerValue(h, n))
			}
			if r := s.do(t, method, filled(path, op), "{}", hdr...); r.status != http.StatusPreconditionRequired {
				t.Errorf("%s without %s, which %s %s requires: answered %d %s, want 428", r.req, missing, method, path, r.status, r.body)
			}
		}
	})
	if n == 0 {
		t.Error("the document requires no header to leave out")
	}
}

// A user's create, which the document says waits to be applied, is held
// until the node has applied it and then answered 201 as documented; and a
// setting's put the settings table refuses is answered 400 as documented.
func TestOpenAPIDescribesWaitsAndRefusals(t *testing.T) {
	hold := api.NewHold(nil)
	s := serve(t, as(entity.RoleAdmin, false), func(d *api.Deps) { d.Applied = hold.Applied })
	doc := openAPIDoc(t)
	if got := at(doc, "paths", "/api/v1/users", "post", "x-dittofs-waits-applied"); got != true {
		t.Fatalf("POST /api/v1/users: x-dittofs-waits-applied %v, want true", got)
	}
	answered := s.async(t, http.MethodPost, "/api/v1/users", `{"spec":{"name":"docs"}}`, "Idempotency-Key", "user")
	await(t, hold.Reached, "the node's Applied")
	select {
	case r := <-answered:
		t.Fatalf("%s: answered %d %s before the node applied it", r.req, r.status, r.body)
	default:
	}
	close(hold.Release)
	r := <-answered
	if r.status != http.StatusCreated {
		t.Fatalf("%s: answered %d %s, want 201", r.req, r.status, r.body)
	}
	conforms(t, doc, http.MethodPost, "/api/v1/users", r, "ETag", "Location")

	r = s.do(t, http.MethodPut, "/api/v1/settings/adapters.nfs.port", `{"spec":{"value":12049}}`, "If-None-Match", "*")
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("%s: answered %d %s, want 503, no table handed in", r.req, r.status, r.body)
	}
	conforms(t, doc, http.MethodPut, "/api/v1/settings/{sub}", r)
}

// A viewer's cancel of a share's deletion is refused 403, as the document
// says: the call needs the role that starts the operation's kind, which its
// operation marks, not the viewer its row asks.
func TestOpenAPIDescribesRoleByKind(t *testing.T) {
	var role atomic.Uint32
	role.Store(uint32(entity.RoleAdmin))
	s := serve(t, func(r *http.Request) (auth.Caller, error) { return as(entity.APIRole(role.Load()), false)(r) })
	doc := openAPIDoc(t)
	path := "/api/v1/operations/{id}:cancel"
	if got := at(doc, "paths", path, "post", "x-dittofs-role-by-kind"); got != true {
		t.Fatalf("POST %s: x-dittofs-role-by-kind %v, want true", path, got)
	}
	if got := at(doc, "paths", path, "post", "x-dittofs-role"); got != "viewer" {
		t.Fatalf("POST %s: x-dittofs-role %v, want viewer", path, got)
	}
	op := s.putOp(t, newOp(apiv1.OpShareDelete, entity.Principal{}), 0)
	role.Store(uint32(entity.RoleViewer))
	r := s.do(t, http.MethodPost, opPath(op.ID)+":cancel", "", "Idempotency-Key", "cancel")
	if r.status != http.StatusForbidden {
		t.Fatalf("%s as a viewer: answered %d %s, want 403", r.req, r.status, r.body)
	}
	conforms(t, doc, http.MethodPost, path, r)
}

// Every value of every closed set a query parameter documents is one the
// server takes: a listing's filter is not refused 400 naming it, nor is a
// delete's ?snapshots= on a share that exists.
func TestOpenAPIEnumsAreTaken(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	doc := openAPIDoc(t)
	ns := s.namespace(t)
	sent := 0
	operations(doc, func(method, path string, op map[string]any) {
		for _, p := range params(op) {
			values, _ := at(p, "schema", "enum").([]any)
			if p["in"] != "query" || values == nil {
				continue
			}
			name := p["name"].(string)
			for _, v := range values {
				sent++
				var r reply
				switch {
				case method == http.MethodGet:
					r = s.do(t, method, filled(path, op)+"?"+name+"="+v.(string), "")
				case method == http.MethodDelete && path == "/api/v1/shares/{id}":
					_, loc, sh := s.post(t, newShare("enum"+strconv.Itoa(sent), ns))
					r = s.do(t, method, loc+"?"+name+"="+v.(string), "", "If-Match", `"`+strconv.FormatUint(sh.Generation, 10)+`"`, "Idempotency-Key", "enum-"+strconv.Itoa(sent))
				default:
					t.Fatalf("%s %s: documents the closed set ?%s=, which this test sends nowhere", method, path, name)
				}
				if r.status == http.StatusBadRequest && r.problem(t).Field == name {
					t.Errorf("%s: refused ?%s=%v, which %s %s documents: %s", r.req, name, v, method, path, r.body)
				}
			}
		}
	})
	if sent == 0 {
		t.Error("the document names no closed set to send")
	}
}
