//go:build engine_conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
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
// 9·10⁸. With no offload or eviction in this part, a read proves nothing.
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
// the device, raises journal_lost and creates no journal. The operator's
// acknowledgement that serves them again is not built here.
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

	if _, err := f.E.Write(ctx, f.Second.ID, entity.NewFileID(), 0, []byte("second"), opts); err != nil {
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
// to it with its first version above the limit it had. The rewrite of the
// shards' journal identity and a share whose removal reads offloaded are not
// built here.
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

// TestRFC8A_VersionLimitRaise is the raise halves of the version limit row:
// every step between the raise's start and its crash, at a block of 16, with
// the store blocking or failing the raise's commit and failing a Bind's limit
// write. It stays red until the conformance package can set the block, which
// only the engine's own tests can today.
func TestRFC8A_VersionLimitRaise(t *testing.T) {
	enginetest.New(t)

	t.Fatal("the raise halves need a block of 16, which this package cannot set yet")
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
// condition clears.
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

	f.RefuseOpens(false)
	eventually(t, "the retry's timer is set", func() bool {
		return f.Clock.Pending() == 1
	})
	f.Clock.Advance(30 * time.Second)

	eventually(t, "the share attaches", func() bool {
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

// unreclaimed is the journal's figures for share id: its unreclaimed bytes.
func unreclaimed(f *enginetest.Fixture, id entity.ShareID) int64 {
	return f.E.Stats().Journals[f.Dir].Shares[id].UnreclaimedBytes
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

// RFC 8 §15.2's "scoped repack": share A at its limit with unreclaimed bytes in
// segments shared with B's data, and emptier segments of B elsewhere: A's write
// is served after the repack of A's scope, and B's emptier segments stay.
func TestRFC8A_CapacityScopedRepack(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 8*mib)

	_, fb := f.File("b")
	fa := secondFile(t, f, "a")

	overwriteTwice(t, f, f.Share.ID, fb, 2)
	overwriteTwice(t, f, f.Second.ID, fa, 4)
	f.Clock.Advance(time.Minute)

	mustSecond(t, f, fa, 4*mib, mib, 'c')

	if unreclaimed(f, f.Share.ID) == 0 {
		t.Error("B's unreclaimed bytes are gone: the repack was journal-wide")
	}
}

// Two refusals race for the only unreclaimed segments, the first refusal's
// repack held at the journal's storage: the second answers ErrDelay, never
// ErrNoSpace, and is served once the hold is let go.
func TestRFC8A_CapacityRacingRefusals(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 4*mib)

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

// The same with nothing left to free: no space, and the first write's bytes
// still read back.
func TestRFC8A_CapacityNothingLeftToFree(t *testing.T) {
	f := enginetest.New(t)
	limitedSecond(t, f, 2*mib)

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

	// the deadline is real time, since the caller's context cannot
	// end on the test clock; the assertion is that the answer does not wait for it.
	start := time.Now()
	err := putSecond(t, f, fa, 2*mib, mib, 'c')

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
	f := enginetest.New(t)
	limitedSecond(t, f, 2*mib)

	fa := secondFile(t, f, "a")
	mustSecond(t, f, fa, 0, 2*mib, 'd')

	_, fb := f.File("b")
	overwriteTwice(t, f, f.Share.ID, fb, 2)
	f.Clock.Advance(time.Minute)

	_, fc := f.File("c")
	write(t, f, fc, 0, bytes.Repeat([]byte{'e'}, mib))
}
