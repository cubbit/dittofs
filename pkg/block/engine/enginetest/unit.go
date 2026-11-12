package enginetest

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Unit is the engine composed for a unit test: over the fake journal, the
// real store on an in-memory kv.KV and a test clock, with one share bound
// through Bind unless the test asks for none, and, when asked, the real
// open-state table over that store as its CloneHold. Everything runs under
// the profiler.
type Unit struct {
	E     *engine.Engine
	J     *FakeJournal // the fake journal, under the wrapper UnitOptions.Journal set
	FS    *kvstoretest.FS
	Clock *sim.TestClock
	Share engine.Share     // the share as the engine binds it
	// Opens is the real open-state table, if UnitOptions.OpenState. It is the
	// engine's CloneHold unless a UnitOptions.Deps function replaces Holds, and
	// it serves Share's shard once NewUnit binds Share; a test that sets
	// Unbound binds the share itself and calls Recovery().ServeShard.
	Opens *openstate.Table
	Sink  *Sink // Opens's sink, if Opens is set
}

// UnitOptions is what a test asks of NewUnit.
type UnitOptions struct {
	Deps       []func(*engine.Deps)              // each changes the engine's Deps before New
	Journal    func(*FakeJournal) engine.Journal // wraps the fake journal the engine opens; nil for none
	KV         func(kv.KV) kv.KV                 // wraps the store's kv.KV; nil for none
	StoreClock func() time.Time                  // the in-memory kv.KV's clock, which stamps its versions; nil is time.Now
	Unbound    bool                              // leaves the share unbound: the test binds what it needs
	Unstarted  bool                              // leaves the engine unstarted, and so the share unbound
	// OpenState composes the real open-state table as the engine's
	// CloneHold. Its sweep waits on the unit's clock, so a test counting the
	// clock's pending timers leaves it off and gives a CloneHold in Deps.
	OpenState bool
	// Stop stops the engine when the test ends; nil is Close under a
	// deadline, an error failing the test.
	Stop func(*engine.Engine)
}

// NewUnit is the unit as o asks.
func NewUnit(t *testing.T, o UnitOptions) *Unit {
	t.Helper()
	prof.Watch(t, prof.Options{})

	u := &Unit{
		J:     NewFakeJournal(),
		Clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	var j engine.Journal = u.J
	if o.Journal != nil {
		j = o.Journal(u.J)
	}
	u.FS = kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{Clock: o.StoreClock})
		if err != nil {
			tb.Fatalf("unit: store: %v", err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		if o.KV != nil {
			return o.KV(db)
		}
		return db
	})

	d := engine.Deps{
		Node:    entity.NodeID{1},
		Devices: []string{"unit"},
		Open: func(context.Context, string, journal.Version, journal.JournalID) (engine.Journal, error) {
			return j, nil
		},
		Existence: u.FS.Store.Existence(),
		Content:   u.FS.Store.Content(),
		Lifecycle: u.FS.Store.Lifecycle(),
		Clock:     u.Clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		},
	}

	if o.OpenState {
		u.Sink = &Sink{}
		tb, err := openstate.New(openstate.Deps{Records: u.FS.Store, Clock: u.Clock, Rand: rand.Reader,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatalf("unit: open state: %v", err)
		}
		if err := tb.Start(t.Context(), u.Sink, noOrder); err != nil {
			t.Fatalf("unit: open state start: %v", err)
		}
		t.Cleanup(tb.Stop)
		u.Opens = tb
		d.Holds = Holds(tb, u.FS.Store.Opens())
	}

	for _, f := range o.Deps {
		f(&d)
	}

	e, err := engine.New(d)
	if err != nil {
		t.Fatal(err)
	}
	// An existence age no test reaches unless it applies its own.
	if err := e.Apply(engine.Settings{ExistenceAge: 24 * time.Hour, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	// The cleanup drops its reference once run, so the profiler's heap check,
	// which runs last, does not count what the engine still held.
	stop := o.Stop
	if stop == nil {
		stop = func(e *engine.Engine) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := e.Close(ctx); err != nil {
				t.Errorf("unit: engine close: %v", err)
			}
		}
	}
	held := e
	t.Cleanup(func() { stop(held); held = nil })

	if err := e.Apply(Settings); err != nil {
		t.Fatal(err)
	}

	u.E = e
	u.Share = ShareOf(t, u.FS.Store, u.FS.Share.ID, 1<<30)

	if o.Unstarted {
		return u
	}

	if err := e.Start(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	if o.Unbound {
		return u
	}

	if err := e.Bind(t.Context(), u.Share); err != nil {
		t.Fatal(err)
	}
	if u.Opens != nil {
		if err := u.Opens.Recovery().ServeShard(t.Context(), u.Share.ID, u.Share.Shard, 1); err != nil {
			t.Fatalf("unit: serve the share's shard: %v", err)
		}
	}

	return u
}

// Create creates an empty file in the unit's share.
func (u *Unit) Create(t *testing.T, name string) entity.FileID {
	t.Helper()
	_, made := u.FS.Create(t, u.FS.Admin, u.FS.Root, name, kvstore.Regular(0o644))
	return made.ID
}

// BindShare binds another share on the unit's journal, in namespace ns.
func (u *Unit) BindShare(t *testing.T, name string, ns entity.NamespaceID) entity.ShareID {
	t.Helper()
	s := AddShare(t, u.FS.Store, name, 1<<30)
	s.Namespace = ns
	if err := u.E.Bind(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s.ID
}
