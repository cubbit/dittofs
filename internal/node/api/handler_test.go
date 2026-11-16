package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// served is the API over a harness's store, serving the test rows beside the
// production table, its series in reg and its spans in spans.
type served struct {
	url    string
	client *http.Client
	reg    *metrics.Registry
	spans  *tracetest.SpanRecorder
	store  kvstore.ControlPlane
}

func serve(t *testing.T, authenticate func(*http.Request) (auth.Caller, error)) *served {
	t.Helper()
	h := nodetest.New(t, nodetest.InMemory(), nodetest.Watch())
	s := &served{reg: metrics.New("test", "test"), spans: tracetest.NewSpanRecorder(), store: h.Store().ControlPlane()}
	srv := api.NewWithTestRows(api.Deps{
		Ready:        func() string { return "ready" },
		Live:         func() bool { return true },
		Addr:         "127.0.0.1:0",
		MetricsAddr:  "127.0.0.1:0",
		Metrics:      s.reg,
		Logger:       slog.New(slog.NewTextHandler(t.Output(), nil)),
		Authenticate: authenticate,
		Store:        s.store,
		Tracer:       sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(s.spans)).Tracer("api"),
	})
	addr, _, err := srv.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		// The test's own context has ended by now.
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	s.url, s.client = "http://"+addr.String(), &http.Client{Transport: tr}
	return s
}

// as authenticates every request as an account of role.
func as(role entity.APIRole, mustChange bool) func(*http.Request) (auth.Caller, error) {
	return func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Role: role, MustChange: mustChange}, nil
	}
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (s *served) do(t *testing.T, method, path, body string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Header, b}
}

// problem decodes r as a problem body, failing unless it is one.
func (r reply) problem(t *testing.T) apiv1.Problem {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != apiv1.MediaTypeProblem {
		t.Fatalf("answer %d %s: content type %q, want %s", r.status, r.body, ct, apiv1.MediaTypeProblem)
	}
	var p apiv1.Problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("problem %s: %v", r.body, err)
	}
	if p.Status != r.status {
		t.Fatalf("problem %s: status %d, answered %d", r.body, p.Status, r.status)
	}
	return p
}

const thing = "/api/v1/things/" + api.TestID

// With no Authenticate, every row but the public ones, and a path no row
// names, answers 401 unauthenticated; /api/version and the probes answer.
func TestNoAuthenticateAnswers401(t *testing.T) {
	s := serve(t, nil)
	rows := append(api.Rows(), api.Row{Method: http.MethodGet, Path: "/api/v1/no-such-thing"})
	for _, rt := range rows {
		r := s.do(t, rt.Method, rt.Path, "")
		if rt.Public {
			if r.status != http.StatusOK {
				t.Errorf("public %s %s: answered %d %s, want 200", rt.Method, rt.Path, r.status, r.body)
			}
			continue
		}
		if p := r.problem(t); r.status != http.StatusUnauthorized || p.Code != apiv1.CodeUnauthenticated || p.RequestID == "" {
			t.Errorf("%s %s: answered %d %q request %q, want 401 unauthenticated with a request ID", rt.Method, rt.Path, r.status, p.Code, p.RequestID)
		}
	}
	for _, probe := range []string{"/health/live", "/health/ready"} {
		if r := s.do(t, http.MethodGet, probe, ""); r.status != http.StatusOK {
			t.Errorf("%s: answered %d, want 200", probe, r.status)
		}
	}
}

// /api/version answers the majors served and nothing else, to anyone.
func TestVersion(t *testing.T) {
	s := serve(t, nil)
	r := s.do(t, http.MethodGet, "/api/version", "")
	if r.status != http.StatusOK || string(r.body) != `{"majors":["v1"]}` {
		t.Errorf("GET /api/version: answered %d %s, want 200 {\"majors\":[\"v1\"]}", r.status, r.body)
	}
}

// A POST on {id}:verb reaches its verb's row; an unknown verb, a verb where
// the row takes none, no verb where the rows take one, and a name where an
// ID goes are each 404 not_found.
func TestPathsAndVerbs(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	for _, verb := range []string{"poke", "prod"} {
		r := s.do(t, http.MethodPost, thing+":"+verb, "")
		if r.status != http.StatusOK || string(r.body) != `{"verb":"`+verb+`"}` {
			t.Errorf("POST :%s: answered %d %s, want 200 naming %s", verb, r.status, r.body, verb)
		}
	}
	if r := s.do(t, http.MethodGet, thing, ""); r.status != http.StatusOK || !bytes.Contains(r.body, []byte(api.TestID)) {
		t.Errorf("GET %s: answered %d %s, want 200 naming its ID", thing, r.status, r.body)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, thing + ":nope"},
		{http.MethodPost, thing},
		{http.MethodGet, thing + ":poke"},
		{http.MethodGet, "/api/v1/things/payroll"},
		{http.MethodPost, "/api/v1/things/payroll:poke"},
	} {
		r := s.do(t, tc.method, tc.path, "")
		if p := r.problem(t); r.status != http.StatusNotFound || p.Code != "not_found" {
			t.Errorf("%s %s: answered %d %q, want 404 not_found", tc.method, tc.path, r.status, p.Code)
		}
	}
}

// A caller that may read a resource but not change it is refused 403, one
// that may not read it 404, as though it did not exist.
func TestRoles(t *testing.T) {
	var role atomic.Uint32
	s := serve(t, func(r *http.Request) (auth.Caller, error) {
		return as(entity.APIRole(role.Load()), false)(r)
	})
	for _, tc := range []struct {
		role         entity.APIRole
		method, path string
		status       int
	}{
		{entity.RoleViewer, http.MethodGet, thing, http.StatusOK},
		{entity.RoleViewer, http.MethodPost, thing + ":poke", http.StatusForbidden},
		{entity.RoleOperator, http.MethodPost, thing + ":poke", http.StatusOK},
		{entity.RoleViewer, http.MethodPut, "/api/v1/netgroups/" + api.TestID, http.StatusForbidden},
		{entity.RoleViewer, http.MethodGet, "/api/v1/accounts/" + api.TestID, http.StatusNotFound},
		{entity.RoleOperator, http.MethodGet, "/api/v1/accounts/" + api.TestID, http.StatusNotFound},
		{entity.RoleAdmin, http.MethodGet, "/api/v1/accounts/" + api.TestID, http.StatusOK},
		{entity.RoleNone, http.MethodGet, thing, http.StatusNotFound},
	} {
		role.Store(uint32(tc.role))
		body := ""
		if tc.method == http.MethodPut {
			body = `{"members":[]}`
		}
		r := s.do(t, tc.method, tc.path, body)
		if r.status != tc.status {
			t.Errorf("%v %s %s: answered %d %s, want %d", tc.role, tc.method, tc.path, r.status, r.body, tc.status)
		}
		if r.status == http.StatusForbidden && r.problem(t).Code != apiv1.CodeForbidden {
			t.Errorf("%v %s %s: code %q, want forbidden", tc.role, tc.method, tc.path, r.problem(t).Code)
		}
	}
}

// A caller that must change its password is refused 403
// password_change_required on a row that does not admit it, and admitted on
// one that does.
func TestMustChangePassword(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, true))
	r := s.do(t, http.MethodGet, thing, "")
	if p := r.problem(t); r.status != http.StatusForbidden || p.Code != apiv1.CodePasswordChangeRequired {
		t.Errorf("GET %s: answered %d %q, want 403 password_change_required", thing, r.status, p.Code)
	}
	if r := s.do(t, http.MethodPost, "/api/v1/password", ""); r.status != http.StatusOK {
		t.Errorf("POST /api/v1/password: answered %d %s, want 200", r.status, r.body)
	}
}

// A write answers what its transaction committed, which the store then
// holds; a body naming a field the request does not declare is refused 400
// invalid naming it, in a problem body with every key, and writes nothing.
func TestWrite(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	r := s.do(t, http.MethodPut, "/api/v1/netgroups/"+api.TestID, `{"members":["10.0.0.0/8"]}`)
	var put struct{ Version uint64 }
	if err := json.Unmarshal(r.body, &put); r.status != http.StatusOK || err != nil || put.Version == 0 {
		t.Fatalf("PUT netgroup: answered %d %s, want 200 with its version", r.status, r.body)
	}
	other := "1d6f3c2a-4b5e-4f7a-8c9d-0e1f2a3b4c5d"
	r = s.do(t, http.MethodPut, "/api/v1/netgroups/"+other, `{"members":["x"],"colour":"red"}`)
	p := r.problem(t)
	if r.status != http.StatusBadRequest || p.Code != "invalid" || p.Field != "colour" {
		t.Errorf("unknown field: answered %d %q field %q, want 400 invalid naming colour", r.status, p.Code, p.Field)
	}
	var keys map[string]any
	if err := json.Unmarshal(r.body, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "code", "title", "detail", "field", "retryable", "request_id"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("problem %s: no key %q", r.body, k)
		}
	}
	var got []entity.Netgroup
	for n, err := range s.store.Netgroups(t.Context(), "") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if len(got) != 1 || got[0].Name != api.TestID || !slices.Equal(got[0].Members, []string{"10.0.0.0/8"}) || got[0].Version != put.Version {
		t.Errorf("netgroups held: %+v, want only %s with 10.0.0.0/8 at the answered version %d", got, api.TestID, put.Version)
	}
}

// Each call counts in api_requests_total by op and outcome and in
// api_request_seconds by op; an error no row names answers 500 and counts in
// api_unmapped_errors_total; each call opens one span named by its op, and
// no attribute of any span carries a body field.
func TestSeriesAndSpans(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	const marker = "body-field-7f3a"
	put := "PUT /api/v1/netgroups/{id}"
	s.do(t, http.MethodPut, "/api/v1/netgroups/"+api.TestID, `{"members":["`+marker+`"]}`)
	s.do(t, http.MethodPut, "/api/v1/netgroups/"+api.TestID, `{"members":["`+marker+`"],"colour":1}`)
	if r := s.do(t, http.MethodGet, "/api/v1/fail", ""); r.status != http.StatusInternalServerError {
		t.Errorf("GET /api/v1/fail: answered %d %s, want 500", r.status, r.body)
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"dittofs_api_requests_total", map[string]string{"op": put, "outcome": "ok"}, 1},
		{"dittofs_api_requests_total", map[string]string{"op": put, "outcome": "invalid"}, 1},
		{"dittofs_api_requests_total", map[string]string{"op": "GET /api/v1/fail", "outcome": "unmapped"}, 1},
		{"dittofs_api_request_seconds", map[string]string{"op": put}, 2},
		{"dittofs_api_unmapped_errors_total", nil, 1},
	} {
		if got := sample(t, s.reg, tc.name, tc.labels); got != tc.want {
			t.Errorf("%s%v: got %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
	var names []string
	for _, sp := range s.spans.Ended() {
		names = append(names, sp.Name())
		for _, a := range sp.Attributes() {
			if strings.Contains(a.Value.String(), marker) {
				t.Errorf("span %s: attribute %s carries the body field %q", sp.Name(), a.Key, a.Value.String())
			}
		}
	}
	if want := []string{put, put, "GET /api/v1/fail"}; !slices.Equal(names, want) {
		t.Errorf("spans: got %q, want %q", names, want)
	}
}

// sample is the value of the series name with labels in reg: a counter's
// value, or a histogram's count.
func sample(t *testing.T, reg *metrics.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if labels[l.GetName()] != l.GetValue() {
					continue next
				}
			}
			if h := m.GetHistogram(); h != nil {
				return float64(h.GetSampleCount())
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}
