package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
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

// pendingTurn is a status turn earlier advances released, or else the next
// one.
func pendingTurn(t *testing.T, h *nodetest.Harness, ch <-chan statusTurn) statusTurn {
	t.Helper()
	select {
	case tr := <-ch:
		return tr
	default:
		return nextTurn(t, h, ch)
	}
}

// turnAt is the first of the next ten status turns, those earlier advances
// released first, whose status satisfies ok.
func turnAt(t *testing.T, h *nodetest.Harness, ch <-chan statusTurn, what string, ok func(statusTurn) bool) statusTurn {
	t.Helper()
	for range 10 {
		if tr := pendingTurn(t, h, ch); ok(tr) {
			return tr
		}
	}
	t.Fatalf("no status turn %s within ten turns", what)
	return statusTurn{}
}

// drain discards the status turns already delivered.
func drain(ch <-chan statusTurn) {
	for {
		select {
		case <-ch:
		default:
			return
		}
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

// storedCondition is a condition as a status carries it.
type storedCondition struct {
	Name, Scope, ID, Subject string
}

// conditionsOf is the conditions a status carries.
func conditionsOf(t *testing.T, s entity.NodeStatus) []storedCondition {
	t.Helper()
	if s.Conditions == nil {
		return nil
	}
	var conds []storedCondition
	if err := json.Unmarshal(s.Conditions, &conds); err != nil {
		t.Fatalf("status conditions %q: %v", s.Conditions, err)
	}
	return conds
}

// behind reports whether a status carries config_generation_behind for the
// counter of kind.
func behind(t *testing.T, s entity.NodeStatus, kind string) bool {
	t.Helper()
	return slices.ContainsFunc(conditionsOf(t, s), func(c storedCondition) bool {
		return c.Name == "config_generation_behind" && c.Scope == "node" && c.Subject == kind
	})
}

// The status is written at the writer's first turn, then not again over ten
// turns with nothing changed; a settings change, and then a new principal,
// is each written by the first turn after the node reconciled it, with
// every position it holds, as read back through the store.
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

	for _, change := range []struct {
		what string
		make func()
		at   func(kvstore.Counters) uint64
	}{
		{"settings change", func() { configure(t, h, storeA) }, func(c kvstore.Counters) uint64 { return c.Settings }},
		{"new principal", func() {
			if _, err := h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: entity.NewUser(), Name: "bob"}, 0, entity.Alloc{}); err != nil {
				t.Fatalf("create a user: %v", err)
			}
		}, func(c kvstore.Counters) uint64 { return c.Principals }},
	} {
		before := h.Node().Positions()
		change.make()
		now := counters(t, h)
		if change.at(now) == change.at(before) {
			t.Fatalf("%s: counter %d did not move", change.what, change.at(now))
		}
		caughtUp(t, h)
		tr := turnAt(t, h, ch, "holding the "+change.what, func(tr statusTurn) bool {
			return tr.s.Settings == now.Settings && tr.s.Principals == now.Principals
		})
		if !tr.wrote {
			t.Fatalf("first status turn holding the %s: wrote nothing, want it written", change.what)
		}
		got := readStatus(t, h)
		if got.Settings != now.Settings || got.ShareList != now.ShareList || got.Principals != now.Principals || got.Claims != 0 {
			t.Errorf("status read back once the %s was written: got settings %d, share list %d, principals %d, claims %d; want %+v and claims 0",
				change.what, got.Settings, got.ShareList, got.Principals, got.Claims, now)
		}
	}
}

// A status write that failed is made again at the next turn: the status
// last written moves only once a write commits.
//
// Reconcile catches up through Applied, which wakes a pass without moving the
// clock, so every status turn here is released by one advance the test waits
// on, and none is still running when the store is read back.
func TestStatusWriteRetriedAfterAFault(t *testing.T) {
	logs := make(logLines, 256)
	h, f := faulted(t, func(cfg *node.Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	ch := watchStatus(h)
	started(t, h)
	awaitTurn(t, ch)
	before := readStatus(t, h)
	setFault(t, f, nil, sim.Rule{Op: kvtest.OpSet, Key: statusKey(h), Count: 1, Action: sim.FaultAction{Err: errors.New("status write lost")}})
	configure(t, h, storeA)
	want := counters(t, h).Settings
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a backstop: Applied returns on the position moving
	defer cancel()
	if err := h.Node().Applied(ctx); err != nil {
		t.Fatalf("Applied after the settings change: %v", err)
	}

	if tr := turnAt(t, h, ch, "at the settings change", func(tr statusTurn) bool { return tr.s.Settings == want }); tr.wrote {
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
	var statusLogs []string
	for len(logs) != 0 {
		if line := <-logs; strings.Contains(line, "loop=status") {
			statusLogs = append(statusLogs, line)
		}
	}
	if len(statusLogs) != 2 || !strings.Contains(statusLogs[0], "node store fault started") ||
		!strings.Contains(statusLogs[0], "status write lost") ||
		!strings.Contains(statusLogs[1], "node store fault cleared") ||
		!strings.Contains(statusLogs[1], "failures_total=1") {
		t.Errorf("status logs: got %q, want a fault started by the failed write and cleared after the retry", statusLogs)
	}
}

// statusKey matches the node's status row, as a FaultKV rule names it.
func statusKey(h *nodetest.Harness) string {
	id := h.NodeID()
	return keys.Describe(append(keys.ID([]byte{keys.KindN}, id[:]), keys.NSt))
}

// A status write held past the store bound ends its turn, and the turns go
// on; while it runs no turn writes, so a newer status is not overwritten by
// it committing late, and once it settles the next turn writes the status
// as it then is.
func TestStatusWriteHeldPastItsBound(t *testing.T) {
	h, f := faulted(t)
	ch := watchStatus(h)
	started(t, h)
	awaitTurn(t, ch)
	// The delay waits on a clock of its own, so moving the node's clock past
	// the bound does not release it.
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpSet, Key: statusKey(h), Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	defer held.Advance(time.Hour) // a failed test's Stop finds no write held

	configure(t, h, storeA)
	first := counters(t, h).Settings
	caughtUp(t, h)
	heldUntil(t, h, held, "status write")
	drain(ch)
	h.Clock().Advance(20 * time.Second)
	if tr := awaitTurn(t, ch); tr.wrote || tr.s.Settings != first {
		t.Fatalf("turn whose write was held past the bound: wrote %v at settings %d, want not written at %d", tr.wrote, tr.s.Settings, first)
	}

	configure(t, h, storeB)
	second := counters(t, h).Settings
	caughtUp(t, h)
	if tr := turnAt(t, h, ch, "at the second change", func(tr statusTurn) bool { return tr.s.Settings == second }); tr.wrote {
		t.Fatalf("turn at settings %d while the write of %d is held: wrote it, want no write until that one settles", second, first)
	}

	// The held write settles some real time after its release, so the turns
	// are polled until one writes, a millisecond apart.
	held.Advance(time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	tr := pendingTurn(t, h, ch)
	for ; !tr.wrote; tr = pendingTurn(t, h, ch) {
		if time.Now().After(deadline) {
			t.Fatal("no status turn wrote within 10 s of the held write's release")
		}
		<-time.After(time.Millisecond)
	}
	if tr.s.Settings != second {
		t.Errorf("first status written once the held write settled: settings %d, want %d", tr.s.Settings, second)
	}
	if got := readStatus(t, h); got.Settings != second {
		t.Errorf("status read back once the held write settled: settings %d, want %d, not the held write's %d", got.Settings, second, first)
	}
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
	caughtUp(t, h)
	s := turnAt(t, h, ch, "past the refused share", func(tr statusTurn) bool { return tr.s.ShareList == want }).s
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
	ran := make(chan struct{}, 1)
	h.Node().OnRunnerTurn(func() {
		select {
		case ran <- struct{}{}:
		default:
		}
	})
	started(t, h)
	awaitTurn(t, ch)
	const kind = "no.such.kind"
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: kind,
		Target: entity.ResourceRef{Kind: entity.ResourceInstallation, ID: []byte{1}}, State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	// A runner turn that starts after the write lists the operation.
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("no runner turn within 10 s")
	}
	h.Clock().Advance(time.Second)
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("no runner turn within 10 s")
	}
	turnAt(t, h, ch, "refusing kind "+kind, func(tr statusTurn) bool {
		return slices.ContainsFunc(tr.s.Refused, func(r entity.RefusedRecord) bool {
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

// awaitRead waits for a pass to deliver the counters it read.
func awaitRead(t *testing.T, reached <-chan kvstore.Counters) kvstore.Counters {
	t.Helper()
	select {
	case c := <-reached:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no pass read the counters within 10 s")
		return kvstore.Counters{}
	}
}

// A pass frozen after reading a settings change holds the settings position
// back: config_generation_lag_seconds grows on the clock from that read, and
// config_generation_behind, naming the settings counter, is raised in the
// status once the lag passes 5 s, not before; once the pass goes on, both
// clear and the position series reaches the counter.
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
	h.Node().Wake()
	if c := awaitRead(t, reached); c.Settings != want {
		t.Fatalf("frozen pass read settings %d, want the change at %d", c.Settings, want)
	}

	// The clock has not moved since the frozen pass read the change.
	if lag := series(t, h, "dittofs_config_generation_lag_seconds", "settings"); lag != 0 {
		t.Fatalf("settings lag as the frozen pass read the change: got %v s, want 0", lag)
	}
	for lag := 1.0; lag <= 6; lag++ {
		tr := nextTurn(t, h, ch)
		if got := series(t, h, "dittofs_config_generation_lag_seconds", "settings"); got != lag {
			t.Fatalf("settings lag %v s after the read: got %v s", lag, got)
		}
		if raised := behind(t, tr.s, "settings"); raised != (lag > 5) {
			t.Fatalf("config_generation_behind of settings at a lag of %v s: raised %v, want raised only past 5 s (conditions %+v)", lag, raised, conditionsOf(t, tr.s))
		}
	}
	if s := readStatus(t, h); !behind(t, s, "settings") {
		t.Errorf("conditions read back at a lag of 6 s: got %+v, want config_generation_behind of settings", conditionsOf(t, s))
	}
	if got := series(t, h, "dittofs_node_reconciled_position", "settings"); got != float64(before) {
		t.Errorf("settings position series while frozen: got %v, want %d", got, before)
	}

	frozen.Store(false)
	close(release)
	caughtUp(t, h)
	s := turnAt(t, h, ch, "caught up", func(tr statusTurn) bool { return tr.wrote && tr.s.Settings == want }).s
	if behind(t, s, "settings") {
		t.Errorf("conditions once caught up: got %+v, want config_generation_behind cleared", conditionsOf(t, s))
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

// The lag counts from the first read above the position, not the latest:
// with every pass failing after its read, it grows a second a turn over
// seven turns, whatever the retried passes read, and
// config_generation_behind is raised once it passes 5 s.
func TestStatusLagCountsFromTheFirstRead(t *testing.T) {
	reached := make(chan kvstore.Counters, 64)
	var fk *kvtest.FaultKV
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		fk = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = fk
		return node.NewPausing(func(c kvstore.Counters) {
			select {
			case reached <- c:
			default:
			}
		})(cfg)
	}))
	ch := watchStatus(h)
	started(t, h)
	awaitTurn(t, ch)
	setFault(t, fk, nil, sim.Rule{Op: kvtest.OpScan, Key: nsScan, Action: sim.FaultAction{Err: errors.New("scan lost")}})
	configure(t, h, storeA)
	want := counters(t, h).Settings
	for len(reached) > 0 {
		<-reached
	}
	h.Node().Wake()
	if c := awaitRead(t, reached); c.Settings != want {
		t.Fatalf("first pass after the change read settings %d, want %d", c.Settings, want)
	}
	for lag := 1.0; lag <= 7; lag++ {
		tr := nextTurn(t, h, ch)
		if got := series(t, h, "dittofs_config_generation_lag_seconds", "settings"); got != lag {
			t.Fatalf("settings lag %v s after the first read, every pass failing: got %v s", lag, got)
		}
		if raised := behind(t, tr.s, "settings"); raised != (lag > 5) {
			t.Fatalf("config_generation_behind of settings at a lag of %v s: raised %v, want raised only past 5 s", lag, raised)
		}
	}
	if got := series(t, h, "dittofs_node_reconcile_errors_total", "shares"); got < 2 {
		t.Errorf("share reconcile errors over seven turns of failing passes: got %v, want the passes retried", got)
	}
}

// A share pass whose read fails, and a Bind the engine fails, each count
// one reconcile error of the shares; neither counts one of the settings.
func TestReconcileErrorsCounted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(sh entity.Share) sim.Rule
	}{
		{"failed namespaces scan", func(entity.Share) sim.Rule {
			return sim.Rule{Op: kvtest.OpScan, Key: nsScan, Count: 1, Action: sim.FaultAction{Err: errors.New("scan lost")}}
		}},
		{"failed bind", func(sh entity.Share) sim.Rule {
			return sim.Rule{Op: kvtest.OpBefore, Key: floorRead(sh.ID), Count: 1, Action: sim.FaultAction{Err: errInjected}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := faulted(t)
			started(t, h)
			configure(t, h, storeA)
			caughtUp(t, h)
			sh := createShare(t, h, "a", storeA)
			setFault(t, f, nil, tc.fault(sh))
			caughtUp(t, h)
			if got := series(t, h, "dittofs_node_reconcile_errors_total", "shares"); got != 1 {
				t.Errorf("share reconcile errors after one %s: got %v, want 1", tc.name, got)
			}
			if got := series(t, h, "dittofs_node_reconcile_errors_total", "settings"); got != 0 {
				t.Errorf("settings reconcile errors with apply never failing: got %v, want 0", got)
			}
		})
	}
}

// An engine condition reaches the stored status: a journal recorded for the
// node's device and missing from it raises journal_lost, which the status
// carries naming the journal's directory.
func TestStatusCarriesAnEngineCondition(t *testing.T) {
	lost := filepath.Join(t.TempDir(), "lost")
	if err := os.Mkdir(lost, 0o700); err != nil {
		t.Fatal(err)
	}
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.Journals = []bootstrap.Journal{{Path: lost}}
		return node.New(cfg)
	}))
	if err := h.Store().RecordJournal(t.Context(), [16]byte{'l', 'o', 's', 't'}, h.NodeID(), entity.JournalDir(lost)); err != nil {
		t.Fatalf("record a journal for the empty device: %v", err)
	}
	ch := watchStatus(h)
	started(t, h)
	if tr := awaitTurn(t, ch); !tr.wrote {
		t.Fatalf("first status turn: wrote nothing, want the status written")
	}
	want := storedCondition{Name: "journal_lost", Scope: "journal", ID: "6c6f7374000000000000000000000000", Subject: lost}
	if got := conditionsOf(t, readStatus(t, h)); !slices.Contains(got, want) {
		t.Errorf("conditions read back with the device's journal missing: got %+v, want %+v", got, want)
	}
}
