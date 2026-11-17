package enginewire_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Compose over a real store and an initialised journal directory, its
// caller passing no clock to the journal: Open opens the journal recorded
// under the expected identity and refuses another, the Tracer's span ends,
// and a clone's hold taken through Holds is held by the table Compose
// returned.
func TestCompose(t *testing.T) {
	ctx := t.Context()
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	dir := t.TempDir()
	id, err := journal.Init(ctx, dir, journal.InstallationID{1}, journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("journal init: %v", err)
	}

	table, d, err := enginewire.Compose(enginewire.Params{
		Node:    entity.NodeID{1},
		Devices: []string{dir},
		Store:   fs.Store,
		Clock:   sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Journal: []journal.Option{journaltest.SuitableMount()},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	j, err := d.Open(ctx, dir, journal.Version{}, id)
	if err != nil {
		t.Fatalf("Open of the journal under its own identity: got %v, want it opened", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close the journal: %v", err)
	}
	if j, err := d.Open(ctx, dir, journal.Version{}, journal.JournalID{9}); err == nil {
		_ = j.Close()
		t.Errorf("Open under another identity: got the journal, want a refusal")
	}

	if _, end := d.Tracer(ctx, "span"); end == nil {
		t.Errorf("Tracer: got a nil end, want a function ending the span")
	} else {
		end(nil)
	}

	if err := table.Start(ctx, &enginetest.Sink{}, func(entity.FileID) func() { return func() {} }); err != nil {
		t.Fatalf("open state start: %v", err)
	}
	t.Cleanup(table.Stop)
	_, src := fs.Create(t, fs.Admin, fs.Root, "src", kvstore.Regular(0o644))
	_, dst := fs.Create(t, fs.Admin, fs.Root, "dst", kvstore.Regular(0o644))
	if err := table.Recovery().ServeShard(ctx, fs.Share.ID, entity.ShardID(src.Shard), 1); err != nil {
		t.Fatalf("serve shard: %v", err)
	}
	ex := fs.Store.Existence()
	failed, err := ex.Commit(ctx, []kvstore.PendingExistence{{Share: fs.Share.ID, File: src.ID,
		Ops: []kvstore.PendingOp{{Kind: kvstore.OpWrite, Version: entity.VersionOf(1), Extent: entity.ExtentOf(0, 4096)}}}}, nil)
	if err == nil {
		err = failed[src.ID]
	}
	if err == nil {
		err = ex.Clone(ctx, kvstore.CloneOp{Share: fs.Share.ID, File: dst.ID, Version: entity.VersionOf(5),
			Spec: entity.CloneSpec{SrcShare: fs.Share.ID, Src: src.ID, Len: 4096}}, nil)
	}
	if err != nil {
		t.Fatalf("clone src onto dst: %v", err)
	}
	o, err := d.Holds.Hold(ctx, engine.CloneSrc{Share: fs.Share.ID, Shard: entity.ShardID(src.Shard), File: src.ID, Dst: dst.ID, AsOf: entity.VersionOf(5)})
	if err != nil {
		t.Fatalf("Hold of the clone's source: %v", err)
	}
	if err := table.Opens().CloneClose(ctx, o); err != nil {
		t.Errorf("CloneClose on Compose's table of the hold Holds took: got %v, want the table holding it", err)
	}
}

// composed is Compose over a real store and an initialised journal, and the
// engine built from it.
func composed(t *testing.T) (*kvstoretest.FS, *openstate.Table, *engine.Engine) {
	t.Helper()
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	dir := t.TempDir()
	if _, err := journal.Init(t.Context(), dir, journal.InstallationID{1}, journaltest.SuitableMount()); err != nil {
		t.Fatalf("journal init: %v", err)
	}
	table, d, err := enginewire.Compose(enginewire.Params{
		Node:    entity.NodeID{1},
		Devices: []string{dir},
		Store:   fs.Store,
		Clock:   sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Journal: []journal.Option{journaltest.SuitableMount()},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	e, err := engine.New(d)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return fs, table, e
}

// Open state is running between Start and Stop, and stopped once Stop closed
// the engine; a Start the engine refuses stops open state again. A stopped
// table refuses serving a shard.
func TestStartAndStop(t *testing.T) {
	prof.Watch(t, prof.Options{})
	noOrder := func(entity.FileID) func() { return func() {} }

	fs, table, e := composed(t)
	if err := enginewire.Start(t.Context(), table, e, &enginetest.Sink{}, noOrder, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := table.Recovery().ServeShard(t.Context(), fs.Share.ID, fs.Share.Shard, 1); err != nil {
		t.Errorf("serve a shard after Start: got %v, want open state running", err)
	}
	if err := enginewire.Stop(t.Context(), table, e); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := table.Recovery().ServeShard(t.Context(), fs.Share.ID, fs.Share.Shard, 2); !errors.Is(err, openstate.ErrStopped) {
		t.Errorf("serve a shard after Stop: got %v, want ErrStopped", err)
	}

	fs, table, e = composed(t)
	if err := e.Close(t.Context()); err != nil {
		t.Fatalf("close the engine before its start: %v", err)
	}
	if err := enginewire.Start(t.Context(), table, e, &enginetest.Sink{}, noOrder, nil); err == nil || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Fatalf("Start of a closed engine: got %v, want the engine's refusal", err)
	}
	if err := table.Recovery().ServeShard(t.Context(), fs.Share.ID, fs.Share.Shard, 1); !errors.Is(err, openstate.ErrStopped) {
		t.Errorf("serve a shard after a Start the engine refused: got %v, want ErrStopped", err)
	}
}
