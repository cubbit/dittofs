package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform/platformtest"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/recovery"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// The tests in this file drive the reopen seam: recovery.Run over a journal
// directory the running journal no longer holds, then a core continued from
// what it assembled, wrapped in the journal's own constructor. It is the
// mapping the open runs once the recovery after the assembly is built; every
// check here reads its answers through the journal's public surface, so a
// mapping that drops what the rebuild produced fails at the caller that
// consumes it.

// closeDead closes what a crash left of a journal, through the view the crash
// ended, so no close changes the directory; a journal another open replaced
// is closed the same way.
func closeDead(t *testing.T, j *Journal) {
	t.Helper()
	t.Cleanup(func() {
		if j == nil {
			return
		}
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Logf("closing the dead journal: %v", err)
		}
	})
}

// assembledJournal is the harness of the reopen seam: a directory on the real
// filesystem reached through the crash-simulating seam, holding either a
// journal a running process abandoned or the planted image of one, and the
// open's options so every reopen resolves them as the production open does.
type assembledJournal struct {
	seam *kit.Seam
	dir  string
	id   JournalID
}

// newAssembledJournal inits a journal in a fresh directory, through the seam,
// with the open's settings — a small segment cap so a write rolls at a
// readable size — and clocks one per open, as the open's options name a
// process that has not started yet. The format file names the identity the
// reopen seam expects. No journal is left open: the first caller opens one.
func newAssembledJournal(t *testing.T) *assembledJournal {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	t.Cleanup(seam.Abandon)
	f := &assembledJournal{seam: seam, dir: dir}
	f.id, err = Init(context.Background(), dir, installID,
		WithMountProbe(platformtest.SuitableGather(dir)), WithStorage(seam.FS()))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return f
}

// seededJournal plants the format file over the seam's directory as an Init
// would have left it, so a planted workload needs no running journal. The
// caller plants its own segments beside it and reopens through the seam.
func seededJournal(t *testing.T) *assembledJournal {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	t.Cleanup(seam.Abandon)
	plantFormatFile(t, seam)
	return &assembledJournal{seam: seam, dir: dir, id: inspectIdentity}
}

// opts returns the options the journal's open resolves: the settings, a
// fresh clock, a suitable mount for the directory and the seam's current
// view. The caller passes set to narrow a setting, such as the extent bound
// of a reopen under a lower limit.
func (f *assembledJournal) opts(set func(*Settings)) []Option {
	// The harness's own settings beside the small cap: two streams, two
	// content spares and the fixture's headroom, so a settled pool is a
	// handful of small files rather than the defaults' dozens.
	s := openSettings()
	s.Streams = 2
	s.SpareTarget = 2
	s.HeadroomRecords = 1024
	if set != nil {
		set(&s)
	}
	return []Option{
		WithClock(kit.NewStopwatchClock()),
		WithSettings(s),
		WithMountProbe(platformtest.SuitableGather(f.dir)),
		WithStorage(f.seam.FS()),
	}
}

// open opens the journal's directory the way the production open does, for
// the running journal a test crashes later.
func (f *assembledJournal) open(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(context.Background(), f.dir, Version{}, f.id, nil, f.opts(nil)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("Close: %v", err)
		}
	})
	return j
}

// reopen replays the open over the directory through the reopen seam: the
// recovery the open runs, the failed list the durability layer of this
// process holds for the directory, and the core continued from the state the
// recovery assembled. The spare-pool settle is waited for as the open's own
// SparesReady callback waits. floor is the caller's floor, nil marks none.
func (f *assembledJournal) reopen(t *testing.T, floor Version, set func(*Settings)) *Journal {
	t.Helper()
	j, err := f.tryReopen(t, floor, set)
	if err != nil {
		t.Fatalf("the reopen seam over %s: %v", f.dir, err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("Close: %v", err)
		}
	})
	return j
}

// tryReopen is reopen returning the seam's error rather than failing the
// test, for a row that asserts a refusal.
func (f *assembledJournal) tryReopen(t *testing.T, floor Version, set func(*Settings)) (*Journal, error) {
	t.Helper()
	opts := f.opts(set)
	cfg, err := resolveConfig(f.dir, opts)
	if err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		return nil, ErrNoClock
	}
	failed, err := durability.FailedFor(f.dir, f.id)
	if err != nil {
		return nil, err
	}
	st, err := recovery.Run(context.Background(), recovery.Deps{
		FS:     cfg.Storage,
		Clock:  cfg.Clock,
		Tracer: cfg.Tracer,
		Log:    cfg.Logger,

		// The failed list is the directory's own, carried as data: the
		// numbers a read-only journal on the directory, earlier in this
		// process, could write no loss record for.
		Failed: failed,

		ExtentLimit: cfg.ExtentLimit,
		SegmentCap:  core.SegmentCapOf(cfg.Settings),
	}, floor, f.id, nil)
	if err != nil {
		return nil, err
	}
	if st.Assembled.Set == nil || st.Rebuilt.Index == nil || st.Rebuilt.Counters == nil {
		return nil, fmt.Errorf("the recovery over %s left the state half assembled", f.dir)
	}
	s := resolveDefaults(cfg.Settings)
	c, err := datapath.New(datapath.Deps{
		FS:            cfg.Storage,
		Clock:         cfg.Clock,
		Tracer:        cfg.Tracer,
		Log:           cfg.Logger,
		Dir:           f.dir,
		Identity:      f.id,
		Streams:       s.Streams,
		SegmentCap:    core.SegmentCapOf(s),
		ExtentLimit:   s.ExtentLimit,
		Descriptors:   cfg.Budget,
		Targets:       buildTargets(s),
		MaxBytes:      s.MaxBytes,
		IdleSealBytes: s.IdleSealBytes,

		// The highest id the directory holds: the maker draws its first
		// spare above it, so a fresh spare never meets a rebuilt
		// segment's id.
		LastSegment: st.Attached.MaxID,
	}, datapath.State{
		Index:    st.Rebuilt.Index,
		Counters: st.Rebuilt.Counters,
		Set:      st.Assembled.Set,
		Open: &datapath.OpenState{
			Live:     st.Assembled.Live,
			Forgets:  st.Assembled.Forgets,
			Unsynced: st.Assembled.Unsynced,
			Dirty:    dirtySeed(st.Rebuilt.Index),
		},
	})
	if err != nil {
		return nil, err
	}
	j := newJournal(c)
	// A backstop only, the same bound mustWaitSpares takes: the wait itself
	// is the pool's settle, and a seam that drops what a row's fill needs
	// must fail the row's own assertion rather than hang it to the runner's
	// timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := c.WaitSpares(ctx); err != nil {
		_ = j.Close()
		return nil, err
	}
	return j, nil
}

// crash ends the seam's view under a power loss, so the bytes no sync made
// durable are lost, and abandons the journal the killed process was holding:
// a dead process closes nothing.
func (f *assembledJournal) crash(t *testing.T, j *Journal) {
	t.Helper()
	if err := f.seam.Crash(); err != nil {
		t.Fatalf("crash: %v", err)
	}
	closeDead(t, j)
}

// segmentFileNames returns the .seg names the directory holds now.
func (f *assembledJournal) segmentFileNames(t *testing.T) []string {
	t.Helper()
	names, err := f.seam.List()
	if err != nil {
		t.Fatalf("list the journal's directory: %v", err)
	}
	var out []string
	for _, name := range names {
		if strings.HasSuffix(name, ".seg") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// within waits for the named condition to hold, polling it with a short
// pause between polls, until the generous backstop ends. It is the bounded
// wait of a check that waits on the worker's own rhythm; the backstop is the
// only time bound, and its end fails the check naming what never came.
func waitFor(t *testing.T, what string, backstop time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(backstop)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not hold within %s", what, backstop)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdsStill asserts the condition stays false through the whole span, the
// bounded wait that shows something does not happen: the span covers many
// passes of the worker's own rhythm, whose every pass would act were the
// condition to hold.
func staysThrough(t *testing.T, what string, span time.Duration, cond func() bool) {
	t.Helper()
	if cond() {
		t.Fatalf("%s holds already, want it not to", what)
		return
	}
	deadline := time.Now().Add(span)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s came to hold within %s, want it not to", what, span)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustWaitSpares waits for the running journal's pool to reach its spares,
// the wait the open hands its caller through the SparesReady hook, over a
// generous backstop: the pool's settle alone is the wait.
func mustWaitSpares(t *testing.T, j *Journal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // backstop only; the wait is the settle
	defer cancel()
	if err := j.core.WaitSpares(ctx); err != nil {
		t.Fatalf("the journal's pool did not reach its spares: %v", err)
	}
}

// segmentsOfFile returns the segments the index says hold file id's records.
// It fails on the inspector's own error, not on what it finds.
func segmentsOfFile(t *testing.T, j *Journal, id FileID) map[SegmentID]bool {
	t.Helper()
	entries, err := j.Inspect().File(id)
	if err != nil {
		t.Fatalf("inspect file %v: %v", id, err)
	}
	segs := map[SegmentID]bool{}
	for _, e := range entries {
		segs[e.Location.Segment] = true
	}
	return segs
}

// shareOf attaches tag to j and returns its handle, failing on the refusal.
func shareOf(t *testing.T, j *Journal, tag ShareTag) *Handle {
	t.Helper()
	h, err := j.Share(tag, 0, Version{})
	if err != nil {
		t.Fatalf("attach share %d: %v", tag, err)
	}
	return h
}

// payload returns n bytes of a repeatable pattern a write can carry.
func payload(n int) []byte {
	return bytes.Repeat([]byte{0xa5}, n)
}

// TestReopenAssembled_CountersAboveTheDiskAndTheFloor holds the halves of the
// counters' rule at a reopened journal's caller: after a crash the first
// content version the reopened journal issues exceeds every version on the
// disk, and reopened with a floor above them all, the first version exceeds
// the floor too. The write is the consumer: a counter the seam dropped would
// hand the same version out twice.
func TestReopenAssembled_CountersAboveTheDiskAndTheFloor(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	file := fileOf(1)

	v1 := writeReady(t, h, file, "first version on disk")
	if _, err := h.Sync(t.Context(), file); err != nil {
		t.Fatalf("Sync the first write: %v", err)
	}
	v2 := writeReady(t, h, file, "second version on disk")
	if _, err := h.Sync(t.Context(), file); err != nil {
		t.Fatalf("Sync the second write: %v", err)
	}
	onDisk := max(v1.Counter, v2.Counter)
	f.crash(t, j)

	j2 := f.reopen(t, Version{}, nil)
	withLimit(t, j2)
	if got := j2.Stats().NextVersion.Counter; got <= onDisk {
		t.Fatalf("Stats().NextVersion after the reopen = %d, want above every version on disk (%d)", got, onDisk)
	}
	h2 := shareOf(t, j2, 1)
	v3 := writeReady(t, h2, file, "the reopened journal's first write")
	if v3.Counter <= onDisk {
		t.Fatalf("the first write after the reopen drew version %d, want above every version on disk (%d)", v3.Counter, onDisk)
	}

	// The floor is the caller's own: reopened above every number the disk
	// holds, the first version the journal issues exceeds the floor itself.
	floor := Version{Counter: v3.Counter + 100}
	f.crash(t, j2)
	j3 := f.reopen(t, floor, nil)
	withLimit(t, j3)
	h3 := shareOf(t, j3, 1)
	v4 := writeReady(t, h3, file, "the write past the floor")
	if v4.Counter <= floor.Counter {
		t.Fatalf("the first write after a reopen at floor %d drew version %d, want above the floor", floor.Counter, v4.Counter)
	}
}

// TestReopenAssembled_SparesDrawAboveTheRebuiltSegments holds the seam's
// highest-id rule: the spare maker a reopened journal builds draws its first
// id above every segment the directory holds, so the pool readies its spares
// and a write past the reopen lands on a segment of its own, not refused for
// want of a spare the pool could never make.
func TestReopenAssembled_SparesDrawAboveTheRebuiltSegments(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	file := fileOf(1)

	// Two segments the crash leaves behind: write until the file's entries
	// cover two, so the directory holds ids the maker's first draws would
	// collide with were the seam to keep the drawing fresh.
	writeReady(t, h, file, "the first segment's record")
	for len(segmentsOfFile(t, j, file)) < 2 {
		if _, err := h.WriteAt(t.Context(), file, 16<<10, payload(16<<10), 1); err != nil {
			var ns *NoSpaceError
			if errors.As(err, &ns) && ns.Limit == LimitSpare {
				t.Fatalf("the write rolled with no spare ready: %v", err)
			}
			t.Fatalf("write until the file spans two segments: %v", err)
		}
	}
	if _, err := h.Sync(t.Context(), file); err != nil {
		t.Fatalf("Sync the two segments' records: %v", err)
	}
	below := f.segmentFileNames(t)
	if len(below) < 2 {
		t.Fatalf("the directory holds %d segment files before the crash, want at least two: %v", len(below), below)
	}
	f.crash(t, j)

	j2 := f.reopen(t, Version{}, nil)
	withLimit(t, j2)
	h2 := shareOf(t, j2, 1)
	var v Version
	for {
		var err error
		v, err = h2.WriteAt(t.Context(), file, 32<<10, payload(16), 1)
		if err == nil {
			break
		}
		var ns *NoSpaceError
		if errors.As(err, &ns) && ns.Limit == LimitSpare {
			t.Fatalf("the first write after the reopen was refused for want of a spare: the pool did not draw above the directory's segments")
		}
		t.Fatalf("the first write after the reopen: %v", err)
	}
	if v == (Version{}) {
		t.Fatal("the write after the reopen drew no version")
	}
}

// TestReopenAssembled_TheFailedListRaisesTheCounter holds the failed list's
// in-process rule at the write that consumes it: a journal driven read-only
// until its unsynced records land on the directory's failed list, crashed and
// reopened in the same process, issues its next stable write a sequence
// number above the failed one, and a second reopen holds that write.
func TestReopenAssembled_TheFailedListRaisesTheCounter(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	fa, fb := fileOf(1), fileOf(2)

	// The synced record stays; three more are appended and never synced.
	writeReady(t, h, fa, "the synced record")
	if _, err := h.Sync(t.Context(), fa); err != nil {
		t.Fatalf("Sync the record the crash keeps: %v", err)
	}
	writeReady(t, h, fa, "the record the read-only journal fails: B")
	writeReady(t, h, fa, "the record the read-only journal fails: C")
	writeReady(t, h, fb, "the record the read-only journal fails: D")

	// The device refuses every writing operation: the journal turns
	// read-only at the next sync and fails the records no sync covered,
	// whose sequence numbers join the directory's failed list.
	writingOps := []string{kit.OpOpen, kit.OpWrite, kit.OpSync, kit.OpCreate,
		kit.OpAllocate, kit.OpRename, kit.OpRemove, kit.OpTruncate, kit.OpSyncDir}
	rules := make([]kit.Rule, 0, len(writingOps))
	for _, op := range writingOps {
		rules = append(rules, kit.Failing(op, "*", 0, syscall.EROFS))
	}
	if err := f.seam.Schedule(rules...); err != nil {
		t.Fatalf("schedule the read-only device: %v", err)
	}
	if _, err := h.Sync(t.Context(), fa); !errors.Is(err, ErrJournalReadOnly) {
		t.Fatalf("the Sync on the read-only device returned %v, want ErrJournalReadOnly", err)
	}

	// The failed list is the durability layer's own record of the numbers
	// whose loss record could not be appended.
	var failed []uint64
	waitFor(t, "the failed list to hold the failed records", 30*time.Second, func() bool {
		seqs, err := durability.FailedFor(f.dir, f.id)
		if err != nil {
			t.Fatalf("read the directory's failed list: %v", err)
		}
		failed = make([]uint64, 0, len(seqs))
		for _, s := range seqs {
			failed = append(failed, uint64(s))
		}
		return len(failed) == 3
	})
	if len(failed) != 3 {
		t.Fatalf("the failed list holds %v, want the three unsynced records' numbers", failed)
	}
	f.seam.SetFault(nil)
	f.crash(t, j)

	// The reopen in place carries the failed list as data, and the first
	// stable write's number stays above the failed one.
	j2 := f.reopen(t, Version{}, nil)
	withLimit(t, j2)
	h2 := shareOf(t, j2, 1)
	v := writeReady(t, h2, fa, "the first stable write after the reopen")
	marks, err := h2.Sync(t.Context(), fa)
	if err != nil {
		t.Fatalf("Sync the first stable write after the reopen: %v", err)
	}
	if len(marks) == 0 {
		t.Fatal("the Sync returned no mark, want the stream's")
	}
	if marks[0].Sequence <= failed[len(failed)-1] {
		t.Fatalf("the first stable write's sync mark carries sequence %d, want above the failed list's %d",
			marks[0].Sequence, failed[len(failed)-1])
	}
	if v == (Version{}) {
		t.Fatal("the first stable write drew no version")
	}

	// The second reopen is the same process again: the write whose number
	// stayed above the failed one is held and reads back whole.
	f.crash(t, j2)
	j3 := f.reopen(t, Version{}, nil)
	h3 := shareOf(t, j3, 1)
	p := make([]byte, len("the first stable write after the reopen"))
	n, missing, _, _, err := h3.ReadAt(t.Context(), fa, 0, p)
	if err != nil || n != len(p) || len(missing) != 0 {
		t.Fatalf("reading the earlier write back after the second reopen: n %d, missing %+v, err %v", n, missing, err)
	}
	if string(p) != "the first stable write after the reopen" {
		t.Fatalf("the earlier read wrote %q back, want what its record staged", p)
	}
}

// TestReopenAssembled_ARivalledFileIsRefusedThroughEitherTag holds the
// conflict surface at its caller: a file the rebuild answers under two tags
// is named in Stats().TagConflicts with both, and every operation on it
// through either share is refused as another share's.
func TestReopenAssembled_ARivalledFileIsRefusedThroughEitherTag(t *testing.T) {
	f := seededJournal(t)
	rival := fileOf(6)
	plantSegment(t, f.seam, segOwner,
		plantRecord(1, ownerTag, rival, 50, 70, 0, 32))
	plantSegment(t, f.seam, segRival,
		plantRecord(1, rivalTag, rival, 51, 75, 32, 64))
	j := f.reopen(t, Version{}, nil)

	conflicts := j.Stats().TagConflicts
	if len(conflicts) != 1 {
		t.Fatalf("Stats().TagConflicts holds %+v, want the one rivalled file", conflicts)
	}
	c := conflicts[0]
	if c.File != rival || (c.Tags != [2]ShareTag{ownerTag, rivalTag} && c.Tags != [2]ShareTag{rivalTag, ownerTag}) {
		t.Fatalf("TagConflicts names file %v under tags %v, want %v under %v and %v",
			c.File, c.Tags, rival, ownerTag, rivalTag)
	}

	// Every operation on the file through either share is refused, each
	// naming the other share as the file's owner.
	for _, tag := range []ShareTag{ownerTag, rivalTag} {
		h := shareOf(t, j, tag)
		if _, err := h.WriteAt(t.Context(), rival, 0, payload(8), 1); !errors.Is(err, ErrOtherShare) {
			t.Fatalf("WriteAt on the rivalled file through share %d: %v, want ErrOtherShare", tag, err)
		}
		p := make([]byte, 8)
		if _, _, _, _, err := h.ReadAt(t.Context(), rival, 0, p); !errors.Is(err, ErrOtherShare) {
			t.Fatalf("ReadAt on the rivalled file through share %d: %v, want ErrOtherShare", tag, err)
		}
		if _, err := h.Extents(rival); !errors.Is(err, ErrOtherShare) {
			t.Fatalf("Extents on the rivalled file through share %d: %v, want ErrOtherShare", tag, err)
		}
	}
}

// reopenFigures is one snapshot of the figures the equality rows compare,
// taken through the journal's public surface: journal-wide bytes and file
// counts, the per-share figures Stats reports for every tag that holds a
// record, and each share's own file and dirty-file sets.
type reopenFigures struct {
	files, segments   int
	usedBytes         int64
	journalDirtyBytes int64
	journalHeldBytes  int64
	shares            map[ShareTag]ShareStats
	sets              map[ShareTag]map[FileID]bool
	dirtySets         map[ShareTag]map[FileID]bool
	dirtyZeroOfShare  bool // every dirty file's write time is the zero time
	dirtyDatedOfShare bool // at least one dirty file names a write time

	// segFiles is the number of .seg files the directory held at the
	// figure's read. For a before figure, taken on the journal the crash
	// is about to end, it is the file count the crash hands the reopen.
	segFiles int
	// freshSegBytes is the bytes of the segment files the directory grew
	// by since the figure's journal opened: for a reopened journal, whole
	// fresh spares its own pool made above every name the crash left, the
	// storage the reopen allocated itself.
	freshSegBytes int64
}

// figuresOf captures the figures of j for tags, reading each share's file
// and dirty-file set through a handle of its own first, then the Stats
// figures with no handle attached, so every tag it names is an unattached
// share's own.
func figuresOf(t *testing.T, j *Journal, tags []ShareTag) reopenFigures {
	t.Helper()
	out := reopenFigures{
		shares:    map[ShareTag]ShareStats{},
		sets:      map[ShareTag]map[FileID]bool{},
		dirtySets: map[ShareTag]map[FileID]bool{},
	}
	for _, tag := range tags {
		h := shareOf(t, j, tag)
		set := map[FileID]bool{}
		for id := range h.Files() {
			set[id] = true
		}
		out.sets[tag] = set
		out.dirtySets[tag] = map[FileID]bool{}
		zero, dated := true, false
		for id, when := range h.DirtyFiles() {
			out.dirtySets[tag][id] = true
			if !when.IsZero() {
				zero = false
				dated = true
			}
		}
		out.dirtyZeroOfShare = out.dirtyZeroOfShare || zero && len(out.dirtySets[tag]) > 0
		out.dirtyDatedOfShare = out.dirtyDatedOfShare || dated
		if err := h.Close(); err != nil {
			t.Fatalf("close the share %d handle: %v", tag, err)
		}
	}
	st := j.Stats()
	out.files = st.Files
	out.segments = st.Segments
	out.usedBytes = st.UsedBytes
	out.journalDirtyBytes = st.DirtyBytes
	out.journalHeldBytes = st.HeldBytes
	for tag, ss := range st.Shares {
		out.shares[tag] = ss
	}
	return out
}

// equalityFixture is the two-file, two-tag, two-segment workload the
// before/after equality rows share, crashed with every record synced and its
// handles closed, and reopened through the seam.
//
// It returns the figures before the crash and after the reopen, over the
// same tags: the caller asserts one figure of the pair, naming what it
// checks. The writes roll the first file onto a second segment, so the
// crash leaves a rolled-onto segment beside an active one and the reopen
// must rebuild both from what the crash kept.
func equalityFixture(t *testing.T) (before, after reopenFigures) {
	t.Helper()
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	tagA, tagB := ShareTag(1), ShareTag(2)
	fa, fb := fileOf(1), fileOf(2)

	ha := shareOf(t, j, tagA)
	writeReady(t, ha, fa, "the file the first tag holds")
	for len(segmentsOfFile(t, j, fa)) < 2 {
		if _, err := ha.WriteAt(t.Context(), fa, 16<<10, payload(16<<10), 1); err != nil {
			t.Fatalf("write the first tag's file onto a second segment: %v", err)
		}
	}
	hb := shareOf(t, j, tagB)
	writeReady(t, hb, fb, "the file the second tag holds")
	if _, err := ha.Sync(t.Context(), fa); err != nil {
		t.Fatalf("Sync the first tag's file: %v", err)
	}
	if _, err := hb.Sync(t.Context(), fb); err != nil {
		t.Fatalf("Sync the second tag's file: %v", err)
	}
	mustWaitSpares(t, j)
	before = figuresOf(t, j, []ShareTag{tagA, tagB})
	crashFiles := f.segmentFileNames(t)
	before.segFiles = len(crashFiles)

	f.crash(t, j)
	j2 := f.reopen(t, Version{}, nil)
	after = figuresOf(t, j2, []ShareTag{tagA, tagB})
	after.segFiles = len(f.segmentFileNames(t))
	after.freshSegBytes = freshSegBytes(t, f, crashFiles)
	return before, after
}

// freshSegBytes answers the bytes of the segment files the directory holds
// that crash did not: whole fresh spares the reopened journal's pool made
// above every name the crash kept, the storage the reopen allocated
// itself. Their sizes are read straight off the directory, for the fresh
// spare is a whole segment at the cap the pool made durable under its
// final name.
func freshSegBytes(t *testing.T, f *assembledJournal, crash []string) int64 {
	t.Helper()
	kept := make(map[string]bool, len(crash))
	for _, name := range crash {
		kept[name] = true
	}
	var sum int64
	for _, name := range f.segmentFileNames(t) {
		if kept[name] {
			continue
		}
		fi, err := os.Stat(filepath.Join(f.dir, name))
		if err != nil {
			t.Fatalf("stat the fresh segment %s: %v", name, err)
		}
		sum += fi.Size()
	}
	return sum
}

// TestReopenAssembled_EqualityOfFiles holds that the file sets the running
// journal reported per share survive the reopen: the same shares hold the
// same files, naming the share and the file the comparison fails on.
func TestReopenAssembled_EqualityOfFiles(t *testing.T) {
	before, after := equalityFixture(t)
	for _, tag := range []ShareTag{1, 2} {
		if !maps.Equal(before.sets[tag], after.sets[tag]) {
			t.Fatalf("share %d's file set after the reopen = %v, want %v: the rebuild dropped or added a file of the share",
				tag, maps.Keys(after.sets[tag]), maps.Keys(before.sets[tag]))
		}
	}
	if after.files != before.files {
		t.Fatalf("Stats().Files after the reopen = %d, want %d: a file the first journal held is not the second's", after.files, before.files)
	}
}

// TestReopenAssembled_EqualityOfDirtyFiles holds that the dirty-file list is
// rebuilt whole: every share's dirty set survives the reopen, with the
// journal-wide dirty and held bytes beside them — the axes the open's seed
// deltas carry, so a seed that spelled no bytes fails here. The write times
// are not the first journal's — a rebuilt dirty file carries the zero write
// time, the open's own restored-content figure — so the row compares the
// sets and pins the zero time besides.
func TestReopenAssembled_EqualityOfDirtyFiles(t *testing.T) {
	before, after := equalityFixture(t)
	if !after.dirtyZeroOfShare {
		t.Fatalf("the reopened journal's dirty files name write times, want the zero time restored content carries")
	}
	if !before.dirtyDatedOfShare {
		t.Fatalf("the first journal's dirty files all name the zero time, want at least one write time the writes carried")
	}
	for _, tag := range []ShareTag{1, 2} {
		if !maps.Equal(before.dirtySets[tag], after.dirtySets[tag]) {
			t.Fatalf("share %d's dirty-file set after the reopen = %v, want %v: the dirty seed missed or added a file",
				tag, maps.Keys(after.dirtySets[tag]), maps.Keys(before.dirtySets[tag]))
		}
	}
	if got, want := after.journalDirtyBytes, before.journalDirtyBytes; got != want {
		t.Fatalf("Stats().DirtyBytes after the reopen = %d, want %d: the seed must carry the dirty bytes the first journal held", got, want)
	}
	if got, want := after.journalHeldBytes, before.journalHeldBytes; got != want {
		t.Fatalf("Stats().HeldBytes after the reopen = %d, want %d: the seed must carry the held bytes the first journal held", got, want)
	}
}

// TestReopenAssembled_EqualityOfShareFigures holds the per-share figures at
// the caller that reads them: every tag that holds a record — attached or
// not, for no handle is open at either read — reports the same figures after
// the reopen as before the crash, charge and byte axes alike: the record
// bytes still on disk (the withheld and forgotten records' included) beside
// the held, dirty and unreclaimed bytes the running journal's own deltas
// counted. A seed that spells no byte figures fails here, for every byte
// axis it leaves reads zero.
func TestReopenAssembled_EqualityOfShareFigures(t *testing.T) {
	before, after := equalityFixture(t)
	for _, tag := range []ShareTag{1, 2} {
		b, a := before.shares[tag], after.shares[tag]
		if a != b {
			t.Fatalf("share %d's figures after the reopen = %+v, want %+v: the seed charged or counted the share differently than the running journal did",
				tag, a, b)
		}
	}
}

// TestReopenAssembled_EqualityOfUsedBytes holds the journal-wide footprint
// equality: the reopened journal accounts the crash's whole footprint — the
// same bytes the running journal's own figure named, its pool's spare files
// and the sealed segments alike, every empty spare-origin segment among the
// set's entries at the cap it takes on disk — and the only storage its
// figure names over that is what its own spare pool allocated after the
// reopen: whole fresh spares, made above every file the crash kept, both
// pools settled.
func TestReopenAssembled_EqualityOfUsedBytes(t *testing.T) {
	before, after := equalityFixture(t)
	want := before.usedBytes + after.freshSegBytes
	if after.usedBytes != want {
		t.Fatalf("Stats().UsedBytes after the reopen = %d, want %d: the crash's whole footprint (%d, the running journal's own figure) plus the fresh spares its own pool made (%d): a segment the first journal accounted is not the second's or the seed changed its size",
			after.usedBytes, want, before.usedBytes, after.freshSegBytes)
	}
	if after.segments != before.segFiles {
		t.Fatalf("Stats().Segments after the reopen = %d, want %d: one ledger entry per segment file the crash left, empty spare-origin ones among the loaded segments", after.segments, before.segFiles)
	}
}

// plantSealed bytes writes one planted segment sealed, with a write's own
// records, one catalogue entry per record, the seal marker and the trailer,
// as the journal's seal writer leaves them. It returns the segment's whole
// length, the post-seal size the ledger accounts it at.
func plantSealed(t *testing.T, seam *kit.Seam, id core.SegmentID, recs ...raw.Record) int64 {
	t.Helper()
	b := raw.SegmentHeader{Identity: inspectIdentity, ID: uint64(id)}.Bytes()
	var entries [][raw.CatalogEntrySize]byte
	for i := range recs {
		recs[i].Identity = inspectIdentity
		recs[i].Segment = uint64(id)
		recs[i].RecordOffset = uint32(len(b))
		entries = append(entries, raw.Entry{
			FileID:       recs[i].FileID,
			Offset:       recs[i].Offset,
			RecordOffset: recs[i].RecordOffset,
			Length:       uint64(recs[i].Length) & 0xffffffff, // a length column that fits one entry
			Seq:          recs[i].Seq,
			Version:      recs[i].Version,
			Kind:         recs[i].Kind,
			Tag:          recs[i].Tag,
		}.Bytes())
		b = append(b, recs[i].Bytes()...)
	}
	markerOff := len(b)
	b = append(b, raw.SealMarker{Identity: inspectIdentity, Segment: uint64(id), Offset: uint64(markerOff)}.Bytes()...)
	for _, e := range entries {
		b = append(b, e[:]...)
	}
	b = append(b, raw.Trailer(1, uint64(markerOff+raw.SealMarkerSize), entries...)...)
	name := format.SegmentName(id)
	file, err := seam.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := file.WriteAt(b, 0); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync %s: %v", name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	if err := seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory after %s: %v", name, err)
	}
	return int64(len(b))
}

// TestReopenAssembled_UsedBytesAccountsBySegmentStateNotFileSize pins the
// seed's accounting rule at the figure it serves: a sealed segment is
// accounted at its post-seal size and an active one at the cap, whatever
// either file's length says, so a seed that read the sizes off the disk
// cannot pass. Stats().UsedBytes is the consumer, naming both figures.
func TestReopenAssembled_UsedBytesAccountsBySegmentStateNotFileSize(t *testing.T) {
	f := seededJournal(t)
	segSealed := core.SegmentID(931)
	segActive := core.SegmentID(932)

	write := plantRecord(1, cutsTag, fileOf(2), 20, 30, 0, 64)
	sealedLen := plantSealed(t, f.seam, segSealed, write)
	plantSegment(t, f.seam, segActive,
		plantRecord(1, piecesTag, fileOf(1), 10, 10, 0, 64))
	if sealedLen >= openSegmentCap {
		t.Fatalf("precondition: the sealed segment's post-seal size %d is not short of the cap %d", sealedLen, openSegmentCap)
	}

	j := f.reopen(t, Version{}, nil)
	st := j.Stats()

	// The pool's spares are accounted at the cap beside the data segments,
	// so the two planted segments' own figures answer UsedBytes less the
	// spare bytes: the figure a seed that read sizes off the disk could not
	// report, with the active segment's file short of its accounted cap.
	sparesBytes := int64(st.Spares)*openSegmentCap + st.HeadroomBytes + st.RepackReserveBytes
	got := st.UsedBytes - sparesBytes
	want := openSegmentCap + sealedLen
	if got != want {
		t.Fatalf("the two data segments' accounted bytes = %d, want %d: the active segment accounted at the cap %d and the sealed one at its post-seal size %d",
			got, want, openSegmentCap, sealedLen)
	}
	if st.Segments != 2 || st.Sealed != 1 {
		t.Fatalf("Stats().Segments = %d with %d sealed, want 2 segments with the sealed one counted", st.Segments, st.Sealed)
	}
}

// TestReopenAssembled_AForgottenTagsLeftoverRecordsStillCharge holds the
// charge a forgotten tag keeps: a record withheld below a forget record is
// in no share's index, but its bytes are the share's used figure still, and
// the forget record charges its own slot too, so the share Stats reports
// keeps the figures the running journal charged at append — for a share no
// handle of is attached.
func TestReopenAssembled_AForgottenTagsLeftoverRecordsStillCharge(t *testing.T) {
	f := seededJournal(t)
	write := plantRecord(1, marksTag, fileOf(3), 10, 10, 0, 64)
	forget := raw.Record{
		Kind: 14, Seq: 30, Tag: uint32(marksTag),
		FileID: [16]byte{}, Identity: inspectIdentity, Segment: uint64(933),
	}
	plantSegment(t, f.seam, 933, write)
	plantSegment(t, f.seam, 934, forget)

	j := f.reopen(t, Version{}, nil)
	st := j.Stats()
	ss, ok := st.Shares[marksTag]
	if !ok {
		t.Fatalf("Stats().Shares holds no figure for the forgotten share %v, want its used bytes: %v", marksTag, st.Shares)
	}
	want := int64(len(write.Bytes()) + len(forget.Bytes()))
	if ss.UsedBytes != want {
		t.Fatalf("the forgotten share %d's UsedBytes = %d, want %d: the withheld record's and the forget record's own charged slots",
			marksTag, ss.UsedBytes, want)
	}
	if st.Files != 0 {
		t.Fatalf("Stats().Files = %d, want 0: the withheld record is not a held extent of any file", st.Files)
	}

	// The withheld record is not served: the file it staged reads missing
	// whole, for no entry the index rebuilt holds it.
	h := shareOf(t, j, marksTag)
	p := make([]byte, 64)
	n, missing, _, _, err := h.ReadAt(t.Context(), fileOf(3), 0, p)
	if err != nil || n != 0 || len(missing) != 1 || missing[0] != (Extent{Off: 0, Len: 64}) {
		t.Fatalf("reading the withheld record's extent: n %d, missing %+v, err %v, want the whole extent missing", n, missing, err)
	}
}

// TestReopenAssembled_OffloadedRecordsDecideTheDirtyList holds the dirty
// figures the planted offloaded records decide: an offloaded record above a
// held extent takes its file out of the dirty list and its bytes out of
// Stats().DirtyBytes, while one below the extent's version, and one an
// unmark above clears, leave the extent dirty.
func TestReopenAssembled_OffloadedRecordsDecideTheDirtyList(t *testing.T) {
	f := seededJournal(t)
	over := fileOf(3)    // a mark above the write: clean
	below := fileOf(4)   // a mark below the write's version: dirty
	cleared := fileOf(5) // a mark above the write, an unmark above the mark: dirty

	plantSegment(t, f.seam, segMarks,
		plantRecord(1, marksTag, over, 30, 40, 0, 64),
		plantRecord(8, marksTag, over, 31, 41, 0, 64),
		plantRecord(1, marksTag, below, 32, 50, 0, 64),
		plantRecord(8, marksTag, below, 33, 49, 0, 64))
	plantSegment(t, f.seam, segUnmark,
		plantRecord(1, marksTag, cleared, 40, 60, 0, 64),
		plantRecord(8, marksTag, cleared, 41, 61, 0, 64),
		plantRecord(9, marksTag, cleared, 42, 62, 0, 64))

	j := f.reopen(t, Version{}, nil)
	h := shareOf(t, j, marksTag)
	stop := map[FileID]bool{}
	dirty := map[FileID]bool{}
	for id := range h.DirtyFiles() {
		dirty[id] = true
	}
	for id := range h.Files() {
		stop[id] = true
	}
	if !stop[over] || !stop[below] || !stop[cleared] {
		t.Fatalf("the share's files after the reopen = %v, want all three planted files", maps.Keys(stop))
	}
	if dirty[over] {
		t.Fatalf("the over-marked file %v is in the dirty list, want the offloaded record above its extent to take it out", over)
	}
	if !dirty[below] {
		t.Fatalf("the below-marked file %v is not in the dirty list, want the mark below its version to leave the extent dirty", below)
	}
	if !dirty[cleared] {
		t.Fatalf("the unmarked file %v is not in the dirty list, want the unmark above its offloaded record to leave the extent dirty", cleared)
	}

	// Stats().DirtyBytes holds the two extents still dirty — the
	// below-marked file's and the cleared file's, 64 bytes each — and
	// excludes the over-marked file's extent the open's seed never lists.
	if got := j.Stats().DirtyBytes; got != 128 {
		t.Fatalf("Stats().DirtyBytes after the reopen = %d, want 128: the below-marked and cleared files' two 64-byte extents, the offloaded extent excluded", got)
	}
}

// TestReopenAssembled_CloneTargetSurvivesAndUntilYieldsIt holds the clone
// target's marker at the caller that replays it: the marker the rebuild
// restored carries its spec whole, and Since yields it — until Settle covers
// it, the removal marker a reopened journal starts unsettled.
func TestReopenAssembled_CloneTargetSurvivesAndUntilYieldsIt(t *testing.T) {
	f := seededJournal(t)
	spec := CloneSpec{
		SrcShare: core.ShareID{0x5}, Src: fileOf(9), SrcOff: 48, DstOff: 200, Len: 16, AsOf: v(30),
	}
	clone := raw.Record{
		Kind: 7, FileID: fileOf(2).Bytes(), Seq: 22,
		Version: raw.Version{Counter: 32}, Tag: uint32(cutsTag),
		Offset: 200, Length: 16,
		Clone: &raw.CloneBody{
			Share: spec.SrcShare, File: spec.Src.Bytes(), Off: uint64(spec.SrcOff),
			AsOf: raw.Version{Counter: spec.AsOf.Counter},
		},
	}
	plantSegment(t, f.seam, segCuts,
		plantRecord(1, cutsTag, fileOf(2), 20, 30, 0, 64),
		plantRecord(4, cutsTag, fileOf(2), 21, 31, 40, 0),
		clone)

	j := f.reopen(t, Version{}, nil)
	entries, err := j.Inspect().File(fileOf(2))
	if err != nil {
		t.Fatalf("inspect the cloned file: %v", err)
	}
	var marker *IndexEntry
	for i := range entries {
		if entries[i].Kind == EntryMarker && entries[i].Removal == ChangeCloneTarget {
			marker = &entries[i]
		}
	}
	if marker == nil {
		t.Fatalf("the cloned file's entries hold no clone-target marker: %+v", entries)
	}
	if marker.Clone == nil || *marker.Clone != spec {
		t.Fatalf("the clone-target marker's spec = %+v, want %+v", marker.Clone, spec)
	}

	// Since yields the marker until the caller settles it.
	h := shareOf(t, j, cutsTag)
	markerYielded := func() *CloneSpec {
		for ch, err := range h.Since(t.Context(), fileOf(2), Version{}) {
			if err != nil {
				t.Fatalf("Since over the cloned file: %v", err)
			}
			if ch.Kind == ChangeCloneTarget {
				return ch.Clone
			}
		}
		return nil
	}
	if got := markerYielded(); got == nil || *got != spec {
		t.Fatalf("Since did not yield the clone-target change with its spec, got %+v, want %+v", got, spec)
	}
	h.Settle(fileOf(2), v(32))
	if got := markerYielded(); got != nil {
		t.Fatalf("Since yielded the settled clone-target change %+v, want it settled away", got)
	}
}

// TestReopenAssembled_LowerExtentLimitOpensAndRefusesWrites holds the index
// bound's rule at the caller: a directory reopened under an extent limit
// below the entries it already holds opens — the bound refuses later writes,
// never the open — and every WriteAt and Fill on it is refused naming the
// index bound alone.
func TestReopenAssembled_LowerExtentLimitOpensAndRefusesWrites(t *testing.T) {
	f := seededJournal(t)
	plantSegment(t, f.seam, segPieces,
		plantRecord(1, piecesTag, fileOf(1), 10, 10, 0, 64),
		plantRecord(1, piecesTag, fileOf(1), 11, 20, 0, 32))
	plantSegment(t, f.seam, segCuts,
		plantRecord(1, cutsTag, fileOf(2), 20, 30, 0, 64),
		plantRecord(1, cutsTag, fileOf(3), 30, 40, 0, 64))

	// The lower bound still opens: the rebuild accepted an index over it.
	j, err := f.tryReopen(t, Version{}, func(s *Settings) { s.ExtentLimit = 1 })
	if err != nil {
		t.Fatalf("the reopen under an extent limit below the index's entries: %v, want the open to build", err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ErrClosed) {
			t.Errorf("Close: %v", err)
		}
	})
	withLimit(t, j)
	h := shareOf(t, j, piecesTag)

	_, werr := h.WriteAt(t.Context(), fileOf(1), 96, payload(8), 1)
	var ns *NoSpaceError
	if !errors.As(werr, &ns) || ns.Limit != LimitIndex {
		t.Fatalf("WriteAt on an index over its bound = %v, want a NoSpaceError naming the index bound", werr)
	}
	g := shareOf(t, j, cutsTag)
	ferr := g.Fill(t.Context(), fileOf(2), 128, payload(8), v(40), v(100))
	if !errors.As(ferr, &ns) || ns.Limit != LimitIndex {
		t.Fatalf("Fill on an index over its bound = %v, want a NoSpaceError naming the index bound", ferr)
	}
}

// TestReopenAssembled_ForgetLivenessHoldsItsHoldersTillTheyRetire holds the
// forget record's liveness at the retirement worker that consumes it: a
// segment holding only settled, outranked removals retires with no read of
// its own, the segments holding the forgotten tag's records below the
// forget stay while the forget record lives, and once the last of them
// retires the forget record's own segment retires behind it.
func TestReopenAssembled_ForgetLivenessHoldsItsHoldersTillTheyRetire(t *testing.T) {
	f := seededJournal(t)
	segBelow, segForget, segRemovals := core.SegmentID(941), core.SegmentID(942), core.SegmentID(943)
	tagForgotten := core.ShareTag(31)
	tagRemovals := core.ShareTag(32)
	removed := fileOf(7)

	// The holder segment carries the forgotten tag's record below the
	// forget and one of its records above it: a record above the forget
	// feeds the index, so the file's content — not the withheld record — is
	// what keeps the segment held while it lives.
	below := plantRecord(1, tagForgotten, fileOf(8), 10, 10, 0, 64)
	above := fileOf(9)
	forget := raw.Record{Kind: uint8(14), Seq: 30, Tag: uint32(tagForgotten), FileID: [16]byte{}}
	plantSealed(t, f.seam, segBelow,
		below,
		plantRecord(1, tagForgotten, above, 40, 40, 0, 64))
	plantSealed(t, f.seam, segForget, forget)
	plantSealed(t, f.seam, segRemovals,
		plantRecord(4, tagRemovals, removed, 20, 20, 0, 0),
		plantRecord(6, tagRemovals, removed, 21, 21, 0, 0))

	j := f.reopen(t, Version{}, nil)
	segFile := func(id core.SegmentID) string { return format.SegmentName(id) }
	gone := func(id core.SegmentID) bool {
		for _, name := range f.segmentFileNames(t) {
			if name == segFile(id) {
				return false
			}
		}
		return true
	}
	// The seam counts every read its view took: the reads of the segment's
	// slots the journal read at the arena of its own open included. The
	// counter is the row's witness for the no-read half below — the same
	// counts a worker that read the segment to decide would move.
	readsOf := func(id core.SegmentID) int {
		n := f.seam.CallsFor(kit.OpRead, segFile(id))
		return n + f.seam.CallsFor(kit.OpReadDirect, segFile(id))
	}
	readsBefore := readsOf(segRemovals)

	// The removals are settled at the caller: metadata has recorded both,
	// the truncate the delete outranks and the delete itself. Their segment
	// holds nothing else, so the settle retires it, and the worker's claim
	// — the tracker's Held re-check, the durability gate and the ledger's
	// state — decides from the tables in memory alone: a path that read
	// the segment to learn what it holds moves the counter this row holds
	// against.
	h := shareOf(t, j, tagRemovals)
	h.Settle(removed, v(21))
	waitFor(t, "the settled removals' segment to retire", 30*time.Second, func() bool { return gone(segRemovals) })
	if got := readsOf(segRemovals); got != readsBefore {
		t.Fatalf("retiring the settled removals' segment took the segment's bytes to read: %d read calls at the claim, want %d, the count the open itself left: the claim owes its answer to the tables in memory, not to the segment",
			got-readsBefore, readsBefore)
	}

	// The forget record keeps its holder alive: the segment holding the
	// tag's record below the forget and the forget record's own stay put
	// through many passes of the worker's own rhythm.
	staysThrough(t, "the forget record's own segment to retire", 4*time.Second, func() bool { return gone(segForget) })
	staysThrough(t, "the segment holding the forgotten tag's record below the forget to retire", 4*time.Second, func() bool { return gone(segBelow) })

	// The last holder's retirement is a post-repack retirement simulated
	// by hand: the copy step a repack pass would run — moving the segment's
	// live content out and unholding it — is a stub on this branch, so the
	// row hands the executor the caller's word (NotHeld) that a repacked
	// segment stands for, while the segment's file still holds the content
	// the index names and the row never reads again. The simulation is a
	// real answer for the clause the row pins — the tracker's hold on the
	// forget record survives one holder's retirement, because the durable
	// retirement signal prunes the forget's snapshot whatever word moved
	// the segment — and no better witness exists until the copy pass
	// lands.
	if _, err := j.core.Retirer().Retire(segBelow, segment.NotHeld); err != nil {
		t.Fatalf("retiring the forget record's last holder: %v", err)
	}
	waitFor(t, "the forget record's own segment to retire once its last holder has", 30*time.Second, func() bool { return gone(segForget) })
}
