//go:build engine_conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// write writes p at off of file id and returns its verifier.
func write(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, p []byte) vfserr.Verifier {
	t.Helper()
	v, err := f.E.Write(t.Context(), f.Share.ID, id, off, p, engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatalf("write %d bytes at %d: %v", len(p), off, err)
	}
	return v
}

// read reads n bytes at off of file id, failing on a short read.
func read(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	got, err := f.E.Read(t.Context(), f.Share.ID, id, off, p)
	if err != nil || got != n {
		t.Fatalf("read %d bytes at %d: %d, %v", n, off, got, err)
	}
	return p
}

// eventually waits, in real time, for cond to hold: what the engine's own
// goroutines do after the test clock moves happens in real time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// durable reports whether a crash now would leave p in some file of the
// journal's directory.
func durable(f *enginetest.Fixture, p []byte) bool {
	names, err := f.Seam.List()
	if err != nil {
		return false
	}
	for _, name := range names {
		if image, err := f.Seam.Durable(name); err == nil && bytes.Contains(image, p) {
			return true
		}
	}
	return false
}

// A write is the journal's before any stability point: Overlay, Allocation
// and reads reflect it, and after a crash recovery re-applies its existence
// before the file is served.
func TestRFC8A_JournalAuthority(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	h, id := f.File("a")
	p := []byte("the journal is the authority")
	write(t, f, id, 0, p)

	served := func(when string) {
		t.Helper()
		sp, err := f.E.Allocation(ctx, f.Share.ID, id, 0)
		if err != nil || sp.Class == entity.Hole || sp.Offset > 0 || sp.Offset+sp.Length < int64(len(p)) {
			t.Errorf("%s: Allocation(0) = %+v, %v; want data over [0, %d)", when, sp, err, len(p))
		}
		if got := read(t, f, id, 0, len(p)); !bytes.Equal(got, p) {
			t.Errorf("%s: read %q, want %q", when, got, p)
		}
	}
	if o, pending, err := f.E.Overlay(ctx, f.Share.ID, id); err != nil || !pending || o.Size != int64(len(p)) {
		t.Errorf("before the crash: Overlay = %+v, pending %v, %v; want size %d pending", o, pending, err, len(p))
	}
	served("before the crash")

	// The sync bound is shorter than the existence age, so the write is made
	// durable and its existence is still uncommitted when the crash comes:
	// only recovery can commit it. The journal's worker arms the bound's timer
	// some time after the write answers, and an advance that lands first fires
	// nothing. The open-state sweep and the idle sealer are pending from the
	// start and the group commit arms one for the write, so the count names
	// the sync's timer only at four.
	eventually(t, "the sync bound's timer is set", func() bool { return f.Clock.Pending() == 4 })
	f.Clock.Advance(journal.SyncBound)
	eventually(t, "the write is durable within the sync bound", func() bool { return durable(f, p) })
	f.Crash()
	if got := f.Committed(h).Size; got != int64(len(p)) {
		t.Errorf("served after the crash with committed size %d, want %d re-applied", got, len(p))
	}
	served("after the crash")
}

// TestRFC8A_IncarnationAtStart, the engine half: a restart with no journal
// attach raises the incarnation of the shard it serves, in the start form, so
// the shard opens a new grace instance. A start that raised incarnations only
// at attach would repeat the grace instance. Holding open-state answers and
// grace until the raise commits is open state's half, not checked here.
func TestRFC8A_IncarnationAtStart(t *testing.T) {
	f := enginetest.New(t)

	before, err := f.FS.Store.Shard(t.Context(), f.Share.Shard)
	if err != nil {
		t.Fatal(err)
	}

	f.Restart()

	after, err := f.FS.Store.Shard(t.Context(), f.Share.Shard)
	if err != nil {
		t.Fatal(err)
	}

	if after.Incarnation <= before.Incarnation || after.GraceInstance != after.Incarnation {
		t.Errorf(
			"shard after a restart: incarnation %d, grace instance %d, "+
				"from %d and %d; want a start raise",
			after.Incarnation,
			after.GraceInstance,
			before.Incarnation,
			before.GraceInstance,
		)
	}
}

// Two writes in one process answer one verifier; a write after a restart
// answers another.
func TestRFC8A_Verifier(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	v1 := write(t, f, id, 0, []byte("one"))
	if v2 := write(t, f, id, 3, []byte("two")); v2 != v1 {
		t.Errorf("two writes in one process: verifiers %x and %x differ", v1, v2)
	}
	f.Restart()
	if v3 := write(t, f, id, 6, []byte("three")); v3 == v1 {
		t.Errorf("a write after a restart answers the verifier %x from before it", v3)
	}
}

// A Commit over an append answers after the journal sync and before any
// metadata transaction; a crash before the group commit leaves the size and
// mtime to recovery, which commits them before the file is served.
func TestRFC8A_StableWriteLazyExistence(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	h, id := f.File("a")
	p := []byte("appended, then committed")
	// The store stamps the create from the wall clock and keeps the later of a
	// file's mtime and a write's, so the write is stamped after the create.
	f.Clock.Jump(f.Committed(h).Modify.Sub(f.Clock.Now()) + time.Second)
	at := f.Clock.Now()

	f.FS.Counts.Take()
	write(t, f, id, 0, p)
	if _, _, err := f.E.Commit(ctx, f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if c := f.FS.Counts.Take(); c.Sets+c.Deletes > 0 {
		t.Errorf("Write and Commit wrote metadata: %+v; existence waits for the group commit", c)
	}

	// The crash drops every unsynced record: what survives was synced before
	// Commit answered.
	f.Crash()
	if c := f.Committed(h); c.Size != int64(len(p)) || !c.Modify.Equal(at) {
		t.Errorf("after the crash: committed size %d, mtime %v; want size %d, mtime %v", c.Size, c.Modify, len(p), at)
	}
	if got := read(t, f, id, 0, len(p)); !bytes.Equal(got, p) {
		t.Errorf("after the crash: read %q, want %q", got, p)
	}
}

// TestRFC8A_AttachAboveTheFloor, adapted: a share whose files' versions reach
// far above the journal's counter is bound, and a write over its content is
// given a version above them. The store stamps its own versions from the
// clock in nanoseconds, so the floor is set relative to the store's, not at
// 9·10⁸. The half left: the offload, eviction and read are the engine's
// second part's, which offloads; the restore repeat is RFC 12's, and the
// clone repeat is checked with them, once a read can prove it.
func TestRFC8A_AttachAboveTheFloor(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.FS.Create(t, f.FS.Admin, f.SecondRoot, "imported", kvstore.Regular(0o644))

	base, err := f.FS.Store.Content().VersionFloor(ctx, []entity.ShareID{f.Second.ID})
	if err != nil {
		t.Fatal(err)
	}

	imported := entity.VersionOf(base.Counter() + 1e12)
	pending := []kvstore.PendingExistence{
		{
			Share: f.Second.ID,
			File:  file.ID,
			Ops: []kvstore.PendingOp{
				{
					Version: imported,
					Kind:    kvstore.OpWrite,
					Extent:  entity.Extent{Length: 4096},
					Mtime:   f.Clock.Now(),
					Ctime:   f.Clock.Now(),
				},
			},
		},
	}

	failed, err := f.FS.Store.Existence().Commit(ctx, pending, nil)
	if err != nil || failed[file.ID] != nil {
		t.Fatalf("commit the imported content: %v, %v", err, failed[file.ID])
	}

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	_, err = f.E.Write(ctx, f.Second.ID, file.ID, 0, []byte("new"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatalf("write over the imported content: %v", err)
	}

	h := f.HandleOf(f.Second.Tag)
	changes := 0

	for c, err := range h.Since(ctx, journal.NewFileID(file.ID), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}

		changes++

		if c.Version.Counter <= imported.Counter() {
			t.Errorf("the write's version %d is not above the imported %d", c.Version.Counter, imported.Counter())
		}
	}

	if changes == 0 {
		t.Error("the journal holds no change of the write")
	}
}

// lostJournal reports whether f's engine raises journal_lost for its device.
func lostJournal(f *enginetest.Fixture) bool {
	for _, c := range f.E.Stats().Journals[f.Dir].Conditions {
		if c.Name == "journal_lost" {
			return true
		}
	}

	return false
}

// TestRFC8A_JournalGone, adapted: two shares written without offload, the
// journal directory deleted, and a restart refuses both shares by name with
// the device, raises journal_lost and creates no journal. The half left: the
// operator's acknowledgement that serves them again, and the Lost read after
// it, are the engine's second part's; the replicated journal's rejoin is the
// cluster's.
func TestRFC8A_JournalGone(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.File("f")
	write(t, f, file, 0, []byte("unoffloaded"))

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	opts := engine.WriteOpts{
		Done: func() {
		},
	}

	// A file the store holds, so its existence commits before the restart
	// rather than failing stale until the close bound.
	if _, err := f.E.Write(ctx, f.Second.ID, secondFile(t, f, "s"), 0, []byte("second"), opts); err != nil {
		t.Fatalf("write to the second share: %v", err)
	}

	f.RestartAfter(func() {
		if err := os.RemoveAll(f.Dir); err != nil {
			t.Fatalf("delete the journal: %v", err)
		}
	})

	for _, sh := range []engine.Share{f.Share, f.Second} {
		if got := f.E.Stats().Shares[sh.ID]; got.Device != f.Dir || got.Refused == nil {
			t.Errorf("share %v: device %q, refused %v; want refused on %q", sh.ID, got.Device, got.Refused, f.Dir)
		}
	}

	if !lostJournal(f) {
		t.Error("journal_lost is not raised")
	}

	if _, err := os.Stat(f.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted journal directory: %v, want it still gone", err)
	}
}

// TestRFC8A_AttachAfterDeviceLoss, adapted: the device's journal destroyed
// and initialised again, its share is refused by name until the store
// records the new journal, as the operator's step would, and then attaches
// to it with its first version above the limit it had. The half left: the
// operator's acknowledgement that re-initialises the device and rewrites the
// shards' journal identity, and a share whose removal reads offloaded, are
// the engine's second part's.
func TestRFC8A_AttachAfterDeviceLoss(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.File("f")
	write(t, f, file, 0, []byte("lost"))

	old, err := f.FS.Store.VersionLimit(ctx, f.Share.ID)
	if err != nil {
		t.Fatal(err)
	}

	var id journal.JournalID

	f.RestartAfter(func() {
		id = f.Reinit()
	})

	if got := f.E.Stats().Shares[f.Share.ID]; got.Device != f.Dir || got.Refused == nil {
		t.Fatalf("before the journal is recorded: device %q, refused %v; want refused on %q", got.Device, got.Refused, f.Dir)
	}

	f.RestartAfter(func() {
		if err := f.FS.Store.RecordJournal(ctx, id, f.Node, entity.JournalDir(f.Dir)); err != nil {
			t.Fatalf("record the new journal: %v", err)
		}
	})

	if got := f.E.Stats().Shares[f.Share.ID]; got.Refused != nil {
		t.Fatalf("after the journal is recorded, the share is refused: %v", got.Refused)
	}

	_, again := f.File("again")
	write(t, f, again, 0, []byte("after"))

	changes := 0

	for c, err := range f.Handle().Since(ctx, journal.NewFileID(again), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}

		changes++

		if c.Version.Counter <= old.Counter() {
			t.Errorf("the first write's version %d is not above the old limit %d", c.Version.Counter, old.Counter())
		}
	}

	if changes == 0 {
		t.Error("the journal holds no change of the write")
	}
}

// TestRFC8A_VersionLimit, its start and crash halves: the share's limit is
// committed before the first version is drawn, and after a crash with a
// version reported and nothing synced the restarted journal issues above it.
// The raise halves are TestRFC8A_VersionLimitRaise.
func TestRFC8A_VersionLimit(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.File("a")

	limits, err := f.FS.Store.Lifecycle().VersionLimits(ctx, []entity.ShareID{f.Share.ID})
	if err != nil {
		t.Fatal(err)
	}

	before, err := f.E.Draw(ctx, f.Share.ID, file)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	if limits[f.Share.ID].Counter() <= before.Counter() {
		t.Fatalf(
			"the share's limit %d is not committed above the first version %d",
			limits[f.Share.ID].Counter(),
			before.Counter(),
		)
	}

	f.Crash()

	after, err := f.E.Draw(ctx, f.Share.ID, file)
	if err != nil {
		t.Fatalf("draw after the crash: %v", err)
	}

	if after.Counter() <= before.Counter() {
		t.Errorf(
			"the first version after the crash, %d, is not above the one reported before it, %d",
			after.Counter(),
			before.Counter(),
		)
	}
}

// TestRFC8A_VersionLimitRaise is the raise halves of the version limit row,
// at a block of 16: the raise commits before the journal is given its limit;
// a write at the limit while the raise's commit is blocked answers ErrDelay
// and succeeds once it commits; a raise whose commit fails once with no write
// pending commits on its own; and a Bind whose limit write fails, after its
// Share moved the counter to the limit, leaves the raise to the journal. Each
// step runs on a fixture of its own, so no raise of another is in flight.
// At this block one write a second already puts the peak rate's term of the
// trigger above the versions left, so every raise falls due on the next
// version.
func TestRFC8A_VersionLimitRaise(t *testing.T) {
	const block = 16

	t.Run("committed before the journal is given it", func(t *testing.T) {
		f := enginetest.New(t, enginetest.WithVersionBlock(block))
		_, id := f.File("a")
		old := journalLimit(f)

		held := f.HoldNextWrite(time.Second, nil)
		write(t, f, id, 0, []byte("x"))
		wait(t, "the raise reaches the store", held)

		if got := journalLimit(f); got != old {
			t.Errorf("journal limit %d while the raise's commit is held, want %d", got, old)
		}

		f.Clock.Advance(time.Second)
		eventually(t, "the journal is given the raised limit", func() bool {
			return journalLimit(f) > old
		})

		if j, s := journalLimit(f), storeLimit(t, f, f.Share.ID); s < j {
			t.Errorf("journal limit %d above the committed limit %d", j, s)
		}
	})

	t.Run("a write at the limit waits for the blocked raise", func(t *testing.T) {
		f := enginetest.New(t, enginetest.WithVersionBlock(block))
		_, id := f.File("a")

		f.HoldNextWrite(time.Second, nil)
		err := writeToTheLimit(t, f, id, block)

		var noSpace *journal.NoSpaceError
		if errors.As(err, &noSpace) || !errors.As(err, new(*vfserr.Delay)) {
			t.Fatalf("the write at the limit = %v, want ErrDelay, not ErrNoSpace", err)
		}

		f.Clock.Advance(time.Second)
		write(t, f, id, 0, []byte("x"))
	})

	t.Run("a raise failing once commits with no write pending", func(t *testing.T) {
		f := enginetest.New(t, enginetest.WithVersionBlock(block))
		_, id := f.File("a")

		f.HoldNextWrite(time.Second, errors.New("an injected failure of the raise"))

		if err := writeToTheLimit(t, f, id, block); !errors.As(err, new(*vfserr.Delay)) {
			t.Fatalf("the write at the limit = %v, want ErrDelay", err)
		}

		at := journalLimit(f)
		f.Clock.Advance(time.Second)
		// The raise retries on the clock; nothing is written or drawn.
		eventually(t, "the failed raise retries and commits", func() bool {
			f.Clock.Advance(10 * time.Millisecond)
			return journalLimit(f) > at
		})

		write(t, f, id, 0, []byte("x"))
	})

	t.Run("a failed Bind leaves the raise to the journal", func(t *testing.T) {
		f := enginetest.New(t, enginetest.WithVersionBlock(block))
		ctx := t.Context()
		_, id := f.File("a")
		last := journalLimit(f)

		up := map[entity.ShareID]entity.JournalVersion{f.Second.ID: entity.VersionOf(last)}
		if err := f.FS.Store.Lifecycle().RaiseVersionLimits(ctx, up); err != nil {
			t.Fatal(err)
		}

		// Of the Bind's writes only its limit write sets a key of the share:
		// the incarnation raise sets its shard's.
		f.FailUpdatesSetting(func(key []byte) bool { return bytes.Contains(key, f.Second.ID[:]) })

		if err := f.E.Bind(ctx, f.Second); err == nil {
			t.Fatal("Bind succeeded with its limit write failing")
		}

		f.FailUpdatesSetting(nil)

		if next := f.E.Stats().Journals[f.Dir].Stats.NextVersion.Counter; next < last {
			t.Fatalf("next version %d after the Bind's Share, want at least the limit %d", next, last)
		}

		eventually(t, "the journal's raise commits with no write", func() bool {
			return journalLimit(f) > last
		})

		write(t, f, id, 0, []byte("x"))
	})
}

// journalLimit is the version limit the journal was last given.
func journalLimit(f *enginetest.Fixture) uint64 {
	return f.E.Stats().Journals[f.Dir].Stats.VersionLimit.Counter
}

// storeLimit is share id's version limit as the store has committed it.
func storeLimit(t *testing.T, f *enginetest.Fixture, id entity.ShareID) uint64 {
	t.Helper()

	limits, err := f.FS.Store.Lifecycle().VersionLimits(t.Context(), []entity.ShareID{id})
	if err != nil {
		t.Fatal(err)
	}

	return limits[id].Counter()
}

// writeToTheLimit writes file id until a write is refused, each bounded by a
// short context so one that waits for a raise ends, and returns that write's
// error. More than block writes accepted fails the test: the limit is at
// most a block above the first.
func writeToTheLimit(t *testing.T, f *enginetest.Fixture, id entity.FileID, block int) error {
	t.Helper()

	for range block + 1 {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		_, err := f.E.Write(ctx, f.Share.ID, id, 0, []byte("x"), engine.WriteOpts{Done: func() {}})
		cancel()

		if err != nil {
			return err
		}
	}

	t.Fatalf("%d writes accepted with the raise held, want at most %d", block+1, block)

	return nil
}

// A share bound with a limit several blocks above the journal's moves the
// counter past the journal's limit; its own limit is above the version it
// draws, and the first share draws again after the raise its Bind made.
func TestRFC8A_VersionLimitCountsFromCounter(t *testing.T) {
	const block = 1 << 24 // RFC 8 §2.7's block

	f := enginetest.New(t)
	ctx := t.Context()
	_, file := f.File("a")

	limits, err := f.FS.Store.Lifecycle().VersionLimits(ctx, []entity.ShareID{f.Share.ID})
	if err != nil {
		t.Fatal(err)
	}

	high := map[entity.ShareID]entity.JournalVersion{
		f.Second.ID: entity.VersionOf(limits[f.Share.ID].Counter() + 3*block),
	}

	if err := f.FS.Store.Lifecycle().RaiseVersionLimits(ctx, high); err != nil {
		t.Fatal(err)
	}

	if err := f.E.Bind(ctx, f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	v, err := f.E.Draw(ctx, f.Second.ID, file)
	if err != nil {
		t.Fatalf("draw on the bound share: %v", err)
	}

	got, err := f.FS.Store.Lifecycle().VersionLimits(ctx, []entity.ShareID{f.Second.ID})
	if err != nil {
		t.Fatal(err)
	}

	if got[f.Second.ID].Counter() <= v.Counter() {
		t.Errorf(
			"the bound share's limit %d is not above its version %d",
			got[f.Second.ID].Counter(),
			v.Counter(),
		)
	}

	if _, err := f.E.Draw(ctx, f.Share.ID, file); err != nil {
		t.Errorf("the first share's draw after the Bind's raise = %v", err)
	}
}

// dirContents is the name and size of every file of dir, for a test that
// asserts a directory was left as it was.
func dirContents(t *testing.T, dir string) map[string]int64 {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the journal directory: %v", err)
	}

	out := map[string]int64{}

	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}

		out[e.Name()] = info.Size()
	}

	return out
}

// unsuitableJournal reports whether f's engine raises journal_unsuitable for
// its device.
func unsuitableJournal(f *enginetest.Fixture) bool {
	for _, c := range f.E.Stats().Journals[f.Dir].Conditions {
		if c.Name == "journal_unsuitable" {
			return true
		}
	}

	return false
}

// TestRFC8A_UnsuitableJournalAtStart, adapted to one journal device: a start
// over a directory the platform refuses succeeds, names the share with its
// device and the rule, raises journal_unsuitable and changes nothing in the
// directory; once the check clears and 30 s pass, the share attaches by
// itself, its content reads back, its shard's incarnation is raised in the
// start form, the write verifier differs from before the restart and the
// condition clears. The half left: the second device whose share stays
// served needs the fixture to run two journal devices, which it adds with
// the change that composes them.
func TestRFC8A_UnsuitableJournalAtStart(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	v1 := write(t, f, id, 0, []byte("unoffloaded"))

	var dir map[string]int64

	f.RestartAfter(func() {
		f.RefuseOpens(true)

		dir = dirContents(t, f.Dir)
	})

	got := f.E.Stats().Shares[f.Share.ID]
	if got.Device != f.Dir || !errors.Is(got.Refused, journal.ErrUnsuitableStorage) {
		t.Errorf(
			"share: device %q, refused %v; want %q and the rule",
			got.Device, got.Refused, f.Dir,
		)
	}

	if !unsuitableJournal(f) {
		t.Error("journal_unsuitable is not raised")
	}

	if now := dirContents(t, f.Dir); !maps.Equal(now, dir) {
		t.Errorf("the journal directory changed: %v, was %v", now, dir)
	}

	before, err := f.FS.Store.Shard(t.Context(), f.Share.Shard)
	if err != nil {
		t.Fatal(err)
	}

	// Each poll moves the clock by the retry's interval: the engine's other
	// timers share the clock, so no pending count says the retry's is armed.
	f.RefuseOpens(false)
	eventually(t, "the share attaches", func() bool {
		f.Clock.Advance(30 * time.Second)
		return f.E.Stats().Shares[f.Share.ID].Refused == nil
	})

	if !bytes.Equal(read(t, f, id, 0, 11), []byte("unoffloaded")) {
		t.Error("the content does not read back after the attach")
	}

	after, err := f.FS.Store.Shard(t.Context(), f.Share.Shard)
	if err != nil {
		t.Fatal(err)
	}

	if want := before.Incarnation + 1; after.Incarnation != want || after.GraceInstance != want {
		t.Errorf(
			"shard after the attach: incarnation %d, grace %d; want %d and %d",
			after.Incarnation, after.GraceInstance, want, want,
		)
	}

	if v2 := write(t, f, id, 11, []byte("more")); v2 == v1 {
		t.Errorf("a write after the attach answers the verifier %x from before the restart", v2)
	}

	if unsuitableJournal(f) {
		t.Error("journal_unsuitable is still raised after the attach")
	}
}

// TestRFC8A_RetryLaterAtTheShareLimit is RFC 8 §15.2's "§10.2 retry later" for
// a share at its journal limit: the write that reaches the limit is retry-later
// for capacity, never no space.
func TestRFC8A_RetryLaterAtTheShareLimit(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	second := f.Second
	second.Limit = 64 << 10

	if err := f.E.Bind(ctx, second); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	file := secondFile(t, f, "full")
	p := make([]byte, 16<<10)

	for i := range 16 {
		_, err := f.E.Write(ctx, second.ID, file, int64(i*len(p)), p, engine.WriteOpts{Done: func() {}})
		if err == nil {
			continue
		}

		var d *vfserr.Delay

		if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
			t.Fatalf("the write at the limit: %v, want ErrDelay(DelayCapacity)", err)
		}

		if errors.Is(err, vfserr.ErrNoSpace) {
			t.Fatalf("the write at the limit: %v, want retry-later, not no space", err)
		}

		return
	}

	t.Fatal("the share never reached its journal limit")
}

// The fixture journal's segment and maximum, as the fixture sets them: the
// conformance package imports no test kit.
const (
	mib      = 1 << 20
	maxBytes = 64 * mib
)

// bound is a generous real-time backstop for a wait that must end: a wrong
// engine fails on the assertion instead of hanging the package.
const bound = 10 * time.Second

// within waits for ch, failing t naming what never happened.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(bound):
		t.Fatalf("%s: not within %v", what, bound)
	}

	panic("unreachable")
}

// putSecond writes n bytes of c at off of file id of the fixture's second
// share, under a context that ends after bound.
func putSecond(
	t *testing.T,
	f *enginetest.Fixture,
	id entity.FileID,
	off int64,
	n int,
	c byte,
) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()

	_, err := f.E.Write(
		ctx,
		f.Second.ID,
		id,
		off,
		bytes.Repeat([]byte{c}, n),
		engine.WriteOpts{Done: func() {}},
	)

	return err
}

// mustSecond is putSecond that must be served.
func mustSecond(t *testing.T, f *enginetest.Fixture, id entity.FileID, off int64, n int, c byte) {
	t.Helper()

	if err := putSecond(t, f, id, off, n, c); err != nil {
		t.Fatalf("write of %d bytes at %d: %v", n, off, err)
	}
}

// limitedSecond binds the second share with a journal limit of limit bytes.
func limitedSecond(t *testing.T, f *enginetest.Fixture, limit int64) {
	t.Helper()

	f.Second.Limit = limit

	if err := f.E.Bind(t.Context(), f.Second); err != nil {
		t.Fatalf("Bind: %v", err)
	}
}

// recordBytes is what one write of n bytes counts against a share's limit:
// its payload and the record's header and padding, as the journal counts
// them. It is measured on a fixture of its own, stopped before the row
// builds its own, and read from the share's UsedBytes, since the journal's
// record format is not this package's to import.
func recordBytes(t *testing.T, n int) int64 {
	t.Helper()

	var used int64

	t.Run("record bytes", func(t *testing.T) {
		f := enginetest.New(t)
		_, id := f.File("probe")
		write(t, f, id, 0, make([]byte, n))

		js := f.E.Stats().Journals[f.Dir]
		used = js.Stats.Shares[attachedTag(js, f.Share.ID)].UsedBytes
	})

	if used <= int64(n) {
		t.Fatalf("one write of %d bytes counts %d against the share, want more than its payload", n, used)
	}

	return used
}

// unreclaimed is the journal's figures for share id: its unreclaimed bytes,
// read through the carried snapshot, whose Shares map the entry's TagShares
// keys by the share's tag.
func unreclaimed(f *enginetest.Fixture, id entity.ShareID) int64 {
	js := f.E.Stats().Journals[f.Dir]
	return js.Stats.Shares[attachedTag(js, id)].UnreclaimedBytes
}

// attachedTag is the tag the entry names for attached share id; it panics
// when the entry names none, since every caller hands a share Stats serves.
func attachedTag(js engine.JournalStats, id entity.ShareID) journal.ShareTag {
	for tag, sid := range js.TagShares {
		if sid == id {
			return tag
		}
	}
	panic("no attached share holds the id in TagShares")
}

// overwriteTwice writes mebibytes of 'a' then of 'b' over the same range of
// file id of share sh, so that many mebibytes are superseded once synced.
func overwriteTwice(
	t *testing.T,
	f *enginetest.Fixture,
	sh entity.ShareID,
	id entity.FileID,
	mebibytes int,
) {
	t.Helper()

	for _, c := range []byte{'a', 'b'} {
		for i := range mebibytes {
			p := bytes.Repeat([]byte{c}, mib)

			opts := engine.WriteOpts{Done: func() {}}

			if _, err := f.E.Write(t.Context(), sh, id, int64(i*mib), p, opts); err != nil {
				t.Fatalf("write %d of %c: %v", i, c, err)
			}
		}
	}
}

// RFC 8 §15.2's "scoped repack": share A at its limit with unreclaimed bytes
// in segments shared with B's data, and emptier segments of B elsewhere: A's
// write is served after the repack of A's scope, and B's emptier segments
// stay.
//
// The layout the scenario names is built, not hoped for, and it holds against
// the journal's own background retirement, which takes any segment the index
// holds nothing live in, whatever share its dead bytes belong to: every
// segment holding B's dead bytes also holds B's live data — a superseding
// phase over all but the first chunk leaves each one holding live bytes — so
// only a relief's own pass can lower B's figure, never the background
// reclamation. B's records go in first and end with oversized writes that
// leave the stream's active segment too full for one of A's chunks, so A's
// records roll on and no segment holding B's bytes takes one of A's —
// a share-scoped pass never selects a segment without its share's records. A
// writes the same partial-supersede shape as B, so its dead bytes sit in
// segments the pass can select and free for the write below. The setup fills
// whole segments and rolls the stream for each phase, and every roll takes a
// spare from the pool, which remakes its spares in real time: the pool is
// sized for every roll the setup and the write below can draw, since a roll
// that finds it empty is refused as capacity — and a spare refusal the
// engine relieves with a journal-wide repack, defeating the scoping this row
// asserts.
func TestRFC8A_CapacityScopedRepack(t *testing.T) {
	// pool is the spare segments every open keeps: enough for every roll
	// the setup draws — its writes fill a dozen segments — with margin for
	// the pool's own making to lag the rolls under load, and the spare the
	// write below rolls onto. A starved pool would let a spare refusal run
	// the journal-wide relief and hollow out the figures this row reads:
	// the pool rides far above the demand, and the settle loop below still
	// waits for it to be whole again before the write runs.
	const pool = 20

	f := enginetest.New(t, enginetest.WithSegmentCap(2*mib), enginetest.WithSpares(pool))
	limitedSecond(t, f, 5*mib)

	_, fb := f.File("b")
	fa := secondFile(t, f, "a")

	// A phase writes its n 512 KiB chunks from the first on: a superseding
	// phase that starts one chunk in leaves that chunk of the phase before
	// live, beside the dead bytes it supersedes, in the same segments.
	const chunk = 512 << 10
	phase := func(sh entity.ShareID, id entity.FileID, first, n int, c byte) {
		t.Helper()
		for i := first; i < first+n; i++ {
			if _, err := f.E.Write(
				t.Context(),
				sh,
				id,
				int64(i*chunk),
				bytes.Repeat([]byte{c}, chunk),
				engine.WriteOpts{Done: func() {}},
			); err != nil {
				t.Fatalf("write %d of %c: %v", i, c, err)
			}
		}
	}

	// B: a seeding phase that covers the first chunk and one that supersedes
	// all but it, then two fillers, each too big for the room any B-filled
	// segment can have left, so the segment the second one leaves active
	// has too little room for A's first chunk and A's records roll on from
	// B's.
	phase(f.Share.ID, fb, 0, 4, 'a')
	phase(f.Share.ID, fb, 1, 3, 'b')
	for i, c := range []byte{'y', 'z'} {
		if _, err := f.E.Write(
			t.Context(), f.Share.ID, fb, 4*mib+int64(i)*3*mib/2,
			bytes.Repeat([]byte{c}, 3*mib/2), engine.WriteOpts{Done: func() {}},
		); err != nil {
			t.Fatalf("write B's filler %c: %v", c, err)
		}
	}

	// A: the same two phases over one range, whose superseded bytes the
	// syncs below turn into the dead bytes in A's own segments its pass
	// can free.
	phase(f.Second.ID, fa, 0, 4, 'a')
	phase(f.Second.ID, fa, 1, 3, 'b')

	// The syncs the moves fire end both shares' windows, whose superseded
	// bytes land in the figures only once the sync timers — armed by the
	// journal's workers some time after the writes answer — have fired. The
	// clock moves, in steps the sync bound dwarfs, until both shares'
	// figures and the spare pool have settled: two rounds of moves apart
	// read the same bytes beyond zero and a whole pool, so the write below
	// finds a spare for its roll every time. A synced release the moves
	// land inside the write itself is a figure that rises after this loop
	// — a legal arrival the assertion below tolerates — never one it must
	// wait for: nothing may lower B's figure but a pass that runs inside
	// the write, whose scoping is what the row asserts.
	var wasB, wasA, wasSpares int64
	for deadline := time.Now().Add(bound); ; time.Sleep(10 * time.Millisecond) {
		f.Clock.Advance(5 * time.Second)
		nowB, nowA := unreclaimed(f, f.Share.ID), unreclaimed(f, f.Second.ID)
		nowSpares := int64(f.E.Stats().Journals[f.Dir].Stats.Spares)
		if nowB > 0 && nowA > 0 && nowB == wasB && nowA == wasA && nowSpares == wasSpares &&
			nowSpares == pool {
			break
		}
		wasB, wasA, wasSpares = nowB, nowA, nowSpares
		if time.Now().After(deadline) {
			t.Fatalf("the sync accounting both shares' superseded bytes: not within %v", bound)
		}
	}

	// A is at its limit — live data, dead bytes and this write together
	// cross it — and this write alone triggers the relief.
	before := unreclaimed(f, f.Share.ID)
	mustSecond(t, f, fa, 2*mib, 2*mib, 'c')

	// No run of the relief may lower B's figure: only a pass frees dead
	// bytes, a pass scoped to A cannot reach any segment without A's
	// records, and every one of B's holds both its dead bytes and its live
	// data. The journal's own background retirement is out of the candidates
	// the same way: it takes only what the index holds nothing live in. A
	// journal-wide pass is how B's segments, the leanest of the candidates,
	// would be freed first.
	if got := unreclaimed(f, f.Share.ID); got < before {
		t.Errorf("B's unreclaimed bytes after the write: %d, want no less than the %d it had before: the relief swept B's dead segments, the repack was journal-wide",
			got, before)
	}
}

// Two refusals race for the only unreclaimed segments, the first refusal's
// repack held at the journal's storage: the second answers ErrDelay, never
// ErrNoSpace, and is served once the hold is let go.
func TestRFC8A_CapacityRacingRefusals(t *testing.T) {
	// Room for four 1 MiB records: the overwrites fill it, the repack frees
	// the two superseded ones, and the two racing writes take their place.
	limit := 4 * recordBytes(t, mib)

	f := enginetest.New(t)
	limitedSecond(t, f, limit)

	fa := secondFile(t, f, "a")
	overwriteTwice(t, f, f.Second.ID, fa, 2)
	f.Clock.Advance(time.Minute)

	hold := f.Seam.HoldFirst(t, "remove", "*")
	first := make(chan error, 1)

	go func() {
		first <- putSecond(t, f, fa, 2*mib, mib, 'c')
	}()

	within(t, "the first repack reaching the storage seam", hold.Reached())

	err := putSecond(t, f, fa, 3*mib, mib, 'd')

	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
		t.Errorf(
			"the second refusal while the first's repack is held: %v, want ErrDelay(DelayCapacity)",
			err,
		)
	}

	hold.Release()

	if err := within(t, "the first write's answer", first); err != nil {
		t.Errorf("the first write once the hold is let go: %v, want served", err)
	}

	mustSecond(t, f, fa, 3*mib, mib, 'd')
}

// An overwrite at the share's limit left unsynced by the held sync timer: the
// share's unreclaimed bytes are 0 before the scope sync, and the write is served
// after the last step.
func TestRFC8A_CapacityOverwriteServedAfterTheLastStep(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 4*mib)

	fa := secondFile(t, f, "a")
	overwriteTwice(t, f, f.Second.ID, fa, 2)

	if got := unreclaimed(f, f.Second.ID); got != 0 {
		t.Fatalf("UnreclaimedBytes = %d before the scope sync, want 0", got)
	}

	mustSecond(t, f, fa, 2*mib, mib, 'c')
}

// A refusal the maintained figures can answer settles without running a
// repack pass: the share holds nothing but live, synced bytes, so the scope's
// sync frees nothing and no pass can relieve — the refusal answers no space
// straight from the figures, without appending one byte or removing one
// segment on the segmented backing, which is what a pass would do. This is
// the proof the at-once row's speed bound rests on: no pass, no seal in the
// caller's goroutine, no cycle.
func TestRFC8A_CapacityNoSpaceWithoutAPass(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 2*mib)

	fa := secondFile(t, f, "a")
	mustSecond(t, f, fa, 0, mib, 'k')

	// What a repack pass would touch on the segmented backing: the copies it
	// appends and the retirements it removes. The scope's own sync touches
	// neither, and the refused write is refused before its appends, so the
	// refusal's whole relief cannot move these counts without a pass.
	segCalls := func() (writes, removes int) {
		for id := 1; id <= 40; id++ {
			name := fmt.Sprintf("%020d.seg", id)
			writes += f.Seam.CallsFor("write", name)
			removes += f.Seam.CallsFor("remove", name)
		}
		return writes, removes
	}
	beforeW, beforeR := segCalls()

	if err := putSecond(t, f, fa, mib, mib, 'x'); !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a write at the limit with nothing to free: %v, want ErrNoSpace", err)
	}

	afterW, afterR := segCalls()
	if afterW != beforeW || afterR != beforeR {
		t.Errorf("the relief over nothing-to-free appended %d writes and removed %d segments over the segmented backing (before %d and %d), want both unchanged: the figures answer this refusal without a pass",
			afterW-beforeW, afterR-beforeR, beforeW, beforeR)
	}
	got := make([]byte, mib)
	if _, err := f.E.Read(t.Context(), f.Second.ID, fa, 0, got); err != nil || got[0] != 'k' {
		t.Errorf("the first write reads back %q, %v, want 'k'", got[:1], err)
	}
}

// The same with nothing left to free: no space, and the first write's bytes
// still read back.
func TestRFC8A_CapacityNothingLeftToFree(t *testing.T) {
	// Exactly the first write's record: the share is at its limit with it.
	limit := recordBytes(t, 2*mib)

	f := enginetest.New(t)
	limitedSecond(t, f, limit)

	fa := secondFile(t, f, "a")
	mustSecond(t, f, fa, 0, 2*mib, 'k')

	if err := putSecond(t, f, fa, 2*mib, mib, 'x'); !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a write at the limit with nothing to free: %v, want ErrNoSpace", err)
	}

	got := make([]byte, mib)
	if _, err := f.E.Read(t.Context(), f.Second.ID, fa, 0, got); err != nil || got[0] != 'k' {
		t.Errorf("the first write reads back %q, %v, want 'k'", got[:1], err)
	}
}

// The journal at its maximum with its unreclaimed bytes only in active segments
// after the sync: the write is served after the last step.
func TestRFC8A_CapacityJournalMaximumActiveSegmentsOnly(t *testing.T) {
	f := enginetest.New(t)

	_, id := f.File("a")
	n := int(maxBytes/mib) / 2

	overwriteTwice(t, f, f.Share.ID, id, n)
	write(t, f, id, int64(n*mib), bytes.Repeat([]byte{'c'}, mib))
}

// A refusal's pass held mid-pass on a failed segment whose window has resolved,
// then a second refusal: ErrDelay, served once the hold is let go.
func TestRFC8A_CapacityResolvedFailedSegmentCountsAsLeft(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 4*mib)

	fa := secondFile(t, f, "a")
	overwriteTwice(t, f, f.Second.ID, fa, 2)
	f.FailNextSync()
	f.Clock.Advance(time.Minute)

	hold := f.Seam.HoldFirst(t, "remove", "*")
	first := make(chan error, 1)

	go func() {
		first <- putSecond(t, f, fa, 2*mib, mib, 'c')
	}()

	within(t, "the first repack reaching the storage seam", hold.Reached())

	var d *vfserr.Delay
	if err := putSecond(t, f, fa, 3*mib, mib, 'd'); !errors.As(err, &d) {
		t.Errorf("the second refusal: %v, want ErrDelay", err)
	}

	hold.Release()

	if err := within(t, "the first write's answer", first); err != nil {
		t.Errorf("the first write: %v, want served", err)
	}
}

// A timer sync's failed window in a file of share A, then a refusal of A whose
// scope sync takes the window's report: the file's next Commit fails once and
// the one after succeeds.
func TestRFC8A_CapacityScopeSyncTakesAFailedWindow(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 4*mib)

	fa := secondFile(t, f, "a")
	overwriteTwice(t, f, f.Second.ID, fa, 2)
	f.FailNextWindow()
	f.Clock.Advance(time.Minute)

	mustSecond(t, f, fa, 2*mib, mib, 'c')

	if _, seq, err := f.E.Commit(t.Context(), f.Second.ID, fa, 0, 0); err != nil || seq != 1 {
		t.Errorf("the file's next Commit: sequence %d, %v, want the raised sequence 1", seq, err)
	}

	if _, seq, err := f.E.Commit(t.Context(), f.Second.ID, fa, 0, 0); err != nil || seq != 1 {
		t.Errorf("the Commit after: sequence %d, %v, want 1 again and no error", seq, err)
	}
}

// A refusal while a failed window is unresolved: the window is made in a
// segment that also holds share A's synced, superseded bytes, its writes all
// B's, and the seam holds the sync that resolves it. A, at its limit, is
// refused: its scope sync covers only A's files, so it does not wait on that
// sync. ErrDelay comes under a tenth of the caller's deadline, and the write is
// served once the window resolves.
func TestRFC8A_CapacityRefusalWhileAWindowIsUnresolved(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 4*mib)

	fa := secondFile(t, f, "a")
	overwriteTwice(t, f, f.Second.ID, fa, 2)
	f.Clock.Advance(time.Minute)

	_, fb := f.File("b")
	write(t, f, fb, 0, bytes.Repeat([]byte{'w'}, 4<<10))

	f.FailNextWindow()

	hold := f.Seam.Hold(t, "sync", "*")
	f.Clock.Advance(time.Minute)
	within(t, "the re-append's sync reaching the seam", hold.Reached())

	// The refused write parks the capacity answer behind the share seal's
	// caller-side fsync waiting on the seam, so it is given its own bounded
	// deadline: the bounded wait IS the assertion (the harness's allowance,
	// for a wait no injected clock can advance), failing on its own message
	// rather than letting a parked write hold the suite until its package
	// timeout. On the landed design the refusal answers ErrDelay long
	// before the deadline, inside the tenth of the caller's bound.
	start := time.Now()
	parked := make(chan error, 1)
	go func() { parked <- putSecond(t, f, fa, 2*mib, mib, 'c') }()
	var err error
	select {
	case err = <-parked:
	case <-time.After(bound / 10):
		// The held sync must be let go first, or the cleanup joins the
		// parked write and the suite hangs behind the row again.
		hold.Release()
		t.Fatal("the refused write is still parked on the seal's sync: the capacity answer waited on the caller's own fsync")
	}

	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
		t.Fatalf("a refusal over an unresolved window: %v, want ErrDelay(DelayCapacity)", err)
	}

	if took := time.Since(start); took > bound/10 {
		t.Errorf("the refusal took %v, want under %v: it waited on the sync", took, bound/10)
	}

	hold.Release()
	f.Clock.Advance(time.Minute)
	mustSecond(t, f, fa, 2*mib, mib, 'c')
}

// Small overwrites at the index bound left unsynced: the write is served after
// the last step's sync.
func TestRFC8A_CapacityIndexBoundServedAfterTheSync(t *testing.T) {
	f := enginetest.New(t)

	_, id := f.File("a")

	for range 64 {
		write(t, f, id, 0, bytes.Repeat([]byte{'s'}, 4<<10))
	}

	write(t, f, id, 0, bytes.Repeat([]byte{'t'}, 4<<10))
}

// One share at its limit with dirty bytes and another share's segments with
// superseded bytes: the repack frees space for a third share's write.
func TestRFC8A_CapacityRepackReserveServesAThirdShare(t *testing.T) {
	// Exactly the dirty write's record: the share is at its limit with it.
	limit := recordBytes(t, 2*mib)

	f := enginetest.New(t)
	limitedSecond(t, f, limit)

	fa := secondFile(t, f, "a")
	mustSecond(t, f, fa, 0, 2*mib, 'd')

	_, fb := f.File("b")
	overwriteTwice(t, f, f.Share.ID, fb, 2)
	f.Clock.Advance(time.Minute)

	_, fc := f.File("c")
	write(t, f, fc, 0, bytes.Repeat([]byte{'e'}, mib))
}
