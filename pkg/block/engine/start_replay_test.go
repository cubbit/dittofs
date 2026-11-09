package engine_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// The times the replay tests use, so every assertion on a time is exact: the
// restart's clock is after any time the store stamps at creation, and a
// write's modification time falls between the two.
var (
	restartAt  = time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC)
	writeMtime = time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC)
	otherMtime = time.Date(2031, 7, 1, 0, 0, 0, 0, time.UTC)
	committed  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// the messages start step 3 logs.
const (
	abortedReleaseMessage = "a release aborted by a holder before the restart: " +
		"content not offloaded reads lost"
	unappliedMessage = "removal ended unapplied, its journal marker settled"
)

// fname names file f in the call log.
func fname(f entity.FileID) string {
	return hex.EncodeToString(f[:])
}

// vname names version v in the call log.
func vname(v journal.Version) string {
	return strconv.FormatUint(v.Counter, 10)
}

// settleCall is the call log's entry for a Settle of f through v.
func settleCall(f entity.FileID, v journal.Version) string {
	return fmt.Sprintf("settle %s %s", fname(f), vname(v))
}

// logExistence wraps the store's existence view: it logs each Commit and
// Remove in the journal's call log, keeps what they were handed, and holds or
// fails them when a test asks.
type logExistence struct {
	engine.Existence
	j *logJournal

	mu        sync.Mutex
	commits   [][]kvstore.PendingOp
	removes   []kvstore.RemoveOp
	commitErr error
	failedOf  map[entity.FileID]error // files the Commit answers as failed, writing nothing
	failAt    int                     // the Commit call that fails, from 1; 0 for all
	calls     int
	removeErr error
	gate      *gate
	before    func()
	after     func()
}

func (x *logExistence) Commit(
	ctx context.Context,
	pending []kvstore.PendingExistence,
	marks *kvstore.MarksWrite,
) (map[entity.FileID]error, error) {
	x.mu.Lock()

	for _, p := range pending {
		_ = x.j.add("commit " + fname(p.File))
		x.commits = append(x.commits, slices.Clone(p.Ops))
	}

	x.calls++

	g, err, before, after := x.gate, x.commitErr, x.before, x.after
	x.gate = nil

	if x.failAt != 0 && x.failAt != x.calls {
		err = nil
	}

	x.mu.Unlock()

	if g != nil {
		close(g.entered)
		<-g.release
	}

	if err != nil {
		return nil, err
	}

	if len(x.failedOf) > 0 {
		return x.failedOf, nil
	}

	if before != nil {
		before()
	}

	failed, err := x.Existence.Commit(ctx, pending, marks)

	if after != nil {
		after()
	}

	return failed, err
}

func (x *logExistence) Remove(
	ctx context.Context,
	share entity.ShareID,
	file entity.FileID,
	epoch uint64,
	op kvstore.RemoveOp,
	pending []kvstore.PendingOp,
) error {
	_ = x.j.add("remove " + fname(file))

	x.mu.Lock()
	x.removes = append(x.removes, op)
	err := x.removeErr
	x.mu.Unlock()

	if err != nil {
		return err
	}

	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

func (x *logExistence) setCommitErr(err error) {
	x.mu.Lock()
	x.commitErr = err
	x.mu.Unlock()
}

func (x *logExistence) committedOps() [][]kvstore.PendingOp {
	x.mu.Lock()

	defer x.mu.Unlock()

	return slices.Clone(x.commits)
}

// Sync, Since and Settle log the calls of a start's replay, and fail those a
// test arms in the journal's fail map.
func (h *logHandle) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	for _, id := range ids {
		if err := h.j.add("sync " + fname(entity.FileID(id.Bytes()))); err != nil {
			return nil, err
		}
	}

	return h.Handle.Sync(ctx, ids...)
}

func (h *logHandle) Since(
	ctx context.Context,
	id journal.FileID,
	from journal.Version,
) iter.Seq2[journal.Change, error] {
	if err := h.j.add("since " + fname(entity.FileID(id.Bytes()))); err != nil {
		return func(yield func(journal.Change, error) bool) {
			yield(journal.Change{}, err)
		}
	}

	return h.Handle.Since(ctx, id, from)
}

func (h *logHandle) Settle(id journal.FileID, v journal.Version) {
	_ = h.j.add(settleCall(entity.FileID(id.Bytes()), v))
	h.Handle.Settle(id, v)
}

// kvPerTx is a kv.KV over a counting one that keeps the counts of each
// Update on its own, in order.
type kvPerTx struct {
	kv.KV
	counting *kv.Counting

	mu  sync.Mutex
	txs []kv.UpdateCounts
}

func (k *kvPerTx) Update(ctx context.Context, fn func(kv.Txn) error) error {
	err := k.KV.Update(ctx, fn)
	u := k.counting.TakeUpdates()

	k.mu.Lock()
	k.txs = append(k.txs, u)
	k.mu.Unlock()

	return err
}

func (k *kvPerTx) count() int {
	k.mu.Lock()

	defer k.mu.Unlock()

	return len(k.txs)
}

func (k *kvPerTx) since(n int) []kv.UpdateCounts {
	k.mu.Lock()

	defer k.mu.Unlock()

	return slices.Clone(k.txs[n:])
}

// rig is a starter whose journal the test fills before Start, as a crash
// leaves it, and whose existence view it can watch.
type rig struct {
	*starter
	ex *logExistence
	jh engine.Handle // a handle on share one's tag, writing the journal as the crashed process did
}

func newRig(t *testing.T, opts ...starterOption) *rig {
	t.Helper()

	x := &logExistence{}

	opts = append(opts, withStartDeps(func(d *engine.Deps) {
		x.Existence = d.Existence
		d.Existence = x
	}))

	s := newStarter(t, opts...)
	x.j = s.j

	if err := s.j.FakeJournal.SetVersionLimit(journal.Version{Counter: 1 << 40}); err != nil {
		t.Fatal(err)
	}

	r := &rig{
		starter: s,
		ex:      x,
	}
	r.jh = r.handle(t, s.one.Tag)

	return r
}

// handle is a handle on tag in the fake journal, above the versions the
// tests commit to the store.
func (r *rig) handle(t *testing.T, tag journal.ShareTag) engine.Handle {
	t.Helper()

	h, err := r.j.FakeJournal.Share(tag, 0, journal.Version{Counter: 100})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// newFile creates an empty file in share one's root.
func (r *rig) newFile(t *testing.T, name string) (entity.Handle, entity.FileID) {
	t.Helper()

	h, f := r.fs.Create(t, r.fs.Admin, r.fs.Root, name, kvstore.Regular(0o644))

	return h, f.ID
}

// commitSize commits existence of a write of [0, size) for file f of share
// one at version 1, below every version the journal draws, as a start that
// found the file at that size would.
func (r *rig) commitSize(t *testing.T, f entity.FileID, size int64) {
	t.Helper()

	pending := []kvstore.PendingExistence{
		{
			Share: r.one.ID,
			File:  f,
			Ops: []kvstore.PendingOp{
				{
					Version: entity.VersionOf(1),
					Kind:    kvstore.OpWrite,
					Extent:  entity.Extent{Length: size},
					Mtime:   committed,
					Ctime:   committed,
				},
			},
		},
	}

	failed, err := r.fs.Store.Existence().Commit(t.Context(), pending, nil)
	if err != nil || failed[f] != nil {
		t.Fatalf("commit size %d: %v, %v", size, err, failed[f])
	}
}

// write puts n bytes of 'a' at off of f in the journal, with the mtime, and
// returns the version.
func (r *rig) write(t *testing.T, f entity.FileID, off, n int64, mtime time.Time) journal.Version {
	t.Helper()

	return r.writeOn(t, r.jh, f, off, n, mtime)
}

func (r *rig) writeOn(
	t *testing.T,
	h engine.Handle,
	f entity.FileID,
	off, n int64,
	mtime time.Time,
) journal.Version {
	t.Helper()

	v, err := h.WriteAt(t.Context(), journal.NewFileID(f), off, fill(int(n), 'a'), mtime.UnixNano())
	if err != nil {
		t.Fatal(err)
	}

	return v
}

// file is the committed file.
func (r *rig) file(t *testing.T, h entity.Handle) entity.File {
	t.Helper()

	f, err := r.fs.Store.Files().Get(t.Context(), r.fs.Admin, h)
	if err != nil {
		t.Fatal(err)
	}

	return f
}

func (r *rig) applied(t *testing.T, f entity.FileID) entity.JournalVersion {
	t.Helper()

	v, err := r.fs.Store.Existence().Applied(t.Context(), r.one.ID, f)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func (r *rig) usage(t *testing.T) entity.Usage {
	t.Helper()

	u, err := r.fs.Store.Capacity().Usage(t.Context(), r.one.ID)
	if err != nil {
		t.Fatal(err)
	}

	return u
}

// atRestart sets the engine's clock to restartAt, once the files are created.
func (r *rig) atRestart() {
	r.clock.Jump(restartAt.Sub(r.clock.Now()))
}

// begin starts the engine at restartAt and fails the test if it does not.
func (r *rig) begin(t *testing.T) {
	t.Helper()

	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// restart stops the running engine's background work, reopens the journal as
// a new process finds it, and builds a new engine over the same store.
func (r *rig) restart(t *testing.T) {
	t.Helper()

	r.e.CloseGroupCommits()
	r.j.Reopen()

	e, err := engine.New(r.deps)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(e.CloseGroupCommits)

	if err := e.Apply(enginetest.Settings); err != nil {
		t.Fatal(err)
	}

	r.e = e
}

// calls is the journal's call log from mark on, keeping the calls whose
// first word is one of kinds.
func (r *rig) calls(mark int, kinds ...string) []string {
	var out []string

	for _, c := range r.j.log()[mark:] {
		if slices.Contains(kinds, strings.Fields(c)[0]) {
			out = append(out, c)
		}
	}

	return out
}

func (r *rig) settles(mark int) []string {
	return r.calls(mark, "settle")
}

// logged is the records the engine logged at Warn or above.
func (r *rig) logged() []slog.Record {
	var out []slog.Record

	for _, rec := range r.logs.records() {
		if rec.Level >= slog.LevelWarn {
			out = append(out, rec)
		}
	}

	return out
}

// replayAttr is the value of the attribute name of rec.
func replayAttr(rec slog.Record, name string) any {
	var v any

	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == name {
			v = a.Value.Any()

			return false
		}

		return true
	})

	return v
}

func jver(v journal.Version) entity.JournalVersion {
	return entity.JournalVersion(v.Bytes())
}

// sameOp reports each field of got that differs from want.
func sameOp(t *testing.T, i int, got, want kvstore.PendingOp) {
	t.Helper()

	if got.Version != want.Version || got.Kind != want.Kind ||
		got.Extent != want.Extent || got.Size != want.Size {
		t.Errorf(
			"op %d = %+v, want version %v kind %v extent %v size %d",
			i, got, want.Version, want.Kind, want.Extent, want.Size,
		)
	}

	if !got.Mtime.Equal(want.Mtime) {
		t.Errorf("op %d Mtime = %v, want %v", i, got.Mtime, want.Mtime)
	}

	if !got.Ctime.Equal(want.Ctime) {
		t.Errorf("op %d Ctime = %v, want %v", i, got.Ctime, want.Ctime)
	}

	if !slices.Equal(got.Over, want.Over) {
		t.Errorf("op %d Over = %v, want %v", i, got.Over, want.Over)
	}

	if !slices.Equal(got.Lost, want.Lost) {
		t.Errorf("op %d Lost = %v, want %v", i, got.Lost, want.Lost)
	}
}

func sameOps(t *testing.T, got, want []kvstore.PendingOp) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%d ops committed %+v, want %d", len(got), got, len(want))
	}

	for i := range want {
		sameOp(t, i, got[i], want[i])
	}
}

func wantNoLoud(t *testing.T, r *rig) {
	t.Helper()

	if got := r.logged(); len(got) != 0 {
		t.Errorf("logged %d records at Warn or above: %v, want none", len(got), got)
	}
}

// A write acknowledged and synced but not committed when the process died is
// committed before the file is served, with the size and the modification
// time its record carries, and the journal is settled through it.
func TestStartReplaysAWriteWithItsMtime(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	before := r.file(t, h)
	v1 := r.write(t, f, 0, 4096, writeMtime)

	r.begin(t)

	got := r.file(t, h)

	if got.Size != 4096 || got.Charged != 4096 {
		t.Errorf("size %d, charged %d, want 4096 and 4096", got.Size, got.Charged)
	}

	if !got.Modify.Equal(writeMtime) {
		t.Errorf(
			"Modify = %v, want the record's %v (it was %v)",
			got.Modify, writeMtime, before.Modify,
		)
	}

	if !got.Change.Equal(restartAt) {
		t.Errorf("Change = %v, want the restart's %v", got.Change, restartAt)
	}

	if a := r.applied(t, f); a != jver(v1) {
		t.Errorf("applied = %v, want %v", a, jver(v1))
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v1)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v1))
	}
}

// A write, a truncate to the size the write left and a write past it replay
// as the journal ordered them: the file ends 12 KiB long with a hole between.
// The truncate masks nothing, so it is a grow in the one commit, as it is on
// the live path.
func TestStartReplaysInVersionOrder(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	id := journal.NewFileID(f)

	r.write(t, f, 0, 4096, writeMtime)

	if _, err := r.jh.Truncate(t.Context(), id, 4096); err != nil {
		t.Fatal(err)
	}

	v3 := r.write(t, f, 8192, 4096, writeMtime)

	r.begin(t)

	got := r.file(t, h)

	if got.Size != 12288 || got.Charged != 8192 {
		t.Errorf("size %d, charged %d, want 12288 and 8192", got.Size, got.Charged)
	}

	sp, err := r.e.Allocation(t.Context(), r.one.ID, f, 4096)
	if err != nil || sp.Class != entity.Hole || sp.Offset != 4096 || sp.Length != 4096 {
		t.Errorf("Allocation(4096) = %+v, %v, want the hole [4096, 8192)", sp, err)
	}

	if a := r.applied(t, f); a != jver(v3) {
		t.Errorf("applied = %v, want %v", a, jver(v3))
	}

	want := []string{"commit " + fname(f)}

	if c := r.calls(0, "commit", "remove"); !slices.Equal(c, want) {
		t.Errorf("calls %v, want %v", c, want)
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v3)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v3))
	}
}

// A truncate the crash cut off before its removal record is applied: the
// size is the truncate's and phase 1's removal record exists for the loop.
func TestStartAppliesATruncateTheCrashCut(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	v2, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 4096)
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	got := r.file(t, h)

	if got.Size != 4096 {
		t.Errorf("size %d, want 4096", got.Size)
	}

	if a := r.applied(t, f); a != jver(v2) {
		t.Errorf("applied = %v, want %v", a, jver(v2))
	}

	n := fname(f)
	want := []string{"since " + n, "sync " + n, "remove " + n, settleCall(f, v2)}

	if c := r.calls(0, "since", "sync", "commit", "remove", "settle"); !slices.Equal(c, want) {
		t.Errorf("calls %v, want %v", c, want)
	}

	r.wantRemovals(t, got, []kvstore.UnfinishedRemoval{{File: f, Version: jver(v2)}})

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}
}

// wantRemovals checks the removal records still to finish for the file's
// shard.
func (r *rig) wantRemovals(t *testing.T, f entity.File, want []kvstore.UnfinishedRemoval) {
	t.Helper()

	var got []kvstore.UnfinishedRemoval

	for u, err := range r.fs.Store.Content().UnfinishedRemovals(t.Context(), r.one.ID, f.Shard) {
		if err != nil {
			t.Fatal(err)
		}

		got = append(got, u)
	}

	if !slices.Equal(got, want) {
		t.Errorf("unfinished removals %v, want %v", got, want)
	}
}

// A deallocate the crash cut off is applied as a hole of its own extent.
func TestStartAppliesADeallocateTheCrashCut(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 12288)

	x := journal.Extent{Off: 4096, Len: 4096}

	v2, err := r.jh.Deallocate(t.Context(), journal.NewFileID(f), x)
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	got := r.file(t, h)

	if got.Size != 12288 || got.Charged != 8192 {
		t.Errorf("size %d, charged %d, want 12288 and 8192", got.Size, got.Charged)
	}

	sp, err := r.e.Allocation(t.Context(), r.one.ID, f, 4096)
	if err != nil || sp.Class != entity.Hole || sp.Offset != 4096 || sp.Length != 4096 {
		t.Errorf("Allocation(4096) = %+v, %v, want the hole [4096, 8192)", sp, err)
	}

	r.wantRemovals(t, got, []kvstore.UnfinishedRemoval{{File: f, Version: jver(v2)}})

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}
}

// A write overwritten in its middle comes back from the journal as pieces of
// one version: the replay commits it as one write, and hands the store the
// runs the later write supersedes.
func TestStartMergesThePiecesOfOneWrite(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 12288, writeMtime)
	v2 := r.write(t, f, 4096, 4096, otherMtime)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 12288},
			Mtime:   writeMtime,
			Ctime:   restartAt,
		},
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 4096},
			Mtime:   otherMtime,
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 4096, Length: 4096}},
		},
	})

	got := r.file(t, h)

	if got.Size != 12288 || got.Charged != 12288 {
		t.Errorf("size %d, charged %d, want 12288 and 12288", got.Size, got.Charged)
	}

	if a := r.applied(t, f); a != jver(v2) {
		t.Errorf("applied = %v, want %v", a, jver(v2))
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}
}

// A write the journal lost in its middle after its sync is one write with a
// lost run: the file has its size, charges only the bytes kept, and the lost
// run reads as lost.
func TestStartCommitsAPartialLossInTheMiddleOfAWrite(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 12288, writeMtime)
	r.j.Lose(journal.NewFileID(f), journal.Extent{Off: 4096, Len: 4096}, journal.LossCorrupt)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 12288},
			Mtime:   writeMtime,
			Ctime:   restartAt,
			Lost:    []entity.Extent{{Offset: 4096, Length: 4096}},
		},
	})

	got := r.file(t, h)

	if got.Size != 12288 || got.Charged != 8192 {
		t.Errorf("size %d, charged %d, want 12288 and 8192", got.Size, got.Charged)
	}

	dst := make([]byte, 4096)

	if _, err := r.e.Read(t.Context(), r.one.ID, f, 4096, dst); !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("Read of the lost run = %v, want ErrLost", err)
	}

	n, err := r.e.Read(t.Context(), r.one.ID, f, 0, dst)
	if err != nil || n != 4096 || !bytes.Equal(dst, fill(4096, 'a')) {
		t.Errorf("Read of the first run = %d, %v, %q..., want the bytes written", n, err, dst[:4])
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v1)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v1))
	}
}

// The usage the replayed writes add is recorded: the share is charged the
// bytes re-applied, exactly.
func TestStartRecordsTheUsageItReplays(t *testing.T) {
	r := newRig(t)
	r.newFile(t, "a")
	_, f := r.newFile(t, "b")
	before := r.usage(t)

	r.write(t, f, 0, 4096, writeMtime)
	r.write(t, f, 4096, 4096, writeMtime)

	r.begin(t)

	got := r.usage(t)

	if got.Bytes != before.Bytes+8192 || got.Files != before.Files {
		t.Errorf("usage %+v, want %d bytes and %d files", got, before.Bytes+8192, before.Files)
	}
}

// A second restart over the same store applies nothing: the files' applied
// versions are above everything the journal yields.
func TestASecondStartReplaysNothing(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	id := journal.NewFileID(f)

	r.write(t, f, 0, 4096, writeMtime)

	if _, err := r.jh.Truncate(t.Context(), id, 4096); err != nil {
		t.Fatal(err)
	}

	r.write(t, f, 8192, 4096, writeMtime)

	r.begin(t)

	first := r.file(t, h)

	if first.Size != 12288 {
		t.Fatalf("size %d after the first start, want 12288", first.Size)
	}

	usage := r.usage(t)
	mark := len(r.j.log())

	r.restart(t)

	if err := r.start(t); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if c := r.calls(mark, "commit", "remove"); len(c) != 0 {
		t.Errorf("the second start made %v, want nothing", c)
	}

	// Settle is not persisted, so the markers recovery brings back at or
	// below applied are settled again, and nothing above it.
	want := []string{settleCall(f, journal.VersionFromBytes(first.Applied))}

	if s := r.settles(mark); !slices.Equal(s, want) {
		t.Errorf("the second start settled %v, want %v", s, want)
	}

	if got := r.usage(t); got != usage {
		t.Errorf("usage after the second start %+v, want %+v", got, usage)
	}

	if got := r.file(t, h); got.Size != first.Size || got.Applied != first.Applied {
		t.Errorf(
			"file after the second start %+v, want size %d applied %v",
			got, first.Size, first.Applied,
		)
	}

	wantNoLoud(t, r)
}

// A replay with more operations than one transaction holds commits in
// several, as the store splits it by its key budget, handed over as one
// ordered batch.
func TestStartCommitsAReplayInTransactionsWithinK(t *testing.T) {
	var per *kvPerTx

	r := newRig(t, withStore(func(tb testing.TB) kv.KV {
		c := &kv.Counting{
			KV: limitKV{
				KV:      memKV(tb),
				entries: 12,
			},
		}
		per = &kvPerTx{
			KV:       c,
			counting: c,
		}

		return per
	}))
	h, f := r.newFile(t, "a")

	var last journal.Version

	for i := range int64(11) {
		last = r.write(t, f, 8192*i, 4096, writeMtime)
	}

	var first, end int

	r.ex.before = func() {
		first = per.count()
	}
	r.ex.after = func() {
		end = per.count()
	}

	r.begin(t)

	txs := per.since(first)[:end-first]

	if len(txs) != 5 {
		t.Fatalf("the replay's Commit took %d transactions, want 5", len(txs))
	}

	wantWrites := []int{7, 6, 6, 6, 6}

	for i, u := range txs {
		if u.Updates != 1 || u.MostWrites != wantWrites[i] {
			t.Errorf("transaction %d: %+v, want one update writing %d keys", i+1, u, wantWrites[i])
		}

		if u.MostWrites > 12 {
			t.Errorf("transaction %d wrote %d keys, the backend holds 12", i+1, u.MostWrites)
		}
	}

	if n := len(r.ex.committedOps()); n != 1 {
		t.Errorf("%d Commit calls, want 1", n)
	}

	if got := r.file(t, h); got.Size != 86016 {
		t.Errorf("size %d, want 86016", got.Size)
	}

	if a := r.applied(t, f); a != jver(last) {
		t.Errorf("applied = %v, want %v", a, jver(last))
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, last)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, last))
	}
}

// The file is synced in the journal, then its existence commits, then the
// journal is settled: in that order.
func TestStartSyncsThenCommitsThenSettles(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 4096, writeMtime)

	r.begin(t)

	n := fname(f)
	want := []string{"since " + n, "sync " + n, "commit " + n, settleCall(f, v1)}

	if c := r.calls(0, "since", "sync", "commit", "remove", "settle"); !slices.Equal(c, want) {
		t.Errorf("calls %v, want %v", c, want)
	}
}

// A commit that fails fails Start and settles nothing; the next start yields
// the write again and commits it.
func TestAFailedReplayCommitFailsStartAndSettlesNothing(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 4096, writeMtime)

	r.ex.setCommitErr(errInjected)
	r.atRestart()

	if err := r.start(t); !errors.Is(err, errInjected) {
		t.Fatalf("Start = %v, want the injected failure", err)
	}

	if s := r.settles(0); len(s) != 0 {
		t.Errorf("settled %v after a failed commit, want nothing", s)
	}

	closes := r.calls(0, "close")
	wantCloses := []string{
		fmt.Sprintf("close handle %d", r.two.Tag),
		fmt.Sprintf("close handle %d", r.one.Tag),
		"close journal",
	}

	if !slices.Equal(closes, wantCloses) {
		t.Errorf("closes %v, want %v", closes, wantCloses)
	}

	r.ex.setCommitErr(nil)
	r.j.Reopen()

	mark := len(r.j.log())

	if err := r.start(t); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	n := fname(f)
	want := []string{"since " + n, "sync " + n, "commit " + n, settleCall(f, v1)}

	if c := r.calls(mark, "since", "sync", "commit", "remove", "settle"); !slices.Equal(c, want) {
		t.Errorf("calls of the second start %v, want %v", c, want)
	}

	if got := r.file(t, h); got.Size != 4096 {
		t.Errorf("size %d, want 4096", got.Size)
	}
}

// Files are replayed one at a time: each file's changes are read, synced,
// committed and settled before the next file's are read.
func TestStartReplaysFileByFile(t *testing.T) {
	r := newRig(t)

	for _, name := range []string{"a", "b", "c"} {
		_, f := r.newFile(t, name)
		r.write(t, f, 0, 4096, writeMtime)
	}

	r.begin(t)

	c := r.calls(0, "since", "sync", "commit", "remove", "settle")

	if len(c) != 12 {
		t.Fatalf("calls %v, want 12", c)
	}

	seen := map[string]bool{}

	for i := 0; i < len(c); i += 4 {
		file := strings.Fields(c[i])[1]

		for j, kind := range []string{"since", "sync", "commit", "settle"} {
			if f := strings.Fields(c[i+j]); f[0] != kind || f[1] != file {
				t.Errorf("call %d = %q, want %s of %s", i+j, c[i+j], kind, file)
			}
		}

		if seen[file] {
			t.Errorf("file %s is replayed in two groups: %v", file, c)
		}

		seen[file] = true
	}
}

// Every served share's files are replayed, and a share being removed, which
// has no handle, is not touched.
func TestStartReplaysEveryServedShare(t *testing.T) {
	r := newRig(t)
	h1, f1 := r.newFile(t, "a")

	grant := entity.ShareGrant{
		Share:     r.two.ID,
		Principal: r.fs.Admin.Principal,
		Access:    entity.AccessAdmin,
	}

	if err := r.fs.Store.ControlPlane().SetGrant(t.Context(), grant); err != nil {
		t.Fatal(err)
	}

	root, _, err := r.fs.Store.Files().Root(t.Context(), r.fs.Admin, r.two.ID)
	if err != nil {
		t.Fatal(err)
	}

	h2, file2 := r.fs.Create(t, r.fs.Admin, root, "b", kvstore.Regular(0o644))
	f2 := file2.ID

	r.write(t, f1, 0, 4096, writeMtime)
	r.writeOn(t, r.handle(t, r.two.Tag), f2, 0, 8192, writeMtime)

	removed := entity.NewShareID()
	r.lc.removed = map[entity.ShareID]entity.TagRecord{
		removed: {
			Tag:   92,
			State: entity.TagOffloaded,
		},
	}
	r.seed(t, 92)

	r.atRestart()

	handed := engine.Share{
		ID:    removed,
		Tag:   92,
		State: engine.Serving,
	}

	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("Start panicked replaying a share with no handle: %v", p)
			}
		}()

		if err := r.e.Start(t.Context(), []engine.Share{r.one, r.two, handed}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}()

	if got := r.file(t, h1); got.Size != 4096 {
		t.Errorf("share one's file size %d, want 4096", got.Size)
	}

	if got := r.file(t, h2); got.Size != 8192 {
		t.Errorf("share two's file size %d, want 8192", got.Size)
	}

	if s := r.calls(0, "since"); len(s) != 2 {
		t.Errorf("since calls %v, want exactly the two served files", s)
	}
}

// A synced write the journal lost whole is committed from its loss record as
// one write with its whole extent lost: the file has the size, charges
// nothing, keeps its modification time and reads as lost.
func TestStartCommitsALossAsLost(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	before := r.file(t, h)
	usage := r.usage(t)
	v1 := r.write(t, f, 0, 4096, writeMtime)
	r.j.Lose(journal.NewFileID(f), journal.Extent{Off: 0, Len: 4096}, journal.LossCorrupt)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 4096},
			Ctime:   restartAt,
			Lost:    []entity.Extent{{Offset: 0, Length: 4096}},
		},
	})

	got := r.file(t, h)

	if got.Size != 4096 || got.Charged != 0 {
		t.Errorf("size %d, charged %d, want 4096 and 0", got.Size, got.Charged)
	}

	if !got.Modify.Equal(before.Modify) {
		t.Errorf("Modify = %v, want it kept at %v", got.Modify, before.Modify)
	}

	if !got.Change.Equal(restartAt) {
		t.Errorf("Change = %v, want %v", got.Change, restartAt)
	}

	if u := r.usage(t); u != usage {
		t.Errorf("usage %+v, want %+v unchanged", u, usage)
	}

	dst := make([]byte, 4096)

	if _, err := r.e.Read(t.Context(), r.one.ID, f, 0, dst); !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("Read = %v, want ErrLost", err)
	}

	if a := r.applied(t, f); a != jver(v1) {
		t.Errorf("applied = %v, want %v", a, jver(v1))
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v1)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v1))
	}
}

// With no hook to rebuild a clone, a clone marker fails Start: the
// destination is not served as it was before the clone (RFC 8 §2.5 step 4).
func TestStartFailsOnACloneMarkerWithNoHook(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	id := journal.NewFileID(f)

	r.write(t, f, 0, 4096, writeMtime)

	if _, err := r.jh.CloneTarget(t.Context(), id, journal.CloneSpec{DstOff: 4096, Len: 4096}); err != nil {
		t.Fatal(err)
	}

	r.atRestart()

	if err := r.start(t); !errors.Is(err, engine.ErrUnbuilt) {
		t.Errorf("Start = %v, want ErrUnbuilt", err)
	}
}

// A clone marker is handed to the rebuild, never replayed as a removal, and
// nothing above it is applied or settled.
func TestStartHandsACloneMarkerToTheHookAndAppliesNothingAbove(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	id := journal.NewFileID(f)
	v1 := r.write(t, f, 0, 4096, writeMtime)

	spec := journal.CloneSpec{
		DstOff: 4096,
		Len:    4096,
	}

	v2, err := r.jh.CloneTarget(t.Context(), id, spec)
	if err != nil {
		t.Fatal(err)
	}

	r.write(t, f, 8192, 4096, writeMtime)

	type marker struct {
		share entity.ShareID
		file  entity.FileID
		spec  journal.CloneSpec
		v     journal.Version
	}

	var got []marker

	hook := func(s entity.ShareID, file entity.FileID, sp journal.CloneSpec, v journal.Version) {
		got = append(got, marker{s, file, sp, v})
	}

	r.e.SetCloneMarkerHook(hook)

	r.begin(t)

	if len(got) != 1 || got[0] != (marker{r.one.ID, f, spec, v2}) {
		t.Errorf("clone markers handed %+v, want one: share, file, spec and version %v", got, v2)
	}

	if c := r.calls(0, "remove"); len(c) != 0 {
		t.Errorf("removals %v, want none", c)
	}

	if g := r.file(t, h); g.Size != 4096 {
		t.Errorf("size %d, want 4096: only the write below the marker", g.Size)
	}

	if a := r.applied(t, f); a != jver(v1) {
		t.Errorf("applied = %v, want %v", a, jver(v1))
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v1)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v1))
	}
}

// A release the crash left, with no holder, is finished: the file is gone.
func TestStartSettlesAReleaseTheCrashLeft(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.write(t, f, 0, 4096, writeMtime)

	ns := r.fs.Store.Namespace()

	if _, err := ns.Unlink(t.Context(), r.fs.Admin, r.fs.Root, []byte("a")); err != nil {
		t.Fatal(err)
	}

	v2, err := r.jh.Delete(t.Context(), journal.NewFileID(f))
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	_, err = r.fs.Store.Existence().Size(t.Context(), r.one.ID, f)
	if !errors.Is(err, entity.ErrStale) {
		t.Errorf("the released file's record: %v, want it gone", err)
	}

	if c := r.calls(0, "remove"); len(c) != 1 {
		t.Errorf("removals %v, want one", c)
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}

	wantNoLoud(t, r)
}

// A file the store no longer has, whose release committed before the crash
// came, is settled quietly: nothing is applied and nothing is lost.
func TestStartSettlesAFileThatIsGone(t *testing.T) {
	r := newRig(t)
	f := entity.NewFileID()
	r.write(t, f, 0, 4096, writeMtime)

	v2, err := r.jh.Delete(t.Context(), journal.NewFileID(f))
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	if c := r.calls(0, "commit", "remove"); len(c) != 0 {
		t.Errorf("calls %v, want none for a file that is gone", c)
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}

	wantNoLoud(t, r)
}

// heldRelease leaves a release a holder will refuse: the file keeps its name,
// and the journal holds its delete marker.
func (r *rig) heldRelease(t *testing.T) (entity.Handle, entity.FileID, journal.Version) {
	t.Helper()

	h, f := r.newFile(t, "a")
	r.write(t, f, 0, 8, writeMtime)

	v2, err := r.jh.Delete(t.Context(), journal.NewFileID(f))
	if err != nil {
		t.Fatal(err)
	}

	return h, f, v2
}

// A release a holder refuses is settled without its removal and reported as
// lost, naming the file: the file stays as it was.
func TestStartSettlesAReleaseAHolderAborts(t *testing.T) {
	r := newRig(t)
	h, f, v2 := r.heldRelease(t)
	before := r.file(t, h)

	r.begin(t)

	got := r.file(t, h)

	if got.Size != before.Size || got.Applied != before.Applied {
		t.Errorf("file %+v, want size %d applied %v as before", got, before.Size, before.Applied)
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}

	loud := r.logged()

	if len(loud) != 1 {
		t.Fatalf("logged %d records at Warn or above: %v, want exactly one", len(loud), loud)
	}

	if loud[0].Level != slog.LevelError || loud[0].Message != abortedReleaseMessage ||
		replayAttr(loud[0], "file") != f {
		t.Errorf(
			"logged %v at %v, want %q at Error naming file %v",
			loud[0].Message, loud[0].Level, abortedReleaseMessage, f,
		)
	}
}

// The abort at start does not raise the file's loss sequence: the release was
// aborted before the restart, and a raised sequence would fail one SMB flush
// for nothing.
func TestStartDoesNotRaiseTheLossSequenceOfAnAbortedRelease(t *testing.T) {
	r := newRig(t)
	_, f, v2 := r.heldRelease(t)
	_, sibling := r.newFile(t, "b")

	r.begin(t)

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Fatalf("settles %v, want exactly %q: the abort did not run", s, settleCall(f, v2))
	}

	if got := r.e.LossSeq(r.one.ID, f); got != 0 {
		t.Errorf("loss sequence of the aborted release's file = %d, want 0", got)
	}

	if got := r.e.LossSeq(r.one.ID, sibling); got != 0 {
		t.Errorf("loss sequence of a file with no marker = %d, want 0", got)
	}
}

// A removal the store refuses as invalid ends unapplied: the writes below it
// are committed, its marker is settled and the loss is reported naming the
// file.
func TestStartSettlesARemovalTheStoreRefuses(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	v2, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 4096)
	if err != nil {
		t.Fatal(err)
	}

	// The store's own refusal of a removal it cannot take, harvested from it.
	refused := r.fs.Store.Existence().Remove(t.Context(), r.one.ID, f, 0, kvstore.RemoveOp{}, nil)
	if !kvstore.Refused(refused) {
		t.Fatalf("the store answered %v to an invalid removal, want a refusal", refused)
	}

	r.ex.removeErr = refused

	r.begin(t)

	got := r.file(t, h)

	if got.Size != 8192 {
		t.Errorf("size %d, want 8192: the removal was not applied", got.Size)
	}

	r.wantRemovals(t, got, nil)

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}

	loud := r.logged()

	if len(loud) != 1 {
		t.Fatalf("logged %d records at Warn or above: %v, want exactly one", len(loud), loud)
	}

	if loud[0].Level != slog.LevelError || loud[0].Message != unappliedMessage ||
		replayAttr(loud[0], "file") != f || replayAttr(loud[0], "kind") != "truncate" {
		t.Errorf(
			"logged %v at %v, want %q at Error naming file %v and kind truncate",
			loud[0].Message, loud[0].Level, unappliedMessage, f,
		)
	}

	if seq := r.e.LossSeq(r.one.ID, f); seq != 0 {
		t.Errorf("loss sequence = %d, want 0", seq)
	}
}

// A marker the journal yields again at the next start, since Settle is not
// persisted, is handled again without failing Start or committing anything.
func TestADiscardedMarkerComesBackAndIsDiscardedAgain(t *testing.T) {
	r := newRig(t)
	h, f, v2 := r.heldRelease(t)

	r.begin(t)

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Fatalf("settles %v after the first start, want exactly %q", s, settleCall(f, v2))
	}

	before := r.file(t, h)
	mark := len(r.j.log())

	r.restart(t)

	if err := r.start(t); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if c := r.calls(mark, "commit"); len(c) != 0 {
		t.Errorf("the second start committed %v, want nothing", c)
	}

	if got := r.file(t, h); got.Size != before.Size || got.Applied != before.Applied {
		t.Errorf(
			"file %+v after the second start, want size %d applied %v",
			got, before.Size, before.Applied,
		)
	}
}

// An error the journal answers reading a file's changes fails Start, which
// closes what it opened and serves nothing; run again with the error gone, it
// replays the file.
func TestStartFailsOnASinceError(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.write(t, f, 0, 4096, writeMtime)

	r.j.fail = map[string]error{"since " + fname(f): errInjected}
	r.atRestart()

	if err := r.start(t); !errors.Is(err, errInjected) {
		t.Fatalf("Start = %v, want the injected failure", err)
	}

	wantCloses := []string{
		fmt.Sprintf("close handle %d", r.two.Tag),
		fmt.Sprintf("close handle %d", r.one.Tag),
		"close journal",
	}

	if closes := r.calls(0, "close"); !slices.Equal(closes, wantCloses) {
		t.Errorf("closes %v, want %v", closes, wantCloses)
	}

	if err := r.write2(t); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("a write after the failed Start = %v, want ErrDelay(DelayFrozen)", err)
	}

	r.j.fail = nil
	r.j.Reopen()

	if err := r.start(t); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if got := r.file(t, h); got.Size != 4096 {
		t.Errorf("size %d, want 4096", got.Size)
	}
}

func (r *rig) write2(t *testing.T) error {
	t.Helper()

	return r.write2To(t, r.one.ID)
}

func (r *rig) write2To(t *testing.T, id entity.ShareID) error {
	t.Helper()

	return r.starter.write(t, id)
}

// No file is served before step 3 has run: while its commit is held, every
// call on the share answers ErrDelay, and the shard's incarnation is raised
// already; once Start returns, the file reads its committed size.
func TestNoFileIsServedBeforeStepThreeRuns(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.write(t, f, 0, 4096, writeMtime)

	before := r.shardOf(t, r.one.Shard)
	g := newGate(t)
	r.ex.gate = g
	r.atRestart()

	done := make(chan error, 1)

	go func() {
		done <- r.start(t)
	}()

	select {
	case <-g.entered:
	case err := <-done:
		t.Fatalf("Start returned %v before the replay's commit", err)
	}

	if _, _, err := r.e.Overlay(t.Context(), r.one.ID, f); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Overlay while the commit is held = %v, want ErrDelay(DelayFrozen)", err)
	}

	if _, err := r.e.Allocation(t.Context(), r.one.ID, f, 0); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Allocation while the commit is held = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := r.write2(t); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("a write while the commit is held = %v, want ErrDelay(DelayFrozen)", err)
	}

	if got := r.shardOf(t, r.one.Shard); got.Incarnation != before.Incarnation+1 {
		t.Errorf(
			"incarnation %d while the commit is held, want %d: step 2 before step 3",
			got.Incarnation, before.Incarnation+1,
		)
	}

	g.open()

	if err := <-done; err != nil {
		t.Fatalf("Start: %v", err)
	}

	if o, pending, err := r.e.Overlay(t.Context(), r.one.ID, f); err != nil || pending {
		t.Errorf("Overlay after Start = %+v, pending %v, %v, want nothing pending", o, pending, err)
	}

	if got := r.file(t, h); got.Size != 4096 {
		t.Errorf("committed size %d after Start, want 4096", got.Size)
	}
}

// A replayed write leaves no entry in the engine, calls no Done and reserves
// nothing: its existence is committed before Start returns, not by the group
// commit later.
func TestReplayLeavesNoEntryAndCallsNoDone(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")

	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}

	opts := engine.WriteOpts{
		Done: func() {
		},
	}

	if _, err := r.e.Write(t.Context(), r.one.ID, f, 0, []byte("hello"), opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	r.restart(t)

	if err := r.start(t); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if n := r.e.FileEntries(); n != 0 {
		t.Errorf("%d file entries after the replay, want 0", n)
	}

	if n := r.e.PendingApplied(); n != 0 {
		t.Errorf("%d operations went through the group commit, want 0", n)
	}

	if rs := r.j.Reserves(); len(rs) != 0 {
		t.Errorf("reservations %v, want none", rs)
	}

	if got := r.file(t, h); got.Size != 5 {
		t.Errorf("size %d right after Start, want 5", got.Size)
	}
}

// A file whose replay needs two commits settles the journal after each, at
// the version of the commit it follows: when the second fails, Start fails
// with the markers of the first already settled and nothing of the second.
func TestAFailedSecondCommitSettlesOnlyTheFirst(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	id := journal.NewFileID(f)
	v1 := r.write(t, f, 0, 4096, writeMtime)

	v2, err := r.jh.Truncate(t.Context(), id, 4096)
	if err != nil {
		t.Fatal(err)
	}

	r.write(t, f, 8192, 4096, writeMtime)

	r.ex.mu.Lock()
	r.ex.commitErr = errInjected
	r.ex.failAt = 2
	r.ex.mu.Unlock()

	r.atRestart()

	if err := r.start(t); !errors.Is(err, errInjected) {
		t.Fatalf("Start = %v, want the injected failure", err)
	}

	n := fname(f)
	wantCalls := []string{"commit " + n, "remove " + n, "commit " + n}

	if c := r.calls(0, "commit", "remove"); !slices.Equal(c, wantCalls) {
		t.Errorf("calls %v, want %v", c, wantCalls)
	}

	wantSettles := []string{settleCall(f, v1), settleCall(f, v2)}

	if s := r.settles(0); !slices.Equal(s, wantSettles) {
		t.Errorf(
			"settles %v, want %v: each commit that succeeded, none of the failed one",
			s, wantSettles,
		)
	}

	if a := r.applied(t, f); a != jver(v2) {
		t.Errorf("applied = %v, want %v", a, jver(v2))
	}

	if got := r.file(t, h); got.Size != 4096 {
		t.Errorf("size %d, want 4096: the removal committed, the second write did not", got.Size)
	}
}

// A truncate up the crash cut off is replayed as a grow that moves the
// modification time to the restart's, as a replayed truncate down does and as
// the live path does for both.
func TestStartReplaysATruncateUpWithTheRestartsTime(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 4096)

	v2, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 8192)
	if err != nil {
		t.Fatal(err)
	}

	r.begin(t)

	got := r.file(t, h)

	if got.Size != 8192 {
		t.Errorf("size %d, want 8192", got.Size)
	}

	if !got.Modify.Equal(restartAt) {
		t.Errorf("Modify = %v, want the restart's %v (it was %v)", got.Modify, restartAt, committed)
	}

	if !got.Change.Equal(restartAt) {
		t.Errorf("Change = %v, want %v", got.Change, restartAt)
	}

	if a := r.applied(t, f); a != jver(v2) {
		t.Errorf("applied = %v, want %v", a, jver(v2))
	}

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}
}

// A synced loss the journal put back in its ring at open is a loss nobody
// read: the first drain after the file is served raises the file's loss
// sequence once, the replay having raised it not at all, and a live write and
// commit on the file raise it no more.
func TestALossFoundAtOpenRaisesTheSequenceOnce(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 4096, writeMtime)
	x := journal.Extent{Off: 0, Len: 4096}

	// The loss is in the journal's changes, as a record on disk is; its event
	// reaches the ring only as the one found at open.
	_ = r.j.LoseLater(journal.NewFileID(f), x, journal.LossCorrupt)
	r.j.Report(journal.LossEvent{
		Share:   r.one.Tag,
		File:    journal.NewFileID(f),
		Extent:  x,
		Version: v1,
		Reason:  journal.LossCorrupt,
		AtOpen:  true,
	})

	r.begin(t)

	if got := r.e.LossSeq(r.one.ID, f); got != 1 {
		t.Errorf("loss sequence after the first drain = %d, want 1", got)
	}

	opts := engine.WriteOpts{
		Done: func() {
		},
	}

	if _, err := r.e.Write(t.Context(), r.one.ID, f, 8192, []byte("hello"), opts); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, _, err := r.e.Commit(t.Context(), r.one.ID, f, 0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := r.e.LossSeq(r.one.ID, f); got != 1 {
		t.Errorf("loss sequence after a live write and commit = %d, want 1", got)
	}
}

// The size the replay tracks decides, for each truncate, whether it is a grow
// or a removal: a truncate down is a removal after the writes below it commit,
// one at or above the size is a grow, and each step moves the size the next one
// is judged by.
func TestStartTracksTheSizeAcrossTruncates(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	id := journal.NewFileID(f)
	v1 := r.write(t, f, 0, 4096, writeMtime)

	var vs []journal.Version

	for _, size := range []int64{6144, 7000, 6500} {
		v, err := r.jh.Truncate(t.Context(), id, size)
		if err != nil {
			t.Fatal(err)
		}

		vs = append(vs, v)
	}

	r.begin(t)

	n := fname(f)
	want := []string{
		"commit " + n, "remove " + n, "commit " + n, "remove " + n,
	}

	if c := r.calls(0, "commit", "remove"); !slices.Equal(c, want) {
		t.Fatalf("calls %v, want %v", c, want)
	}

	ops := r.ex.committedOps()

	if len(ops) != 2 {
		t.Fatalf("%d Commit calls, want 2", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 4096},
			Mtime:   writeMtime,
			Ctime:   restartAt,
		},
	})
	sameOps(t, ops[1], []kvstore.PendingOp{
		{
			Version: jver(vs[1]),
			Kind:    kvstore.OpGrow,
			Size:    7000,
			Mtime:   restartAt,
			Ctime:   restartAt,
		},
	})

	got := r.file(t, h)

	if got.Size != 6500 || got.Charged != 6144 {
		t.Errorf("size %d, charged %d, want 6500 and 6144", got.Size, got.Charged)
	}

	if a := r.applied(t, f); a != jver(vs[2]) {
		t.Errorf("applied = %v, want %v", a, jver(vs[2]))
	}

	sp, err := r.e.Allocation(t.Context(), r.one.ID, f, 6144)
	if err != nil || sp.Class != entity.Hole || sp.Offset != 6144 || sp.Length != 356 {
		t.Errorf("Allocation(6144) = %+v, %v, want the hole [6144, 6500)", sp, err)
	}

	wantSettles := []string{
		settleCall(f, v1), settleCall(f, vs[0]), settleCall(f, vs[1]), settleCall(f, vs[2]),
	}

	if s := r.settles(0); !slices.Equal(s, wantSettles) {
		t.Errorf("settles %v, want %v", s, wantSettles)
	}
}

// A file the store answers as failed in its own right fails Start, and
// nothing is settled for it: the journal keeps what the store did not record.
func TestAFileTheStoreFailsFailsStartAndIsNotSettled(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.write(t, f, 0, 4096, writeMtime)

	r.ex.failedOf = map[entity.FileID]error{f: errInjected}
	r.atRestart()

	if err := r.start(t); !errors.Is(err, errInjected) {
		t.Fatalf("Start = %v, want the file's failure", err)
	}

	if s := r.settles(0); len(s) != 0 {
		t.Errorf("settled %v for a file the store failed, want nothing", s)
	}
}

// A sync that fails fails Start before anything is committed or settled, for
// the writes' commit and for the removal's first transaction alike.
func TestAFailedSyncFailsStartBeforeAnyCommitOrSettle(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, r *rig) entity.FileID
	}{
		{
			name: "a write",
			make: func(t *testing.T, r *rig) entity.FileID {
				_, f := r.newFile(t, "a")
				r.write(t, f, 0, 4096, writeMtime)

				return f
			},
		},
		{
			name: "a truncate",
			make: func(t *testing.T, r *rig) entity.FileID {
				_, f := r.newFile(t, "a")
				r.commitSize(t, f, 8192)

				if _, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 4096); err != nil {
					t.Fatal(err)
				}

				return f
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			f := c.make(t, r)

			r.j.fail = map[string]error{"sync " + fname(f): errInjected}
			r.atRestart()

			if err := r.start(t); !errors.Is(err, errInjected) {
				t.Fatalf("Start = %v, want the injected failure", err)
			}

			if got := r.calls(0, "commit", "remove", "settle"); len(got) != 0 {
				t.Errorf("calls %v after a failed sync, want none", got)
			}
		})
	}
}

// Two lost runs of one write, found in the order the journal cut them and not
// in offset order, are committed as one write with its lost runs in offset
// order, as the store requires.
func TestStartCommitsTwoLossesOfOneWriteInOffsetOrder(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 12288, writeMtime)
	id := journal.NewFileID(f)

	r.j.Lose(id, journal.Extent{Off: 8192, Len: 2048}, journal.LossCorrupt)
	r.j.Lose(id, journal.Extent{Off: 2048, Len: 2048}, journal.LossCorrupt)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 12288},
			Mtime:   writeMtime,
			Ctime:   restartAt,
			Lost: []entity.Extent{
				{Offset: 2048, Length: 2048},
				{Offset: 8192, Length: 2048},
			},
		},
	})

	if got := r.file(t, h); got.Size != 12288 || got.Charged != 8192 {
		t.Errorf("size %d, charged %d, want 12288 and 8192", got.Size, got.Charged)
	}
}

// A removal the store reports as finding its file gone is settled quietly,
// like a file the store no longer has.
func TestStartSettlesARemovalWhoseFileTheStoreFindsGone(t *testing.T) {
	r := newRig(t)
	h, f := r.newFile(t, "a")
	r.commitSize(t, f, 8192)

	v2, err := r.jh.Truncate(t.Context(), journal.NewFileID(f), 4096)
	if err != nil {
		t.Fatal(err)
	}

	r.ex.removeErr = entity.ErrStale

	r.begin(t)

	if s := r.settles(0); !slices.Equal(s, []string{settleCall(f, v2)}) {
		t.Errorf("settles %v, want exactly %q", s, settleCall(f, v2))
	}

	if got := r.file(t, h); got.Size != 8192 {
		t.Errorf("size %d, want 8192: the removal was not applied", got.Size)
	}

	wantNoLoud(t, r)
}
