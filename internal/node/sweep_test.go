package node_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// The sweep's interval, as the node sets it, and RFC 23's floor for the
// audit retention, which the node keeps.
const (
	sweepInterval  = health.IdleEvery
	auditRetention = 90 * 24 * time.Hour
)

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
	select {
	case calls := <-ch:
		return calls
	case <-time.After(10 * time.Second):
		t.Fatal("no sweep turn within 10 s")
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
	// Polling the store's time is the wait: its ceiling is raised on real
	// time, which no clock the test holds moves.
	deadline := time.Now().Add(10 * time.Second)
	for storeNow(t, h).Before(before) {
		if time.Now().After(deadline) {
			t.Fatalf("the store's time did not reach %v within 10 s", before)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
