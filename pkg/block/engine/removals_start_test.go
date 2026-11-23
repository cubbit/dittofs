package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const batchSpan = "engine.removal_batch"

// releaseKeys is the keys a released file whose removal is done holds until
// its prune: its removal (F‖id‖rm‖version), the removal's index entry
// (S‖id‖rr‖shard‖file‖version), its existence fence (fx) and offload fence
// (fo), and its version-floor entry (S‖id‖vf‖...).
const releaseKeys = 5

// cutCursor is where the first batch of the three-hole removal leaves its
// cursor (pin).
const cutCursor = 12288

// batchLog counts the removal batch spans that ended, so a test can wait on
// the loop having run an item.
type batchLog struct {
	ended chan struct{} // one value per batch span that ended
}

func newBatchLog() *batchLog {
	return &batchLog{
		ended: make(chan struct{}, 1<<15),
	}
}

func (g *batchLog) tracer(
	ctx context.Context,
	name string,
	_ ...slog.Attr,
) (context.Context, func(error)) {
	return ctx, func(error) {
		if name == batchSpan {
			g.ended <- struct{}{}
		}
	}
}

// waitBatches returns once n loop items have run to the end of their span.
func (g *batchLog) waitBatches(t *testing.T, n int) {
	t.Helper()

	for range n {
		select {
		case <-g.ended:
		case <-t.Context().Done():
			t.Fatal("the loop ran fewer items than the test waited for")
		}
	}
}

// drain forgets the spans that ended so far.
func (g *batchLog) drain() {
	for {
		select {
		case <-g.ended:
		default:
			return
		}
	}
}

// startContent is the store's content view recording each phase 2, prune and
// listing that reaches the store, and failing them when a test asks.
type startContent struct {
	engine.Content

	mu        sync.Mutex
	resumes   map[entity.FileID]int
	prunes    map[entity.FileID][]entity.JournalVersion
	listed    []entity.ShareID
	frozen    bool  // Resume and PruneRemovals answer an error and count nothing
	failPrune int   // the next PruneRemovals calls answer pruneErr, leaving the store as it was
	pruneErr  error // what a failing PruneRemovals answers; errPruneFailed when nil
	listErr   error // when set, the listing yields it and nothing else
	attempts  map[entity.FileID]int
}

func newStartContent() *startContent {
	return &startContent{
		resumes:  map[entity.FileID]int{},
		prunes:   map[entity.FileID][]entity.JournalVersion{},
		attempts: map[entity.FileID]int{},
	}
}

func (c *startContent) UnfinishedRemovals(
	ctx context.Context,
	share entity.ShareID,
	shard uint64,
) iter.Seq2[kvstore.UnfinishedRemoval, error] {
	c.mu.Lock()
	c.listed = append(c.listed, share)
	listErr := c.listErr
	c.mu.Unlock()

	if listErr != nil {
		return func(yield func(kvstore.UnfinishedRemoval, error) bool) {
			yield(kvstore.UnfinishedRemoval{}, listErr)
		}
	}

	return c.Content.UnfinishedRemovals(ctx, share, shard)
}

func (c *startContent) Resume(
	ctx context.Context,
	share entity.ShareID,
	file entity.FileID,
) (bool, error) {
	c.mu.Lock()
	frozen := c.frozen
	c.resumes[file]++

	if frozen {
		c.resumes[file]--
	}

	c.mu.Unlock()

	if frozen {
		return false, errFrozen
	}

	return c.Content.Resume(ctx, share, file)
}

func (c *startContent) PruneRemovals(
	ctx context.Context,
	share entity.ShareID,
	file entity.FileID,
	atOrBelow entity.JournalVersion,
) error {
	c.mu.Lock()
	frozen := c.frozen
	fail := !frozen && c.failPrune > 0

	if !frozen {
		c.attempts[file]++
	}

	if fail {
		c.failPrune--
	}

	c.mu.Unlock()

	if frozen {
		return errFrozen
	}

	if fail {
		if c.pruneErr != nil {
			return c.pruneErr
		}

		return errPruneFailed
	}

	if err := c.Content.PruneRemovals(ctx, share, file, atOrBelow); err != nil {
		return err
	}

	c.mu.Lock()
	c.prunes[file] = append(c.prunes[file], atOrBelow)
	c.mu.Unlock()

	return nil
}

var (
	errFrozen      = errors.New("the test froze the content view")
	errPruneFailed = errors.New("an injected prune failure")
)

// freeze makes the engine's phase 2 and prune fail without touching the store,
// as a process that has already stopped does.
func (c *startContent) freeze(on bool) {
	c.mu.Lock()
	c.frozen = on
	c.mu.Unlock()
}

// forget drops what the calls so far recorded.
func (c *startContent) forget() {
	c.mu.Lock()
	clear(c.resumes)
	clear(c.prunes)
	clear(c.attempts)
	clear(c.listed)
	c.listed = nil
	c.mu.Unlock()
}

func (c *startContent) resumed(f entity.FileID) int {
	c.mu.Lock()

	defer c.mu.Unlock()

	return c.resumes[f]
}

func (c *startContent) pruneAttempts(f entity.FileID) int {
	c.mu.Lock()

	defer c.mu.Unlock()

	return c.attempts[f]
}

func (c *startContent) resumedAny() int {
	c.mu.Lock()

	defer c.mu.Unlock()

	n := 0
	for _, k := range c.resumes {
		n += k
	}

	return n
}

func (c *startContent) prunedAt(f entity.FileID) []entity.JournalVersion {
	c.mu.Lock()

	defer c.mu.Unlock()

	return slices.Clone(c.prunes[f])
}

func (c *startContent) listedShares() []entity.ShareID {
	c.mu.Lock()

	defer c.mu.Unlock()

	return slices.Clone(c.listed)
}

// wantNoRuns checks that no removal run of any result has been counted: the
// loop is held, so no posted item can have run at all.
func (r *rig) wantNoRuns(t *testing.T, what string) {
	t.Helper()

	runs := r.e.Stats().RemovalRuns
	n := runs["advanced"] + runs["conflict"] + runs["done"] + runs["error"]
	if n != 0 {
		t.Errorf("%s: removal runs %v, want none", what, runs)
	}
}

// wantPosted checks that Start posted exactly n items to the removal loop,
// which has not been let run yet: the loop holds them all, so a missing post
// fails here and not as a wait that never ends.
func (r *rig) wantPosted(t *testing.T, n int) {
	t.Helper()

	if got := r.e.RemovalsPending(); got != n {
		t.Fatalf("Start posted %d items to the removal loop, want %d", got, n)
	}
}

// keysOf is every key in the store, in hex, that holds file f's ID, whatever
// it is under. A released file leaves five until its prune: its removal, the
// removal's index entry, its existence and offload fences, and its
// version-floor entry; the prune deletes all five.
func (r *rig) keysOf(t *testing.T, f entity.FileID) []string {
	t.Helper()

	var out []string

	err := r.db.View(t.Context(), func(rd kv.Reader) error {
		for row, err := range rd.Scan(nil, nil) {
			if err != nil {
				return err
			}

			if bytes.Contains(row.Key, f[:]) {
				out = append(out, fmt.Sprintf("%x", row.Key))
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// fileIn creates an empty file named name in share sh's root, granting the
// rig's admin the share first when it is not share one.
func (r *rig) fileIn(t *testing.T, sh engine.Share, name string) entity.FileID {
	t.Helper()

	ctx := t.Context()

	if sh.ID != r.fs.Share.ID {
		g := entity.ShareGrant{Share: sh.ID, Principal: r.fs.Admin.Principal, Access: entity.AccessAdmin}
		if _, err := r.fs.Store.ControlPlane().SetGrant(ctx, g, 0); err != nil && !errors.Is(err, entity.ErrGenerationMismatch) {
			t.Fatal(err)
		}
	}

	root, _, err := r.fs.Store.Files().Root(ctx, r.fs.Admin, sh.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, f := r.fs.Create(t, r.fs.Admin, root, name, kvstore.Regular(0o644))

	return f.ID
}

// sizeIn commits existence of a write of [0, size) for file f of share sh at
// version 1, as commitSize does for share one.
func (r *rig) sizeIn(t *testing.T, sh entity.ShareID, f entity.FileID, size int64) {
	t.Helper()

	pending := []kvstore.PendingExistence{{
		Share: sh,
		File:  f,
		Ops: []kvstore.PendingOp{{
			Version: entity.VersionOf(1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Length: size},
			Mtime:   committed,
			Ctime:   committed,
		}},
	}}

	failed, err := r.fs.Store.Existence().Commit(t.Context(), pending, nil)
	if err != nil || failed[f] != nil {
		t.Fatalf("commit size %d: %v, %v", size, err, failed[f])
	}
}

// truncateAt writes phase 1 of a truncate of file f of share sh to its
// length less 4096, at version v, through the store, as the write path does:
// the store then holds the truncate's removal record, not done.
func (r *rig) truncateAt(t *testing.T, sh entity.ShareID, f entity.FileID, v entity.JournalVersion) {
	t.Helper()

	size, err := r.fs.Store.Existence().Size(t.Context(), sh, f)
	if err != nil {
		t.Fatal(err)
	}

	op := kvstore.RemoveOp{
		Version: v,
		Kind:    entity.RemoveTruncate,
		Extent:  entity.Extent{Offset: size - 4096, Length: 4096},
		Time:    committed,
	}

	if err := r.fs.Store.Existence().Remove(t.Context(), sh, f, 0, op, nil); err != nil {
		t.Fatal(err)
	}
}

// removalIn creates a file of 8192 bytes in share sh and truncates it at
// version v through the store, leaving its removal record not done.
func (r *rig) removalIn(t *testing.T, sh engine.Share, name string, v entity.JournalVersion) entity.FileID {
	t.Helper()

	f := r.fileIn(t, sh, name)
	r.sizeIn(t, sh.ID, f, 8192)
	r.truncateAt(t, sh.ID, f, v)

	return f
}

// cloneInto writes phase 1 of a clone of [4096, 8192) of a new file of
// share src onto [8192, 12288) of file f of share one at version v, through
// the store, as the clone's phase 1 does: the store then holds the clone's
// removal record, not done, and a hold on the source. It returns the spec.
func (r *rig) cloneInto(t *testing.T, src engine.Share, f entity.FileID, v entity.JournalVersion) entity.CloneSpec {
	t.Helper()

	from := r.fileIn(t, src, "source of "+fname(f))
	r.sizeIn(t, src.ID, from, 8192)

	spec := entity.CloneSpec{
		SrcShare: src.ID,
		Src:      from,
		SrcOff:   4096,
		DstOff:   8192,
		Len:      4096,
		AsOf:     entity.VersionOf(1),
	}

	op := kvstore.CloneOp{Share: r.one.ID, File: f, Spec: spec, Version: v, Time: committed}
	if err := r.fs.Store.Existence().Clone(t.Context(), op, nil); err != nil {
		t.Fatal(err)
	}

	return spec
}

// sibling adds a share named name counting content in share one's
// namespaces, so the store takes a clone between the two, and returns it as
// the engine binds it. Start is not handed it unless a test hands it.
func (r *rig) sibling(t *testing.T, name string) engine.Share {
	t.Helper()

	one, _, err := r.fs.Store.ShareIndex().Share(t.Context(), r.one.ID)
	if err != nil {
		t.Fatal(err)
	}

	sh := entity.Share{
		ID:         entity.NewShareID(),
		Name:       name,
		Path:       "/" + name,
		Root:       entity.NewFileID(),
		Namespaces: one.Namespaces,
		Write:      one.Write,
	}

	if _, err := r.fs.Store.ControlPlane().PutShare(t.Context(), sh, 0); err != nil {
		t.Fatal(err)
	}

	return enginetest.ShareOf(t, r.fs.Store, sh.ID, 1<<30)
}

// listed is the unfinished removals the store lists for share one's shard.
func (r *rig) listed(t *testing.T) []kvstore.UnfinishedRemoval {
	t.Helper()

	var out []kvstore.UnfinishedRemoval

	for u, err := range r.fs.Store.Content().UnfinishedRemovals(t.Context(), r.one.ID, uint64(r.one.Shard)) {
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, u)
	}

	return out
}

// truncated creates a file of size bytes committed in the store and cuts it
// to size to in the journal only, as a crash after the journal step leaves it,
// and returns the version of the truncate.
func (r *rig) truncated(
	t *testing.T,
	name string,
	size, to int64,
) (entity.FileID, journal.Version) {
	t.Helper()

	_, f := r.newFile(t, name)
	r.commitSize(t, f, size)

	v, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), to)
	if err != nil {
		t.Fatal(err)
	}

	return f, v
}

// A truncate the crash cut off is applied by step 3, which writes its removal
// record, and step 4 lists the record the replay just wrote: the loop runs its
// phase 2 once and prunes it at its own version. A listing before the replay
// would miss it until the next start.
func TestStartAppliesTheReplayThenResumesTheRemovalItCreated(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	v2, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 4096)
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	got := r.file(t, h)

	r.wantRemovals(t, got, []kvstore.UnfinishedRemoval{{File: f, Version: jver(v2)}})

	// The loop is held: no posted item can have run at all, so no run is
	// counted yet; the metric moves only with the loop.
	r.wantNoRuns(t, "runs before the loop was let run")

	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)

	if n := r.content.resumed(f); n != 1 {
		t.Errorf("Resume ran %d times, want 1", n)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{jver(v2)}) {
		t.Errorf("pruned at %v, want exactly the truncate's version %v", p, jver(v2))
	}

	r.wantRemovals(t, got, nil)
}

// Ten thousand unfinished removals are posted and Start returns before the
// loop runs a batch: the batches are held at their span until the test lets
// them go, once Start has returned, and none had run by then. Then every file
// is resumed and pruned once, at its own version.
func TestStartPostsTenThousandRemovalsAndRunsNoBatchBeforeItReturns(t *testing.T) {
	const n = 10_000

	r := newRig(t)

	want := make(map[entity.FileID]entity.JournalVersion, n)

	for i := range n {
		v := entity.VersionOf(uint64(i + 2))
		want[r.removalIn(t, r.one, fmt.Sprintf("f%d", i), v)] = v
	}

	r.atRestart()

	// Start runs on its own goroutine so that one that waits for the loop,
	// which is held, ends the test with a message instead of never. The bound
	// is waitBound, on the failure path only: a Start that works returns at
	// once.
	started := make(chan error, 1)

	go func() {
		started <- r.start(t)
	}()

	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(waitBound):
		t.Fatalf(
			"Start did not return within %v with %d removals posted and the loop held",
			waitBound,
			n,
		)
	}

	if got := r.content.resumedAny(); got != 0 {
		t.Errorf("%d batches ran by the time Start returned, want 0", got)
	}

	// The loop is held until the test lets it run, so no posted item ran
	// before Start returned.
	r.wantNoRuns(t, "runs by the time Start returned")

	r.wantPosted(t, n)
	r.run()
	r.gate.waitBatches(t, n)

	for f, v := range want {
		if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{v}) {
			t.Fatalf("file %v pruned at %v, want exactly %v", f, p, v)
		}

		if k := r.content.resumed(f); k != 1 {
			t.Fatalf("file %v resumed %d times, want 1", f, k)
		}
	}
}

// A clone removal not done is handed to the rebuild with its record, and
// nothing is posted for its file: the clone's batches run only through the
// rebuild.
func TestAClonedRemovalIsHandedToTheHookAndNotPosted(t *testing.T) {
	r := newRig(t)
	f := r.newFile2(t, "dst")
	v := entity.VersionOf(7)
	spec := r.cloneInto(t, r.one, f, v)

	recs := r.listed(t)
	if len(recs) != 1 || recs[0].File != f || recs[0].Removal.Kind != entity.RemoveClone ||
		recs[0].Removal.Clone == nil || *recs[0].Removal.Clone != spec {
		t.Fatalf("setup: the store lists %+v, want the clone's one removal of spec %+v", recs, spec)
	}

	rec := recs[0].Removal

	type handed struct {
		share entity.ShareID
		file  entity.FileID
		rec   entity.Removal
	}

	var got []handed

	r.e.SetCloneRemovalHook(func(s entity.ShareID, file entity.FileID, m entity.Removal) {
		got = append(got, handed{s, file, m})
	})

	r.begin(t)
	r.wantPosted(t, 0)
	r.run()

	if len(got) != 1 || got[0].share != r.one.ID || got[0].file != f ||
		!reflect.DeepEqual(got[0].rec, rec) {
		t.Errorf(
			"clone removals handed %+v, want one: share %v, file %v, %+v",
			got,
			r.one.ID,
			f,
			rec,
		)
	}

	// Nothing was posted for the clone's file, so no run is counted for a
	// removal the rebuild alone runs.
	r.wantNoRuns(t, "runs for the clone's file")

	if p := r.content.prunedAt(f); len(p) != 0 {
		t.Errorf("the clone's file was pruned at %v, want never", p)
	}
}

// A clone removal that is done needs only its prune: it is posted as any done
// removal is, and the rebuild is not handed it.
func TestADoneCloneIsPrunedAndNotHanded(t *testing.T) {
	r := newRig(t)
	f := r.newFile2(t, "dst")
	v := entity.VersionOf(7)
	r.cloneInto(t, r.one, f, v)

	// The clone's phase 2 ran to its end and the process died before the
	// prune.
	if _, err := r.fs.Store.Content().Resume(t.Context(), r.one.ID, f); err != nil {
		t.Fatal(err)
	}

	if recs := r.listed(t); len(recs) != 1 || !recs[0].Removal.Done {
		t.Fatalf("setup: the store lists %+v, want the clone's one removal, done", recs)
	}

	handed := 0

	r.e.SetCloneRemovalHook(func(entity.ShareID, entity.FileID, entity.Removal) {
		handed++
	})

	r.begin(t)
	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)

	if handed != 0 {
		t.Errorf("a done clone was handed to the rebuild %d times, want 0", handed)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{v}) {
		t.Errorf("pruned at %v, want exactly %v", p, v)
	}

	if n := r.content.resumed(f); n != 0 {
		t.Errorf("Resume ran %d times for a done removal, want 0", n)
	}
}

// A clone removal whose source share this start does not serve fails Start,
// as the destination may not be served before the clone is done, and posts
// nothing.
func TestACloneRemovalOfAnUnservedSourceFailsStart(t *testing.T) {
	r := newRig(t)
	f := r.newFile2(t, "dst")
	r.cloneInto(t, r.sibling(t, "unserved"), f, entity.VersionOf(7))

	r.atRestart()

	if err := r.start(t); !errors.Is(err, engine.ErrSourceNotServed) {
		t.Errorf("Start = %v, want ErrSourceNotServed", err)
	}

	r.wantPosted(t, 0)
}

// failingKV is a kv.KV that lets a number of Updates through and then fails
// each one, as a store that stopped between two batches of a removal does.
type failingKV struct {
	kv.KV
	left atomic.Int64 // Updates still let through; negative: all of them
}

func (k *failingKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if k.left.Load() >= 0 && k.left.Add(-1) < 0 {
		k.left.Store(0)

		return errors.New("an injected store failure")
	}

	return k.KV.Update(ctx, fn)
}

// cursorsOf is the cursor of each unfinished removal of share one's shard.
func (r *rig) cursorsOf(t *testing.T) []int64 {
	t.Helper()

	var out []int64

	for u, err := range r.fs.Store.Content().UnfinishedRemovals(t.Context(), r.one.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, u.Removal.Cursor)
	}

	return out
}

// A removal crashed between two batches resumes from its cursor at the next
// start, the extent reads as removed before the loop runs and after, and the
// record is pruned. The store makes a batch of one hole, so the three holes
// of the file take three batches, and the second one fails.
func TestARemovalCrashedBetweenBatchesResumesAndReadsRemovedThroughout(t *testing.T) {
	db := &failingKV{}
	db.left.Store(-1)

	r := newRig(t, withStore(func(tb testing.TB) kv.KV {
		db.KV = limitKV{memKV(tb), 14}

		return db
	}))
	_, f := r.newFile(t, "a")
	r.commitSize(t, f, 24576)

	for i, off := range []int64{4096, 12288, 20480} {
		v := entity.VersionOf(uint64(2 + i))
		op := kvstore.RemoveOp{
			Version: v,
			Kind:    entity.RemoveDeallocate,
			Extent:  entity.Extent{Offset: off, Length: 4096},
			Time:    committed,
		}
		c := r.fs.Store.Content()

		if err := r.fs.Store.Existence().Remove(t.Context(), r.one.ID, f, 0, op, nil); err != nil {
			t.Fatal(err)
		}

		if _, err := c.Resume(t.Context(), r.one.ID, f); err != nil {
			t.Fatal(err)
		}

		if err := c.PruneRemovals(t.Context(), r.one.ID, f, v); err != nil {
			t.Fatal(err)
		}
	}

	v, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 2048)
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)
	r.wantPosted(t, 1)
	db.left.Store(1)
	r.run()
	r.gate.waitBatches(t, 1)

	if got := r.cursorsOf(t); !slices.Equal(got, []int64{cutCursor}) {
		t.Fatalf("cursor after the first batch %v, want [%d]", got, cutCursor)
	}

	// The process dies here: the second batch failed and nothing runs again.
	r.content.freeze(true)
	r.restart(t)
	r.content.forget()
	r.content.freeze(false)
	r.gate.drain()
	db.left.Store(-1)

	if err := r.start(t); err != nil {
		t.Fatal(err)
	}

	r.wantReadsRemoved(t, f)

	if got := r.cursorsOf(t); !slices.Equal(got, []int64{cutCursor}) {
		t.Fatalf("cursor at the restart %v, want the first batch's [%d]", got, cutCursor)
	}

	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)
	r.wantReadsRemoved(t, f)

	if n := r.content.resumed(f); n != 1 {
		t.Errorf("Resume ran %d times after the restart, want 1", n)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{jver(v)}) {
		t.Errorf("pruned at %v, want exactly %v", p, jver(v))
	}

	if got := r.cursorsOf(t); len(got) != 0 {
		t.Errorf("unfinished removals %v after the loop, want none", got)
	}
}

// wantReadsRemoved checks the file reads as the truncate left it: 2048 bytes
// not yet carved, and a hole from there on.
func (r *rig) wantReadsRemoved(t *testing.T, f entity.FileID) {
	t.Helper()

	sp, err := r.e.Allocation(t.Context(), r.one.ID, f, 0)
	if err != nil || sp.Offset != 0 || sp.Length != 2048 || sp.Class != entity.Uncarved {
		t.Errorf("Allocation(0) = %+v, %v, want [0, 2048) uncarved", sp, err)
	}

	sp, err = r.e.Allocation(t.Context(), r.one.ID, f, 4096)
	if err != nil || sp.Offset != 4096 || sp.Length != 0 || sp.Class != entity.Hole {
		t.Errorf("Allocation(4096) = %+v, %v, want a hole at 4096", sp, err)
	}
}

// A release whose last batch committed and whose prune did not is pruned at
// the next start, with no batch run: its record and its index entry go.
func TestARemovalDoneButNotPrunedIsPrunedAtRestart(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	ns := r.fs.Store.Namespace()

	if _, err := ns.Unlink(t.Context(), r.fs.Admin, r.fs.Root, []byte("a")); err != nil {
		t.Fatal(err)
	}

	v, err := r.jh.Delete(t.Context(), journal.NewFileID(f))
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	// The last batch committed and the process died before the prune.
	if _, err := r.fs.Store.Content().Resume(t.Context(), r.one.ID, f); err != nil {
		t.Fatal(err)
	}

	r.content.freeze(true)
	r.restart(t)
	r.content.forget()
	r.content.freeze(false)
	r.gate.drain()

	r.wantRemovals(
		t,
		entity.File{Shard: 0},
		[]kvstore.UnfinishedRemoval{{File: f, Version: jver(v)}},
	)

	if err := r.start(t); err != nil {
		t.Fatal(err)
	}

	if got := r.keysOf(t, f); len(got) != releaseKeys {
		t.Fatalf("the released file holds %d keys before its prune, want %d", len(got), releaseKeys)
	}

	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)

	if got := r.keysOf(t, f); len(got) != 0 {
		t.Errorf("the released file holds %d keys after its prune, want none: %q", len(got), got)
	}

	if n := r.content.resumed(f); n != 0 {
		t.Errorf("Resume ran %d times for a removal already done, want 0", n)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{jver(v)}) {
		t.Errorf("pruned at %v, want exactly %v", p, jver(v))
	}

	r.wantRemovals(t, entity.File{Shard: 0}, nil)
}

// A plain removal older than a clone not done, in one file, is left pending:
// nothing is posted for the file, the clone is handed to the rebuild, the
// store keeps both records not done, and a read of the file waits for the
// clone, as the store answers it while the clone is pending, rather than
// reading a removal half run.
func TestAPlainRemovalOlderThanAnUnfinishedCloneIsLeftPendingAndReadsStayCorrect(t *testing.T) {
	r := newRig(t)
	f := r.removalIn(t, r.one, "a", entity.VersionOf(2))
	cv := entity.VersionOf(3)
	r.cloneInto(t, r.one, f, cv)

	recs := r.listed(t)
	if len(recs) != 2 || recs[1].Removal.Kind != entity.RemoveClone {
		t.Fatalf("setup: the store lists %+v, want the truncate's removal and then the clone's", recs)
	}

	rec := recs[1].Removal

	var handed []entity.Removal

	r.e.SetCloneRemovalHook(func(_ entity.ShareID, _ entity.FileID, m entity.Removal) {
		handed = append(handed, m)
	})

	r.begin(t)
	r.wantPosted(t, 0)
	r.run()

	if len(handed) != 1 || !reflect.DeepEqual(handed[0], rec) {
		t.Errorf("clone removals handed %+v, want exactly %+v", handed, rec)
	}

	// The pending clone holds the file: nothing was posted, so no run is
	// counted for it.
	r.wantNoRuns(t, "runs for the file, which the pending clone holds")

	if p := r.content.prunedAt(f); len(p) != 0 {
		t.Errorf("the file was pruned at %v, want never", p)
	}

	r.wantRemovals(
		t,
		entity.File{Shard: 0},
		[]kvstore.UnfinishedRemoval{{File: f, Version: entity.VersionOf(2)}, {File: f, Version: cv}},
	)

	for _, u := range r.listed(t) {
		if u.Removal.Done {
			t.Errorf("the removal at %v is done, want both left pending", u.Version)
		}
	}

	for _, off := range []int64{0, 4096} {
		if _, err := r.e.Allocation(t.Context(), r.one.ID, f, off); !isDelay(err, vfserr.DelayClone) {
			t.Errorf("Allocation(%d) = %v, want ErrDelay(DelayClone) while the clone is pending", off, err)
		}
	}
}

// The removals of one file are one item: phase 2 once, then the prune at the
// newest version listed.
func TestOneFileWithManyRemovalsIsOneItemPrunedAtTheNewest(t *testing.T) {
	r := newRig(t)
	f := r.newFile2(t, "a")
	r.commitSize(t, f, 16384)

	// The first removal's phase 2 ran to its end; the two after it did not.
	r.truncateAt(t, r.one.ID, f, entity.VersionOf(2))

	if _, err := r.fs.Store.Content().Resume(t.Context(), r.one.ID, f); err != nil {
		t.Fatal(err)
	}

	r.truncateAt(t, r.one.ID, f, entity.VersionOf(3))
	r.truncateAt(t, r.one.ID, f, entity.VersionOf(4))

	if recs := r.listed(t); len(recs) != 3 || !recs[0].Removal.Done || recs[1].Removal.Done || recs[2].Removal.Done {
		t.Fatalf("setup: the store lists %+v, want three removals, the first alone done", recs)
	}

	r.begin(t)
	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)

	if n := r.content.resumed(f); n != 1 {
		t.Errorf("Resume ran %d times, want 1", n)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{entity.VersionOf(4)}) {
		t.Errorf("pruned at %v, want exactly one prune at version 4", p)
	}

	if n := r.e.RemovalsPending(); n != 0 {
		t.Errorf("%d items pending, want the file's one item done", n)
	}
}

// Both served shares are listed and their removals run; a share whose removal
// reached offloaded is not listed, as it has no binding to run them on.
func TestStartPostsTheRemovalsOfEveryServedShare(t *testing.T) {
	r := newRig(t)
	gone := enginetest.AddShare(t, r.fs.Store, "gone", 1<<30)
	v := entity.VersionOf(2)
	one := r.removalIn(t, r.one, "one", v)
	two := r.removalIn(t, r.two, "two", v)
	off := r.removalIn(t, gone, "off", v)
	removeShare(t, r.fs.Store, gone, entity.TagOffloaded)

	r.begin(t)
	r.wantPosted(t, 2)
	r.run()
	r.gate.waitBatches(t, 2)

	listed := r.content.listedShares()
	slices.SortFunc(listed, func(a, b entity.ShareID) int {
		return bytes.Compare(a[:], b[:])
	})

	want := []entity.ShareID{r.one.ID, r.two.ID}
	slices.SortFunc(want, func(a, b entity.ShareID) int {
		return bytes.Compare(a[:], b[:])
	})

	if !slices.Equal(listed, want) {
		t.Errorf("shares listed %v, want exactly %v", listed, want)
	}

	for _, f := range []entity.FileID{one, two} {
		if n := r.content.resumed(f); n != 1 {
			t.Errorf("file %v resumed %d times, want 1", f, n)
		}
	}

	if n := r.content.resumed(off); n != 0 {
		t.Errorf("the removed share's file resumed %d times, want 0", n)
	}
}

// A device the journal refused has no handles and no journal: step 4 lists
// nothing and Start still succeeds.
func TestARefusedDeviceListsNoRemovals(t *testing.T) {
	r := newRig(t)

	r.openErr = journal.ErrNoJournal
	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := r.content.listedShares(); len(got) != 0 {
		t.Errorf("shares listed %v, want none", got)
	}
}

// retryUntilBatch moves the engine's clock past the loop's backoff, again and
// again, until the loop runs one more item. The loop sets an item's backoff
// as it puts it back, so a move that came first is outrun by the next.
func (r *rig) retryUntilBatch(t *testing.T) {
	t.Helper()

	const attempts = 1_000_000

	for range attempts {
		select {
		case <-r.gate.ended:
			return
		default:
		}

		r.clock.Advance(time.Second)
		runtime.Gosched()
	}

	t.Fatal("the loop never ran the item again")
}

// A prune that fails does not fail Start, which has already answered; the
// removal stays pending, the run counts as an error, and the loop runs the
// item again after its backoff and prunes it, at the removal's own version.
func TestAPruneThatFailsAtStartLeavesTheRemovalAndIsRetried(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	ns := r.fs.Store.Namespace()

	if _, err := ns.Unlink(t.Context(), r.fs.Admin, r.fs.Root, []byte("a")); err != nil {
		t.Fatal(err)
	}

	v, err := r.jh.Delete(t.Context(), journal.NewFileID(f))
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	if _, err := r.fs.Store.Content().Resume(t.Context(), r.one.ID, f); err != nil {
		t.Fatal(err)
	}

	r.content.freeze(true)
	r.restart(t)
	r.content.forget()
	r.content.freeze(false)
	r.gate.drain()

	r.content.failPrune = 1

	if err := r.start(t); err != nil {
		t.Fatalf("Start = %v, want nil: the prune runs after Start", err)
	}

	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)

	if n := r.content.pruneAttempts(f); n != 1 {
		t.Errorf("%d prune attempts, want 1", n)
	}

	if got := r.e.Stats().RemovalRuns["error"]; got != 1 {
		t.Errorf("removal runs ended in error = %d, want 1", got)
	}

	if got := r.e.Stats().RemovalRuns["done"]; got != 0 {
		t.Errorf("removal runs done = %d, want 0", got)
	}

	if got := r.keysOf(t, f); len(got) != releaseKeys {
		t.Errorf("the file holds %d keys after the failed prune, want %d", len(got), releaseKeys)
	}

	r.wantRemovals(
		t,
		entity.File{Shard: 0},
		[]kvstore.UnfinishedRemoval{{File: f, Version: jver(v)}},
	)

	r.retryUntilBatch(t)

	if n := r.content.pruneAttempts(f); n != 2 {
		t.Errorf("%d prune attempts after the retry, want 2", n)
	}

	if p := r.content.prunedAt(f); !slices.Equal(p, []entity.JournalVersion{jver(v)}) {
		t.Errorf("pruned at %v, want exactly %v", p, jver(v))
	}

	if got := r.e.Stats().RemovalRuns["done"]; got != 1 {
		t.Errorf("removal runs done = %d after the retry, want 1", got)
	}

	r.wantRemovals(t, entity.File{Shard: 0}, nil)
}

// A listing that fails fails Start with that error and closes the journal,
// and posts nothing: serving on would leave the share's removals without
// their phase 2 until the next start.
func TestAListingErrorFailsStartAndClosesTheJournal(t *testing.T) {
	r := newRig(t)
	boom := errors.New("an injected listing failure")

	r.content.listErr = boom
	r.atRestart()

	if err := r.start(t); !errors.Is(err, boom) {
		t.Fatalf("Start = %v, want the listing failure", err)
	}

	if _, err := r.j.FakeJournal.Share(9, 0, journal.Version{}); !errors.Is(
		err,
		journal.ErrClosed,
	) {
		t.Errorf("Share on the journal after a failed Start = %v, want journal.ErrClosed", err)
	}

	r.wantPosted(t, 0)
}

// A clone removal that cannot be rebuilt fails Start whether its file is the
// last one listed or not: a file listed after it must not let Start go on.
func TestACloneRemovalFollowedByAnotherFileFailsStart(t *testing.T) {
	r := newRig(t)
	v := entity.VersionOf(7)

	// The index lists a shard's removals in file order: the clone's file is
	// the lesser, so the plain removal is listed after it.
	clone, plain := r.newFile2(t, "a"), r.newFile2(t, "b")
	if bytes.Compare(clone[:], plain[:]) > 0 {
		clone, plain = plain, clone
	}

	r.cloneInto(t, r.sibling(t, "unserved"), clone, v)
	r.sizeIn(t, r.one.ID, plain, 8192)
	r.truncateAt(t, r.one.ID, plain, v)

	if recs := r.listed(t); len(recs) != 2 || recs[0].File != clone || recs[1].File != plain {
		t.Fatalf("setup: the store lists %+v, want the clone's removal and then the plain one", recs)
	}

	r.atRestart()

	if err := r.start(t); !errors.Is(err, engine.ErrSourceNotServed) {
		t.Errorf("Start = %v, want ErrSourceNotServed", err)
	}

	r.wantPosted(t, 0)
}

// A prune that meets a store conflict is counted as a conflict, not an error,
// and is retried like any failed run (RFC 8 §14 labels a run done, advanced,
// conflict or error).
func TestAPruneConflictIsCountedAsAConflict(t *testing.T) {
	r := newRig(t)
	f := r.removalIn(t, r.one, "a", entity.VersionOf(7))

	// The removal's phase 2 ran to its end and the process died before the
	// prune.
	if _, err := r.fs.Store.Content().Resume(t.Context(), r.one.ID, f); err != nil {
		t.Fatal(err)
	}

	r.content.failPrune = 1
	r.content.pruneErr = kvstore.ErrConflict

	r.begin(t)
	r.wantPosted(t, 1)
	r.run()
	r.gate.waitBatches(t, 1)

	if got := r.e.Stats().RemovalRuns["conflict"]; got != 1 {
		t.Errorf("removal runs ended in conflict = %d, want 1", got)
	}

	if got := r.e.Stats().RemovalRuns["error"]; got != 0 {
		t.Errorf("removal runs ended in error = %d, want 0", got)
	}

	if got := r.e.Stats().RemovalRuns["done"]; got != 0 {
		t.Errorf("removal runs done = %d, want 0", got)
	}
}
