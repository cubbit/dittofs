package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"github.com/google/uuid"
)

// onDisk is a control plane over Badger on disk under t.TempDir(), behind a
// FaultKV the test may fault, and the KV beneath it, to read raw.
type onDisk struct {
	st    *kvstore.Store
	cp    kvstore.ControlPlane
	fault *kvtest.FaultKV
	raw   kv.KV
}

func newOnDisk(t *testing.T) *onDisk {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir(), NodeID: testNode, Clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the KV: %v", err)
		}
	})
	f := kvtest.NewFaultKV(db)
	if err := kvstore.Init(t.Context(), f, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	return &onDisk{st: st, cp: st.ControlPlane(), fault: f, raw: db}
}

func (o *onDisk) use(d *api.Deps) { d.Store = o.cp }

// one authenticates every request as the same account of role, eli under
// session-1, whose principal it returns.
func one(role entity.APIRole) (func(*http.Request) (auth.Caller, error), entity.Principal) {
	p := entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}
	return func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: p, Name: "eli", Credential: "session-1", Role: role}, nil
	}, p
}

func knobAt(id string) string { return "/api/v1/knobs/" + id }

// audits is every audit record the store holds, in store-time order.
func (s *served) audits(t *testing.T) []entity.AuditRecord {
	t.Helper()
	var out []entity.AuditRecord
	for a, err := range s.store.AuditRecords(t.Context(), entity.AuditFilter{}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func outcomes(as []entity.AuditRecord) []string {
	var out []string
	for _, a := range as {
		out = append(out, string(a.Outcome))
	}
	return out
}

// A write commits its record, its audit record and its idempotency record
// together or none of them: with the idempotency record's write faulted, on
// Badger on disk, neither the knob nor an applied audit record is there,
// and the refusal has its own; with the fault gone, the same request
// commits all three, and the audit record holds who, by name and credential,
// where, what, its path with its query, and how.
func TestWriteCommitsWholeOrNothing(t *testing.T) {
	disk := newOnDisk(t)
	caller, who := one(entity.RoleOperator)
	s := serve(t, caller, disk.use)
	id := uuid.NewString()
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpSet, Key: "IK‖*", Action: sim.FaultAction{Err: vfserr.ErrUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	disk.fault.SetFault(sched)
	put := func() reply {
		return s.do(t, http.MethodPut, knobAt(id)+"?trace=1", `{"spec":{"value":"on"}}`, "If-None-Match", "*", "Idempotency-Key", "k1")
	}
	if r := put(); r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable || r.header.Get("Retry-After") != "1" {
		t.Fatalf("PUT with its idempotency record faulted: answered %d %s Retry-After %q, want 503 unavailable with Retry-After 1", r.status, r.body, r.header.Get("Retry-After"))
	}
	if got := s.knob(t, id); got.Generation != 0 {
		t.Errorf("knob after the faulted commit: %+v, want none", got)
	}
	refusals := s.audits(t)
	if got := outcomes(refusals); !slices.Equal(got, []string{"refused: unavailable"}) {
		t.Fatalf("audit outcomes after the faulted commit: %q, want only the refusal", got)
	}
	if a := refusals[0]; a.Change != nil || a.After != 0 {
		t.Errorf("refusal's audit record: change %s after %d, want no change and no generation after", a.Change, a.After)
	}
	disk.fault.SetFault(nil)
	if r := put(); r.status != http.StatusOK || r.header.Get("ETag") != `"1"` {
		t.Fatalf("PUT once the fault is gone: answered %d %s ETag %q, want 200 ETag \"1\"", r.status, r.body, r.header.Get("ETag"))
	}
	if got := s.knob(t, id); got.Generation != 1 {
		t.Errorf("knob after the commit: %+v, want generation 1", got)
	}
	recs := s.audits(t)
	if got := outcomes(recs); !slices.Equal(got, []string{"refused: unavailable", "applied"}) {
		t.Fatalf("audit outcomes: %q, want the refusal then the applied write", got)
	}
	a := recs[1]
	var req struct {
		Method         string `json:"method"`
		Path           string `json:"path"`
		RequestID      string `json:"request_id"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(a.Request, &req); err != nil {
		t.Fatalf("audit request %s: %v", a.Request, err)
	}
	sid, _ := entity.ParseShareID(id)
	for _, c := range []struct {
		field     string
		got, want any
	}{
		{"actor", a.Actor, who},
		{"actor name", a.ActorName, "eli"},
		{"credential", a.Credential, "session-1"},
		{"node", a.Node, testNode},
		{"source is 127.0.0.1", strings.HasPrefix(a.Source, "127.0.0.1:"), true},
		{"request method", req.Method, http.MethodPut},
		{"request path", req.Path, knobAt(id) + "?trace=1"},
		{"request has an ID", req.RequestID != "", true},
		{"request key", req.IdempotencyKey, "k1"},
		{"target kind", a.Target.Kind, entity.ResourceSetting},
		{"target ID", string(a.Target.ID), string(sid[:])},
		{"before", a.Before, uint64(0)},
		{"after", a.After, uint64(1)},
		{"change", string(a.Change), `{"spec.value":[null,"on"]}`},
	} {
		if c.got != c.want {
			t.Errorf("applied audit record's %s: got %v, want %v", c.field, c.got, c.want)
		}
	}
	for outcome, want := range map[string]float64{"applied": 1, "refused": 1} {
		if got := sample(t, s.reg, "dittofs_audit_records_total", map[string]string{"outcome": outcome}); got != want {
			t.Errorf("audit_records_total{%s}: got %v, want %v", outcome, got, want)
		}
	}
}

// Each method's preconditions: a missing header 428, an action that changes
// the spec among them; a tag that is not the record's, If-None-Match: * on a
// record and If-Match on none 412, each naming the record's tag when there is
// one; a key over 255 bytes, If-Match beside If-None-Match, and an If-Match
// of * or of a list, on one line or two, 400. None of them writes, and each 412 and 428 is
// counted by kind. With them right, a PUT, a PATCH (a null resetting its
// field), an action and a DELETE commit, each answering the record's new tag
// but the DELETE, whose record is gone, and each audited with the tags it
// found and left.
func TestPreconditions(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	k := knobAt(id)
	if r := s.do(t, http.MethodPut, k, `{"spec":{"value":"a","turns":2}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("create: answered %d %s", r.status, r.body)
	}

	g := s.firstGen(t, id, "knob")
	cur, stale := etagOf(g), etagOf(g+1)
	long := strings.Repeat("k", 256)
	for _, tc := range []struct {
		name, method, path string
		hdr                []string
		status             int
		code               apiv1.Code
		field              string
		names              []string
	}{
		{"PUT without If-Match", http.MethodPut, k, nil, 428, apiv1.CodePreconditionRequired, "", nil},
		{"PATCH without If-Match", http.MethodPatch, k, nil, 428, apiv1.CodePreconditionRequired, "", nil},
		{"DELETE without If-Match", http.MethodDelete, k, []string{"Idempotency-Key", "d1"}, 428, apiv1.CodePreconditionRequired, "", nil},
		{"DELETE without a key", http.MethodDelete, k, []string{"If-Match", cur}, 428, apiv1.CodePreconditionRequired, "", nil},
		{"action without a key", http.MethodPost, k + ":turn", []string{"If-Match", cur}, 428, apiv1.CodePreconditionRequired, "", nil},
		{"action without If-Match", http.MethodPost, k + ":turn", []string{"Idempotency-Key", "a1"}, 428, apiv1.CodePreconditionRequired, "", nil},
		{"PUT with a stale tag", http.MethodPut, k, []string{"If-Match", stale}, 412, apiv1.CodeGenerationMismatch, "", []string{cur}},
		{"PUT with an unquoted tag", http.MethodPut, k, []string{"If-Match", strconv.FormatUint(g, 10)}, 412, apiv1.CodeGenerationMismatch, "", []string{cur}},
		{"PUT If-None-Match on a record", http.MethodPut, k, []string{"If-None-Match", "*"}, 412, apiv1.CodeGenerationMismatch, "", []string{cur}},
		{"PUT If-Match on no record", http.MethodPut, knobAt(uuid.NewString()), []string{"If-Match", cur}, 412, apiv1.CodeGenerationMismatch, "", nil},
		{"action with a stale tag", http.MethodPost, k + ":turn", []string{"If-Match", stale, "Idempotency-Key", "a2"}, 412, apiv1.CodeGenerationMismatch, "", []string{cur}},
		{"a key of 256 bytes", http.MethodPost, k + ":turn", []string{"If-Match", cur, "Idempotency-Key", long}, 400, "invalid", "Idempotency-Key", nil},
		{"If-Match beside If-None-Match", http.MethodPut, k, []string{"If-Match", cur, "If-None-Match", "*"}, 400, "invalid", "If-Match", nil},
		{"If-Match of *", http.MethodPut, k, []string{"If-Match", "*"}, 400, "invalid", "If-Match", nil},
		{"If-Match of a list", http.MethodPut, k, []string{"If-Match", cur + ", " + stale}, 400, "invalid", "If-Match", nil},
		{"If-Match on two lines", http.MethodPut, k, []string{"If-Match", cur, "If-Match", stale}, 400, "invalid", "If-Match", nil},
		{"If-Match on two lines, the stale first", http.MethodPut, k, []string{"If-Match", stale, "If-Match", cur}, 400, "invalid", "If-Match", nil},
	} {
		body := `{"spec":{"value":"b"}}`
		if tc.method == http.MethodPost || tc.method == http.MethodDelete {
			body = ""
		}
		r := s.do(t, tc.method, tc.path, body, tc.hdr...)
		if r.status != tc.status {
			t.Errorf("%s: answered %d %s, want %d", tc.name, r.status, r.body, tc.status)
			continue
		}
		if p := r.problem(t); p.Code != tc.code || p.Field != tc.field || !slices.Equal(p.Names, tc.names) {
			t.Errorf("%s: answered %q field %q names %q, want %q field %q names %q", tc.name, p.Code, p.Field, p.Names, tc.code, tc.field, tc.names)
		}
	}

	if got := s.knob(t, id); got.Generation != g {
		t.Errorf("knob after the refusals: generation %d, want %d", got.Generation, g)
	}
	for kind, want := range map[string]float64{"missing": 6, "generation": 5, "idempotency": 0} {
		if got := sample(t, s.reg, "dittofs_api_precondition_failures_total", map[string]string{"kind": kind}); got != want {
			t.Errorf("api_precondition_failures_total{%s}: got %v, want %v", kind, got, want)
		}
	}
	for _, step := range []struct {
		name, method, path, body string
		hdr                      []string
		tag, spec                string
	}{
		{"PUT", http.MethodPut, k, `{"spec":{"value":"c","turns":2}}`, []string{"If-Match", etagOf(g)}, etagOf(g + 1), `{"value":"c","turns":2}`},
		{"PATCH a field", http.MethodPatch, k, `{"spec":{"value":"d"}}`, []string{"If-Match", etagOf(g + 1)}, etagOf(g + 2), `{"value":"d","turns":2}`},
		{"PATCH a null", http.MethodPatch, k, `{"spec":{"turns":null}}`, []string{"If-Match", etagOf(g + 2)}, etagOf(g + 3), `{"value":"d"}`},
		{"action", http.MethodPost, k + ":turn", "", []string{"If-Match", etagOf(g + 3), "Idempotency-Key", strings.Repeat("k", 255)}, etagOf(g + 4), `{"value":"d","turns":1}`},
		{"DELETE", http.MethodDelete, k, "", []string{"If-Match", etagOf(g + 4), "Idempotency-Key", "d2"}, "", ""},
	} {
		r := s.do(t, step.method, step.path, step.body, step.hdr...)
		if r.status != http.StatusOK || r.header.Get("ETag") != step.tag {
			t.Errorf("%s: answered %d %s ETag %q, want 200 ETag %q", step.name, r.status, r.body, r.header.Get("ETag"), step.tag)
		}
		if got := s.knob(t, id); string(got.Value) != step.spec {
			t.Errorf("%s: knob holds %s, want %s", step.name, got.Value, step.spec)
		}
	}
	var tags [][2]uint64
	for _, a := range s.audits(t) {
		if string(a.Outcome) == "applied" {
			tags = append(tags, [2]uint64{a.Before, a.After})
		}
	}
	if want := [][2]uint64{{0, g}, {g, g + 1}, {g + 1, g + 2}, {g + 2, g + 3}, {g + 3, g + 4}, {g + 4, 0}}; !slices.Equal(tags, want) {
		t.Errorf("applied audit records' tags before and after: got %v, want %v", tags, want)
	}
}

// A body the request type refuses is refused before anything else, a
// status field and a patch setting one, an unknown field or a wrong type,
// a patch of null or of nothing among them, ahead of a stale tag, and
// writes nothing.
func TestBodyRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	if r := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("create: answered %d %s", r.status, r.body)
	}

	g := s.firstGen(t, id, "knob")
	for _, tc := range []struct {
		name, method, body, field string
	}{
		{"PUT setting status", http.MethodPut, `{"spec":{"value":"b"},"status":{"applied":true}}`, "status"},
		{"PATCH setting status", http.MethodPatch, `{"status":{"applied":true}}`, "status"},
		{"PATCH of an unknown field", http.MethodPatch, `{"spec":{"colour":"red"}}`, "spec.colour"},
		{"PATCH of a wrong type", http.MethodPatch, `{"spec":{"turns":"many"}}`, "spec.turns"},
		{"PATCH that is no object", http.MethodPatch, `[]`, ""},
		{"PATCH of null", http.MethodPatch, `null`, ""},
		{"PATCH with no body", http.MethodPatch, ``, ""},
	} {
		r := s.do(t, tc.method, knobAt(id), tc.body, "If-Match", etagOf(g+1))
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: answered %d %s, want 400", tc.name, r.status, r.body)
			continue
		}
		if p := r.problem(t); p.Code != "invalid" || p.Field != tc.field {
			t.Errorf("%s: answered %q field %q, want invalid naming %q", tc.name, p.Code, p.Field, tc.field)
		}
	}

	if got := s.knob(t, id); got.Generation != g || string(got.Value) != `{"value":"a"}` {
		t.Errorf("knob after the refused bodies: %s at %d, want {\"value\":\"a\"} at %d", got.Value, got.Generation, g)
	}
}

// gate is a fault that holds the first write of the setting record whose
// key names hold, telling the test on reached, until release closes; and
// counts the reads of the settings generation, one per attempt of a setting
// write.
type gate struct {
	hold             string
	reached, release chan struct{}
	held             atomic.Bool
	genReads         atomic.Int32
}

func newGate(id string) *gate {
	sid, _ := entity.ParseShareID(id)
	return &gate{hold: hex.EncodeToString(sid[:]), reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) At(op string, key []byte) (time.Duration, int, error) {
	k := string(key)
	switch {
	case op == kvtest.OpGet && k == "CFGGEN":
		g.genReads.Add(1)
	case op == kvtest.OpSet && strings.HasPrefix(k, "CFG‖") && strings.Contains(k, g.hold) && g.held.CompareAndSwap(false, true):
		close(g.reached)
		<-g.release
	}
	return 0, 0, nil
}

// await waits for ch to close, failing the test after a generous backstop.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: not reached in 10s", what)
	}
}

// Two writes naming one tag cannot both commit: the first, held after it
// read the record, commits after the second, conflicts, reads the new tag
// and is refused 412.
func TestTwoWritesOneTag(t *testing.T) {
	disk := newOnDisk(t)
	s := serve(t, as(entity.RoleOperator, false), disk.use)
	id := uuid.NewString()
	if r := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("create: answered %d %s", r.status, r.body)
	}
	g := newGate(id)
	disk.fault.SetFault(g)
	first := s.async(t, http.MethodPut, knobAt(id), `{"spec":{"value":"first"}}`, "If-Match", `"1"`)
	await(t, g.reached, "the first write's record")
	second := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"second"}}`, "If-Match", `"1"`)
	close(g.release)
	r := <-first
	if second.status != http.StatusOK {
		t.Errorf("second write: answered %d %s, want 200", second.status, second.body)
	}
	if p := r.problem(t); r.status != http.StatusPreconditionFailed || !slices.Equal(p.Names, []string{`"2"`}) {
		t.Errorf("first write, after the second committed: answered %d %s, want 412 naming \"2\"", r.status, r.body)
	}
	if got := s.knob(t, id); got.Generation != 2 || string(got.Value) != `{"value":"second"}` {
		t.Errorf("knob: %s at %d, want the second write's at 2", got.Value, got.Generation)
	}
}

// A key its account recorded is refused 422 idempotency_mismatch for a
// request that differs from the recorded one in its body, its method or its
// query alone, ahead of the If-Match the PATCH also lacks, and writes
// nothing; the key is its account's own, so another account's request under
// it is applied. A body that does not decode is refused 400 before the key is
// looked up.
func TestKeyScope(t *testing.T) {
	accounts := map[string]entity.Principal{"alice": entity.NewUser(), "bob": entity.NewUser()}
	s := serve(t, func(r *http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: accounts[r.Header.Get("Account")], Role: entity.RoleOperator}, nil
	})
	type req struct{ account, method, path, body string }
	knob := func(account, method, path, body string) req { return req{account, method, path, body} }
	for i, tc := range []struct {
		name          string
		first, second req
		status        int
	}{
		{"another body", knob("alice", http.MethodPut, knobAt(api.TestID), `{"spec":{"value":"a"}}`), knob("alice", http.MethodPut, knobAt(api.TestID), `{"spec":{"value":"b"}}`), 422},
		{"another method", knob("alice", http.MethodPut, knobAt(otherID), `{"spec":{"value":"a"}}`), knob("alice", http.MethodPatch, knobAt(otherID), `{"spec":{"value":"a"}}`), 422},
		{"another query", knob("alice", http.MethodPost, "/api/v1/knobs", `{"spec":{"value":"a"}}`), knob("alice", http.MethodPost, "/api/v1/knobs?v=2", `{"spec":{"value":"a"}}`), 422},
		{"another account", knob("alice", http.MethodPost, "/api/v1/knobs", `{"spec":{"value":"a"}}`), knob("bob", http.MethodPost, "/api/v1/knobs", `{"spec":{"value":"a"}}`), 201},
	} {
		key := "k" + strconv.Itoa(i)
		send := func(q req) reply {
			return s.do(t, q.method, q.path, q.body, "Account", q.account, "Idempotency-Key", key, "If-None-Match", "*")
		}
		first := send(tc.first)
		if first.status/100 != 2 {
			t.Fatalf("%s: first request answered %d %s, want it applied", tc.name, first.status, first.body)
		}
		r := send(tc.second)
		if r.status != tc.status {
			t.Errorf("%s: key reused answered %d %s, want %d", tc.name, r.status, r.body, tc.status)
			continue
		}
		if tc.status == 422 && r.problem(t).Code != apiv1.CodeIdempotencyMismatch {
			t.Errorf("%s: key reused answered %s, want idempotency_mismatch", tc.name, r.body)
		}
		if tc.status == 201 && r.header.Get("Location") == first.header.Get("Location") {
			t.Errorf("%s: key reused answered the first request's Location %q, want a record of its own", tc.name, first.header.Get("Location"))
		}
	}
	got := outcomes(s.audits(t))
	if n := len(got) - len(slices.DeleteFunc(slices.Clone(got), func(o string) bool { return o == "applied" })); n != 5 {
		t.Errorf("audit outcomes %q: %d applied, want 5, the four first requests and the other account's", got, n)
	}
	if got := sample(t, s.reg, "dittofs_api_precondition_failures_total", map[string]string{"kind": "idempotency"}); got != 3 {
		t.Errorf("api_precondition_failures_total{idempotency}: got %v, want 3", got)
	}
	r := s.do(t, http.MethodPut, knobAt(api.TestID), `{"spec":{"colour":1}}`, "Account", "alice", "Idempotency-Key", "k0", "If-None-Match", "*")
	if r.status != http.StatusBadRequest {
		t.Fatalf("a body that does not decode under a recorded key: answered %d %s, want 400", r.status, r.body)
	}
	if p := r.problem(t); p.Code != "invalid" || p.Field != "spec.colour" {
		t.Errorf("a body that does not decode under a recorded key: answered %q field %q, want invalid naming spec.colour", p.Code, p.Field)
	}
}

// A POST on the collection creates a record under a new ID and answers 201
// with a Location naming it, its tag as the ETag, and an audit record
// targeting it. A retry under the same key answers what the first request
// was answered, its status, headers and body, and changes nothing; a request
// refused records nothing, so a corrected retry under its key is a new
// request.
func TestReplay(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	s := serve(t, caller)
	create := func() reply {
		return s.do(t, http.MethodPost, "/api/v1/knobs", `{"spec":{"value":"a"}}`, "Idempotency-Key", "c1")
	}
	first, again := create(), create()
	id, ok := strings.CutPrefix(first.header.Get("Location"), "/api/v1/knobs/")
	sid, err := entity.ParseShareID(id)
	if first.status != http.StatusCreated || !ok || err != nil {
		t.Fatalf("create: answered %d %s Location %q, want 201 naming /api/v1/knobs/{id}", first.status, first.body, first.header.Get("Location"))
	}

	g := s.firstGen(t, id, "knob")
	if first.header.Get("ETag") != etagOf(g) {
		t.Fatalf("create: ETag %q, want %s", first.header.Get("ETag"), etagOf(g))
	}
	if again.status != first.status || !bytes.Equal(again.body, first.body) {
		t.Errorf("replay: answered %d %s, want the first answer %d %s", again.status, again.body, first.status, first.body)
	}
	for _, h := range []string{"ETag", "Location", "Content-Type"} {
		if again.header.Get(h) != first.header.Get(h) {
			t.Errorf("replay's %s: %q, want the first answer's %q", h, again.header.Get(h), first.header.Get(h))
		}
	}

	if got := s.knob(t, id); got.Generation != g || string(got.Value) != `{"value":"a"}` {
		t.Errorf("knob the Location names: %s at %d, want the body at %d", got.Value, got.Generation, g)
	}
	recs := s.audits(t)
	if len(recs) != 1 || recs[0].Target.Kind != entity.ResourceSetting || !bytes.Equal(recs[0].Target.ID, sid[:]) {
		t.Fatalf("audit records after a create and its replay: %+v, want one targeting the created knob", recs)
	}
	if r := s.do(t, http.MethodDelete, knobAt(id), "", "If-Match", etagOf(g+1), "Idempotency-Key", "d1"); r.status != http.StatusPreconditionFailed {
		t.Errorf("DELETE with a stale tag: answered %d %s, want 412", r.status, r.body)
	}
	if r := s.do(t, http.MethodDelete, knobAt(id), "", "If-Match", etagOf(g), "Idempotency-Key", "d1"); r.status != http.StatusOK {
		t.Errorf("corrected DELETE under the refused request's key: answered %d %s, want 200", r.status, r.body)
	}
	for name, want := range map[string]float64{"dittofs_api_idempotent_replays_total": 1, "dittofs_api_idempotency_records_written_total": 2} {
		if got := sample(t, s.reg, name, nil); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

// signingKey is an installation's signing key, the same at every version.
func signingKey(k string) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(k), nil }
}

// An idempotency key's digest is keyed by the installation's signing key:
// under the same key a request replays, under another key it computes
// another digest and answers 422, and the store holds no plain SHA-256 of
// the request.
func TestDigestKeyed(t *testing.T) {
	disk := newOnDisk(t)
	caller, _ := one(entity.RoleOperator)
	var key atomic.Pointer[string]
	key.Store(new("installation-a"))
	s := serve(t, caller, disk.use, func(d *api.Deps) {
		d.SigningKey = func(context.Context) ([]byte, error) { return []byte(*key.Load()), nil }
	})
	const body = `{"spec":{"value":"a"}}`
	create := func() reply {
		return s.do(t, http.MethodPost, "/api/v1/knobs", body, "Idempotency-Key", "c1")
	}
	first := create()
	if first.status != http.StatusCreated {
		t.Fatalf("create: answered %d %s, want 201", first.status, first.body)
	}
	if r := create(); r.status != first.status || !bytes.Equal(r.body, first.body) {
		t.Errorf("replay under the same signing key: answered %d %s, want the first answer %d %s", r.status, r.body, first.status, first.body)
	}
	key.Store(new("installation-b"))
	if r := create(); r.status != http.StatusUnprocessableEntity || r.problem(t).Code != apiv1.CodeIdempotencyMismatch {
		t.Errorf("replay under another signing key: answered %d %s, want 422 %s", r.status, r.body, apiv1.CodeIdempotencyMismatch)
	}
	plain := sha256.Sum256([]byte(http.MethodPost + " /api/v1/knobs\n" + body))
	err := disk.raw.View(t.Context(), func(rd kv.Reader) error {
		for kvp, err := range rd.Scan(nil, nil) {
			if err != nil {
				return err
			}
			if bytes.Contains(kvp.Value, plain[:]) {
				t.Errorf("record %q holds the request's plain SHA-256", kvp.Key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan of the KV: %v", err)
	}
}

// A secret a write carries reaches no record: its pre-write step seals it,
// and neither the record, the audit records, the idempotency record nor
// the answer holds its value, read raw from the KV; the audit record holds
// the field as "set", not even its sealed reference.
func TestSecretReachesNoRecord(t *testing.T) {
	disk := newOnDisk(t)
	caller, _ := one(entity.RoleOperator)
	s := serve(t, caller, disk.use)
	const secret = "hunter2"
	id := uuid.NewString()
	r := s.do(t, http.MethodPut, "/api/v1/vaults/"+id, `{"spec":{"password":"`+secret+`"}}`, "If-None-Match", "*", "Idempotency-Key", "v1")
	if r.status != http.StatusOK || bytes.Contains(r.body, []byte(secret)) {
		t.Fatalf("PUT vault: answered %d %s, want 200 without the password", r.status, r.body)
	}
	if r := s.do(t, http.MethodPut, "/api/v1/vaults/"+id, `{"spec":{"password":"`+secret+`"}}`, "If-Match", `"7"`, "Idempotency-Key", "v2"); r.status != http.StatusPreconditionFailed {
		t.Fatalf("PUT vault with a stale tag: answered %d %s, want 412", r.status, r.body)
	}
	recs := s.audits(t)
	if got := outcomes(recs); !slices.Equal(got, []string{"applied", "refused: generation_mismatch"}) {
		t.Fatalf("audit outcomes: %q, want the write and its refusal", got)
	}
	if got := string(recs[0].Change); got != `{"spec.password":"set"}` {
		t.Errorf("vault write's audited change: %s, want {\"spec.password\":\"set\"}", got)
	}
	n := 0
	err := disk.raw.View(t.Context(), func(rd kv.Reader) error {
		for kvp, err := range rd.Scan(nil, nil) {
			if err != nil {
				return err
			}
			n++
			if bytes.Contains(kvp.Key, []byte(secret)) || bytes.Contains(kvp.Value, []byte(secret)) {
				t.Errorf("record %q holds the secret: %q", kvp.Key, kvp.Value)
			}
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("scan of the KV: %d records, %v", n, err)
	}
}

// A pre-write step that fails refuses the write with the step's error
// before anything is written; a conflict retry of the transaction does not
// run the step again.
func TestPrepare(t *testing.T) {
	disk := newOnDisk(t)
	s := serve(t, as(entity.RoleOperator, false), disk.use)
	failing := uuid.NewString()
	r := s.do(t, http.MethodPut, "/api/v1/vaults/"+failing, `{"spec":{"password":"`+api.Unreachable+`"}}`, "If-None-Match", "*")
	if p := r.problem(t); r.status != http.StatusServiceUnavailable || p.Code != apiv1.CodeUnavailable {
		t.Errorf("PUT vault whose sealing fails: answered %d %q, want 503 unavailable", r.status, p.Code)
	}
	if got := s.setting(t, failing, "vault"); got.Generation != 0 {
		t.Errorf("vault after its sealing failed: %+v, want none", got)
	}
	if got := outcomes(s.audits(t)); !slices.Equal(got, []string{"refused: unavailable"}) {
		t.Errorf("audit outcomes after the sealing failed: %q, want only the refusal", got)
	}

	id := uuid.NewString()
	g := newGate(id)
	disk.fault.SetFault(g)
	held := s.async(t, http.MethodPut, "/api/v1/vaults/"+id, `{"spec":{"password":"p"}}`, "If-None-Match", "*")
	await(t, g.reached, "the vault's record")
	if r := s.do(t, http.MethodPut, knobAt(uuid.NewString()), `{"spec":{"value":"a"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Errorf("knob written while the vault's write is held: answered %d %s, want 200", r.status, r.body)
	}
	close(g.release)
	if r := <-held; r.status != http.StatusOK {
		t.Errorf("vault write after its conflict: answered %d %s, want 200", r.status, r.body)
	}
	if got := g.genReads.Load(); got != 3 {
		t.Errorf("settings generation reads: got %d, want 3, the vault's two attempts and the knob's one", got)
	}
	if got := api.Seals(id); got != 1 {
		t.Errorf("vault sealed %d times across its attempts, want 1", got)
	}
}

// The pre-write step runs only for a request that may write: a replay
// answers without it, a key reused for another request is refused 422
// though the step would fail, and a request lacking a header its method
// requires is refused 428 before it.
func TestPrepareOnlyForAWrite(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	s := serve(t, caller)
	id, bare := uuid.NewString(), uuid.NewString()
	put := func(id, password string, hdr ...string) func() reply {
		return func() reply {
			return s.do(t, http.MethodPut, "/api/v1/vaults/"+id, `{"spec":{"password":"`+password+`"}}`, hdr...)
		}
	}
	for _, step := range []struct {
		name     string
		do       func() reply
		status   int
		id       string
		sealings int
	}{
		{"first write", put(id, "p", "If-None-Match", "*", "Idempotency-Key", "v1"), 200, id, 1},
		{"replay", put(id, "p", "If-None-Match", "*", "Idempotency-Key", "v1"), 200, id, 1},
		{"key reused, the step failing", put(id, api.Unreachable, "If-None-Match", "*", "Idempotency-Key", "v1"), 422, id, 1},
		{"no If-Match", put(bare, "p"), 428, bare, 0},
	} {
		if r := step.do(); r.status != step.status {
			t.Errorf("%s: answered %d %s, want %d", step.name, r.status, r.body, step.status)
		}
		if got := api.Seals(step.id); got != step.sealings {
			t.Errorf("%s: vault sealed %d times, want %d", step.name, got, step.sealings)
		}
	}
}

// The writer's wait ends after a second on the API's clock, and the write
// is answered at its commit while the node has not yet applied it.
func TestWriterWaitsASecondOnTheClock(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	hold := api.NewHold(nil)
	s := serve(t, caller, func(d *api.Deps) { d.Applied = hold.Applied })
	base := s.clock.Pending()
	id := uuid.NewString()
	answered := make(chan polled, 1)
	go func() {
		r, err := s.send(t.Context(), http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*")
		answered <- polled{r, err}
	}()
	await(t, hold.Reached, "the node's Applied")
	eventually(t, "the writer's wait is on the API's clock", func() bool { return s.clock.Pending() == base+1 })
	s.clock.Advance(time.Second)
	select {
	case r := <-answered:
		g := s.firstGen(t, id, "knob")
		if r.err != nil || r.status != http.StatusOK || r.header.Get("ETag") != etagOf(g) {
			t.Errorf("PUT knob once a second passed: answered %d %s ETag %q err %v, want the commit's 200 with ETag %s", r.status, r.body, r.header.Get("ETag"), r.err, etagOf(g))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PUT knob: no answer in 10s once a second passed on the API's clock")
	}
}

// A write raising the settings generation waits for the node to apply it,
// after its commit, and its answer is held until the node has; the answer
// is the commit's own though the wait fails. A replay, a refused write and a
// write raising no generation do not wait.
func TestWriterWaits(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	hold := api.NewHold(errors.New("not applied"))
	release := sync.OnceFunc(func() { close(hold.Release) })
	s := serve(t, caller, func(d *api.Deps) { d.Applied = hold.Applied })
	t.Cleanup(release) // before the harness's own cleanups, which find a held call leaked
	id := uuid.NewString()
	put := func() reply {
		return s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*", "Idempotency-Key", "w1")
	}
	answered := s.async(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*", "Idempotency-Key", "w1")
	await(t, hold.Reached, "the node's Applied")
	g := s.firstGen(t, id, "knob")
	// No event marks an answer that is never sent, so a bounded wait is the
	// assertion here: an answer not held arrives well inside it.
	select {
	case r := <-answered:
		t.Fatalf("PUT knob answered %d %s while Applied is held, want no answer until it returns", r.status, r.body)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	var r reply
	select {
	case r = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("PUT knob: no answer in 10s once Applied returned")
	}
	if r.status != http.StatusOK || r.header.Get("ETag") != etagOf(g) || string(r.body) != `{"spec":{"value":"a"}}` {
		t.Errorf("PUT with Applied failing: answered %d %s ETag %q, want the commit's 200 with ETag %s", r.status, r.body, r.header.Get("ETag"), etagOf(g))
	}
	for _, tc := range []struct {
		name   string
		do     func() reply
		status int
	}{
		{"replay", put, http.StatusOK},
		{"refused write", func() reply {
			return s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"b"}}`, "If-Match", `"9"`)
		}, http.StatusPreconditionFailed},
		{"vault write", func() reply {
			return s.do(t, http.MethodPut, "/api/v1/vaults/"+uuid.NewString(), `{"spec":{"password":"p"}}`, "If-None-Match", "*")
		}, http.StatusOK},
	} {
		if r := tc.do(); r.status != tc.status {
			t.Errorf("%s: answered %d %s, want %d", tc.name, r.status, r.body, tc.status)
		}
		select {
		case <-hold.Reached:
			t.Errorf("%s: waited for the node's Applied, want no wait", tc.name)
		default:
		}
	}
}

// A refusal of an authenticated caller before the write adapter runs is
// audited once, as refused with its code, against the row's kind and the
// path's ID when it parses, and naming the caller by principal, name and
// credential: a role that may not call the row, an ID that does not parse, a
// caller that must change its password; a write's own refusal is audited
// once, by the adapter. A refused GET or HEAD is audited as every other
// refusal is; a path no row names is not audited, nor tried and failed.
func TestRefusalsAudited(t *testing.T) {
	var role atomic.Uint32
	var mustChange atomic.Bool
	who := entity.NewUser()
	s := serve(t, func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: who, Name: "eli", Credential: "token-7", Role: entity.APIRole(role.Load()), MustChange: mustChange.Load()}, nil
	})
	for _, tc := range []struct {
		name         string
		role         entity.APIRole
		mustChange   bool
		method, path string
		status       int
		outcome      string
		kind         entity.ResourceKind
		named        bool
	}{
		{"role", entity.RoleViewer, false, http.MethodPost, thing + ":poke", 403, "refused: forbidden", entity.ResourceShare, true},
		{"ID", entity.RoleOperator, false, http.MethodPost, "/api/v1/things/payroll:poke", 404, "refused: not_found", entity.ResourceShare, false},
		{"password", entity.RoleAdmin, true, http.MethodPut, knobPath, 403, "refused: password_change_required", entity.ResourceSetting, true},
		{"write", entity.RoleOperator, false, http.MethodPut, knobPath, 428, "refused: precondition_required", entity.ResourceSetting, true},
		{"GET", entity.RoleViewer, false, http.MethodGet, "/api/v1/accounts/" + api.TestID, 404, "refused: not_found", entity.ResourceUser, true},
		{"HEAD", entity.RoleViewer, false, http.MethodHead, "/api/v1/accounts/" + api.TestID, 404, "refused: not_found", entity.ResourceUser, true},
		{"GET password", entity.RoleViewer, true, http.MethodGet, thing, 403, "refused: password_change_required", entity.ResourceShare, true},
		{"no row", entity.RoleOperator, false, http.MethodPost, "/api/v1/no-such", 404, "", 0, false},
	} {
		role.Store(uint32(tc.role))
		mustChange.Store(tc.mustChange)
		before := len(s.audits(t))
		if r := s.do(t, tc.method, tc.path, `{"spec":{"value":"a"}}`); r.status != tc.status {
			t.Errorf("%s: answered %d %s, want %d", tc.name, r.status, r.body, tc.status)
			continue
		}
		recs := s.audits(t)[before:]
		if tc.outcome == "" {
			if len(recs) != 0 {
				t.Errorf("%s: audited %q, want nothing", tc.name, outcomes(recs))
			}
			continue
		}
		if len(recs) != 1 {
			t.Errorf("%s: audited %q, want one record", tc.name, outcomes(recs))
			continue
		}
		a := recs[0]
		if string(a.Outcome) != tc.outcome || a.Target.Kind != tc.kind || a.Actor != who || a.ActorName != "eli" || a.Credential != "token-7" {
			t.Errorf("%s: audited %s against %v by %v %q under %q, want %s against %v by %v eli under token-7", tc.name, a.Outcome, a.Target.Kind, a.Actor, a.ActorName, a.Credential, tc.outcome, tc.kind, who)
		}
		var want []byte
		if tc.named {
			sid, _ := entity.ParseShareID(api.TestID)
			want = sid[:]
		}
		if !bytes.Equal(a.Target.ID, want) {
			t.Errorf("%s: audited against ID %x, want %x", tc.name, a.Target.ID, want)
		}
	}
	if strings.Contains(s.log.String(), "audit record was not written") {
		t.Errorf("log %q: an audit record failed to write, want none tried that could not be", s.log.String())
	}
}

// A refusal whose audit record fails to write is answered as it would be,
// and the failure is logged at Error with the request's ID.
func TestRefusalAuditFailureLogged(t *testing.T) {
	disk := newOnDisk(t)
	s := serve(t, as(entity.RoleOperator, false), disk.use)
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpSet, Key: "AU‖*", Action: sim.FaultAction{Err: vfserr.ErrUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	disk.fault.SetFault(sched)
	r := s.do(t, http.MethodPut, knobPath, `{"spec":{"value":"a"}}`)
	if r.status != http.StatusPreconditionRequired {
		t.Fatalf("PUT without If-Match, its audit faulted: answered %d %s, want 428", r.status, r.body)
	}
	id := r.problem(t).RequestID
	for line := range strings.Lines(s.log.String()) {
		var l struct {
			Level, Msg string
			RequestID  string `json:"request_id"`
		}
		if json.Unmarshal([]byte(line), &l) == nil && l.Level == "ERROR" && l.RequestID == id && strings.Contains(l.Msg, "audit record was not written") {
			return
		}
	}
	t.Errorf("log %q: no Error line for request %q's unwritten audit record", s.log.String(), id)
}

// A refused request's audit record holds at most 512 bytes of its path and no
// key over 255 bytes, however long the caller sends them: a role refused
// before the adapter, and the adapter's refusal of a key too long.
func TestAuditRequestBounded(t *testing.T) {
	var role atomic.Uint32
	s := serve(t, func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: entity.NewUser(), Role: entity.APIRole(role.Load())}, nil
	})
	key := strings.Repeat("k", 64<<10)
	for _, tc := range []struct {
		name, method, path string
		role               entity.APIRole
		status             int
	}{
		{"role", http.MethodPost, "/api/v1/things/" + strings.Repeat("x", 64<<10) + ":poke", entity.RoleViewer, 403},
		{"key", http.MethodPut, knobPath, entity.RoleOperator, 400},
	} {
		role.Store(uint32(tc.role))
		before := len(s.audits(t))
		if r := s.do(t, tc.method, tc.path, `{"spec":{"value":"a"}}`, "Idempotency-Key", key, "If-Match", `"1"`); r.status != tc.status {
			t.Errorf("%s: answered %d %s, want %d", tc.name, r.status, r.body, tc.status)
			continue
		}
		recs := s.audits(t)[before:]
		if len(recs) != 1 {
			t.Errorf("%s: audited %q, want one record", tc.name, outcomes(recs))
			continue
		}
		var req struct {
			Path           string `json:"path"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.Unmarshal(recs[0].Request, &req); err != nil {
			t.Fatalf("%s: audit request %s: %v", tc.name, recs[0].Request, err)
		}
		if len(req.Path) > 512 || !strings.HasPrefix(tc.path, req.Path) || req.IdempotencyKey != "" {
			t.Errorf("%s: audit request holds a path of %d bytes and a key of %d, want a prefix of the path of at most 512 and no key", tc.name, len(req.Path), len(req.IdempotencyKey))
		}
	}
}
