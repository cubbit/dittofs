package node_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

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
// node view, the shares from the store's control plane, and an operation's
// progress read by the runner's step for its kind. Stop closes the listener.
func TestNodeServesTheAPI(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	sh := addShare(t, h)
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr, _ := h.Node().Addrs()

	var v apiv1.Version
	getJSON(t, addr, "/api/version", &v)
	if !slices.Equal(v.Majors, []string{"v1"}) {
		t.Errorf("GET /api/version: majors %v, want [v1]", v.Majors)
	}
	var nodes apiv1.Page[apiv1.Node]
	getJSON(t, addr, "/api/v1/nodes", &nodes)
	id := h.NodeID()
	if len(nodes.Items) != 1 || nodes.Items[0].ID != entity.ShareID(id).String() || nodes.Items[0].Status.Address != addr {
		t.Errorf("GET /api/v1/nodes: %+v, want this node alone, at %s", nodes.Items, addr)
	}
	var shares apiv1.Page[apiv1.Share]
	getJSON(t, addr, "/api/v1/shares", &shares)
	if len(shares.Items) != 1 || shares.Items[0].ID != sh.ID.String() {
		t.Errorf("GET /api/v1/shares: %+v, want share %s alone", shares.Items, sh.ID)
	}
	var got apiv1.Operation
	getJSON(t, addr, "/api/v1/operations/"+uuid.UUID(op.ID).String(), &got)
	if want := (apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}); got.Progress != want {
		t.Errorf("GET the operation: progress %+v, want %+v, the deletion's step's", got.Progress, want)
	}

	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := get(t, addr, "/api/version"); got != closed {
		t.Errorf("GET /api/version after Stop: got %+v, want the listener closed", got)
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
}
