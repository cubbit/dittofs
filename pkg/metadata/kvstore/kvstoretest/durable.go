package kvstoretest

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Durable opens a store's data again over the same directory or key prefix,
// closing the store it returned before, with clock as its store time.
type Durable func(tb testing.TB, clock func() time.Time) (open func() kv.KV)

// RunDurable runs the cases that reopen a store over its data or move its
// clock. A backend held in memory cannot reopen, and one under a frozen clock
// cannot advance, so neither runs them.
func RunDurable(t *testing.T, d Durable) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, Durable)
	}{
		{"ReopenKeepsData", testReopenKeepsData},
		{"ClockAdvances", testClockAdvances},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, d) })
	}
}

// clock is the store time a durable case moves. It never sleeps: a case
// advances it, and the store reads it at its next open, since a store holds
// its time under a ceiling raised only at open and on its own ticker.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_800_000_000, 0)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// What a store wrote is there after it is reopened over the same data.
func testReopenKeepsData(t *testing.T, d Durable) {
	open := d(t, newClock().now)
	u := user("alice", uid("1000"))
	db := open()
	must(t, kvstore.Init(ctx, db, uuid.New()))
	first, err := kvstore.Open(ctx, db)
	must(t, err)
	must(t, first.ControlPlane().PutUser(ctx, u))

	s, err := kvstore.Open(ctx, open())
	if err != nil {
		t.Fatalf("store reopened over its data: %v, want the store initialised before", err)
	}
	if got, err := s.Principals().ByName(ctx, entity.PrincipalUser, "alice"); err != nil || got != u.Principal {
		t.Errorf("user alice after a reopen: got %v, %v; want %v", got, err, u.Principal)
	}
}

// A clock advanced by an hour moves the store time of the next transaction
// past it.
func testClockAdvances(t *testing.T, d Durable) {
	clk := newClock()
	open := d(t, clk.now)
	storeTime := func(db kv.KV) (now time.Time) {
		must(t, db.Update(ctx, func(tx kv.Txn) error { now = tx.Now(); return nil }))
		return now
	}
	before := storeTime(open())
	clk.advance(time.Hour)
	if after := storeTime(open()); after.Before(before.Add(time.Hour)) {
		t.Errorf("store time after the clock advanced an hour: got %v, want at or past %v", after, before.Add(time.Hour))
	}
}
