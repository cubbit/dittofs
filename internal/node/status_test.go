package node_test

import (
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
)

// statusTurn is one turn of the status writer: the status it built and
// whether it committed it.
type statusTurn struct {
	s     entity.NodeStatus
	wrote bool
}

// watchStatus has the node deliver each status turn on the channel it
// returns. Call it before Start.
func watchStatus(h *nodetest.Harness) <-chan statusTurn {
	ch := make(chan statusTurn, 1024)
	h.Node().OnStatus(func(s entity.NodeStatus, wrote bool) { ch <- statusTurn{s, wrote} })
	return ch
}

// awaitTurn waits for the next status turn.
func awaitTurn(t *testing.T, ch <-chan statusTurn) statusTurn {
	t.Helper()
	select {
	case tr := <-ch:
		return tr
	case <-time.After(10 * time.Second):
		t.Fatal("no status turn within 10 s")
		return statusTurn{}
	}
}

// nextTurn advances the node's clock a second, the status writer's
// interval, and waits for the turn it releases.
func nextTurn(t *testing.T, h *nodetest.Harness, ch <-chan statusTurn) statusTurn {
	t.Helper()
	h.Clock().Advance(time.Second)
	return awaitTurn(t, ch)
}

// pendingTurn is a status turn caughtUp's advances released, or else the
// next one.
func pendingTurn(t *testing.T, h *nodetest.Harness, ch <-chan statusTurn) statusTurn {
	t.Helper()
	select {
	case tr := <-ch:
		return tr
	default:
		return nextTurn(t, h, ch)
	}
}

// readStatus reads the node's status back through the store, as the API
// would.
func readStatus(t *testing.T, h *nodetest.Harness) entity.NodeStatus {
	t.Helper()
	var got []entity.NodeStatus
	for s, err := range h.Store().Nodes().NodeStatuses(t.Context(), entity.NodeID{}) {
		if err != nil {
			t.Fatalf("NodeStatuses: %v", err)
		}
		got = append(got, s)
	}
	if len(got) != 1 || got[0].Node != h.NodeID() {
		t.Fatalf("node statuses: got %d (%+v), want one, of node %x", len(got), got, h.NodeID())
	}
	return got[0]
}

// conditionNames is the names of the conditions a status carries.
func conditionNames(t *testing.T, s entity.NodeStatus) []string {
	t.Helper()
	if s.Conditions == nil {
		return nil
	}
	var conds []struct{ Name string }
	if err := json.Unmarshal(s.Conditions, &conds); err != nil {
		t.Fatalf("status conditions %q: %v", s.Conditions, err)
	}
	var names []string
	for _, c := range conds {
		names = append(names, c.Name)
	}
	return names
}

// The status is written at the writer's first turn, then not again over ten
// turns with nothing changed; a settings change is written by the first turn
// after the node reconciled it, with every position it holds, as read back
// through the store.
func TestStatusWrittenOnlyOnAChange(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)
	started(t, h)
	if tr := awaitTurn(t, ch); !tr.wrote {
		t.Fatalf("first status turn: wrote nothing, want the status written")
	}
	wrote := 0
	for range 10 {
		if nextTurn(t, h, ch).wrote {
			wrote++
		}
	}
	if wrote != 0 {
		t.Errorf("writes over ten turns with nothing changed: got %d, want 0", wrote)
	}

	configure(t, h, storeA)
	want := counters(t, h).Settings
	caughtUp(t, h)
	for range 10 {
		tr := pendingTurn(t, h, ch)
		if tr.s.Settings != want {
			continue
		}
		if !tr.wrote {
			t.Fatalf("first status turn holding the settings change: wrote nothing, want it written")
		}
		got, pos := readStatus(t, h), h.Node().Positions()
		if got.Settings != pos.Settings || got.ShareList != pos.ShareList || got.Principals != pos.Principals || got.Claims != 0 {
			t.Errorf("status read back once the settings change was written: got settings %d, share list %d, principals %d, claims %d; want %+v and claims 0",
				got.Settings, got.ShareList, got.Principals, got.Claims, pos)
		}
		return
	}
	t.Fatalf("no status turn at settings %d within ten turns", want)
}

// A status write that failed is made again at the next turn: the status
// last written moves only once a write commits.
func TestStatusWriteRetriedAfterAFault(t *testing.T) {
	h, f := faulted(t)
	ch := watchStatus(h)
	started(t, h)
	awaitTurn(t, ch)
	before := readStatus(t, h)
	setFault(t, f, sim.NewTestClock(time.Unix(0, 0)), sim.Rule{Op: kvtest.OpUpdate, Key: "*", Count: 1, Action: sim.FaultAction{Err: errors.New("status write lost")}})
	configure(t, h, storeA)
	want := counters(t, h).Settings
	caughtUp(t, h)

	for range 10 {
		tr := pendingTurn(t, h, ch)
		if tr.s.Settings != want {
			continue
		}
		if tr.wrote {
			t.Fatalf("status turn whose write the fault failed: reported written, want not")
		}
		if got := readStatus(t, h); got.Settings != before.Settings {
			t.Fatalf("status read back after the failed write: settings %d, want %d as before it", got.Settings, before.Settings)
		}
		if tr := nextTurn(t, h, ch); !tr.wrote || tr.s.Settings != want {
			t.Fatalf("turn after the failed write: wrote %v at settings %d, want written at %d", tr.wrote, tr.s.Settings, want)
		}
		if got := readStatus(t, h); got.Settings != want {
			t.Errorf("status read back after the retried write: settings %d, want %d", got.Settings, want)
		}
		return
	}
	t.Fatalf("no status turn at settings %d within ten turns", want)
}

// awaitStatus turns the status writer until a status read back through the
// store satisfies ok, for at most 30 s of real time, as the turns run ahead
// of the store calls of the passes and the runner's turns they release.
func awaitStatus(t *testing.T, h *nodetest.Harness, ch <-chan statusTurn, what string, ok func(entity.NodeStatus) bool) entity.NodeStatus {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		nextTurn(t, h, ch)
		if s := readStatus(t, h); ok(s) {
			return s
		}
	}
	t.Fatalf("no status %s within 30 s of turns: last read back %+v", what, readStatus(t, h))
	return entity.NodeStatus{}
}

// A share refused for its unconfigured remote store does not hold the share
// list position back: the status reports the position past it and lists the
// refusal at the share's own position.
func TestStatusListsARefusedShareBelowItsPosition(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)
	started(t, h)
	awaitTurn(t, ch)
	b := createShare(t, h, "nowhere", storeB)
	configure(t, h, storeA)
	createShare(t, h, "a", storeA)
	want := counters(t, h).ShareList
	if want <= b.Position {
		t.Fatalf("share list version %d after a later share, want above the refused share's %d", want, b.Position)
	}
	s := awaitStatus(t, h, ch, "past the refused share", func(s entity.NodeStatus) bool { return s.ShareList == want })
	if !slices.ContainsFunc(s.Refused, func(r entity.RefusedRecord) bool {
		return r.Record.Kind == entity.ResourceShare && entity.ShareID(r.Record.ID) == b.ID && r.Generation == b.Position
	}) {
		t.Errorf("refused records at share list %d: got %s, want share nowhere at generation %d", s.ShareList, describe(s.Refused), b.Position)
	}
}

// An operation of a kind the node has no step for is reported among the
// status's refused records, naming the kind.
func TestStatusListsAnOperationKindWithNoStep(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchStatus(h)
	started(t, h)
	awaitTurn(t, ch)
	const kind = "no.such.kind"
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: kind,
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: []byte{1}}, State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	awaitStatus(t, h, ch, "refusing kind "+kind, func(s entity.NodeStatus) bool {
		return slices.ContainsFunc(s.Refused, func(r entity.RefusedRecord) bool {
			return r.Record.Kind == entity.ResourceOperation && string(r.Record.ID) == kind
		})
	})
}

// series reads one sample of the series name from the node's registry, the
// one labelled kind when kind is set.
func series(t *testing.T, h *nodetest.Harness, name, kind string) float64 {
	t.Helper()
	mfs, err := h.Metrics().Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if kind != "" && !slices.ContainsFunc(m.GetLabel(), func(l *dto.LabelPair) bool {
				return l.GetName() == "kind" && l.GetValue() == kind
			}) {
				continue
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("series %s{kind=%q}: not in the registry", name, kind)
	return 0
}

// A pass frozen after reading a settings change holds the settings position
// back: config_generation_lag_seconds grows on the clock from that read, and
// config_generation_behind is raised in the status once the lag passes 5 s,
// not before; once the pass goes on, both clear and the position series
// reaches the counter.
func TestStatusRaisesBehindOnAFrozenApply(t *testing.T) {
	var frozen atomic.Bool
	reached, release := make(chan kvstore.Counters, 1), make(chan struct{})
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewPausing(func(c kvstore.Counters) {
		if frozen.Load() {
			reached <- c
			<-release
		}
	})))
	ch := watchStatus(h)
	started(t, h)
	defer func() {
		if frozen.Swap(false) {
			close(release)
		}
	}()
	awaitTurn(t, ch)
	before := h.Node().Positions().Settings

	frozen.Store(true)
	configure(t, h, storeA)
	want := counters(t, h).Settings
	// Turn until a pass reads the change and freezes; the clock then stays
	// where that read was, so the lag counts from zero.
	for deadline := time.Now().Add(30 * time.Second); ; {
		nextTurn(t, h, ch)
		var c kvstore.Counters
		select {
		case c = <-reached:
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no pass read the counters within 30 s of turns")
			}
			continue
		}
		if c.Settings != want {
			t.Fatalf("frozen pass read settings %d, want the change at %d", c.Settings, want)
		}
		break
	}

	// The lag counts from the frozen pass's read, a turn or so back when the
	// read came late; each turn from here adds a second to it.
	lag := series(t, h, "dittofs_config_generation_lag_seconds", "settings")
	if lag > 1 {
		t.Fatalf("settings lag as the frozen pass read the change: got %v s, want at most 1", lag)
	}
	for lag < 6 {
		tr := nextTurn(t, h, ch)
		got := series(t, h, "dittofs_config_generation_lag_seconds", "settings")
		if got != lag+1 {
			t.Fatalf("settings lag a turn after %v s: got %v s, want %v", lag, got, lag+1)
		}
		lag = got
		behind := slices.Contains(conditionNames(t, tr.s), "config_generation_behind")
		if lag <= 5 && behind {
			t.Fatalf("config_generation_behind raised at a lag of %v s, want only past 5 s", lag)
		}
		if lag == 6 && !behind {
			t.Fatalf("conditions at a lag of 6 s: got %v, want config_generation_behind", conditionNames(t, tr.s))
		}
	}
	if got := conditionNames(t, readStatus(t, h)); !slices.Contains(got, "config_generation_behind") {
		t.Errorf("conditions read back at a lag of 6 s: got %v, want config_generation_behind", got)
	}
	if got := series(t, h, "dittofs_node_reconciled_position", "settings"); got != float64(before) {
		t.Errorf("settings position series while frozen: got %v, want %d", got, before)
	}

	frozen.Store(false)
	close(release)
	s := awaitStatus(t, h, ch, "caught up", func(s entity.NodeStatus) bool { return s.Settings == want })
	if got := conditionNames(t, s); slices.Contains(got, "config_generation_behind") {
		t.Errorf("conditions read back once caught up: got %v, want config_generation_behind cleared", got)
	}
	if got := series(t, h, "dittofs_config_generation_lag_seconds", "settings"); got != 0 {
		t.Errorf("settings lag once caught up: got %v, want 0", got)
	}
	if got := series(t, h, "dittofs_node_reconciled_position", "settings"); got != float64(want) {
		t.Errorf("settings position series once caught up: got %v, want %d", got, want)
	}
	if got := series(t, h, "dittofs_node_restart_pending", ""); got != 0 {
		t.Errorf("restart pending: got %v, want 0", got)
	}
}

// A share pass whose read fails counts one reconcile error of the shares.
func TestReconcileErrorsCounted(t *testing.T) {
	h, f := faulted(t)
	started(t, h)
	setFault(t, f, sim.NewTestClock(time.Unix(0, 0)), sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Err: errors.New("scan lost")}})
	configure(t, h, storeA)
	caughtUp(t, h)
	if got := series(t, h, "dittofs_node_reconcile_errors_total", "shares"); got != 1 {
		t.Errorf("share reconcile errors after one failed namespaces scan: got %v, want 1", got)
	}
	if got := series(t, h, "dittofs_node_reconcile_errors_total", "settings"); got != 0 {
		t.Errorf("settings reconcile errors with apply never failing: got %v, want 0", got)
	}
}
