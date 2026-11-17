package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// stepProgress is the progress of every kind: its phase the operation's
// recorded step, its total unknown.
func stepProgress(d *api.Deps) {
	d.Progress = func(_ context.Context, op entity.Operation) apiv1.Progress {
		return apiv1.Progress{Phase: string(op.Step), Total: -1}
	}
}

// putOp writes op as its runner would, expecting generation expect, and
// returns it with its new generation.
func (s *served) putOp(t *testing.T, op entity.Operation, expect uint64) entity.Operation {
	t.Helper()
	gen, err := s.store.PutOperation(t.Context(), op, expect)
	if err != nil {
		t.Fatalf("put operation %s %v: %v", op.Kind, op.State, err)
	}
	op.Generation = gen
	return op
}

// newOp is an operation of kind on a share of its own, pending, started by
// account.
func newOp(kind apiv1.OpKind, account entity.Principal) entity.Operation {
	sh := entity.NewShareID()
	return entity.Operation{ID: entity.OperationID(entity.NewShareID()), Kind: string(kind),
		Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: sh[:]}, Account: account, State: entity.OpPending}
}

func opPath(id entity.OperationID) string { return "/api/v1/operations/" + uuid.UUID(id).String() }

func decodeOp(t *testing.T, what string, body []byte) apiv1.Operation {
	t.Helper()
	var op apiv1.Operation
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatalf("%s: %s is no operation: %v", what, body, err)
	}
	return op
}

// A write that starts an operation answers 202 with the operation's body
// and a Location naming it, which a read answers the same; its audit record's
// outcome is the operation's ID; and a second start on the same target while
// it runs is refused 409 operation_running, naming it.
func TestOperationStarted(t *testing.T) {
	authn, p := one(entity.RoleOperator)
	s := serve(t, authn, stepProgress)
	knob := uuid.NewString()
	r := s.do(t, http.MethodPost, knobAt(knob)+":sweep", "{}", "Idempotency-Key", "k1")
	if r.status != http.StatusAccepted {
		t.Fatalf("POST :sweep: answered %d %s, want 202", r.status, r.body)
	}
	op := decodeOp(t, "POST :sweep", r.body)
	want := apiv1.Operation{ID: op.ID, Kind: apiv1.OpShareDelete, Target: apiv1.Ref{Kind: "setting", ID: knob},
		Account: apiv1.Principal{Kind: "user", ID: p.ID.String()}, State: apiv1.OpPending,
		Progress: apiv1.Progress{Total: -1}, Started: op.Started, Updated: op.Updated}
	if !jsonEqual(op, want) || op.Started.IsZero() {
		t.Errorf("POST :sweep: operation %+v, want %+v started at the commit", op, want)
	}
	loc := r.header.Get("Location")
	if loc != "/api/v1/operations/"+op.ID {
		t.Fatalf("POST :sweep: Location %q, want /api/v1/operations/%s", loc, op.ID)
	}
	if got := s.do(t, http.MethodGet, loc, ""); got.status != http.StatusOK || string(got.body) != string(r.body) {
		t.Errorf("GET %s: answered %d %s, want 200 %s, as the start answered", loc, got.status, got.body, r.body)
	}
	if as := s.audits(t); len(as) != 1 || string(as[0].Outcome) != op.ID {
		t.Errorf("audit outcomes after the start: %q, want [%s]", outcomes(as), op.ID)
	}
	again := s.do(t, http.MethodPost, knobAt(knob)+":sweep", "{}", "Idempotency-Key", "k2")
	if p := again.problem(t); again.status != http.StatusConflict || p.Code != apiv1.CodeOperationRunning || !slices.Equal(p.Names, []string{op.ID}) {
		t.Errorf("a second :sweep while one runs: answered %d %q naming %q, want 409 operation_running naming %s", again.status, p.Code, p.Names, op.ID)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// ?wait= on an operation answers once its runner changes its state, on the
// clock's next second, and otherwise at the wait's end, 60 s on the clock.
func TestOperationWait(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false), stepProgress)
	op := s.putOp(t, newOp(apiv1.OpShareDelete, entity.Principal{}), 0)
	base := s.clock.Pending()
	ch := s.poll(t.Context(), opPath(op.ID)+"?wait=30s")
	eventually(t, "the poll parks", func() bool { return s.clock.Pending() == base+1 })
	op.State = entity.OpRunning
	op = s.putOp(t, op, op.Generation)
	select {
	case p := <-ch:
		t.Fatalf("poll answered %d %s before the clock moved, want it parked", p.status, p.body)
	default:
	}
	s.clock.Advance(time.Second)
	if p := answered(t, "poll after the runner's change", ch); p.err != nil || p.status != http.StatusOK || decodeOp(t, "poll", p.body).State != apiv1.OpRunning {
		t.Errorf("poll after the runner set running: answered %d %s err %v, want 200 running", p.status, p.body, p.err)
	}

	ch = s.poll(t.Context(), opPath(op.ID)+"?wait=60s")
	eventually(t, "the second poll parks", func() bool { return s.clock.Pending() == base+1 })
	s.clock.Advance(59 * time.Second)
	select {
	case p := <-ch:
		t.Fatalf("poll answered %d %s at 59 s, want it parked until 60 s", p.status, p.body)
	default:
	}
	s.clock.Advance(time.Second)
	if p := answered(t, "poll at 60 s", ch); p.err != nil || p.status != http.StatusOK || decodeOp(t, "poll", p.body).State != apiv1.OpRunning {
		t.Errorf("poll at 60 s: answered %d %s err %v, want 200 still running", p.status, p.body, p.err)
	}
	if r := s.do(t, http.MethodGet, opPath(op.ID)+"?wait=61s", ""); r.status != http.StatusBadRequest {
		t.Errorf("GET ?wait=61s: answered %d, want 400", r.status)
	}
}

// :cancel answers 409 not_cancellable, naming the operation, for every kind
// of the closed set; 403 for a caller that may not start its kind; 404 for
// an operation that does not exist; each refusal audited.
func TestCancel(t *testing.T) {
	var role atomic.Uint32
	role.Store(uint32(entity.RoleAdmin))
	s := serve(t, func(r *http.Request) (auth.Caller, error) { return as(entity.APIRole(role.Load()), false)(r) })
	kinds := []apiv1.OpKind{apiv1.OpRotateSigningKey, apiv1.OpResolvePrincipal, apiv1.OpTestDirectory,
		apiv1.OpJoinDomain, apiv1.OpRotateMachinePassword, apiv1.OpShareDelete, apiv1.OpUserDelete,
		apiv1.OpGroupDelete, apiv1.OpNTHashDelete}
	var shareDelete entity.OperationID
	for _, k := range kinds {
		op := s.putOp(t, newOp(k, entity.Principal{}), 0)
		if k == apiv1.OpShareDelete {
			shareDelete = op.ID
		}
		r := s.do(t, http.MethodPost, opPath(op.ID)+":cancel", "")
		id := uuid.UUID(op.ID).String()
		if p := r.problem(t); r.status != http.StatusConflict || p.Code != apiv1.CodeNotCancellable || !slices.Equal(p.Names, []string{id}) {
			t.Errorf("cancel %s: answered %d %q naming %q, want 409 not_cancellable naming %s", k, r.status, p.Code, p.Names, id)
		}
	}
	role.Store(uint32(entity.RoleViewer))
	if r := s.do(t, http.MethodPost, opPath(shareDelete)+":cancel", ""); r.status != http.StatusForbidden || r.problem(t).Code != apiv1.CodeForbidden {
		t.Errorf("viewer cancelling share.delete: answered %d %s, want 403 forbidden", r.status, r.body)
	}
	role.Store(uint32(entity.RoleAdmin))
	if r := s.do(t, http.MethodPost, opPath(entity.OperationID(entity.NewShareID()))+":cancel", ""); r.status != http.StatusNotFound {
		t.Errorf("cancel of no operation: answered %d %s, want 404", r.status, r.body)
	}
	want := len(kinds) + 2
	if as := s.audits(t); len(as) != want || !strings.HasPrefix(string(as[0].Outcome), "refused: not_cancellable") || as[0].Target.Kind != entity.ResourceOperation {
		t.Errorf("cancels audited: %d records, first %q on %v, want %d, the first refused: not_cancellable on an operation", len(as), outcomes(as)[:min(1, len(as))], as[0].Target.Kind, want)
	}
}

// An operation the schedule started, written with the zero Account as
// identity's loop writes it, reads through both routes as the zero
// Principal, never as an account's.
func TestScheduledOperationReadsAsTheSchedule(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	op := s.putOp(t, newOp(apiv1.OpRotateMachinePassword, entity.Principal{}), 0)
	r := s.do(t, http.MethodGet, opPath(op.ID), "")
	if got := decodeOp(t, "GET operation", r.body).Account; r.status != http.StatusOK || got != (apiv1.Principal{}) {
		t.Errorf("GET %s: answered %d account %+v, want 200 the zero principal", opPath(op.ID), r.status, got)
	}
	r = s.do(t, http.MethodGet, "/api/v1/operations", "")
	var page apiv1.Page[apiv1.Operation]
	if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK || len(page.Items) != 1 || page.Items[0].Account != (apiv1.Principal{}) {
		t.Errorf("GET /api/v1/operations: answered %d %s, want 200 one operation with the zero principal", r.status, r.body)
	}
}

// ?state= lists the operations in a state an operation that has not ended
// holds, in ID order, and refuses an ended state, naming the filter.
func TestOperationsByState(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	var running []string
	for i := range 4 {
		op := s.putOp(t, newOp(apiv1.OpShareDelete, entity.Principal{}), 0)
		if i%2 == 0 {
			op.State = entity.OpRunning
			s.putOp(t, op, op.Generation)
			running = append(running, uuid.UUID(op.ID).String())
		}
	}
	slices.Sort(running)
	var got []string
	for path := "/api/v1/operations?state=running&limit=1"; ; {
		r := s.do(t, http.MethodGet, path, "")
		var page apiv1.Page[apiv1.Operation]
		if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK {
			t.Fatalf("GET %s: answered %d %s", path, r.status, r.body)
		}
		for _, op := range page.Items {
			got = append(got, op.ID)
		}
		if page.Next == "" || len(got) > 4 {
			break
		}
		path = "/api/v1/operations?state=running&limit=1&after=" + url.QueryEscape(page.Next)
	}
	if !slices.Equal(got, running) {
		t.Errorf("?state=running a page at a time: %q, want %q", got, running)
	}
	if r := s.do(t, http.MethodGet, "/api/v1/operations?state=succeeded", ""); r.status != http.StatusBadRequest || r.problem(t).Field != "state" {
		t.Errorf("?state=succeeded: answered %d %s, want 400 naming state", r.status, r.body)
	}
}

// The audit reads for admin only, in store-time order, by time range and by
// actor, each record as its write left it.
func TestAuditRead(t *testing.T) {
	var caller atomic.Pointer[auth.Caller]
	s := serve(t, func(*http.Request) (auth.Caller, error) { return *caller.Load(), nil })
	alice := auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "alice", Role: entity.RoleAdmin}
	bob := auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "bob", Role: entity.RoleAdmin}
	var times []time.Time
	for _, c := range []auth.Caller{alice, bob, alice} {
		caller.Store(&c)
		times = append(times, s.clock.Now())
		if r := s.do(t, http.MethodPut, knobAt(uuid.NewString()), `{"spec":{"value":"x"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
			t.Fatalf("PUT knob as %s: answered %d %s", c.Name, r.status, r.body)
		}
		// Under the store clock's ceiling, so each record's store time is
		// the clock's.
		s.clock.Advance(500 * time.Millisecond)
	}
	caller.Store(&alice)
	read := func(q string) (names []string) {
		r := s.do(t, http.MethodGet, "/api/v1/audit"+q, "")
		var page apiv1.Page[apiv1.AuditRecord]
		if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK {
			t.Fatalf("GET /api/v1/audit%s: answered %d %s", q, r.status, r.body)
		}
		for _, a := range page.Items {
			names = append(names, a.ActorName)
		}
		return names
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"", []string{"alice", "bob", "alice"}},
		{"?actor=" + url.QueryEscape(alice.Principal.String()), []string{"alice", "alice"}},
		{"?from=" + url.QueryEscape(times[1].Format(time.RFC3339Nano)) + "&to=" + url.QueryEscape(times[2].Format(time.RFC3339Nano)), []string{"bob"}},
		{"?actor=schedule", nil},
	} {
		if got := read(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("GET /api/v1/audit%s: actors %q, want %q", tc.q, got, tc.want)
		}
	}
	if r := s.do(t, http.MethodGet, "/api/v1/audit?actor=alice", ""); r.status != http.StatusBadRequest || r.problem(t).Field != "actor" {
		t.Errorf("?actor=alice: answered %d %s, want 400 naming actor", r.status, r.body)
	}
	op := auth.Caller{Principal: alice.Principal, Role: entity.RoleOperator}
	caller.Store(&op)
	if r := s.do(t, http.MethodGet, "/api/v1/audit", ""); r.status != http.StatusNotFound {
		t.Errorf("GET /api/v1/audit as operator: answered %d, want 404", r.status)
	}
}

// A node's record reads with its binary version, through the listing and
// by its ID; a node that never registered is 404.
func TestNodeBinaryVersion(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	id := entity.NodeID(uuid.New())
	if _, err := s.nodes.RegisterNode(t.Context(), entity.Node{ID: id, Address: "10.0.0.1:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage, Binary: "v0.20.0"}); err != nil {
		t.Fatal(err)
	}
	want := apiv1.Node{ID: uuid.UUID(id).String(), Address: "10.0.0.1:12049", Binary: "v0.20.0"}
	r := s.do(t, http.MethodGet, "/api/v1/nodes/"+want.ID, "")
	var got apiv1.Node
	if err := json.Unmarshal(r.body, &got); err != nil || r.status != http.StatusOK || got != want {
		t.Errorf("GET node: answered %d %s, want 200 %+v", r.status, r.body, want)
	}
	r = s.do(t, http.MethodGet, "/api/v1/nodes", "")
	var page apiv1.Page[apiv1.Node]
	if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK || !slices.Contains(page.Items, want) {
		t.Errorf("GET nodes: answered %d %s, want 200 listing %+v", r.status, r.body, want)
	}
	if r := s.do(t, http.MethodGet, "/api/v1/nodes/"+uuid.NewString(), ""); r.status != http.StatusNotFound {
		t.Errorf("GET a node never registered: answered %d, want 404", r.status)
	}
}

// api_operations counts an operation while it has not ended and not once
// it has; api_operation_oldest_seconds is its age on the API's clock.
func TestOperationsGauge(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	op := newOp(apiv1.OpShareDelete, entity.Principal{})
	op.State = entity.OpRunning
	op = s.putOp(t, op, 0)
	s.clock.Advance(time.Second)
	started, _, err := s.store.Operation(t.Context(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"kind": string(apiv1.OpShareDelete), "state": "running"}
	if got := sample(t, s.reg, "dittofs_api_operations", labels); got != 1 {
		t.Errorf("api_operations while running: got %v, want 1", got)
	}
	age := s.clock.Now().Sub(started.Started).Seconds()
	if got := sample(t, s.reg, "dittofs_api_operation_oldest_seconds", map[string]string{"kind": string(apiv1.OpShareDelete)}); got != age || age < 0.9 {
		t.Errorf("api_operation_oldest_seconds a second after the start: got %v, want %v, the clock past its store start time", got, age)
	}
	op.State = entity.OpSucceeded
	s.putOp(t, op, op.Generation)
	for _, name := range []string{"dittofs_api_operations", "dittofs_api_operation_oldest_seconds"} {
		if got := sample(t, s.reg, name, nil); got != 0 {
			t.Errorf("%s once the operation ended: got %v, want no series", name, got)
		}
	}
}

// raised is a health source holding the conditions a test raises.
//
// decision: a Source is what a component publishes its conditions through,
// so a source holding what the test sets is the input the evaluator reads,
// not a stand-in for it; the evaluator is the real one.
type raised struct {
	mu sync.Mutex
	c  []health.Condition
}

func (r *raised) set(c ...health.Condition) { r.mu.Lock(); r.c = c; r.mu.Unlock() }

func (r *raised) Conditions(dst []health.Condition) []health.Condition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(dst, r.c...)
}

// /api/v1/health answers a viewer the node's readiness word and the
// conditions the evaluator holds, each with its cause, as they are set and
// cleared, while /health/ready stays one word.
func TestHealthDetail(t *testing.T) {
	var ev health.Evaluator
	src := &raised{}
	ev.Add(src)
	var word atomic.Value
	word.Store("ready")
	s := serve(t, as(entity.RoleViewer, false), func(d *api.Deps) {
		d.Health = &ev
		d.Ready = func() string { return word.Load().(string) }
	})
	read := func() apiv1.Health {
		r := s.do(t, http.MethodGet, "/api/v1/health", "")
		var h apiv1.Health
		if err := json.Unmarshal(r.body, &h); err != nil || r.status != http.StatusOK {
			t.Fatalf("GET /api/v1/health: answered %d %s", r.status, r.body)
		}
		return h
	}
	if got, want := read(), (apiv1.Health{Ready: "ready", Conditions: []apiv1.Condition{}}); !jsonEqual(got, want) {
		t.Errorf("health with nothing raised: %+v, want %+v", got, want)
	}
	src.set(health.Condition{Name: "store_unreachable", Subject: health.Subject{Scope: health.ScopeStore, ID: "rs-1", Name: "s3"},
		Cause: errors.New("dial tcp: connection refused"), Detail: "3 failures"})
	word.Store("draining")
	want := apiv1.Health{Ready: "draining", Conditions: []apiv1.Condition{{Name: "store_unreachable", Scope: "store", ID: "rs-1", Subject: "s3",
		Cause: "dial tcp: connection refused", Detail: "3 failures"}}}
	if got := read(); !jsonEqual(got, want) {
		t.Errorf("health with a condition raised: %+v, want %+v", got, want)
	}
	if r := s.do(t, http.MethodGet, "/health/ready", ""); string(r.body) != "draining" {
		t.Errorf("/health/ready with a condition raised: body %q, want the one word draining", r.body)
	}
	src.set()
	word.Store("ready")
	if got := read(); len(got.Conditions) != 0 || got.Ready != "ready" {
		t.Errorf("health once cleared: %+v, want ready and no condition", got)
	}
}
