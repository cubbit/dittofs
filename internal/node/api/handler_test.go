package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// served is the API over a harness's store, db, serving the test rows beside the
// production table, its series in reg, its spans in spans, its log lines in
// log and its time on clock.
type served struct {
	url    string
	client *http.Client
	reg    *metrics.Registry
	spans  *tracetest.SpanRecorder
	db     *kvstore.Store
	store  kvstore.ControlPlane
	nodes  kvstore.Nodes
	log    *lockedBuffer
	clock  *sim.TestClock
}

// lockedBuffer is a log the server writes and the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// testNode is the node every served API names in its audit records.
var testNode = entity.NodeID{'a', 'p', 'i'}

// serve starts the API over d's defaults, each of edit then changing them.
func serve(t *testing.T, authenticate func(*http.Request) (auth.Caller, error), edit ...func(*api.Deps)) *served {
	t.Helper()
	return serveWith(t, authenticate, func(*api.Server) {}, edit...)
}

// serveWith is serve, calling before on the server before it starts.
func serveWith(t *testing.T, authenticate func(*http.Request) (auth.Caller, error), before func(*api.Server), edit ...func(*api.Deps)) *served {
	t.Helper()
	h := nodetest.New(t, nodetest.InMemory(), nodetest.Watch())
	s := &served{reg: metrics.New("test", "test"), spans: tracetest.NewSpanRecorder(), db: h.Store(), store: h.Store().ControlPlane(), nodes: h.Store().Nodes(), log: &lockedBuffer{}, clock: h.Clock()}
	d := api.Deps{
		Ready:        func() string { return "ready" },
		Live:         func() bool { return true },
		Addr:         "127.0.0.1:0",
		MetricsAddr:  "127.0.0.1:0",
		Metrics:      s.reg,
		Logger:       slog.New(slog.NewJSONHandler(io.MultiWriter(t.Output(), s.log), nil)),
		Authenticate: authenticate,
		Store:        s.store,
		Nodes:        s.nodes,
		Clock:        s.clock,
		Node:         testNode,
		Tracer:       sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(s.spans)).Tracer("api"),
	}
	for _, e := range edit {
		e(&d)
	}
	if cp, ok := d.Store.(kvstore.ControlPlane); ok {
		s.store = cp
	}
	if n, ok := d.Nodes.(kvstore.Nodes); ok {
		s.nodes = n
	}
	srv := api.NewWithTestRows(d)
	before(srv)
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
//
// decision: these tests authenticate through a function answering a fixed
// Caller, which is no stand-in for a collaborator: a Caller is the input the
// API routes on, and the real Authenticate arrives with sign-in. They test
// routing and the adapters; which role holds which right is the role
// table's own test, over auth.Allowed.
func as(role entity.APIRole, mustChange bool) func(*http.Request) (auth.Caller, error) {
	return func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Role: role, MustChange: mustChange}, nil
	}
}

// reply is what a request, method and path, was answered.
type reply struct {
	req    string
	status int
	header http.Header
	body   []byte
}

// send sends method on path with body and the headers hdr names in pairs, a
// name and its value, under ctx.
func (s *served) send(ctx context.Context, method, path, body string, hdr ...string) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.url+path, strings.NewReader(body))
	if err != nil {
		return reply{}, err
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return reply{method + " " + path, resp.StatusCode, resp.Header, b}, err
}

// do is send under the test's context, failing on an error.
func (s *served) do(t *testing.T, method, path, body string, hdr ...string) reply {
	t.Helper()
	r, err := s.send(t.Context(), method, path, body, hdr...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// problem decodes r as a problem body, failing, naming r's request, unless
// it is one.
func (r reply) problem(t *testing.T) apiv1.Problem {
	t.Helper()
	if ct := r.header.Get("Content-Type"); ct != apiv1.MediaTypeProblem {
		t.Fatalf("%s: answered %d %s with content type %q, want %s", r.req, r.status, r.body, ct, apiv1.MediaTypeProblem)
	}
	var p apiv1.Problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("%s: problem %s: %v", r.req, r.body, err)
	}
	if p.Status != r.status {
		t.Fatalf("%s: problem %s has status %d, answered %d", r.req, r.body, p.Status, r.status)
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
// that may not read it 404, as though it did not exist, and one holding the
// right is answered.
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
		{entity.RoleViewer, http.MethodPost, thing + ":poke", http.StatusForbidden},
		{entity.RoleViewer, http.MethodGet, "/api/v1/accounts/" + api.TestID, http.StatusNotFound},
		{entity.RoleOperator, http.MethodPost, thing + ":poke", http.StatusOK},
		{entity.RoleViewer, http.MethodPut, "/api/v1/knobs/" + api.TestID, http.StatusForbidden},
		{entity.RoleViewer, http.MethodPut, "/api/v1/vaults/" + api.TestID, http.StatusForbidden},
	} {
		role.Store(uint32(tc.role))
		r := s.do(t, tc.method, tc.path, "")
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

// A row a caller that must change its password may not read answers it 404,
// as a path no row names does, so that refusal maps no route.
func TestMustChangeMapsNoRoute(t *testing.T) {
	viewer := serve(t, as(entity.RoleViewer, true))
	for _, path := range []string{"/api/v1/accounts/" + api.TestID, "/api/v1/no-such/" + api.TestID} {
		r := viewer.do(t, http.MethodGet, path, "")
		if p := r.problem(t); r.status != http.StatusNotFound || p.Code != "not_found" {
			t.Errorf("must-change viewer, GET %s: answered %d %q, want 404 not_found", path, r.status, p.Code)
		}
	}
}

// A write answers what its transaction committed, with its tag, which the
// store then holds; a body naming a field the request does not declare is
// refused 400 invalid naming it, in a problem body with every key, and writes
// nothing.
func TestWrite(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	r := s.do(t, http.MethodPut, knobPath, `{"spec":{"value":"on"}}`, "If-None-Match", "*")
	if r.status != http.StatusOK || string(r.body) != `{"spec":{"value":"on"}}` || r.header.Get("ETag") != `"1"` {
		t.Fatalf("PUT knob: answered %d %s ETag %q, want 200 with its spec and ETag \"1\"", r.status, r.body, r.header.Get("ETag"))
	}
	r = s.do(t, http.MethodPut, "/api/v1/knobs/"+otherID, `{"spec":{"value":"x","colour":"red"}}`, "If-None-Match", "*")
	p := r.problem(t)
	if r.status != http.StatusBadRequest || p.Code != "invalid" || p.Field != "spec.colour" {
		t.Errorf("unknown field: answered %d %q field %q, want 400 invalid naming spec.colour", r.status, p.Code, p.Field)
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
	if got := s.knob(t, api.TestID); string(got.Value) != `{"value":"on"}` || got.Generation != 1 {
		t.Errorf("knob held: %q at %d, want {\"value\":\"on\"} at 1", got.Value, got.Generation)
	}
	if got := s.knob(t, otherID); got.Generation != 0 {
		t.Errorf("knob held after the refused PUT: %+v, want none", got)
	}
}

const (
	knobPath = "/api/v1/knobs/" + api.TestID
	otherID  = "1d6f3c2a-4b5e-4f7a-8c9d-0e1f2a3b4c5d"
)

// knob is the setting the knob id names holds, its zero value when it holds
// none.
func (s *served) knob(t *testing.T, id string) entity.Setting {
	t.Helper()
	return s.setting(t, id, "knob")
}

func (s *served) setting(t *testing.T, id, key string) entity.Setting {
	t.Helper()
	sid, err := entity.ParseShareID(id)
	if err != nil {
		t.Fatal(err)
	}
	for st, err := range s.store.Settings(t.Context(), entity.SettingScope{Kind: entity.ScopeNode, ID: sid}) {
		if err != nil {
			t.Fatal(err)
		}
		if st.Key == key {
			return st
		}
	}
	return entity.Setting{}
}

// A write whose transaction fails answers the failure's problem and stores
// nothing of it, the record it wrote before failing included.
func TestWriteThatFailsStoresNothing(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	r := s.do(t, http.MethodPut, knobPath, `{"spec":{"value":"`+api.KnobBreaks+`"}}`, "If-None-Match", "*")
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "invalid_argument" {
		t.Errorf("PUT knob that breaks: answered %d %q, want 400 invalid_argument", r.status, p.Code)
	}
	if got := s.knob(t, api.TestID); got.Generation != 0 {
		t.Errorf("knob held after the failed write: %+v, want none", got)
	}
}

// With no store, a write answers 503 unavailable.
func TestWriteWithNoStore(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) { d.Store = nil })
	if r := s.do(t, http.MethodPut, knobPath, `{"spec":{}}`, "If-None-Match", "*"); r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable {
		t.Errorf("PUT with no store: answered %d %s, want 503 unavailable", r.status, r.body)
	}
}

// A body over the bound is refused 400 invalid and writes nothing.
func TestBodyOverTheBound(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	big := `{"spec":{"value":"` + strings.Repeat("a", 1<<20) + `"}}`
	if r := s.do(t, http.MethodPut, knobPath, big, "If-None-Match", "*"); r.status != http.StatusBadRequest || r.problem(t).Code != "invalid" {
		t.Errorf("PUT of %d bytes: answered %d %.200s, want 400 invalid", len(big), r.status, r.body)
	}
	if got := s.knob(t, api.TestID); got.Generation != 0 {
		t.Errorf("knob held after the refused PUT: %+v, want none", got)
	}
}

// A body nesting arrays or objects deeper than the bound, up to the size
// bound, is refused 400 invalid and writes nothing.
func TestDeepBodyRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	for _, body := range []string{
		`{"spec":{"value":` + strings.Repeat("[", 1<<20-20),
		strings.Repeat(`{"a":`, (1<<20)/5-1),
	} {
		if r := s.do(t, http.MethodPut, knobPath, body, "If-None-Match", "*"); r.status != http.StatusBadRequest || r.problem(t).Code != "invalid" {
			t.Errorf("PUT of %d bytes starting %.12s: answered %d %.200s, want 400 invalid", len(body), body, r.status, r.body)
		}
	}
	if got := s.knob(t, api.TestID); got.Generation != 0 {
		t.Errorf("knob held after the refused PUTs: %+v, want none", got)
	}
}

// An Authenticate that fails answers its error's problem and runs no row.
func TestAuthenticateFails(t *testing.T) {
	failing := serve(t, func(*http.Request) (auth.Caller, error) { return auth.Caller{}, vfserr.ErrAccess })
	if r := failing.do(t, http.MethodGet, thing, ""); r.status != http.StatusForbidden || r.problem(t).Code != "access" {
		t.Errorf("GET with Authenticate failing ErrAccess: answered %d %s, want 403 access", r.status, r.body)
	}
}

// An error no row names answers 500 "internal error", never its own text,
// and is logged with the request ID its body carries.
func TestUnnamedErrorIsLoggedNotAnswered(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	r := s.do(t, http.MethodGet, "/api/v1/fail", "")
	p := r.problem(t)
	if r.status != http.StatusInternalServerError || p.Detail != "internal error" || bytes.Contains(r.body, []byte(api.UnnamedError)) {
		t.Errorf("GET /api/v1/fail: answered %d %s, want 500 internal error without the error's text", r.status, r.body)
	}
	if bytes.Contains(r.body, []byte(`"code"`)) {
		t.Errorf("GET /api/v1/fail: answered %s, want no code, since none of the closed set names the error", r.body)
	}
	var line struct {
		RequestID string `json:"request_id"`
		Err       string `json:"err"`
	}
	if err := json.Unmarshal([]byte(s.log.String()), &line); err != nil {
		t.Fatalf("log %q: %v, want one JSON line", s.log.String(), err)
	}
	if p.RequestID == "" || line.RequestID != p.RequestID || line.Err != api.UnnamedError {
		t.Errorf("logged request %q err %q, want the body's request %q and err %q", line.RequestID, line.Err, p.RequestID, api.UnnamedError)
	}
}

// A call's span carries the row's pattern, the path's ID in canonical form
// once it parses, and the outcome; a path segment that is no ID reaches no
// attribute, however long.
func TestSpanAttributes(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	s.do(t, http.MethodGet, "/api/v1/things/"+strings.ToUpper(api.TestID), "")
	long := strings.Repeat("a", 200<<10)
	s.do(t, http.MethodGet, "/api/v1/things/"+long, "")
	spans := s.spans.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans: got %d, want 2", len(spans))
	}
	attrs := func(i int) map[string]string {
		m := map[string]string{}
		for _, a := range spans[i].Attributes() {
			m[string(a.Key)] = a.Value.String()
		}
		return m
	}
	want := map[string]string{"api.pattern": "GET /api/v1/things/{id}", "api.id": api.TestID, "api.outcome": "ok"}
	if got := attrs(0); !maps.Equal(got, want) {
		t.Errorf("span of the read: got %v, want %v", got, want)
	}
	want = map[string]string{"api.pattern": "GET /api/v1/things/{id}", "api.outcome": "not_found"}
	if got := attrs(1); !maps.Equal(got, want) {
		t.Errorf("span of the read naming no ID: got %.300v, want %v", got, want)
	}
}

// Each call counts in api_requests_total by op and outcome and in
// api_request_seconds by op; an error no row names answers 500 and counts in
// api_unmapped_errors_total; each call opens one span named by its op, and
// no attribute of any span carries a body field.
func TestSeriesAndSpans(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	const marker = "body-field-7f3a"
	put := "PUT /api/v1/knobs/{id}"
	s.do(t, http.MethodPut, knobPath, `{"spec":{"value":"`+marker+`"}}`, "If-None-Match", "*")
	s.do(t, http.MethodPut, knobPath, `{"spec":{"value":"`+marker+`"},"colour":1}`, "If-Match", `"1"`)
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

// sample is the value of the first series name with labels in reg, nil
// labels matching any: a counter's or a gauge's value, or a histogram's
// count; 0 when there is none.
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
				if labels != nil && labels[l.GetName()] != l.GetValue() {
					continue next
				}
			}
			if h := m.GetHistogram(); h != nil {
				return float64(h.GetSampleCount())
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}
