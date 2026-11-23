package enginewire_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/engine/enginewire"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/sim"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Compose over a real store and an initialised journal directory, its
// caller passing no clock to the journal: Open opens the journal recorded
// under the expected identity and refuses another, the Tracer's span ends,
// and a clone's hold taken through Holds is held by the table Compose
// returned.
func TestCompose(t *testing.T) {
	ctx := t.Context()
	w := composed(t)
	fs, table, d, dir := w.fs, w.table, w.deps, w.deps.Devices[0]

	j, err := d.Open(ctx, dir, journal.Version{}, w.id, nil)
	if err != nil {
		t.Fatalf("Open of the journal under its own identity: got %v, want it opened", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close the journal: %v", err)
	}
	if j, err := d.Open(ctx, dir, journal.Version{}, journal.JournalID{9}, nil); err == nil {
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

// wiring is what composed built.
type wiring struct {
	fs    *kvstoretest.FS
	table *openstate.Table
	deps  engine.Deps
	e     *engine.Engine
	id    journal.JournalID // the journal initialised in the one device
	dir   string            // the device's journal directory
}

// composed is Compose over a real store and an initialised journal, its
// caller passing no clock to the journal, and the engine built from it.
func composed(t *testing.T) wiring {
	t.Helper()
	return composedWith(t, nil)
}

// composedWith is composed over the host's tracer, nil for none.
func composedWith(t *testing.T, tr trace.Tracer) wiring {
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
	id, err := journal.Init(t.Context(), dir, journal.InstallationID{1}, journaltest.SuitableMount())
	if err != nil {
		t.Fatalf("journal init: %v", err)
	}
	table, d, err := enginewire.Compose(enginewire.Params{
		Node:    node,
		Devices: []string{dir},
		Store:   fs.Store,
		Clock:   sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Journal: []journal.Option{journaltest.SuitableMount()},
		Tracer:  tr,
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	e, err := engine.New(d)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return wiring{fs: fs, table: table, deps: d, e: e, id: id, dir: dir}
}

// node is the node composed builds for.
var node = entity.NodeID{1}

// failingClose is the engine with its Close failing on cue, as one whose
// clone batches did not finish within the close's bound does.
type failingClose struct {
	*engine.Engine
	fail bool
}

func (f *failingClose) Close(ctx context.Context) error {
	if f.fail {
		return errors.New("injected")
	}
	return f.Engine.Close(ctx)
}

// epoch is the node epoch the store records for node.
func epoch(t *testing.T, s *kvstore.Store) uint64 {
	t.Helper()
	rec, _, err := s.Nodes().Node(t.Context(), node)
	if err != nil {
		t.Fatalf("read the node's record: %v", err)
	}
	return rec.Epoch
}

// Open state is running between Start and Stop, and stopped once Stop closed
// the engine. An engine whose close fails leaves open state running, a
// shard still served, and a later Stop closes the engine and stops open
// state. Open state starts before the engine, so a Start the engine refuses
// has raised the node epoch, and stops open state again. A stopped table
// refuses serving a shard.
func TestStartAndStop(t *testing.T) {
	prof.Watch(t, prof.Options{})
	noOrder := func(entity.FileID) func() { return func() {} }

	w := composed(t)
	if err := enginewire.Start(t.Context(), w.table, w.e, &enginetest.Sink{}, noOrder, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.table.Recovery().ServeShard(t.Context(), w.fs.Share.ID, w.fs.Share.Shard, 1); err != nil {
		t.Errorf("serve a shard after Start: got %v, want open state running", err)
	}
	if err := enginewire.Stop(t.Context(), w.table, w.e); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := w.table.Recovery().ServeShard(t.Context(), w.fs.Share.ID, w.fs.Share.Shard, 2); !errors.Is(err, openstate.ErrStopped) {
		t.Errorf("serve a shard after Stop: got %v, want ErrStopped", err)
	}

	w = composed(t)
	if err := enginewire.Start(t.Context(), w.table, w.e, &enginetest.Sink{}, noOrder, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e := &failingClose{Engine: w.e, fail: true}
	if err := enginewire.Stop(t.Context(), w.table, e); err == nil || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Fatalf("Stop with the engine's close failing: got %v, want the engine's error", err)
	}
	if err := w.table.Recovery().ServeShard(t.Context(), w.fs.Share.ID, w.fs.Share.Shard, 1); err != nil {
		t.Errorf("serve a shard after a Stop whose engine close failed: got %v, want open state still running", err)
	}
	e.fail = false
	if err := enginewire.Stop(t.Context(), w.table, e); err != nil {
		t.Fatalf("a second Stop once the engine closes: %v", err)
	}
	if err := w.table.Recovery().ServeShard(t.Context(), w.fs.Share.ID, w.fs.Share.Shard, 2); !errors.Is(err, openstate.ErrStopped) {
		t.Errorf("serve a shard after the second Stop: got %v, want ErrStopped", err)
	}

	w = composed(t)
	if err := w.e.Close(t.Context()); err != nil {
		t.Fatalf("close the engine before its start: %v", err)
	}
	before := epoch(t, w.fs.Store)
	if err := enginewire.Start(t.Context(), w.table, w.e, &enginetest.Sink{}, noOrder, nil); err == nil || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Fatalf("Start of a closed engine: got %v, want the engine's refusal", err)
	}
	if got := epoch(t, w.fs.Store); got != before+1 {
		t.Errorf("node epoch after a Start the engine refused: got %d, want %d, open state started first", got, before+1)
	}
	if err := w.table.Recovery().ServeShard(t.Context(), w.fs.Share.ID, w.fs.Share.Shard, 1); !errors.Is(err, openstate.ErrStopped) {
		t.Errorf("serve a shard after a Start the engine refused: got %v, want ErrStopped", err)
	}
}

// plant writes the bytes of segment id into the device's journal directory.
func (w wiring) plant(t *testing.T, id uint64, b []byte) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(w.dir, fmt.Sprintf("%020d.seg", id)), b, 0o600); err != nil {
		t.Fatalf("plant segment %d: %v", id, err)
	}
}

// An open that meets a record this journal cannot build, a verifying
// reconcile record, fails Start: no retry of the open can clear it, so the
// engine raises no journal_open_refused and arms no retry. An open the
// journal refuses for a cause a retry can clear, a named predecessor cut
// short, leaves Start succeeding with journal_open_refused raised and the
// retry armed.
func TestStartOverARecoveryRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plant   func(t *testing.T, w wiring)
		failed  error // what Start's error must wrap, nil when Start succeeds
		refused bool  // journal_open_refused is raised, its open retried
	}{
		{
			name: "a verifying reconcile record",
			plant: func(t *testing.T, w wiring) {
				b := raw.SegmentHeader{Identity: w.id, ID: 1}.Bytes()
				b = append(b, raw.Record{
					Identity: w.id, Segment: 1, Kind: 15, Seq: 100,
					RecordOffset: uint32(len(b)),
				}.Bytes()...)
				w.plant(t, 1, b)
			},
			failed: journal.ErrNotImplemented,
		},
		{
			name: "a named predecessor cut short",
			plant: func(t *testing.T, w wiring) {
				w.plant(t, 1, make([]byte, 20))
				w.plant(t, 2, raw.SegmentHeader{Identity: w.id, ID: 2, Predecessor: 1}.Bytes())
			},
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := composed(t)
			tc.plant(t, w)

			if err := w.fs.Store.RecordJournal(t.Context(), w.id, node, entity.JournalDir(w.dir)); err != nil {
				t.Fatalf("record the journal: %v", err)
			}

			share := enginetest.ShareOf(t, w.fs.Store, w.fs.Share.ID, 1<<20)
			noOrder := func(entity.FileID) func() { return func() {} }
			err := enginewire.Start(t.Context(), w.table, w.e, &enginetest.Sink{}, noOrder, []engine.Share{share})

			if tc.failed != nil {
				if !errors.Is(err, tc.failed) {
					t.Fatalf("Start = %v, want it failed with %v", err, tc.failed)
				}

				if cs := w.e.Stats().Journals[w.dir].Conditions; len(cs) != 0 {
					t.Errorf("conditions after a failed Start = %+v, want none: no journal_open_refused, no retry", cs)
				}

				return
			}

			if err != nil {
				t.Fatalf("Start = %v, want nil: a refusal a retry can clear does not stop the node", err)
			}

			t.Cleanup(func() { _ = enginewire.Stop(context.Background(), w.table, w.e) })

			found := false

			for _, c := range w.e.Stats().Journals[w.dir].Conditions {
				found = found || c.Name == "journal_open_refused"
			}

			if !found {
				t.Errorf("journal_open_refused was not raised")
			}

		})
	}
}

// Compose over a tracer adapts it into the journal's Tracer option: a commit
// and a repack pass each reach the recorder as a span named
// journal.<op>, and every journal span, whatever its op, carries the journal
// as its identity attribute — the hex of the identity the open resolved. With no tracer,
// TestCompose covers the same composition: the journal opens with no tracer
// at all, and its drives work.
func TestComposeAdaptsTheTracerToTheJournal(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)).Tracer("dittofs")

	w := composedWith(t, tracer)
	j, err := w.deps.Open(t.Context(), w.dir, journal.Version{}, w.id, nil)
	if err != nil {
		t.Fatalf("Open of the journal under its own identity: got %v, want it opened", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	// The journal issues no version until its first limit, as the fixture
	// sets one on every open of its own.
	if err := j.SetVersionLimit(journal.Version{Counter: 1 << 20}); err != nil {
		t.Fatalf("SetVersionLimit of the composed journal: %v", err)
	}
	h, err := j.Share(journal.ShareTag(1), 0, journal.Version{})
	if err != nil {
		t.Fatalf("Share of the opened journal: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	file := journal.NewFileID([16]byte{'t'})
	// A fresh open fills its spare pool in the background, so the first
	// write retries until the pool has one, as the journal's own fixtures do.
	if err := journaltest.RetrySpare(func() error {
		_, err := h.WriteAt(t.Context(), file, 0, []byte("the traced write"), 0)
		return err
	}); err != nil {
		t.Fatalf("WriteAt through the composed journal: %v", err)
	}
	if _, err := h.Sync(t.Context(), file); err != nil {
		t.Fatalf("Sync through the composed journal: %v", err)
	}
	if _, err := j.Repack(t.Context(), journaltest.FixtureSegmentCap, journal.RepackScope{}); err != nil && !errors.Is(err, journal.ErrNotImplemented) {
		t.Fatalf("Repack through the composed journal: %v", err)
	}

	counts := map[string]int{}
	var ident string
	for _, s := range sr.Ended() {
		if !strings.HasPrefix(s.Name(), "journal.") {
			continue
		}
		counts[s.Name()]++
		named := false
		for _, a := range s.Attributes() {
			if string(a.Key) == "journal" {
				ident, named = a.Value.AsString(), true
				if want := hex.EncodeToString(w.id[:]); ident != want {
					t.Errorf("span %s names the journal %s, want the hex of the identity Open resolved, %s", s.Name(), ident, want)
				}
			}
		}
		if !named {
			t.Errorf("span %s carries no journal attribute, want every journal span to name its journal", s.Name())
		}
	}
	for _, name := range []string{"journal.sync", "journal.repack_pass"} {
		if counts[name] < 1 {
			t.Errorf("no %s span after the drives, want at least the one the commit or the repack pass ran (spans seen: %v)", name, counts)
		}
	}
	if ident == "" {
		t.Fatalf("no journal span carried the journal attribute (spans seen: %v)", counts)
	}
}

// RFC 8 §2.5 step 1, RFC 28 §3.3: the composed Open hands the journal the
// sync marks it is given. Over a real directory with the power loss
// simulated beneath the journal, a record is written and synced, the power
// lost, and the record's bytes flipped on disk. Opened with the mark its
// sync returned, the journal knows the record was acknowledged and does not
// cut it as a torn tail: it refuses the open, as it does for every lost
// synced tail until its reconcile can recover what the record held. Opened
// with no marks, the same directory opens with the record cut and no loss
// reported, which is the open the engine made before it passed the marks.
func TestComposeOpensTheJournalWithItsMarks(t *testing.T) {
	for _, c := range []struct {
		name  string
		marks bool
	}{
		{"with the sync's marks", true},
		{"with no marks", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			seam, err := kit.NewSeam(dir)
			if err != nil {
				t.Fatalf("seam: %v", err)
			}
			clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			opts := []journal.Option{journaltest.SuitableMount(), journal.WithStorage(seam.FS()), journal.WithClock(clock)}
			id, err := journal.Init(ctx, dir, journal.InstallationID{1}, opts...)
			if err != nil {
				t.Fatalf("journal init: %v", err)
			}

			j, err := journal.Open(ctx, dir, journal.Version{}, id, nil, opts...)
			if err != nil {
				t.Fatalf("Open before the crash: %v", err)
			}
			if err := j.SetVersionLimit(journal.Version{Counter: 1 << 20}); err != nil {
				t.Fatalf("SetVersionLimit: %v", err)
			}
			h, err := j.Share(journal.ShareTag(1), 0, journal.Version{})
			if err != nil {
				t.Fatalf("Share: %v", err)
			}
			file := journal.NewFileID([16]byte{'l'})
			p := []byte(strings.Repeat("lost", 1024))
			if err := journaltest.RetrySpare(func() error {
				_, err := h.WriteAt(ctx, file, 0, p, 0)
				return err
			}); err != nil {
				t.Fatalf("WriteAt: %v", err)
			}
			synced, err := h.Sync(ctx, file)
			if err != nil || len(synced) != 1 {
				t.Fatalf("Sync returned marks %v, %v; want the record's one mark", synced, err)
			}
			if err := seam.Crash(); err != nil {
				t.Fatalf("crash: %v", err)
			}
			_ = h.Close()
			_ = j.Close()
			flipRecord(t, seam, p)

			var marks []entity.SyncMark
			if c.marks {
				for _, m := range synced {
					marks = append(marks, entity.SyncMark(m))
				}
			}
			// The crash closed every file the seam served, so the journal
			// the wiring opens takes the seam's storage as it is now.
			fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
				db, err := badgerkv.Open(badgerkv.Options{})
				if err != nil {
					tb.Fatal(err)
				}
				tb.Cleanup(func() { _ = db.Close() })
				return db
			})
			_, d, err := enginewire.Compose(enginewire.Params{
				Node: node, Devices: []string{dir}, Store: fs.Store, Clock: clock,
				Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
				Journal: []journal.Option{journaltest.SuitableMount(), journal.WithStorage(seam.FS())},
			})
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}

			ej, err := d.Open(ctx, dir, journal.Version{}, id, marks)
			if c.marks {
				if err == nil {
					_ = ej.Close()
				}
				// Many refusals wrap ErrOpenRefused, so the check names the
				// lost tail's own: the acknowledged record, by its segment,
				// sequence and end, which only the mark the sync returned
				// carries to the journal.
				m := synced[0]
				lost := fmt.Sprintf("segment %d: the sync's acknowledged record, sequence %d ending at %d, verifies nowhere", m.Segment, m.Sequence, m.Offset)
				if !errors.Is(err, journal.ErrOpenRefused) || !strings.Contains(err.Error(), lost) {
					t.Fatalf("Open with the sync's mark = %v, want ErrOpenRefused naming %q", err, lost)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open with no marks = %v, want the record cut as a torn tail", err)
			}
			defer func() { _ = ej.Close() }()
			if losses := ej.Stats().Losses; len(losses) != 0 {
				t.Errorf("Open with no marks reported losses %+v, want none", losses)
			}
		})
	}
}

// The Lifecycle Compose hands the engine logs a sync mark it leaves out to
// the engine's logger: a key under J‖journal‖sm too short for a stream is
// logged there, with its journal and key, when the journal's marks are read.
func TestComposeLogsASkippedMarkToTheEnginesLogger(t *testing.T) {
	var db kv.KV
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		var err error
		if db, err = badgerkv.Open(badgerkv.Options{}); err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	j := [16]byte{'j'}
	// J‖journal‖sm then one byte where the stream's four belong: J is 0x2e
	// and sm 0x01, as kvstore's golden test pins them.
	key := append(append([]byte{0x2e}, j[:]...), 0x01, 7)
	if err := db.Update(t.Context(), func(tx kv.Txn) error { return tx.Set(key, []byte{1}) }); err != nil {
		t.Fatalf("store the malformed mark key: %v", err)
	}

	var logs bytes.Buffer
	_, d, err := enginewire.Compose(enginewire.Params{
		Node: node, Store: fs.Store,
		Clock:  sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, _, err := d.Lifecycle.SyncMarks(t.Context(), j); err != nil {
		t.Fatalf("SyncMarks: %v", err)
	}
	want := fmt.Sprintf("journal=%s key=07", hex.EncodeToString(j[:]))
	if !strings.Contains(logs.String(), want) {
		t.Errorf("the engine's logger holds %q, want a warning naming %q", logs.String(), want)
	}
}

// flipRecord flips the first byte of p in every journal file whose durable
// image holds it, and makes the flip durable.
func flipRecord(t *testing.T, seam *kit.Seam, p []byte) {
	t.Helper()
	names, err := seam.List()
	if err != nil {
		t.Fatalf("list the journal's files: %v", err)
	}
	flipped := false
	for _, name := range names {
		image, err := seam.Durable(name)
		if err != nil {
			continue
		}
		at := strings.Index(string(image), string(p))
		if at < 0 {
			continue
		}
		f, err := seam.Open(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if _, err := f.WriteAt([]byte{^image[at]}, int64(at)); err != nil {
			t.Fatalf("flip a byte of %s: %v", name, err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("sync %s: %v", name, err)
		}
		_ = f.Close()
		flipped = true
	}
	if !flipped {
		t.Fatal("no journal file holds the record")
	}
}
