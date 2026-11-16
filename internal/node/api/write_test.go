package api_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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
	return &onDisk{cp: st.ControlPlane(), fault: f, raw: db}
}

func (o *onDisk) use(d *api.Deps) { d.Store = o.cp }

// one authenticates every request as the same account of role, whose
// principal it returns.
func one(role entity.APIRole) (func(*http.Request) (auth.Caller, error), entity.Principal) {
	p := entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}
	return func(*http.Request) (auth.Caller, error) { return auth.Caller{Principal: p, Role: role}, nil }, p
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
// commits all three, and the audit record holds who, where, what and how.
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
		return s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"on"}}`, "If-None-Match", "*", "Idempotency-Key", "k1")
	}
	if r := put(); r.status != http.StatusServiceUnavailable || r.problem(t).Code != apiv1.CodeUnavailable {
		t.Fatalf("PUT with its idempotency record faulted: answered %d %s, want 503 unavailable", r.status, r.body)
	}
	if got := s.knob(t, id); got.Generation != 0 {
		t.Errorf("knob after the faulted commit: %+v, want none", got)
	}
	if got := outcomes(s.audits(t)); !slices.Equal(got, []string{"refused: unavailable"}) {
		t.Errorf("audit outcomes after the faulted commit: %q, want only the refusal", got)
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
		{"node", a.Node, testNode},
		{"source is 127.0.0.1", strings.HasPrefix(a.Source, "127.0.0.1:"), true},
		{"request method", req.Method, http.MethodPut},
		{"request path", req.Path, knobAt(id)},
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

// Each method's preconditions: a missing header 428, a tag that is not the
// record's, If-None-Match: * on a record and If-Match on none 412, each
// naming the record's tag when there is one, a key over 255 bytes 400; none
// of them writes, and each is counted by kind. With them right, a PUT, a
// PATCH (a null resetting its field), an action and a DELETE commit, each
// answering the record's new tag.
func TestPreconditions(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	k := knobAt(id)
	if r := s.do(t, http.MethodPut, k, `{"spec":{"value":"a","turns":2}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("create: answered %d %s", r.status, r.body)
	}
	long := strings.Repeat("k", 256)
	for _, tc := range []struct {
		name, method, path string
		hdr                []string
		status             int
		code               apiv1.Code
		names              []string
	}{
		{"PUT without If-Match", http.MethodPut, k, nil, 428, apiv1.CodePreconditionRequired, nil},
		{"PATCH without If-Match", http.MethodPatch, k, nil, 428, apiv1.CodePreconditionRequired, nil},
		{"DELETE without If-Match", http.MethodDelete, k, []string{"Idempotency-Key", "d1"}, 428, apiv1.CodePreconditionRequired, nil},
		{"DELETE without a key", http.MethodDelete, k, []string{"If-Match", `"1"`}, 428, apiv1.CodePreconditionRequired, nil},
		{"action without a key", http.MethodPost, k + ":turn", nil, 428, apiv1.CodePreconditionRequired, nil},
		{"PUT with a stale tag", http.MethodPut, k, []string{"If-Match", `"9"`}, 412, apiv1.CodeGenerationMismatch, []string{`"1"`}},
		{"PUT with an unquoted tag", http.MethodPut, k, []string{"If-Match", "1"}, 412, apiv1.CodeGenerationMismatch, []string{`"1"`}},
		{"PUT If-None-Match on a record", http.MethodPut, k, []string{"If-None-Match", "*"}, 412, apiv1.CodeGenerationMismatch, []string{`"1"`}},
		{"PUT If-Match on no record", http.MethodPut, knobAt(uuid.NewString()), []string{"If-Match", `"1"`}, 412, apiv1.CodeGenerationMismatch, nil},
		{"a key of 256 bytes", http.MethodPost, k + ":turn", []string{"Idempotency-Key", long}, 400, "invalid", nil},
	} {
		body := `{"spec":{"value":"b"}}`
		if tc.method == http.MethodPost || tc.method == http.MethodDelete {
			body = ""
		}
		r := s.do(t, tc.method, tc.path, body, tc.hdr...)
		p := r.problem(t)
		if r.status != tc.status || p.Code != tc.code || !slices.Equal(p.Names, tc.names) {
			t.Errorf("%s: answered %d %q names %q, want %d %q names %q", tc.name, r.status, p.Code, p.Names, tc.status, tc.code, tc.names)
		}
	}
	if got := s.knob(t, id); got.Generation != 1 {
		t.Errorf("knob after the refusals: generation %d, want 1", got.Generation)
	}
	for kind, want := range map[string]float64{"missing": 5, "generation": 4, "idempotency": 0} {
		if got := sample(t, s.reg, "dittofs_api_precondition_failures_total", map[string]string{"kind": kind}); got != want {
			t.Errorf("api_precondition_failures_total{%s}: got %v, want %v", kind, got, want)
		}
	}
	for _, step := range []struct {
		name, method, path, body string
		hdr                      []string
		tag, spec                string
	}{
		{"PUT", http.MethodPut, k, `{"spec":{"value":"c","turns":2}}`, []string{"If-Match", `"1"`}, `"2"`, `{"value":"c","turns":2}`},
		{"PATCH a field", http.MethodPatch, k, `{"spec":{"value":"d"}}`, []string{"If-Match", `"2"`}, `"3"`, `{"value":"d","turns":2}`},
		{"PATCH a null", http.MethodPatch, k, `{"spec":{"turns":null}}`, []string{"If-Match", `"3"`}, `"4"`, `{"value":"d"}`},
		{"action", http.MethodPost, k + ":turn", "", []string{"Idempotency-Key", strings.Repeat("k", 255)}, `"5"`, `{"value":"d","turns":1}`},
		{"DELETE", http.MethodDelete, k, "", []string{"If-Match", `"5"`, "Idempotency-Key", "d2"}, `"6"`, `{"value":""}`},
	} {
		r := s.do(t, step.method, step.path, step.body, step.hdr...)
		if r.status != http.StatusOK || r.header.Get("ETag") != step.tag {
			t.Errorf("%s: answered %d %s ETag %q, want 200 ETag %s", step.name, r.status, r.body, r.header.Get("ETag"), step.tag)
		}
		if got := s.knob(t, id); string(got.Value) != step.spec {
			t.Errorf("%s: knob holds %s, want %s", step.name, got.Value, step.spec)
		}
	}
}

// A body the request type refuses is refused before anything else, a
// status field and a patch setting one, an unknown field or a wrong type
// among them, and writes nothing.
func TestBodyRefused(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	id := uuid.NewString()
	if r := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("create: answered %d %s", r.status, r.body)
	}
	for _, tc := range []struct {
		name, method, body, field string
	}{
		{"PUT setting status", http.MethodPut, `{"spec":{"value":"b"},"status":{"applied":true}}`, "status"},
		{"PATCH setting status", http.MethodPatch, `{"status":{"applied":true}}`, "status"},
		{"PATCH of an unknown field", http.MethodPatch, `{"spec":{"colour":"red"}}`, "spec.colour"},
		{"PATCH of a wrong type", http.MethodPatch, `{"spec":{"turns":"many"}}`, "spec.turns"},
		{"PATCH that is no object", http.MethodPatch, `[]`, ""},
	} {
		r := s.do(t, tc.method, knobAt(id), tc.body, "If-Match", `"1"`)
		if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "invalid" || p.Field != tc.field {
			t.Errorf("%s: answered %d %q field %q, want 400 invalid naming %q", tc.name, r.status, p.Code, p.Field, tc.field)
		}
	}
	if got := s.knob(t, id); got.Generation != 1 || string(got.Value) != `{"value":"a"}` {
		t.Errorf("knob after the refused bodies: %s at %d, want {\"value\":\"a\"} at 1", got.Value, got.Generation)
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
	first := make(chan reply, 1)
	go func() {
		first <- s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"first"}}`, "If-Match", `"1"`)
	}()
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

// A key its account recorded for another request is refused 422
// idempotency_mismatch, ahead of the If-Match the request also lacks.
func TestKeyCheckedBeforePreconditions(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	s := serve(t, caller)
	id := uuid.NewString()
	if r := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*", "Idempotency-Key", "k1"); r.status != http.StatusOK {
		t.Fatalf("create under k1: answered %d %s", r.status, r.body)
	}
	r := s.do(t, http.MethodPatch, knobAt(id), `{"spec":{"value":"b"}}`, "Idempotency-Key", "k1")
	if p := r.problem(t); r.status != http.StatusUnprocessableEntity || p.Code != apiv1.CodeIdempotencyMismatch {
		t.Errorf("PATCH under k1 without If-Match: answered %d %q, want 422 idempotency_mismatch", r.status, p.Code)
	}
	if got := sample(t, s.reg, "dittofs_api_precondition_failures_total", map[string]string{"kind": "idempotency"}); got != 1 {
		t.Errorf("api_precondition_failures_total{idempotency}: got %v, want 1", got)
	}
}

// A retry under the same key answers what the first request was answered,
// its status, headers and body, and changes nothing; a request refused
// records nothing, so a corrected retry under its key is a new request.
func TestReplay(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	s := serve(t, caller)
	id := uuid.NewString()
	if r := s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("create: answered %d %s", r.status, r.body)
	}
	turn := func() reply { return s.do(t, http.MethodPost, knobAt(id)+":turn", "", "Idempotency-Key", "t1") }
	first, again := turn(), turn()
	if first.status != http.StatusOK || again.status != first.status || !bytes.Equal(again.body, first.body) {
		t.Errorf("replay: answered %d %s, want the first answer %d %s", again.status, again.body, first.status, first.body)
	}
	for _, h := range []string{"ETag", "Location", "Content-Type"} {
		if again.header.Get(h) != first.header.Get(h) {
			t.Errorf("replay's %s: %q, want the first answer's %q", h, again.header.Get(h), first.header.Get(h))
		}
	}
	if got := s.knob(t, id); got.Generation != 2 || string(got.Value) != `{"value":"a","turns":1}` {
		t.Errorf("knob after a turn and its replay: %s at %d, want one turn at 2", got.Value, got.Generation)
	}
	if r := s.do(t, http.MethodDelete, knobAt(id), "", "If-Match", `"1"`, "Idempotency-Key", "d1"); r.status != http.StatusPreconditionFailed {
		t.Errorf("DELETE with a stale tag: answered %d %s, want 412", r.status, r.body)
	}
	if r := s.do(t, http.MethodDelete, knobAt(id), "", "If-Match", `"2"`, "Idempotency-Key", "d1"); r.status != http.StatusOK {
		t.Errorf("corrected DELETE under the refused request's key: answered %d %s, want 200", r.status, r.body)
	}
	for name, want := range map[string]float64{"dittofs_api_idempotent_replays_total": 1, "dittofs_api_idempotency_records_written_total": 2} {
		if got := sample(t, s.reg, name, nil); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

// A secret a write carries reaches no record: its pre-write step seals it,
// and neither the record, the audit records, the idempotency record nor
// the answer holds its value, read raw from the KV.
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
	if got := outcomes(s.audits(t)); len(got) != 2 {
		t.Fatalf("audit outcomes: %q, want the write and its refusal", got)
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
	held := make(chan reply, 1)
	go func() {
		held <- s.do(t, http.MethodPut, "/api/v1/vaults/"+id, `{"spec":{"password":"p"}}`, "If-None-Match", "*")
	}()
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

// applied records each call of a node's Applied, and whether the write the
// test made last already reads back by then, answering err.
type applied struct {
	mu    sync.Mutex
	calls []bool
	read  func() bool
	err   error
}

func (a *applied) Applied(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, a.read())
	return a.err
}

func (a *applied) seen() []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// A write raising the settings generation waits for the node to apply it,
// once, after its commit; a refused write, a replay and a write raising no
// generation do not.
func TestWriterWaits(t *testing.T) {
	caller, _ := one(entity.RoleOperator)
	id := uuid.NewString()
	var s *served
	ap := &applied{read: func() bool { return s.knob(t, id).Generation == 1 }}
	s = serve(t, caller, func(d *api.Deps) { d.Applied = ap.Applied })
	put := func() reply {
		return s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*", "Idempotency-Key", "w1")
	}
	if r := put(); r.status != http.StatusOK {
		t.Fatalf("PUT knob: answered %d %s", r.status, r.body)
	}
	if got := ap.seen(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("Applied after the write: calls %v, want one, made once the write read back", got)
	}
	put()
	s.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"b"}}`, "If-Match", `"9"`)
	s.do(t, http.MethodPut, "/api/v1/vaults/"+uuid.NewString(), `{"spec":{"password":"p"}}`, "If-None-Match", "*")
	if got := ap.seen(); len(got) != 1 {
		t.Errorf("Applied after a replay, a refusal and a vault write: %d calls, want still 1", len(got))
	}
}

// An Applied that fails leaves the answer the commit's own.
func TestWriterWaitFails(t *testing.T) {
	id := uuid.NewString()
	failing := &applied{read: func() bool { return true }, err: errors.New("not applied")}
	f := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) { d.Applied = failing.Applied })
	r := f.do(t, http.MethodPut, knobAt(id), `{"spec":{"value":"a"}}`, "If-None-Match", "*")
	if r.status != http.StatusOK || r.header.Get("ETag") != `"1"` || string(r.body) != `{"spec":{"value":"a"}}` {
		t.Errorf("PUT with Applied failing: answered %d %s ETag %q, want the commit's 200 with ETag \"1\"", r.status, r.body, r.header.Get("ETag"))
	}
	if got := failing.seen(); len(got) != 1 {
		t.Errorf("failing Applied: %d calls, want 1", len(got))
	}
}
