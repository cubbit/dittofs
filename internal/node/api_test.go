package node_test

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
)

// admin authenticates every request as an admin.
func admin(*http.Request) (auth.Caller, error) {
	return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "admin", Role: entity.RoleAdmin}, nil
}

// getJSON GETs addr's path, wanting 200, and decodes its body into v.
func getJSON(t *testing.T, addr, path string, v any) {
	t.Helper()
	got := get(t, addr, path)
	if got.status != http.StatusOK {
		t.Fatalf("GET %s: answered %d %s, want 200", path, got.status, got.body)
	}
	if err := json.Unmarshal([]byte(got.body), v); err != nil {
		t.Fatalf("GET %s: decode %q: %v", path, got.body, err)
	}
}

// The node serves the management API on its API listener over what it
// holds: its version to anyone; to a caller, its own record from the store's
// node view, the shares from the store's control plane, an operation's
// progress read by the runner's step for its kind, a poll parked on the
// node's clock, and a refusal audited under the node's ID. Stop closes the
// listener.
func TestNodeServesTheAPI(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	sh := addShare(t, h)
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	started(t, h)
	addr, _ := h.Node().Addrs()
	id := h.NodeID()
	nodeID := entity.ShareID(id).String()
	opPath := "/api/v1/operations/" + uuid.UUID(op.ID).String()

	var v apiv1.Version
	getJSON(t, addr, "/api/version", &v)
	if !slices.Equal(v.Majors, []string{"v1"}) {
		t.Errorf("GET /api/version: majors %v, want [v1]", v.Majors)
	}
	var nodes apiv1.Page[apiv1.Node]
	getJSON(t, addr, "/api/v1/nodes", &nodes)
	if len(nodes.Items) != 1 || nodes.Items[0].ID != nodeID || nodes.Items[0].Status.Address != addr {
		t.Errorf("GET /api/v1/nodes: %+v, want node %s alone, at %s", nodes.Items, nodeID, addr)
	}
	var shares apiv1.Page[apiv1.Share]
	getJSON(t, addr, "/api/v1/shares", &shares)
	if len(shares.Items) != 1 || shares.Items[0].ID != sh.ID.String() {
		t.Errorf("GET /api/v1/shares: %+v, want share %s alone", shares.Items, sh.ID)
	}
	var got apiv1.Operation
	getJSON(t, addr, opPath, &got)
	if want := (apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}); got.Progress != want {
		t.Errorf("GET the operation: progress %+v, want %+v, the deletion's step's", got.Progress, want)
	}

	// A poll parks on the node's clock: it does not answer while the clock
	// stands short of its wait, and answers once the clock passes it.
	// Neither call keeps its connection: the leak check that ends the test
	// would otherwise race the transport's goroutines winding down.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	polled := make(chan error, 1)
	var poll *http.Response
	go func() {
		var err error
		poll, err = c.Get("http://" + addr + opPath + "?wait=2s")
		polled <- err
	}()
	parked := h.Clock().Pending()
	for deadline := time.Now().Add(10 * time.Second); h.Clock().Pending() <= parked; runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the poll did not park on the node's clock within 10 s")
		}
	}
	h.Clock().Advance(time.Second)
	select {
	case err := <-polled:
		t.Fatalf("the parked poll answered (%v) with the node's clock a second short of its wait", err)
	case <-time.After(100 * time.Millisecond): // a bounded wait is the assertion: nothing may answer here
	}
	h.Clock().Advance(time.Second)
	select {
	case err := <-polled:
		if err != nil {
			t.Fatalf("GET the operation with ?wait=2s: %v", err)
		}
		_ = poll.Body.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the parked poll: no answer within 10 s of the node's clock passing its wait")
	}
	if poll.StatusCode != http.StatusOK {
		t.Errorf("GET the operation with ?wait=2s: answered %d, want 200 once the wait passed", poll.StatusCode)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+opPath+":cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "cancel-1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST the operation's cancel: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("POST the operation's cancel: answered %d, want 409 not_cancellable", resp.StatusCode)
	}
	var audit apiv1.Page[apiv1.AuditRecord]
	getJSON(t, addr, "/api/v1/audit", &audit)
	refused := slices.IndexFunc(audit.Items, func(r apiv1.AuditRecord) bool {
		return r.Target.Kind == "operation" && r.Target.ID == uuid.UUID(op.ID).String() && strings.Contains(r.Outcome, string(apiv1.CodeNotCancellable))
	})
	if refused < 0 || audit.Items[refused].Node != nodeID {
		t.Errorf("GET /api/v1/audit: %+v, want the cancel of operation %s refused not_cancellable, on node %s", audit.Items, uuid.UUID(op.ID).String(), nodeID)
	}

	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := get(t, addr, "/api/version"); got != closed {
		t.Errorf("GET /api/version after Stop: got %+v, want the listener closed", got)
	}
}

// A node New builds authenticates no caller: its API answers its version and
// refuses every other call 401.
func TestTheAPIAuthenticatesNoCaller(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	started(t, h)
	addr, _ := h.Node().Addrs()
	if got := get(t, addr, "/api/version"); got.status != http.StatusOK {
		t.Errorf("GET /api/version: answered %+v, want 200", got)
	}
	if got := get(t, addr, "/api/v1/nodes"); got.status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/nodes with no caller: answered %+v, want 401", got)
	}
}

// A Start that failed leaves the API serving, its health naming the failed
// step from the node's health evaluator.
func TestTheAPIHealthNamesAFailedStart(t *testing.T) {
	var f *kvtest.FaultKV
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		return node.NewAuthenticating(admin)(cfg)
	}))
	sched, err := sim.NewSchedule(1, nil, failOn(kvtest.OpGet, "CFGGEN", 1))
	if err != nil {
		t.Fatalf("fault schedule: %v", err)
	}
	f.SetFault(sched)
	if err := h.Node().Start(t.Context()); err == nil {
		t.Fatal("Start with the counters' read failing: succeeded, want it refused")
	}
	f.SetFault(nil)
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	addr, _ := h.Node().Addrs()
	var got apiv1.Health
	getJSON(t, addr, "/api/v1/health", &got)
	named := slices.ContainsFunc(got.Conditions, func(c apiv1.Condition) bool {
		return c.Name == "start_failed" && strings.Contains(c.Cause, "start step apply")
	})
	if got.Ready != "not_ready" || !named {
		t.Errorf("GET /api/v1/health after the failed Start: %+v, want not_ready and start_failed naming step apply", got)
	}
	// A node never ready serves no call but the rows that answer before
	// ready, though its store is open: a write would commit a record nothing
	// applies.
	resp, err := http.Get("http://" + addr + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("GET /api/v1/nodes after the failed Start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var p apiv1.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("GET /api/v1/nodes after the failed Start: decode the answer: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || p.Code != apiv1.CodeUnavailable || resp.Header.Get("Retry-After") != "" {
		t.Errorf("GET /api/v1/nodes after the failed Start: %d %q Retry-After %q, want %d %q with none", resp.StatusCode, p.Code, resp.Header.Get("Retry-After"), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	}
}
