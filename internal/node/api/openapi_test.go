package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api/auth"
)

// checkedIn is the committed OpenAPI document.
var checkedIn = filepath.Join("..", "..", "..", "pkg", "node", "apiv1", "openapi.json")

// The committed document is what the generator builds from the route table,
// so a row added or changed without regenerating fails here.
func TestOpenAPIMatchesRouteTable(t *testing.T) {
	got, err := OpenAPI()
	if err != nil {
		t.Fatalf("OpenAPI: %v", err)
	}
	want, err := os.ReadFile(checkedIn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("pkg/node/apiv1/openapi.json differs from the document the route table generates (%d bytes generated, %d committed): run `go run ./cmd/genopenapi`", len(got), len(want))
	}
}

// holder is the least role that holds each right, as RFC 23 §5.2's role
// table states it, written here apart from the auth package's table.
var holder = map[auth.Right]string{
	auth.Public: "none", auth.View: "viewer", auth.Shares: "operator", auth.Stores: "operator",
	auth.Principals: "operator", auth.Accounts: "admin", auth.Drain: "operator", auth.Installation: "admin",
	auth.Audit: "admin", auth.QuotaEvents: "operator", auth.Self: "viewer",
}

// document is the document of rows, decoded as a client reads it.
func document(t *testing.T, rows []route) map[string]any {
	t.Helper()
	doc, err := openAPI(rows)
	if err != nil {
		t.Fatalf("openAPI: %v", err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// docOp is rt's operation in doc, nil when there is none.
func docOp(doc map[string]any, rt *route) map[string]any {
	path := rt.pattern
	if rt.verb != "" {
		path += ":" + rt.verb
	}
	op, _ := dig(doc, "paths", path, strings.ToLower(rt.method)).(map[string]any)
	return op
}

// dig is the value at the keys under v, nil when one is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

// paramsOf is op's parameters, keyed by where and name, "query:limit".
func paramsOf(op map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	ps, _ := op["parameters"].([]any)
	for _, p := range ps {
		p := p.(map[string]any)
		out[p["in"].(string)+":"+p["name"].(string)] = p
	}
	return out
}

// Every row of the route table is an operation of the document, at its path
// and method, with the role that holds its right, no security when public,
// the operation it starts, whether it waits to be applied, the query
// parameters it takes, and the problem answers every call of its kind can
// get, each a component the document defines; every parameter a row
// describes is one it takes.
func TestOpenAPINamesEveryRow(t *testing.T) {
	doc := document(t, routes)
	for i := range routes {
		rt := &routes[i]
		op := docOp(doc, rt)
		if op == nil {
			t.Errorf("%s: no operation", rt.op())
			continue
		}
		if got, want := op["x-dittofs-role"], holder[rt.right]; got != want {
			t.Errorf("%s: x-dittofs-role %v, want %v", rt.op(), got, want)
		}
		sec, hasSec := op["security"]
		if public := rt.right == auth.Public; public != (hasSec && len(sec.([]any)) == 0) {
			t.Errorf("%s: security %v, want empty only for a public row (public %v)", rt.op(), sec, public)
		}
		if got := op["x-dittofs-starts"]; rt.starts != "" && got != string(rt.starts) || rt.starts == "" && got != nil {
			t.Errorf("%s: x-dittofs-starts %v, want %q", rt.op(), got, rt.starts)
		}
		if got := op["x-dittofs-waits-applied"] == true; got != rt.serve.raises {
			t.Errorf("%s: x-dittofs-waits-applied %v, want %v", rt.op(), got, rt.serve.raises)
		}
		params := paramsOf(op)
		takes := append(slices.Clone(rt.serve.filters), rt.query...)
		for _, q := range takes {
			if params["query:"+q] == nil {
				t.Errorf("%s: takes ?%s=, which its operation does not document", rt.op(), q)
			}
		}
		for _, p := range rt.params {
			if !slices.Contains(takes, p.name) {
				t.Errorf("%s: describes ?%s=, which it does not take", rt.op(), p.name)
			}
		}
		responses, _ := op["responses"].(map[string]any)
		want := []int{http.StatusInternalServerError, http.StatusServiceUnavailable}
		if rt.right != auth.Public {
			want = append(want, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound)
		}
		if rt.serve.form == formWrite {
			want = append(want, http.StatusConflict, http.StatusPreconditionRequired)
			if rt.method != http.MethodPost || rt.serve.changesSpec {
				want = append(want, http.StatusPreconditionFailed)
			}
		}
		if rt.starts != "" {
			want = append(want, http.StatusAccepted)
		}
		for _, st := range want {
			if _, ok := responses[strconv.Itoa(st)]; !ok {
				t.Errorf("%s: no %d answer among %v", rt.op(), st, slices.Sorted(maps.Keys(responses)))
			}
		}
		for st, r := range responses {
			ref, _ := r.(map[string]any)["$ref"].(string)
			if name, ok := strings.CutPrefix(ref, "#/components/responses/"); ok && dig(doc, "components", "responses", name) == nil {
				t.Errorf("%s: the %s answer names %s, which the document does not define", rt.op(), st, ref)
			}
		}
	}
}

// Over the test table's knob rows, whose writes raise the settings
// generation: each write waits to be applied and no read does; a POST
// action changing the spec requires If-Match and may be refused 412, and one
// that does not, neither.
func TestOpenAPITestRows(t *testing.T) {
	var knobRows []route
	for _, rt := range testRows {
		if strings.HasPrefix(rt.pattern, "/api/v1/knobs") {
			knobRows = append(knobRows, rt)
		}
	}
	doc := document(t, knobRows)
	for i := range knobRows {
		rt := &knobRows[i]
		op := docOp(doc, rt)
		if got, want := op["x-dittofs-waits-applied"] == true, rt.method != http.MethodGet; got != want {
			t.Errorf("%s: x-dittofs-waits-applied %v, want %v", rt.op(), got, want)
		}
		if rt.verb == "" {
			continue
		}
		ifMatch := dig(paramsOf(op)["header:If-Match"], "required") == true
		_, refuses412 := dig(op, "responses").(map[string]any)["412"]
		if want := rt.serve.changesSpec; ifMatch != want || refuses412 != want {
			t.Errorf("%s: If-Match required %v and 412 listed %v, want both %v", rt.op(), ifMatch, refuses412, want)
		}
	}
}

// An operation's ID is its path's literal segments and its action, so it
// reads as a tool name and moves only with its row.
func TestOpenAPIOperationIDs(t *testing.T) {
	for _, tc := range []struct{ method, pattern, verb, want string }{
		{http.MethodGet, "/api/version", "", "version_get"},
		{http.MethodGet, "/api/v1/shares", "", "shares_list"},
		{http.MethodPost, "/api/v1/shares", "", "shares_create"},
		{http.MethodGet, "/api/v1/shares/{id}", "", "shares_get"},
		{http.MethodPost, "/api/v1/shares/{id}", "quiesce", "shares_quiesce"},
		{http.MethodPut, "/api/v1/shares/{id}/export-policy", "", "shares_export_policy_put"},
		{http.MethodGet, "/api/v1/nodes/{id}/connections", "", "nodes_connections_list"},
	} {
		i := slices.IndexFunc(routes, func(rt route) bool {
			return rt.method == tc.method && rt.pattern == tc.pattern && rt.verb == tc.verb
		})
		if i < 0 {
			t.Fatalf("no row %s %s:%s", tc.method, tc.pattern, tc.verb)
		}
		if got := operationID(&routes[i]); got != tc.want {
			t.Errorf("%s: operation ID %q, want %q", routes[i].op(), got, tc.want)
		}
	}
	twins := []route{
		{method: http.MethodGet, pattern: "/api/v1/a-b", right: auth.View, serve: read(version)},
		{method: http.MethodGet, pattern: "/api/v1/a_b", right: auth.View, serve: read(version)},
	}
	if _, err := openAPI(twins); err == nil || !strings.Contains(err.Error(), "a_b_get") {
		t.Errorf("two rows answering a_b_get: err %v, want one naming the shared ID", err)
	}
}

// A request body's schema requires no field the server defaults and admits
// no field its type lacks; a merge patch is an object, required, whose
// values may be null; an answer's schema requires every field the wire
// always writes.
func TestOpenAPIBodies(t *testing.T) {
	doc := document(t, routes)
	body := func(path, method, ct string) any {
		return dig(doc, "paths", path, method, "requestBody", "content", ct, "schema")
	}
	patch := body("/api/v1/shares/{id}", "patch", "application/merge-patch+json")
	put := body("/api/v1/shares/{id}/export-policy", "put", "application/json")
	problem := dig(doc, "components", "schemas", "Problem")
	for _, tc := range []struct {
		name   string
		schema any
		value  string
		ok     bool
	}{
		{"a patch setting one field", patch, `{"spec":{"path":"/x"}}`, true},
		{"a patch resetting one field", patch, `{"spec":{"name":null}}`, true},
		{"a patch of null", patch, `null`, false},
		{"a patch naming no field of the type", patch, `{"spec":{"bogus":1}}`, false},
		{"a policy naming one field", put, `{"spec":{"flavors":["sys"]}}`, true},
		{"a policy naming no field of the type", put, `{"spec":{"read_only":true,"x":1}}`, false},
		{"a problem missing its request ID", problem, `{"status":400,"title":"Bad Request","retryable":false}`, false},
		{"a problem with every field the wire writes", problem, `{"status":400,"title":"Bad Request","retryable":false,"request_id":"r"}`, true},
	} {
		if tc.schema == nil {
			t.Fatalf("%s: no schema in the document", tc.name)
		}
		var v any
		if err := json.Unmarshal([]byte(tc.value), &v); err != nil {
			t.Fatal(err)
		}
		if err := Validate(doc, tc.schema, v); (err == nil) != tc.ok {
			t.Errorf("%s: %s validates with %v, want valid %v", tc.name, tc.value, err, tc.ok)
		}
	}
	if got := dig(doc, "paths", "/api/v1/shares/{id}", "patch", "requestBody", "required"); got != true {
		t.Errorf("PATCH /shares/{id}: requestBody.required %v, want true", got)
	}
}

// Validate checks v against schema, resolving references in doc, for the
// keywords the document uses: $ref, anyOf, const, enum, type, properties,
// required, additionalProperties false and items. An object schema that
// names its properties and says nothing of others is held closed, so an
// answer carrying a field the document leaves out fails.
func Validate(doc map[string]any, schema, v any) error { return validate(doc, schema, v, "$") }

func validate(doc map[string]any, schema, v any, at string) error {
	s, _ := schema.(map[string]any)
	if ref, ok := s["$ref"].(string); ok {
		r, _ := dig(doc, strings.Split(strings.TrimPrefix(ref, "#/"), "/")...).(map[string]any)
		if r == nil {
			return fmt.Errorf("%s: %s names nothing", at, ref)
		}
		s = r
	}
	if alts, ok := s["anyOf"].([]any); ok {
		for _, a := range alts {
			if validate(doc, a, v, at) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: %v matches no alternative", at, v)
	}
	if c, ok := s["const"]; ok && c != v {
		return fmt.Errorf("%s: %v, want %v", at, v, c)
	}
	if e, ok := s["enum"].([]any); ok && !slices.Contains(e, v) {
		return fmt.Errorf("%s: %v, want one of %v", at, v, e)
	}
	if t, ok := s["type"]; ok {
		names, _ := t.([]any)
		if n, ok := t.(string); ok {
			names = []any{n}
		}
		if !slices.ContainsFunc(names, func(n any) bool { return isType(n.(string), v) }) {
			return fmt.Errorf("%s: %v is not of type %v", at, v, t)
		}
	}
	switch v := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		req, _ := s["required"].([]any)
		for _, r := range req {
			if _, ok := v[r.(string)]; !ok {
				return fmt.Errorf("%s: no %s, which is required", at, r)
			}
		}
		for k, x := range v {
			ps, ok := props[k]
			if !ok {
				if s["additionalProperties"] == false || s["additionalProperties"] == nil && props != nil {
					return fmt.Errorf("%s: %s is no property of the schema", at, k)
				}
				continue
			}
			if err := validate(doc, ps, x, at+"."+k); err != nil {
				return err
			}
		}
	case []any:
		if items, ok := s["items"]; ok {
			for i, x := range v {
				if err := validate(doc, items, x, at+"["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// isType reports whether v, as encoding/json decodes it into an any, is of
// JSON Schema type name.
func isType(name string, v any) bool {
	switch v := v.(type) {
	case nil:
		return name == "null"
	case bool:
		return name == "boolean"
	case string:
		return name == "string"
	case float64:
		return name == "number" || name == "integer" && v == math.Trunc(v)
	case map[string]any:
		return name == "object"
	case []any:
		return name == "array"
	}
	return false
}
