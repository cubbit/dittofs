package api_test

import (
	"context"
	"encoding/base64"
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

// ?wait= on an operation answers once its runner changes its state, or only
// its phase, on the clock's next second; and with neither changed, not at
// 59 s but at the wait's end, 60 s on the clock.
func TestOperationWait(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false), stepProgress)
	op := s.putOp(t, newOp(apiv1.OpShareDelete, entity.Principal{}), 0)
	base := s.clock.Pending()
	for _, tc := range []struct {
		what   string
		change func(*entity.Operation)
		want   func(apiv1.Operation) bool
	}{
		{"the runner set running", func(o *entity.Operation) { o.State = entity.OpRunning },
			func(o apiv1.Operation) bool { return o.State == apiv1.OpRunning }},
		{"the runner moved to the next step", func(o *entity.Operation) { o.Step = []byte("copy") },
			func(o apiv1.Operation) bool { return o.Progress.Phase == "copy" }},
	} {
		ch := s.poll(t.Context(), opPath(op.ID)+"?wait=30s")
		eventually(t, "the poll parks", func() bool { return s.clock.Pending() == base+1 })
		tc.change(&op)
		op = s.putOp(t, op, op.Generation)
		s.clock.Advance(time.Second)
		p := s.settle(t, "poll after "+tc.what, ch, base)
		if p.err != nil || p.status != http.StatusOK || !tc.want(decodeOp(t, "poll", p.body)) {
			t.Errorf("poll a second after %s: answered %d %s err %v, want 200 with the change", tc.what, p.status, p.body, p.err)
		}
	}

	ch := s.poll(t.Context(), opPath(op.ID)+"?wait=60s")
	eventually(t, "the unchanged poll parks", func() bool { return s.clock.Pending() == base+1 })
	s.clock.Advance(59 * time.Second)
	if p := s.settle(t, "poll at 59 s", ch, base); p.status != 0 || p.err != nil {
		t.Fatalf("poll of an unchanged operation answered %d %s err %v at 59 s, want it parked until 60 s", p.status, p.body, p.err)
	}
	s.clock.Advance(time.Second)
	if p := answered(t, "poll at 60 s", ch); p.err != nil || p.status != http.StatusOK || decodeOp(t, "poll", p.body).Progress.Phase != "copy" {
		t.Errorf("poll at 60 s: answered %d %s err %v, want 200 with the operation unchanged", p.status, p.body, p.err)
	}
}

// :cancel answers 409 not_cancellable, naming the operation, for every kind
// of the closed set; 403 for a caller that may not start its kind, the right
// of the row that starts it; 404 for an operation that does not exist; 428
// without an Idempotency-Key, before the operation is read; 400 for a key
// longer than 255 bytes; each refusal audited.
func TestCancel(t *testing.T) {
	var role atomic.Uint32
	role.Store(uint32(entity.RoleAdmin))
	s := serve(t, func(r *http.Request) (auth.Caller, error) { return as(entity.APIRole(role.Load()), false)(r) })
	cancel := func(id entity.OperationID) reply {
		return s.do(t, http.MethodPost, opPath(id)+":cancel", "", "Idempotency-Key", uuid.NewString())
	}
	kinds := []apiv1.OpKind{apiv1.OpRotateSigningKey, apiv1.OpResolvePrincipal, apiv1.OpTestDirectory,
		apiv1.OpJoinDomain, apiv1.OpRotateMachinePassword, apiv1.OpShareDelete, apiv1.OpUserDelete,
		apiv1.OpGroupDelete, apiv1.OpNTHashDelete}
	ids := map[apiv1.OpKind]entity.OperationID{}
	for _, k := range kinds {
		op := s.putOp(t, newOp(k, entity.Principal{}), 0)
		ids[k] = op.ID
		r := cancel(op.ID)
		id := uuid.UUID(op.ID).String()
		if p := r.problem(t); r.status != http.StatusConflict || p.Code != apiv1.CodeNotCancellable || !slices.Equal(p.Names, []string{id}) {
			t.Errorf("cancel %s: answered %d %q naming %q, want 409 not_cancellable naming %s", k, r.status, p.Code, p.Names, id)
		}
	}
	role.Store(uint32(entity.RoleOperator))
	// The test row :sweep starts share.delete for operator; no row starts a
	// signing key's rotation, which asks for admin.
	for _, tc := range []struct {
		kind apiv1.OpKind
		want int
	}{{apiv1.OpShareDelete, http.StatusConflict}, {apiv1.OpRotateSigningKey, http.StatusForbidden}} {
		if r := cancel(ids[tc.kind]); r.status != tc.want {
			t.Errorf("operator cancelling %s: answered %d %s, want %d", tc.kind, r.status, r.body, tc.want)
		}
	}
	role.Store(uint32(entity.RoleViewer))
	if r := cancel(ids[apiv1.OpShareDelete]); r.status != http.StatusForbidden || r.problem(t).Code != apiv1.CodeForbidden {
		t.Errorf("viewer cancelling share.delete: answered %d %s, want 403 forbidden", r.status, r.body)
	}
	role.Store(uint32(entity.RoleAdmin))
	none := entity.OperationID(entity.NewShareID())
	if r := cancel(none); r.status != http.StatusNotFound {
		t.Errorf("cancel of no operation: answered %d %s, want 404", r.status, r.body)
	}
	if r := s.do(t, http.MethodPost, opPath(none)+":cancel", ""); r.status != http.StatusPreconditionRequired || r.problem(t).Code != apiv1.CodePreconditionRequired {
		t.Errorf("cancel of no operation without an Idempotency-Key: answered %d %s, want 428 precondition_required", r.status, r.body)
	}
	for _, tc := range []struct {
		key  int
		want int
	}{{255, http.StatusConflict}, {256, http.StatusBadRequest}} {
		r := s.do(t, http.MethodPost, opPath(ids[apiv1.OpShareDelete])+":cancel", "", "Idempotency-Key", strings.Repeat("k", tc.key))
		if r.status != tc.want || (tc.want == http.StatusBadRequest && r.problem(t).Field != "Idempotency-Key") {
			t.Errorf("cancel with a %d-byte Idempotency-Key: answered %d %s, want %d", tc.key, r.status, r.body, tc.want)
		}
	}
	recs := s.audits(t)
	if want := len(kinds) + 7; len(recs) != want {
		t.Fatalf("cancels audited: %d records %q, want %d", len(recs), outcomes(recs), want)
	}
	if !strings.HasPrefix(string(recs[0].Outcome), "refused: not_cancellable") || recs[0].Target.Kind != entity.ResourceOperation {
		t.Errorf("first cancel audited as %q on %v, want refused: not_cancellable on an operation", recs[0].Outcome, recs[0].Target.Kind)
	}
}

// An operation the schedule started, written with the zero Account as
// identity's loop writes it, reads through both routes with the account
// {"kind":"schedule"}, the spelling the audit's actor filter takes, never as
// an account's; and, on a node with no Progress, with its progress unknown.
func TestScheduledOperationReadsAsTheSchedule(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	op := s.putOp(t, newOp(apiv1.OpRotateMachinePassword, entity.Principal{}), 0)
	const want = `{"kind":"schedule"}`
	r := s.do(t, http.MethodGet, opPath(op.ID), "")
	var one struct{ Account json.RawMessage }
	if err := json.Unmarshal(r.body, &one); err != nil || r.status != http.StatusOK || string(one.Account) != want {
		t.Errorf("GET %s: answered %d account %s, want 200 %s", opPath(op.ID), r.status, one.Account, want)
	}
	if got := decodeOp(t, "GET operation", r.body).Progress; got != (apiv1.Progress{Total: -1}) {
		t.Errorf("GET %s with no Progress: progress %+v, want total -1, unknown", opPath(op.ID), got)
	}
	r = s.do(t, http.MethodGet, "/api/v1/operations", "")
	var page struct {
		Items []struct{ Account json.RawMessage }
	}
	if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK || len(page.Items) != 1 || string(page.Items[0].Account) != want {
		t.Errorf("GET /api/v1/operations: answered %d %s, want 200 one operation with the account %s", r.status, r.body, want)
	}
}

// An operation whose recorded error does not read as a problem reads, alone
// and in a listing, with a 500 problem saying so; one whose result is not
// JSON reads with no result; and neither fails a read.
func TestUnreadableOperation(t *testing.T) {
	unreadable := &apiv1.Problem{Status: http.StatusInternalServerError, Title: "Internal Server Error", Detail: "the operation's error is unreadable"}
	for _, tc := range []struct {
		name          string
		result, error []byte
		want          *apiv1.Problem
	}{
		{"error not json", nil, []byte("not json"), unreadable},
		{"error null", nil, []byte("null"), unreadable},
		{"result not json", []byte("not json"), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := serve(t, as(entity.RoleViewer, false))
			op := newOp(apiv1.OpShareDelete, entity.Principal{})
			op.State, op.Result, op.Error = entity.OpFailed, tc.result, tc.error
			op = s.putOp(t, op, 0)
			r := s.do(t, http.MethodGet, opPath(op.ID), "")
			if got := decodeOp(t, "GET operation", r.body); r.status != http.StatusOK || !jsonEqual(got.Error, tc.want) || got.Result != nil {
				t.Errorf("GET %s: answered %d %s, want 200 with error %+v and no result", opPath(op.ID), r.status, r.body, tc.want)
			}
			r = s.do(t, http.MethodGet, "/api/v1/operations", "")
			var page apiv1.Page[apiv1.Operation]
			if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK || len(page.Items) != 1 || !jsonEqual(page.Items[0].Error, tc.want) {
				t.Errorf("GET /api/v1/operations: answered %d %s, want 200 one operation with error %+v", r.status, r.body, tc.want)
			}
		})
	}
}

// walk reads the listing at path one item a page, following each page's
// cursor, and returns each item's id as it came; it stops after max items,
// so a cursor that never advances ends the walk.
func walk[T any](t *testing.T, s *served, path string, max int, id func(T) string) (got []string) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for q := sep + "limit=1"; ; {
		r := s.do(t, http.MethodGet, path+q, "")
		var page apiv1.Page[T]
		if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK {
			t.Fatalf("GET %s%s: answered %d %s", path, q, r.status, r.body)
		}
		for _, it := range page.Items {
			got = append(got, id(it))
		}
		if page.Next == "" || len(got) > max {
			return got
		}
		q = sep + "limit=1&after=" + url.QueryEscape(page.Next)
	}
}

// ?state= lists the operations in a state an operation that has not ended
// holds, in ID order, and refuses an ended state, naming the filter.
func TestOperationsByState(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	var running []string
	for i := range 16 {
		op := s.putOp(t, newOp(apiv1.OpShareDelete, entity.Principal{}), 0)
		if i%2 == 0 {
			op.State = entity.OpRunning
			s.putOp(t, op, op.Generation)
			running = append(running, uuid.UUID(op.ID).String())
		}
	}
	slices.Sort(running)
	if got := walk(t, s, "/api/v1/operations?state=running", 16, func(o apiv1.Operation) string { return o.ID }); !slices.Equal(got, running) {
		t.Errorf("?state=running a page at a time: %q, want %q", got, running)
	}
	if r := s.do(t, http.MethodGet, "/api/v1/operations?state=succeeded", ""); r.status != http.StatusBadRequest || r.problem(t).Field != "state" {
		t.Errorf("?state=succeeded: answered %d %s, want 400 naming state", r.status, r.body)
	}
}

// GET /operations, /nodes and /audit, read one item a page, each answer
// every item once, in the order one page holding all of them answers.
func TestListingsPageByCursor(t *testing.T) {
	s := serve(t, as(entity.RoleAdmin, false))
	for range 3 {
		s.putOp(t, newOp(apiv1.OpShareDelete, entity.Principal{}), 0)
		id := entity.NodeID(uuid.New())
		if _, err := s.db.RaiseNodeEpoch(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.nodes.RegisterNode(t.Context(), entity.Node{ID: id, Address: "10.0.0.1:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage, Binary: "v0.20.0"}); err != nil {
			t.Fatal(err)
		}
		if r := s.do(t, http.MethodPut, knobAt(uuid.NewString()), `{"spec":{"value":"x"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
			t.Fatalf("PUT knob: answered %d %s", r.status, r.body)
		}
		s.clock.Advance(500 * time.Millisecond)
	}
	pagesOnce(t, s, "/api/v1/operations", func(o apiv1.Operation) string { return o.ID })
	pagesOnce(t, s, "/api/v1/nodes", func(n apiv1.Node) string { return n.ID })
	pagesOnce(t, s, "/api/v1/audit", func(a apiv1.AuditRecord) string { return a.ID })
}

// pagesOnce checks that the three items the listing at path holds, read one
// a page, come back each once, in the order one page answers them.
func pagesOnce[T any](t *testing.T, s *served, path string, id func(T) string) {
	t.Helper()
	r := s.do(t, http.MethodGet, path, "")
	var page apiv1.Page[T]
	if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK || len(page.Items) != 3 || page.Next != "" {
		t.Fatalf("GET %s: answered %d %s, want 200 three items on one page", path, r.status, r.body)
	}
	var all []string
	for _, it := range page.Items {
		all = append(all, id(it))
	}
	if got := walk(t, s, path, 3, id); !slices.Equal(got, all) {
		t.Errorf("GET %s a page at a time: %q, want %q", path, got, all)
	}
}

// The audit reads for admin only, in store-time order, by time range, by
// actor and by target, a target named by the ID the read prints for it,
// each record as its write left it.
func TestAuditRead(t *testing.T) {
	var caller atomic.Pointer[auth.Caller]
	s := serve(t, func(*http.Request) (auth.Caller, error) { return *caller.Load(), nil })
	alice := auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "alice", Role: entity.RoleAdmin}
	bob := auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "bob", Role: entity.RoleAdmin}
	var times []time.Time
	var knobs []string
	for _, c := range []auth.Caller{alice, bob, alice} {
		caller.Store(&c)
		times = append(times, s.clock.Now())
		knobs = append(knobs, uuid.NewString())
		if r := s.do(t, http.MethodPut, knobAt(knobs[len(knobs)-1]), `{"spec":{"value":"x"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
			t.Fatalf("PUT knob as %s: answered %d %s", c.Name, r.status, r.body)
		}
		// Under the store clock's ceiling, at most 2 s ahead of its last
		// raise, so each record's store time is the clock's.
		s.clock.Advance(500 * time.Millisecond)
	}
	// A change the schedule made, with no actor, to a setting, whose
	// reference is its scope and its key, no UUID.
	carol := entity.AuditRecord{ActorName: "carol", Target: entity.SettingRef(entity.SettingScope{Kind: entity.ScopeInstallation}, "realm"), Outcome: []byte("applied")}
	if err := s.store.Write(t.Context(), entity.WriteMeta{Audit: &carol}, func(entity.ControlPlaneTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	caller.Store(&alice)
	page := func(q string) (p apiv1.Page[apiv1.AuditRecord]) {
		r := s.do(t, http.MethodGet, "/api/v1/audit"+q, "")
		if err := json.Unmarshal(r.body, &p); err != nil || r.status != http.StatusOK {
			t.Fatalf("GET /api/v1/audit%s: answered %d %s", q, r.status, r.body)
		}
		return p
	}
	read := func(q string) (names []string) {
		for _, a := range page(q).Items {
			names = append(names, a.ActorName)
		}
		return names
	}
	all := page("").Items
	if len(all) != 4 {
		t.Fatalf("GET /api/v1/audit: %d records, want 4", len(all))
	}
	if got, want := all[3].Actor, (apiv1.Principal{Kind: "schedule"}); got != want {
		t.Errorf("GET /api/v1/audit: the schedule's record reads with actor %+v, want %+v, as ?actor=schedule names it", got, want)
	}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"", []string{"alice", "bob", "alice", "carol"}},
		{"?target=" + url.QueryEscape("setting:"+knobs[1]), []string{"bob"}},
		{"?target=" + url.QueryEscape(all[3].Target.Kind+":"+all[3].Target.ID), []string{"carol"}},
		{"?actor=" + url.QueryEscape(alice.Principal.String()), []string{"alice", "alice"}},
		{"?from=" + url.QueryEscape(times[1].Format(time.RFC3339Nano)) + "&to=" + url.QueryEscape(times[2].Format(time.RFC3339Nano)), []string{"bob"}},
		{"?actor=schedule", []string{"carol"}},
	} {
		if got := read(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("GET /api/v1/audit%s: actors %q, want %q", tc.q, got, tc.want)
		}
	}
	knobID := uuid.MustParse(knobs[1])
	for _, q := range []string{"actor=alice", "target=bad", "target=" + url.QueryEscape("setting:"+strings.ToUpper(knobs[1])),
		"target=setting:" + base64.RawURLEncoding.EncodeToString(knobID[:])} {
		field, _, _ := strings.Cut(q, "=")
		if r := s.do(t, http.MethodGet, "/api/v1/audit?"+q, ""); r.status != http.StatusBadRequest || r.problem(t).Field != field {
			t.Errorf("?%s: answered %d %s, want 400 naming %s", q, r.status, r.body, field)
		}
	}
	op := auth.Caller{Principal: alice.Principal, Role: entity.RoleOperator}
	caller.Store(&op)
	if r := s.do(t, http.MethodGet, "/api/v1/audit", ""); r.status != http.StatusNotFound {
		t.Errorf("GET /api/v1/audit as operator: answered %d, want 404", r.status)
	}
}

// A node reads as a resource, through the listing and by its ID: its epoch
// as its generation, its binary version and registration as its status, and
// the positions its status reports once it reports any; a node that never
// registered is 404.
func TestNodeBinaryVersion(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	id := entity.NodeID(uuid.New())
	if _, err := s.db.RaiseNodeEpoch(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.RaiseNodeEpoch(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.nodes.RegisterNode(t.Context(), entity.Node{ID: id, Address: "10.0.0.1:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage, Binary: "v0.20.0"}); err != nil {
		t.Fatal(err)
	}
	want := apiv1.Node{ID: uuid.UUID(id).String(), Generation: 2, Status: apiv1.NodeStatus{Address: "10.0.0.1:12049", Binary: "v0.20.0"}}
	read := func(what string) {
		t.Helper()
		w, _ := json.Marshal(want)
		r := s.do(t, http.MethodGet, "/api/v1/nodes/"+want.ID, "")
		var got apiv1.Node
		if err := json.Unmarshal(r.body, &got); err != nil || r.status != http.StatusOK || !jsonEqual(got, want) {
			t.Errorf("GET node %s: answered %d %s, want 200 %s", what, r.status, r.body, w)
		}
		r = s.do(t, http.MethodGet, "/api/v1/nodes", "")
		var page apiv1.Page[apiv1.Node]
		if err := json.Unmarshal(r.body, &page); err != nil || r.status != http.StatusOK || !slices.ContainsFunc(page.Items, func(n apiv1.Node) bool { return jsonEqual(n, want) }) {
			t.Errorf("GET nodes %s: answered %d %s, want 200 listing %s", what, r.status, r.body, w)
		}
	}
	read("before it reports a status")
	if err := s.nodes.PutNodeStatus(t.Context(), entity.NodeStatus{Node: id, Settings: 7, ShareList: 3, Principals: 2}); err != nil {
		t.Fatal(err)
	}
	want.Status.Reconciled = &apiv1.Reconciled{Settings: 7, ShareList: 3, Principals: 2}
	read("once it reports a status")
	if r := s.do(t, http.MethodGet, "/api/v1/nodes/"+uuid.NewString(), ""); r.status != http.StatusNotFound {
		t.Errorf("GET a node never registered: answered %d, want 404", r.status)
	}
}

// api_operations counts the operations that have not ended and not one
// once it has; api_operation_oldest_seconds is the age of the oldest of a
// kind on the API's clock.
func TestOperationsGauge(t *testing.T) {
	s := serve(t, as(entity.RoleViewer, false))
	var ops []entity.Operation
	for range 2 {
		op := newOp(apiv1.OpShareDelete, entity.Principal{})
		op.State = entity.OpRunning
		ops = append(ops, s.putOp(t, op, 0))
		s.clock.Advance(time.Second)
	}
	first, _, err := s.store.Operation(t.Context(), ops[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"kind": string(apiv1.OpShareDelete), "state": "running"}
	if got := sample(t, s.reg, "dittofs_api_operations", labels); got != 2 {
		t.Errorf("api_operations with two running: got %v, want 2", got)
	}
	age := s.clock.Now().Sub(first.Started).Seconds()
	if got := sample(t, s.reg, "dittofs_api_operation_oldest_seconds", map[string]string{"kind": string(apiv1.OpShareDelete)}); got != age || age < 1.9 {
		t.Errorf("api_operation_oldest_seconds two seconds after the first start: got %v, want %v, the first operation's age", got, age)
	}
	for _, op := range ops {
		op.State = entity.OpSucceeded
		s.putOp(t, op, op.Generation)
	}
	for _, name := range []string{"dittofs_api_operations", "dittofs_api_operation_oldest_seconds"} {
		if got := sample(t, s.reg, name, nil); got != 0 {
			t.Errorf("%s once both ended: got %v, want no series", name, got)
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
