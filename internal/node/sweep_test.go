package node_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The sweep's interval, as the node sets it, and RFC 23's floor for the
// audit retention, which the node keeps.
const (
	sweepInterval  = health.IdleEvery
	auditRetention = 90 * 24 * time.Hour
)

// storeBound is how long the node waits on one store call.
const storeBound = 20 * time.Second

const idempotencyDeleted = "dittofs_api_idempotency_records_deleted_total"

// watchSweep has the node deliver each sweep turn's Sweep answers on the
// channel it returns. Call it before Start.
func watchSweep(h *nodetest.Harness) <-chan []kvstore.Swept {
	ch := make(chan []kvstore.Swept, 1024)
	h.Node().OnSweep(func(calls []kvstore.Swept) { ch <- calls })
	return ch
}

func awaitSweep(t *testing.T, ch <-chan []kvstore.Swept) []kvstore.Swept {
	t.Helper()
	return awaitSweepOr(t, ch, "no sweep turn within 10 s")
}

// awaitSweepOr is awaitSweep, failing with why when no turn ends.
func awaitSweepOr(t *testing.T, ch <-chan []kvstore.Swept, why string) []kvstore.Swept {
	t.Helper()
	select {
	case calls := <-ch:
		return calls
	case <-time.After(10 * time.Second):
		t.Fatal(why)
		return nil
	}
}

// storeNow is the store's time, as the audit record of an empty write
// takes it.
func storeNow(t *testing.T, h *nodetest.Harness) time.Time {
	t.Helper()
	a := &entity.AuditRecord{Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("store time")}}
	if err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{Audit: a}, func(entity.ControlPlaneTx) error { return nil }); err != nil {
		t.Fatalf("read the store time: %v", err)
	}
	return a.Time
}

// storeReached waits until the store's time has reached at, as it does once
// its ceiling is raised past it, about once a second of real time.
func storeReached(t *testing.T, h *nodetest.Harness, at time.Time) {
	t.Helper()
	// Polling the store's time is the wait: its ceiling is raised on real
	// time, which no clock the test holds moves.
	deadline := time.Now().Add(10 * time.Second)
	for storeNow(t, h).Before(at) {
		if time.Now().After(deadline) {
			t.Fatalf("the store's time did not reach %v within 10 s", at)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sweptAt returns the first sweep turn that reads a store time past at, and
// so deletes what is due at at. The turns before it read a store time before
// at, and delete nothing due there.
//
// It advances the node's clock to a millisecond before at, which releases one
// turn it discards, as the turn before it was awaited. The store's time can
// run ahead of a clock that stands still, but by only a nanosecond per
// transaction, so that turn reads a time before at. It then waits until the
// store's time, which catches up with the clock once its ceiling is raised,
// about once a second of real time, has reached the clock; and then advances
// one sweep interval, releasing the turn it returns. That raise put the
// ceiling two seconds past the clock, so the turn reads a store time over a
// second past at: the records a test wrote while the clock stood still expire
// within nanoseconds of each other, and are all due then.
func sweptAt(t *testing.T, h *nodetest.Harness, ch <-chan []kvstore.Swept, at time.Time) []kvstore.Swept {
	t.Helper()
	before := at.Add(-time.Millisecond)
	d := before.Sub(h.Clock().Now())
	if d < sweepInterval {
		t.Fatalf("sweptAt %v: only %v ahead of the clock, want at least a sweep interval, so the advance releases one turn", at, d)
	}
	h.Clock().Advance(d)
	awaitSweep(t, ch)
	storeReached(t, h, before)
	h.Clock().Advance(sweepInterval)
	return awaitSweep(t, ch)
}

// keyed writes an idempotency record under key with an audit record naming
// target, and returns both as the store wrote them.
func keyed(t *testing.T, h *nodetest.Harness, key string, target entity.ResourceRef) (entity.IdempotencyRecord, entity.AuditRecord) {
	t.Helper()
	i := &entity.IdempotencyRecord{Key: key, Digest: [32]byte{1}}
	a := &entity.AuditRecord{Target: target}
	if err := h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{Audit: a, Idempotency: i}, func(entity.ControlPlaneTx) error { return nil }); err != nil {
		t.Fatalf("write under key %q: %v", key, err)
	}
	return *i, *a
}

// kept reports whether i's record is still in the store.
func kept(t *testing.T, h *nodetest.Harness, i entity.IdempotencyRecord) bool {
	t.Helper()
	err := h.Store().ControlPlane().Replay(t.Context(), i)
	var r *entity.IdempotentReplay
	if err != nil && !errors.As(err, &r) {
		t.Fatalf("replay key %q: %v", i.Key, err)
	}
	return err != nil
}

// audited counts the audit records naming target.
func audited(t *testing.T, h *nodetest.Harness, target entity.ResourceRef) int {
	t.Helper()
	n := 0
	for _, err := range h.Store().ControlPlane().AuditRecords(t.Context(), entity.AuditFilter{Target: &target}, nil) {
		if err != nil {
			t.Fatalf("list the audit records: %v", err)
		}
		n++
	}
	return n
}

// Idempotency records written through the store are gone after the clock
// passes their expiry and one sweep turn, and the series counts them; their
// audit record stays until the audit retention has passed, and is gone at the
// first turn after it.
func TestSweepDeletesExpiredRecords(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	ch := watchSweep(h)
	started(t, h)
	awaitSweep(t, ch)

	target := entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("swept")}
	a, audit := keyed(t, h, "a", target)
	b, _ := keyed(t, h, "b", target)
	if b.Expires.Sub(a.Expires) > time.Second {
		t.Fatalf("expiries %v and %v: more than a second apart, want them written while the clock stood still", a.Expires, b.Expires)
	}

	sweptAt(t, h, ch, a.Expires)
	for _, i := range []entity.IdempotencyRecord{a, b} {
		if kept(t, h, i) {
			t.Errorf("idempotency record %q one sweep turn past its expiry: still in the store, want it deleted", i.Key)
		}
	}
	if got := series(t, h, idempotencyDeleted, ""); got != 2 {
		t.Errorf("%s once both records were swept: got %v, want 2", idempotencyDeleted, got)
	}
	if got := audited(t, h, target); got != 2 {
		t.Errorf("audit records a day after they were written: got %d, want both kept", got)
	}

	sweptAt(t, h, ch, audit.Time.Add(auditRetention-time.Minute))
	if got := audited(t, h, target); got != 2 {
		t.Errorf("audit records a minute short of the retention: got %d, want both kept", got)
	}
	sweptAt(t, h, ch, audit.Time.Add(auditRetention))
	if got := audited(t, h, target); got != 0 {
		t.Errorf("audit records one sweep turn past the retention: got %d, want none", got)
	}
	if got := series(t, h, idempotencyDeleted, ""); got != 2 {
		t.Errorf("%s once the audit records were swept: got %v, want still 2", idempotencyDeleted, got)
	}
}

// twelveKeyBatches reports a transaction entry limit that leaves a store's
// batches twelve keys, beside the five records every batch may write. Every
// call passes through to the KV beneath.
type twelveKeyBatches struct{ kv.KV }

func (b twelveKeyBatches) Limits() kv.TxnLimits {
	l := b.KV.Limits()
	l.TxnEntries = 5 + 12
	return l
}

// A backlog larger than one Sweep call deletes is cleared within one turn:
// the sweep calls again at once while the store answers More.
func TestSweepClearsABacklogInOneTurn(t *testing.T) {
	const fit = 12 / 2 // idempotency records a twelve-key batch holds, two keys each
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.KV = twelveKeyBatches{cfg.KV}
		return node.New(cfg)
	}))
	ch := watchSweep(h)
	started(t, h)
	awaitSweep(t, ch)

	var recs []entity.IdempotencyRecord
	for k := range fit + 1 {
		i, _ := keyed(t, h, fmt.Sprint(k), entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("backlog")})
		recs = append(recs, i)
	}

	if d := recs[fit].Expires.Sub(recs[0].Expires); d > time.Second {
		t.Fatalf("expiries %v apart, want the records written while the clock stood still", d)
	}
	got := sweptAt(t, h, ch, recs[0].Expires)
	if want := []kvstore.Swept{{Idempotency: fit, More: true}, {Idempotency: 1}}; !slices.Equal(got, want) {
		t.Errorf("Sweep answers in the turn past %d records' expiry: got %+v, want %+v", fit+1, got, want)
	}
	for _, i := range recs {
		if kept(t, h, i) {
			t.Errorf("idempotency record %q after the turn: still in the store, want it deleted", i.Key)
		}
	}
	if got := series(t, h, idempotencyDeleted, ""); got != fit+1 {
		t.Errorf("%s after the turn: got %v, want %d", idempotencyDeleted, got, fit+1)
	}
}

func TestSweepLogsOneIncidentForAnUndecodableRow(t *testing.T) {
	logs := make(logLines, 256)
	h, store := faulted(t, func(cfg *node.Config) { cfg.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	turns := watchSweep(h)
	started(t, h)
	awaitSweep(t, turns)

	// An expired row with an unknown record kind stops the real sweep.
	row := append(keys.Uint64([]byte{keys.KindXP}, uint64(time.Second)), 0xff)
	if err := store.Update(t.Context(), func(tx kv.Txn) error { return tx.Set(row, nil) }); err != nil {
		t.Fatalf("write a malformed expiry row: %v", err)
	}
	for range 3 {
		h.Clock().Advance(sweepInterval)
		awaitSweep(t, turns)
	}
	if err := store.Update(t.Context(), func(tx kv.Txn) error { return tx.Delete(row) }); err != nil {
		t.Fatalf("remove the malformed expiry row: %v", err)
	}
	h.Clock().Advance(sweepInterval)
	awaitSweep(t, turns)

	var got []string
	for len(logs) != 0 {
		if line := <-logs; strings.Contains(line, "loop=sweep") {
			got = append(got, line)
		}
	}
	if len(got) != 3 {
		t.Fatalf("sweep logs: got %d, want start, summary and clear: %q", len(got), got)
	}
	for i, want := range [][]string{
		{"node store fault started", "undecodable=true"},
		{"node store fault ongoing", "duration=1m0s", "failures=2", "undecodable=true"},
		{"node store fault cleared", "duration=1m30s", "failures_total=3"},
	} {
		for _, field := range want {
			if !strings.Contains(got[i], field) {
				t.Errorf("sweep log %d = %q, want %q", i+1, got[i], field)
			}
		}
	}
}

// While the store hangs, a Sweep call is given up after its bound and the
// sweep goes on reporting turns, calling the store again only once that call
// has answered, so the node stays live past the watchdog's stall bound; and
// Stop returns with the call still hanging.
func TestSweepBoundedWhileTheStoreHangs(t *testing.T) {
	h, f := faulted(t)
	ch := watchSweep(h)
	started(t, h)
	awaitSweep(t, ch)

	// Every store write hangs, the sweep's among them, from the turn after
	// the one awaited.
	held := sim.NewTestClock(time.Unix(0, 0))
	setFault(t, f, held, sim.Rule{Op: kvtest.OpUpdate, Key: "*", Count: 1 << 20, Action: sim.FaultAction{Delay: time.Hour}})
	defer func() { // the hung calls answer once the test ends
		f.SetFault(nil)
		held.Advance(time.Hour)
	}()
	h.Clock().Advance(sweepInterval)
	// The turn's call may reach the store after the clock moved, so the
	// clock moves a second at a time until the call's bound ends the turn.
	var calls []kvstore.Swept
	hung := h.Clock().Elapsed()
	for got := false; !got; {
		if h.Clock().Elapsed()-hung > 2*storeBound {
			t.Fatalf("no sweep turn ended within %v of the store hanging, want one ended by its call's bound", 2*storeBound)
		}
		h.Clock().Advance(time.Second)
		select {
		case calls, got = <-ch:
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(calls) != 0 {
		t.Fatalf("Sweep answers in the turn the store hung: got %+v, want none", calls)
	}
	// A turn that called Sweep again would block in it until its bound, so
	// a turn not ending names that second call.
	for i := range 3 {
		h.Clock().Advance(sweepInterval)
		calls := awaitSweepOr(t, ch, fmt.Sprintf("no sweep turn %d within 10 s while the store hangs: a second Sweep call blocked the turn, want none while one hangs", i+1))
		if len(calls) != 0 {
			t.Errorf("Sweep answers in turn %d while the store hangs: got %+v, want none", i+1, calls)
		}
	}
	// The other loops report their turns on their own once the clock moved,
	// so liveness is awaited rather than read once. A loop whose own store
	// write began after the last move waits out that write's bound, as it
	// would in time on a real clock, so the clock moves a second at a time
	// while liveness is awaited, at most one bound further.
	api, _ := h.Node().Addrs()
	live := func() bool { return get(t, api, "/health/live") == answer{http.StatusOK, "live"} }
	for from := h.Clock().Elapsed(); !live(); {
		if h.Clock().Elapsed()-from > storeBound {
			t.Fatalf("/health/live not live %v into the store's hang, past the stall bound, with every loop's call given its bound", h.Clock().Elapsed()-hung)
		}
		h.Clock().Advance(time.Second)
		// Polls liveness itself for a moment before the clock moves again.
		for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline) && !live(); {
			<-time.After(10 * time.Millisecond)
		}
	}

	stopped := make(chan error, 1)
	go func() { stopped <- h.Node().Stop(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Stop while the store hangs: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop while the store hangs: not returned within 10 s")
	}
}
