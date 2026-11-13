package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// stepsPerSegment is the count of records the workload's natural plant puts
// in one segment, small enough that a workload spans several.
const stepsPerSegment = 6

// workFiles are the files the workload spreads over, of two shares.
var workFiles = []core.FileID{
	core.NewFileID([16]byte{0x10, 15: 1}),
	core.NewFileID([16]byte{0x10, 15: 2}),
	core.NewFileID([16]byte{0x10, 15: 3}),
	core.NewFileID([16]byte{0x10, 15: 4}),
}

// tagOf is the share the workload writes each file through.
func tagOf(id core.FileID) core.ShareTag {
	if id == workFiles[2] || id == workFiles[3] {
		return 2
	}
	return 1
}

// step is one operation of the shared workload. The external model test and
// the in-package plant read the same list, so what lands on disk and what
// the model is driven with never drift apart.
type step struct {
	kind    uint8
	file    core.FileID
	off     int64
	n       int64
	tag     core.ShareTag
	seq     uint64
	version core.Version
	mtime   int64

	payload []byte
	clone   *raw.CloneBody
	loss    *raw.LossBody

	// syncedAt is the synced-through offset the record names; zero in the
	// workload, kept a field so a tail case can name one.
	syncedAt int64
}

// stepNotebook is the numbers a seeded workload draws as the journal's own
// writer would: one sequence counter over every record, one version counter
// that every record advances, for no two records of any kind may share a
// pair.
type stepNotebook struct {
	seq     uint64
	version core.Version
	steps   []step
}

func (n *stepNotebook) nextSeq() uint64 {
	n.seq++
	return n.seq
}

func (n *stepNotebook) nextVersion() core.Version {
	n.version.Counter++
	return n.version
}

// seededSteps is the one workload the rebuild tests share: a fixed seed
// passes the same sequence of writes, fills, releases, truncates,
// deallocates, deletes and clone targets, each of a file of one of two
// shares, so a test planting it gets the same records as every other and
// the model — driven beside the plant in the external test — holds the
// state the same steps imply.
func seededSteps(seed uint64) []step {
	rng := rand.New(rand.NewPCG(seed, seed))
	notebook := &stepNotebook{}
	for range 80 {
		k := 1 + uint8(rng.IntN(7)) // the kinds the model drives: 1-7
		st := step{kind: k}
		st.file = workFiles[rng.IntN(len(workFiles))]
		st.tag = tagOf(st.file)
		switch k {
		case 1, 2:
			st.n = 1 + int64(rng.IntN(32))
			st.off = int64(rng.IntN(256)) * 8
			st.payload = make([]byte, st.n)
			for i := range st.payload {
				st.payload[i] = byte(rng.IntN(256))
			}
		case 3:
			st.off = int64(rng.IntN(200)) * 8
			st.n = 8 + int64(rng.IntN(32))*8
		case 4:
			st.off = int64(rng.IntN(120)) * 8 // the new size
			st.n = 0
		case 5:
			st.off = int64(rng.IntN(200)) * 8
			st.n = 8 + int64(rng.IntN(64))*8
		case 6:
			st.off, st.n = 0, 0
		case 7:
			st.off = int64(rng.IntN(120)) * 8
			st.n = 1 + int64(rng.IntN(3))
			st.clone = &raw.CloneBody{
				Share: srcShare(),
				File:  srcFile(),
				Off:   0x101,
				AsOf:  raw.Version{Counter: 9},
			}
		}
		st.seq = notebook.nextSeq()
		st.version = notebook.nextVersion()
		st.mtime = int64(rng.IntN(2)) // 0 suspends the write's time update
		notebook.steps = append(notebook.steps, st)
	}
	return notebook.steps
}

// appendStep appends one step's raw record at the offset the record's own
// header must name, as the journal's writer would have left it.
func appendStep(b []byte, id core.SegmentID, st step) []byte {
	r := raw.Record{
		Identity:      journalID,
		Kind:          st.kind,
		Segment:       uint64(id),
		FileID:        st.file.Bytes(),
		Offset:        uint64(st.off),
		Length:        uint64(st.n),
		Seq:           st.seq,
		Version:       raw.Version{Epoch: st.version.Epoch, Counter: st.version.Counter},
		Tag:           uint32(st.tag),
		SyncedThrough: uint64(st.syncedAt),
		RecordOffset:  uint32(len(b)),
	}
	switch st.kind {
	case 1:
		r.Write = &raw.WriteBody{MTime: st.mtime, Payload: st.payload}
	case 2:
		r.Fill = &raw.FillBody{Payload: st.payload}
	case 7:
		r.Clone = st.clone
	case 10:
		r.Loss = st.loss
	}
	return append(b, r.Bytes()...)
}

// plantSteps writes the workload's records into the rig's directory, a whole
// segment file per perSegment records, and returns the ids planted. Each
// record's own offset in its segment is a multiple of 8 the header verifies
// by, and each segment's header names this journal and its own id.
func (r *rig) plantSteps(steps []step, perSegment int) []core.SegmentID {
	r.t.Helper()
	segs, _ := r.plantStepsRecording(steps, perSegment)
	return segs
}

// plantStepsRecording is plantSteps that also tabulates, for every write,
// fill and clone-target record it plants, the payload range the record's body
// takes in its segment — the bytes a header-only walk must not read. The
// ranges come from the same placement the plant draws, so no second guess
// about a record's extent is needed.
func (r *rig) plantStepsRecording(steps []step, perSegment int) ([]core.SegmentID, []core.SegmentRange) {
	r.t.Helper()
	if perSegment < 1 {
		r.t.Fatalf("fixture: %d records to a segment, want at least 1", perSegment)
	}
	var (
		segs     []core.SegmentID
		payloads []core.SegmentRange
	)
	for first := 0; first < len(steps); {
		id := core.SegmentID(len(segs) + 1)
		n := min(perSegment, len(steps)-first)
		b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
		for _, st := range steps[first : first+n] {
			// The payload follows the 96-byte header: a write's 8 bytes of
			// modification time before its payload, and a fill's payload.
			// A clone target's 56-byte spec is not a payload: it is the one
			// body the scan reads back whole, by design.
			switch st.kind {
			case 1:
				payloads = append(payloads, core.SegmentRange{
					Segment: id, Off: int64(len(b)) + format.RecordHeaderSize,
					Len: 8 + int64(len(st.payload))})
			case 2:
				payloads = append(payloads, core.SegmentRange{
					Segment: id, Off: int64(len(b)) + format.RecordHeaderSize,
					Len: int64(len(st.payload))})
			}
			b = appendStep(b, id, st)
		}
		r.plantSeg(id, b)
		segs = append(segs, id)
		first += n
	}
	return segs, payloads
}

// entriesOf reads every workload file's index entries into one comparable
// map, keyed by the file's identifier.
func entriesOf(ix *index.Index) map[string][]index.Entry {
	out := map[string][]index.Entry{}
	for _, f := range workFiles {
		b := f.Bytes()
		out[string(b[:])] = ix.FileEntries(f)
	}
	return out
}

// sameEntries holds that the per-file entries are equal: the same files,
// the same entries in the same order. A marker's Clone spec compares by
// value, for the read-out hands each caller a copy of its own and two
// identical specs are never one pointer.
func sameEntries(want, got map[string][]index.Entry) bool {
	if len(want) != len(got) {
		return false
	}
	oneEntry := func(a, b index.Entry) bool {
		if a == b {
			return true
		}
		if a.Clone == nil || b.Clone == nil {
			return false
		}
		return a.Kind == b.Kind && a.Extent == b.Extent && a.Version == b.Version &&
			a.Tag == b.Tag && a.Record == b.Record && a.Skip == b.Skip &&
			a.Offloaded == b.Offloaded && a.By == b.By && a.Removal == b.Removal &&
			*a.Clone == *b.Clone
	}
	for k, es := range want {
		other, ok := got[k]
		if !ok || !slices.EqualFunc(es, other, oneEntry) {
			return false
		}
	}
	return true
}

// snapshot is the directory's bytes, every file's, taken before a refusal
// the test holds to have changed nothing but .spare.tmp unlinks.
func (r *rig) snapshot() map[string][]byte {
	r.t.Helper()
	out := map[string][]byte{}
	for _, name := range r.listing() {
		out[name] = r.fileBytes(name)
	}
	return out
}

// assertOnlySpareUnlinks holds that after the call came back, the directory
// holds what it did before, except that .spare.tmp files — which hold
// nothing — may be gone: the rebuild phase of this open writes nothing, and
// creates and renames in no file.
func assertOnlySpareUnlinks(t *testing.T, before map[string][]byte, r *rig) {
	t.Helper()
	for name, b := range before {
		after, err := r.seam.Durable(name)
		if err != nil {
			// A file the call unlinked: only a spare's temporary name is
			// allowed to go, for it never held a record.
			if strings.HasSuffix(name, ".spare.tmp") {
				continue
			}
			t.Errorf("Run: the refusal removed %s, which no rule of this open may", name)
			continue
		}
		if !slices.Equal(b, after) {
			t.Errorf("Run: the refusal changed %s: got %x, want the bytes it held %x", name, after, b)
		}
	}
	for _, name := range r.listing() {
		if _, ok := before[name]; !ok {
			t.Errorf("Run: the refusal left %s in the directory, which it did not hold before and no rule of this open may create", name)
		}
	}
}

// TestRunRebuildIsIdenticalInAnyOrderAndReaderCount feeds the same directory
// through every dispatch order the seam has and under one, two and eight
// readers, and the rebuild must give one identical index: precedence, not
// arrival order, decides each byte, whatever reader count the machine
// allows.
func TestRunRebuildIsIdenticalInAnyOrderAndReaderCount(t *testing.T) {
	steps := seededSteps(7)
	for _, procs := range []int{1, 2, 8} {
		t.Run(fmt.Sprintf("%d readers", procs), func(t *testing.T) {
			var want map[string][]index.Entry
			for i, seed := range []uint64{0, 3, 5, 6, 9, 11, 12, 15, 17, 19, 21, 24, 26, 28, 30, 33, 35, 38, 41, 44} {
				r := newRig(t)
				r.writeFormat()
				r.plantSteps(steps, stepsPerSegment)

				deps := r.runDeps()
				deps.rebuildReaders = procs
				if i > 0 {
					deps.rebuildOrder = shuffleOrder(seed)
				}
				st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
				if err != nil {
					t.Fatalf("Run with the order seed %d on %d readers: %v", seed, procs, err)
				}
				got := entriesOf(st.Rebuilt.Index)
				if want == nil {
					want = got
				} else if !sameEntries(want, got) {
					t.Errorf("Run with the order seed %d on %d readers rebuilt a different index than the plain order did", seed, procs)
				}
			}
		})
	}
}

// TestRunRebuildWithManyMoreSegmentsThanReaders holds a
// dispatch of many more segments than the readers — the shared workload's
// records a segment each, on one reader — gives the index the same plant
// gives under more readers than segments: the bound changes nothing the
// answer says, and every segment is read whatever its count against the
// readers'.
//
// The two legs' plants differ in how records share segments, so their
// record locations differ legitimately; what is pinned is each plant's own
// answer against its own: the one-segment-per-record plant under one reader
// against the same plant under eight, and the single-segment plant the same
// way. Both legs also hold the natural plant's answer against its own
// across reader counts, which the order check's reader-count legs cover.
func TestRunRebuildWithManyMoreSegmentsThanReaders(t *testing.T) {
	steps := seededSteps(11)
	build := func(t *testing.T, perSeg, procs int) map[string][]index.Entry {
		t.Helper()
		r := newRig(t)
		r.writeFormat()
		r.plantSteps(steps, perSeg)
		deps := r.runDeps()
		deps.rebuildReaders = procs
		st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return entriesOf(st.Rebuilt.Index)
	}
	t.Run("many segments on one reader", func(t *testing.T) {
		got := build(t, 1, 1)
		want := build(t, 1, 8)
		if !sameEntries(want, got) {
			t.Errorf("Run: the many-segment plant rebuilt a different index on one reader than on eight")
		}
	})
	t.Run("a single segment on eight readers", func(t *testing.T) {
		got := build(t, len(steps), 8)
		want := build(t, len(steps), 1)
		if !sameEntries(want, got) {
			t.Errorf("Run: the single-segment plant rebuilt a different index on eight readers than on one")
		}
	})
}

// TestRunRebuildScansConcurrentlyUpToTheReaderBound holds the reader bound: at most
// rebuildReaders segment readers run at once. Every segment walk's read past
// the header slot — the attach pass reads each header alone, at offset 0 —
// parks where it stands, so the readers the dispatch runs land in the park
// together and the count of them is the count that ran beside each other.
// With the bound, exactly the bound's readers park and the rest never start; the
// poll below waits on that condition, the real one, and releases. A
// dispatch without the bound parks one walk per segment.
func TestRunRebuildScansConcurrentlyUpToTheReaderBound(t *testing.T) {
	const procs = 2
	r := newRig(t)
	r.writeFormat()
	segs := r.plantSteps(seededSteps(11), 1) // one record a segment: one walk each
	if len(segs) <= procs {
		t.Fatalf("fixture: the workload planted %d segments, want more than the %d readers the check forces", len(segs), procs)
	}

	rec := newReadsOf(r.seam.FS())
	rec.holdSegReadsPastTheHeader()
	defer rec.releaseSegReads()
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), Deps{FS: rec, Clock: r.clk, rebuildReaders: procs}, core.Version{}, journalID, nil)
		done <- err
	}()

	// Wait on the real condition — the bound's readers parked — polling the
	// park's own count, whose reach is the assertion: over the bound is the
	// failure itself, caught the moment it happens.
	deadline := time.After(10 * time.Second)
	for {
		got := rec.inFlightSegReads()
		if got > procs {
			// The failure the assertion names: more readers ran than the
			// bound allows. Release, take the error, report at the assert.
			break // handled below: peakSeg holds it
		}
		if got == procs {
			break // the bound's readers, parked; the rest never started
		}
		select {
		case <-deadline:
			t.Fatalf("Run: only %d segment readers reached their walk in 10s, want the %d the bound runs", got, procs)
		case <-time.After(time.Millisecond):
		}
	}
	rec.releaseSegReads()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return once the reads were released")
	}
	// The parked peak is the whole dispatch's answer: the bound runs
	// readers, and the unbounded break parks one walk per
	// segment — over twenty here.
	if got, want := rec.peakSegmentReads(), procs; got > want {
		t.Errorf("Run: %d segment readers ran beside each other, want at most %d, the bound", got, want)
	}
}

// TestRunRebuildFailsOnAVerifyingReconcileRecord keeps the reconcile ceiling
// through the whole open: a verifying reconcile or reconciled record fails
// the open with ErrNotImplemented, and a record of an unknown kind, a
// non-zero epoch or the largest counter fails it with ErrUnknownFormat, each
// raw enough for errors.Is — the refusals
// reach Run's caller as the open's error — and the directory keeps every
// byte but the .spare.tmp files the attach pass unlinked.
func TestRunRebuildFailsOnAVerifyingReconcileRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  func(id core.SegmentID) raw.Record
		kind string // what the refusal must name
		want error  // the sentinel the open's error must wrap
	}{
		{"reconcile", func(id core.SegmentID) raw.Record {
			return rec(15, func(r *raw.Record) {
				r.Identity = journalID
				r.Segment = uint64(id)
				r.FileID = [16]byte{}
				r.Offset, r.Length = 0, 0
				r.Version = raw.Version{}
				r.Tag = 0
			})
		}, "reconcile", core.ErrNotImplemented},
		{"reconciled", func(id core.SegmentID) raw.Record {
			return rec(16, func(r *raw.Record) {
				r.Identity = journalID
				r.Segment = uint64(id)
				r.FileID = [16]byte{}
				r.Offset, r.Length = 0, 0
				r.Version = raw.Version{}
				r.Tag = 0
				r.Reconciled = &raw.ReconciledBody{Segment: 1, Start: 1, End: 2}
			})
		}, "reconciled", core.ErrNotImplemented},
		{"kind 17", func(core.SegmentID) raw.Record { return rec(17, nil) }, "segment 4", core.ErrUnknownFormat},
		{"kind 128", func(core.SegmentID) raw.Record { return rec(128, nil) }, "segment 4", core.ErrUnknownFormat},
		{"epoch 1", func(core.SegmentID) raw.Record {
			return rec(3, func(r *raw.Record) { r.Version = raw.Version{Epoch: 1, Counter: 200} })
		}, "segment 4", core.ErrUnknownFormat},
		{"the largest version counter", func(core.SegmentID) raw.Record {
			return rec(3, func(r *raw.Record) { r.Version = raw.Version{Counter: math.MaxUint64} })
		}, "segment 4", core.ErrUnknownFormat},
		{"the largest sequence number", func(core.SegmentID) raw.Record {
			return rec(5, func(r *raw.Record) { r.Seq = math.MaxUint64 })
		}, "segment 4", core.ErrUnknownFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			id := core.SegmentID(4)
			r.plantSeg(id, buildSeg(id, tc.rec(id), tc.rec(id)))

			before := r.snapshot()
			_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run over a %s record got err %v, want %v, raw through the open's error", tc.name, err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.kind) {
				t.Errorf("Run: the refusal %q does not name %q", err.Error(), tc.kind)
			}
			assertOnlySpareUnlinks(t, before, r)
		})
	}
}

// TestRunRebuildARefusalBesideAnAttachRangeAnswersTheUnreadableRanges holds
// that a range the attach pass collected is not lost behind a later refusal
// that read nothing wrong: segment 1's header is unreadable on every attempt
// (the attach pass collects the range and goes on), segment 2 holds a
// reconcile record the rebuild refuses. The open answers with the
// unreadable ranges, naming segment 1, not with the raw refusal.
func TestRunRebuildARefusalBesideAnAttachRangeAnswersTheUnreadableRanges(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, buildSeg(1, rec(3, nil)))
	r.plantSeg(2, buildSeg(2, rec(15, func(r *raw.Record) {
		r.FileID = [16]byte{}
		r.Offset, r.Length = 0, 0
		r.Version = raw.Version{}
		r.Tag = 0
	})))
	if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, format.SegmentName(1), 1, 0, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule: %v", err)
	}

	_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Run with segment 1's header unreadable and a reconcile record in segment 2 got err %v, want an UnreadableRangesError", err)
	}
	if !slices.ContainsFunc(ure.Ranges, func(sr core.SegmentRange) bool { return sr.Segment == 1 }) {
		t.Errorf("Run: the refusal names ranges %v and none of segment 1, the unreadable header", ure.Ranges)
	}
}

// TestRunRebuildCarriesTheConfiguredExtentBound holds that the index the
// rebuild hands on carries the bound the open was configured with: a
// directory already holding more entries than the bound still opens, and a
// write past it is then refused. The same directory rebuilt with no bound set
// admits the write.
func TestRunRebuildCarriesTheConfiguredExtentBound(t *testing.T) {
	probe := core.Extent{Off: 1 << 20, Len: 8}
	for _, tc := range []struct {
		name    string
		limit   int64
		refused bool
	}{
		{"a bound below the entries on disk", 1, true},
		{"no bound", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSteps(seededSteps(5), stepsPerSegment)
			deps := r.runDeps()
			deps.ExtentLimit = tc.limit

			st, err := Run(context.Background(), deps, core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("Run with the bound %d over a directory past it: %v, want the open to build", tc.limit, err)
			}
			_, err = st.Rebuilt.Index.CheckWrite(workFiles[0], probe)
			if got := err != nil; got != tc.refused {
				t.Errorf("CheckWrite after a rebuild with the bound %d got err %v, want refused=%v", tc.limit, err, tc.refused)
			}
		})
	}
}

// TestRunRebuildReleasesTheConvertedRecordsAndKeepsTheAside holds that the
// scans the open carries on keep the aside headers the later phases read but
// not the converted content records, which the index already holds.
func TestRunRebuildReleasesTheConvertedRecordsAndKeepsTheAside(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, buildSeg(1, rec(3, nil), rec(10, func(r *raw.Record) {
		r.Seq, r.Version = 101, raw.Version{Counter: 201}
		r.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8}
	})))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(st.Rebuilt.Scans) != 1 {
		t.Fatalf("Run: %d scans, want 1", len(st.Rebuilt.Scans))
	}
	sc := st.Rebuilt.Scans[0]
	if sc.Records != nil {
		t.Errorf("Run: the scan still holds %d converted records, want them released once the index holds them", len(sc.Records))
	}
	if len(sc.Aside) != 1 {
		t.Errorf("Run: the scan holds %d aside records, want the 1 the later phases read", len(sc.Aside))
	}
}

// TestRunRebuildEIOTwiceThenSucceedsBuildsTheSameIndex holds the recovery
// half of the read-fault rule through the open: a segment read that faults
// twice and then succeeds rebuilds the index the same directory with the
// fault off gives. The attach pass's one healthy header read is read 1 of the
// segment, so the fault starts at read 2 — the rebuild's own first read —
// and the recorder proves both faults landed on reads past the header, so the
// retry that cleared them was the rebuild's.
func TestRunRebuildEIOTwiceThenSucceedsBuildsTheSameIndex(t *testing.T) {
	steps := seededSteps(5)
	seg := format.SegmentName(2)
	build := func(faulted bool) (map[string][]index.Entry, []oneRead) {
		r := newRig(t)
		r.writeFormat()
		r.plantSteps(steps, stepsPerSegment)
		if faulted {
			if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, seg, 2, 2, syscall.EIO)); err != nil {
				t.Fatalf("fixture: schedule: %v", err)
			}
		}
		rec := newReadsOf(r.seam.FS())
		st, err := Run(context.Background(), Deps{FS: rec, Clock: r.clk}, core.Version{}, journalID, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return entriesOf(st.Rebuilt.Index), rec.failedReads()
	}
	want, noFaults := build(false)
	got, failed := build(true)
	if len(noFaults) != 0 {
		t.Fatalf("fixture: the no-fault run saw failed reads %v, want none", noFaults)
	}
	if len(failed) != 2 {
		t.Fatalf("Run: the recorder saw %d failed reads past the header, want the 2 the fault schedule names: %v", len(failed), failed)
	}
	for _, f := range failed {
		if f.name != seg || f.off == 0 {
			t.Errorf("Run: a fault landed on %s at %d, want a read of %s past its header", f.name, f.off, seg)
		}
	}
	if !sameEntries(want, got) {
		t.Errorf("Run: the two-EIO rebuild differs from the no-fault one: got %+v, want %+v", got, want)
	}
}

// TestRunRebuildEIOEveryAttemptFailsWithUnreadableRangesNamingTheSegment
// holds the refusal half of the read-fault rule: a segment read no bounded attempt completes
// fails the whole open with *core.UnreadableRangesError naming that segment,
// with the directory keeping the bytes it held. The fault lets the attach
// pass's header read through and fails every read after it, so the failure
// is the rebuild's own, whichever read of the walk it falls on.
func TestRunRebuildEIOEveryAttemptFailsWithUnreadableRangesNamingTheSegment(t *testing.T) {
	for from := 2; from <= 6; from++ {
		t.Run(fmt.Sprintf("from read %d", from), func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSteps(seededSteps(5), stepsPerSegment)
			if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, format.SegmentName(2), from, 0, syscall.EIO)); err != nil {
				t.Fatalf("fixture: schedule: %v", err)
			}
			before := r.snapshot()

			_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
			var ure *core.UnreadableRangesError
			if !errors.As(err, &ure) {
				t.Fatalf("Run with every read of segment 2 past read %d failing got err %v, want an UnreadableRangesError", from, err)
			}
			if !slices.ContainsFunc(ure.Ranges, func(sr core.SegmentRange) bool { return sr.Segment == 2 }) {
				t.Errorf("Run: the refusal names ranges %v and none of segment 2, the one whose reads failed", ure.Ranges)
			}
			assertOnlySpareUnlinks(t, before, r)
		})
	}
}

// TestRunRebuildASizeOrCloneSpecReadThatFailsIsUnreadableRanges: the two
// reads the walk's own do not make — the segment's size and a clone target's
// spec — answer a failure no attempt cleared as the unreadable range the open
// refuses on, never as the raw I/O error. The size fault fails every open of
// segment 2 after the attach pass's; the spec fault fails every read after
// the attach pass's header read and the walk's two reads of the record's slot.
func TestRunRebuildASizeOrCloneSpecReadThatFailsIsUnreadableRanges(t *testing.T) {
	cloneSeg := func(id core.SegmentID) []byte {
		return buildSeg(id, rec(7, func(r *raw.Record) {
			r.Offset, r.Length = 0x100, 3
			r.Clone = &raw.CloneBody{Share: srcShare(), File: srcFile(), Off: 0x101, AsOf: raw.Version{Counter: 9}}
		}))
	}
	for _, tc := range []struct {
		name string
		rule kit.Rule
		want core.SegmentRange // the range the refusal must name
	}{
		{"the size", kit.FailingFrom(kit.OpOpen, format.SegmentName(2), 2, 0, syscall.EIO),
			core.SegmentRange{Segment: 2, Off: 0, Len: math.MaxInt64}},
		{"the clone spec", kit.FailingFrom(kit.OpRead, format.SegmentName(2), 4, 0, syscall.EIO),
			core.SegmentRange{Segment: 2, Off: format.SegmentHeaderSize + format.RecordHeaderSize, Len: 56}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.plantSeg(2, cloneSeg(2))
			if err := r.seam.Schedule(tc.rule); err != nil {
				t.Fatalf("fixture: schedule: %v", err)
			}

			_, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
			var ure *core.UnreadableRangesError
			if !errors.As(err, &ure) {
				t.Fatalf("Run with %s read failing got err %v, want an UnreadableRangesError", tc.name, err)
			}
			if !slices.Contains(ure.Ranges, tc.want) {
				t.Errorf("Run: the refusal names ranges %v, want one to be %+v", ure.Ranges, tc.want)
			}
		})
	}
}

// TestRebuildIndexTwoSegmentsFailingAtOnceAnswerWithTheLowestSegmentID holds
// the precedence between failures that land together: both segments' walks
// are parked inside the read that widens a record slot to its header, so each worker is past the bound's
// context check when released, and both fail on the same kind of refusal.
// The phase answers with the failure of the lowest segment id, whatever the
// order the segments were dispatched in and whichever worker failed first.
func TestRebuildIndexTwoSegmentsFailingAtOnceAnswerWithTheLowestSegmentID(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	for _, id := range []core.SegmentID{1, 3} {
		r.plantSeg(id, buildSeg(id, rec(17, nil)))
	}
	rec := newReadsOf(r.seam.FS())
	rec.holdSegReadsLongerThan(format.SealMarkerSize)
	ctx := context.Background()
	rd := newReader(ctx, rec, r.clk, nil)
	// Dispatched highest first, so an answer taken by dispatch order or by
	// the first failure to land is segment 3's.
	own := []SegmentInfo{
		{ID: 3, Header: format.SegmentHeader{Identity: journalID, ID: 3}},
		{ID: 1, Header: format.SegmentHeader{Identity: journalID, ID: 1}},
	}

	done := make(chan error, 1)
	go func() {
		_, err := rebuildIndex(ctx, rd, journalID, own, core.Version{}, math.MaxInt64, 2, nil, nil, false)
		done <- err
	}()
	for rec.inFlightSegReads() < 2 {
		runtime.Gosched()
	}
	rec.releaseSegReads()

	err := <-done
	if !errors.Is(err, core.ErrUnknownFormat) {
		t.Fatalf("rebuildIndex with two segments refused got err %v, want ErrUnknownFormat", err)
	}
	if got := err.Error(); !contains(got, "segment 1:") || contains(got, "segment 3:") {
		t.Errorf("rebuildIndex with segments 3 and 1 refused got %q, want the refusal of segment 1, the lowest id", got)
	}
}

// TestRebuildIndexAFailedSegmentStopsTheDispatch: with one reader slot and
// segment 1's read parked, then released to fail, the slot frees only because
// segment 1 failed, and the dispatch does not start segment 2 behind that
// error. The dispatcher is blocked on the slot while segment 1 is parked, so
// the slot it takes is the one the failure freed; the scan-start hook names
// every segment whose scan began, and only segment 1's did.
func TestRebuildIndexAFailedSegmentStopsTheDispatch(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSteps(seededSteps(5), stepsPerSegment)
	if err := r.seam.Schedule(kit.FailingFrom(kit.OpRead, format.SegmentName(1), 1, 0, syscall.EIO)); err != nil {
		t.Fatalf("fixture: schedule: %v", err)
	}

	rec := newReadsOf(r.seam.FS())
	rec.holdSegReadsPastTheHeader()
	ctx := context.Background()
	rd := newReader(ctx, rec, r.clk, nil)
	own := []SegmentInfo{
		{ID: 1, Header: format.SegmentHeader{Identity: journalID, ID: 1}},
		{ID: 2, Header: format.SegmentHeader{Identity: journalID, ID: 2}},
	}
	var (
		mu      sync.Mutex
		started []core.SegmentID
	)
	onStart := func(id core.SegmentID) {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, id)
	}

	done := make(chan error, 1)
	go func() {
		_, err := rebuildIndex(ctx, rd, journalID, own, core.Version{}, math.MaxInt64, 1, nil, onStart, false)
		done <- err
	}()
	// Segment 1's walk is parked in its first read. Polling the condition
	// itself; the test timeout is the backstop.
	for rec.inFlightSegReads() < 1 {
		runtime.Gosched()
	}
	rec.releaseSegReads()
	err := <-done
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("rebuildIndex with segment 1 failing got err %v, want an UnreadableRangesError", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []core.SegmentID{1}; !slices.Equal(started, want) {
		t.Errorf("rebuildIndex started the scans of segments %v, want %v: none behind segment 1's error", started, want)
	}
}

// TestRebuildIndexCancelledMidFeedChangesNothing holds that a
// cancellation that lands while a segment's scan is inside the bound
// answers with the context's error, the directory keeps the bytes it held
// — the phase writes nothing — and no half-built answer comes back: the
// index and counters of the refused call are empty.
//
// The check calls rebuildIndex directly, without the attach pass before
// it, so the held read it stops is the feed's own, the one the open's
// boundary checks never isolate: the attach pass reads every segment's
// header too, and a hold Run reaches first parks there.
func TestRebuildIndexCancelledMidFeedChangesNothing(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	steps := seededSteps(4)
	ids := r.plantSteps(steps, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := r.seam.HoldFirst(t, kit.OpRead, format.SegmentName(2))
	before := r.snapshot()
	own := make([]SegmentInfo, len(ids))
	for i, id := range ids {
		own[i] = SegmentInfo{ID: id}
	}

	done := make(chan rebuiltOrErr, 1)
	rd := newReader(ctx, r.seam.FS(), r.clk, nil)
	go func() {
		out, err := rebuildIndex(ctx, rd, journalID, own, core.Version{}, math.MaxInt64, 1, nil, nil, false)
		done <- rebuiltOrErr{out, err}
	}()

	<-held.Reached() // a read of segment 2 is inside the bound
	cancel()
	held.Release()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("rebuildIndex cancelled mid-feed got err %v, want context.Canceled", got.err)
		}
		if got.out.Index != nil || got.out.Counters != nil {
			t.Errorf("rebuildIndex cancelled mid-feed returned an answer of index %v and counters %v, want none",
				got.out.Index, got.out.Counters)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("rebuildIndex cancelled mid-feed did not return")
	}
	assertOnlySpareUnlinks(t, before, r)
}

// rebuiltOrErr is the cancelled feed's answer channel payload.
type rebuiltOrErr struct {
	out rebuilt
	err error
}

// TestRunRebuildRefusesAFloorWithAnEpoch holds the refusal of a floor
// whose epoch half is not zero is refused the way a record carrying one is,
// and the refused open returns no half-built state.
func TestRunRebuildRefusesAFloorWithAnEpoch(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.plantSteps(seededSteps(2), stepsPerSegment)

	st, err := Run(context.Background(), r.runDeps(), core.Version{Epoch: 1, Counter: 40}, journalID, nil)
	if err == nil {
		t.Fatalf("Run with a floor of a non-zero epoch returned no error, want the open to fail")
	}
	if !errors.Is(err, core.ErrUnknownFormat) {
		t.Errorf("Run: the epoch floor got err %v, want ErrUnknownFormat, the refusal a record's epoch takes", err)
	}
	if st.Rebuilt.Index != nil {
		t.Errorf("Run: a refused floor returned state with an index, want none")
	}
}

// TestRunRebuildCountsOneResyncPerScan sums the scans' resync counts: a bad
// slot before a verifying record in each of two segments counts two, the
// count being scans that resynchronised, not bad ranges.
func TestRunRebuildCountsOneResyncPerScan(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	build := func(id core.SegmentID) []byte {
		b := raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes()
		b = append(b, 0, 0, 0, 0, 0, 0, 0, 1) // a bad slot the walk resynchronises past
		rc := rec(3, nil)
		rc.Identity = journalID
		rc.Segment = uint64(id)
		rc.RecordOffset = uint32(len(b))
		b = append(b, rc.Bytes()...)
		return b
	}
	r.plantSeg(core.SegmentID(3), build(3))
	r.plantSeg(core.SegmentID(9), build(9))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := st.Rebuilt.ScanResyncs, 2; got != want {
		t.Errorf("Run: ScanResyncs got %d, want %d, one per scan that resynchronised past a bad slot", got, want)
	}
}

// TestRunRebuildTooShortForASlotDoesNotFailTheOpen holds the empty scan through
// the open: a segment too short to hold a record slot contributes a scan of
// nothing and fails nothing — the tail phases' judgement is later.
func TestRunRebuildTooShortForASlotDoesNotFailTheOpen(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(8)
	r.plantSeg(id, append(raw.SegmentHeader{Identity: journalID, ID: uint64(id)}.Bytes(), make([]byte, 40)...))

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run over a segment shorter than one record slot: %v, want no error", err)
	}
	for _, sc := range st.Rebuilt.Scans {
		if sc.ID == id && (len(sc.Records) != 0 || len(sc.Aside) != 0) {
			t.Errorf("Run: the scan of the short segment %d holds records or asides, want none: no slot was scanned", id)
		}
	}
}

// TestRunRebuildAsideCarriedNotFed pins that aside records are carried, not fed, through the rebuild:
// a loss, hold, unhold, stamp or forget record's header reached Scans and
// nothing of it was fed — the index holds no entry of its file — and the
// aside kinds' synced-through offsets count into their scan like any
// verifying record's, so an aside record carrying the highest one decides
// the scan's SyncedThrough.
func TestRunRebuildAsideCarriedNotFed(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	id := core.SegmentID(6)
	asideFile := [16]byte{0x20, 15: 5}
	seg := buildSeg(id,
		rec(10, func(rc *raw.Record) {
			rc.FileID = asideFile
			rc.Loss = &raw.LossBody{Dropped: 99, Flags: 1 | 1<<8}
			rc.SyncedThrough = 240 // no other record in the segment names higher
		}),
		rec(1, func(rc *raw.Record) {
			rc.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
		}))
	r.plantSeg(id, seg)

	st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var scan *segmentScan
	for i := range st.Rebuilt.Scans {
		if st.Rebuilt.Scans[i].ID == id {
			scan = &st.Rebuilt.Scans[i]
		}
	}
	if scan == nil {
		t.Fatalf("Run: the scans name no segment %d, want the planted one", id)
	}
	if len(scan.Aside) != 1 || scan.Aside[0].Header.Kind != format.KindLoss {
		t.Fatalf("Run: the scan of segment %d sets %d records aside, want the one loss record of kind %s", id, len(scan.Aside), format.KindLoss)
	}
	if got, want := scan.SyncedThrough, int64(240); got != want {
		t.Errorf("Run: the scan's SyncedThrough got %d, want %d, the offset its aside record carries", got, want)
	}
	if got := len(st.Rebuilt.Index.FileEntries(core.NewFileID(asideFile))); got != 0 {
		t.Errorf("Run: the aside record's file holds %d index entries, want none: an aside record is fed nowhere", got)
	}
}

// oneRead is one read a recorder saw: the file's name and the byte range it
// returned bytes of.
type oneRead struct {
	name string
	off  int64
	n    int64
}

// readsOf is a pass-through recorder over one FS: every ReadAt a file read
// made, its name, its offset and the bytes it returned, as the device saw
// them. It holds no opinion and answers nothing — the FS underneath does —
// so a check can ask what bytes a rebuild read without changing what the
// rebuild sees. It is the recording wrapper the payload acceptance wraps
// the seam with: what is recorded is the requirement — that no payload byte
// of a write or fill record was read.
type readsOf struct {
	fs storage.FS

	mu    sync.Mutex
	calls []oneRead
	// failed is every read past a header that returned an error other than
	// the end of the file, as the device reported it.
	failed []oneRead

	// inSeg is how many gated segment reads are mid-call now, and peakSeg the
	// most there ever were at once: the bound the rebuild's readers allow
	// shows as the peak, for each scan's reads are sequential inside its
	// segment, so the peak names how many readers ran beside each other.
	inSeg   int
	peakSeg int

	// gate, once closed, holds every later segment read past the header
	// slot at offset 0 — the attach pass reads each header alone — until
	// ReleaseSegReads frees them: the fault that stops each reader mid-walk
	// so the count the bound allows is the one that lands.
	gate  chan struct{}
	gated bool
	// gateOver is the length a read must exceed to be held: zero holds every
	// read past the header.
	gateOver int
}

func newReadsOf(fs storage.FS) *readsOf { return &readsOf{fs: fs} }

// holdSegReadsPastTheHeader makes every later segment read past offset 0
// block where it is, inside the read, until ReleaseSegReads.
func (r *readsOf) holdSegReadsPastTheHeader() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = make(chan struct{})
	r.gated = true
}

// holdSegReadsLongerThan holds only the segment reads past offset 0 longer
// than n bytes, until ReleaseSegReads: the walk's first read of a record slot
// is a seal marker's 32 bytes, and the read that widens it to the header is
// the one a check parks to stop the walk inside a record.
func (r *readsOf) holdSegReadsLongerThan(n int) {
	r.holdSegReadsPastTheHeader()
	r.mu.Lock()
	r.gateOver = n
	r.mu.Unlock()
}

// releaseSegReads frees every read holdSegReadsPastTheHeader is holding.
func (r *readsOf) releaseSegReads() {
	r.mu.Lock()
	gate := r.gate
	r.gated = false
	r.gate = nil
	r.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// inFlightSegReads returns how many gated segment reads are blocked now.
func (r *readsOf) inFlightSegReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inSeg
}

func (r *readsOf) Create(name string) (storage.File, error) { return r.fs.Create(name) }

func (r *readsOf) Open(name string) (storage.File, error) {
	f, err := r.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return readRec{f: f, name: name, of: r}, nil
}

func (r *readsOf) Rename(a, b string) error { return r.fs.Rename(a, b) }
func (r *readsOf) Remove(n string) error    { return r.fs.Remove(n) }
func (r *readsOf) List() ([]string, error)  { return r.fs.List() }
func (r *readsOf) SyncDir() error           { return r.fs.SyncDir() }

// saw returns every read recorded so far, the calls in the order they
// returned.
func (r *readsOf) saw() []oneRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// failedReads returns every read past a header the device failed.
func (r *readsOf) failedReads() []oneRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.failed)
}

// peakSegmentReads returns the most segment reads that were mid-call at
// once.
func (r *readsOf) peakSegmentReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peakSeg
}

// readRec is one opened file whose reads are recorded.
type readRec struct {
	f    storage.File
	name string
	of   *readsOf
}

func (f readRec) ReadAt(p []byte, off int64) (int, error) {
	kind, _ := format.ClassifyName(f.name)
	if kind != format.NameSegment || off == 0 {
		// The attach pass reads each header alone, at offset 0; the gated
		// reads are the walks', which start at the first record slot.
		return f.f.ReadAt(p, off)
	}
	f.of.mu.Lock()
	gate := f.of.gate
	if len(p) <= f.of.gateOver {
		gate = nil
	}
	f.of.inSeg++
	if f.of.inSeg > f.of.peakSeg {
		f.of.peakSeg = f.of.inSeg
	}
	f.of.mu.Unlock()
	if gate != nil {
		<-gate // parked where the reader the bound ran mid-walk stands
	}
	f.of.mu.Lock()
	f.of.inSeg--
	f.of.mu.Unlock()
	n, err := f.f.ReadAt(p, off)
	f.of.mu.Lock()
	if n > 0 {
		f.of.calls = append(f.of.calls, oneRead{name: f.name, off: off, n: int64(n)})
	}
	if err != nil && !errors.Is(err, io.EOF) {
		f.of.failed = append(f.of.failed, oneRead{name: f.name, off: off, n: int64(n)})
	}
	f.of.mu.Unlock()
	return n, err
}

func (f readRec) ReadAtDirect(p []byte, off int64) (int, error) { return f.f.ReadAtDirect(p, off) }
func (f readRec) WriteAt(p []byte, off int64) (int, error)      { return f.f.WriteAt(p, off) }
func (f readRec) Allocate(off, n int64) error                   { return f.f.Allocate(off, n) }
func (f readRec) Sync() error                                   { return f.f.Sync() }
func (f readRec) Truncate(size int64) error                     { return f.f.Truncate(size) }
func (f readRec) Size() (int64, error)                          { return f.f.Size() }
func (f readRec) Close() error                                  { return f.f.Close() }
func (f readRec) Name() string                                  { return f.name }

// TestRunRebuildReadsNoPayloadOfAnyNewestOrOlderSegment holds the no-payload-read
// half of the scan rule through the whole open: no recorded read overlaps a write or fill
// record's payload range, in any segment, newest or older alike. The
// rule covers the segments that are not the newest of
// a stream — the header-only walk reads no write's or fill's body anywhere,
// so every segment of the plant is asserted the same way; a clone target's
// 56-byte spec is the one body the scan reads back whole, by design, and
// the newest segment's tail proof arrives with the phase that owns it.
func TestRunRebuildReadsNoPayloadOfAnyNewestOrOlderSegment(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	payloads := r.plantStepsPayloads(seededSteps(3), stepsPerSegment)
	if len(payloads) == 0 {
		t.Fatalf("fixture: the workload planted no write, fill or clone-target record")
	}

	rec := newReadsOf(r.seam.FS())
	if _, err := Run(context.Background(), Deps{FS: rec, Clock: r.clk}, core.Version{}, journalID, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// No read the open made overlaps a payload range of the segment it read.
	sawAny := false
	for _, read := range rec.saw() {
		kind, id := format.ClassifyName(read.name)
		if kind != format.NameSegment {
			continue
		}
		sawAny = true
		for _, pr := range payloads {
			if pr.Segment != id {
				continue
			}
			if read.off+read.n > pr.Off && read.off < pr.Off+pr.Len {
				t.Errorf("Run: a read of segment %d at [%d,+%d) overlaps the payload range [%d,+%d) it must not read",
					id, read.off, read.n, pr.Off, pr.Len)
			}
		}
	}
	if !sawAny {
		t.Fatalf("fixture: the recorder saw no read of a segment, want the rebuild's header reads recorded")
	}
}

// plantStepsPayloads plants the workload and returns its payload ranges,
// for the test that asserts nothing read them.
func (r *rig) plantStepsPayloads(steps []step, perSegment int) []core.SegmentRange {
	r.t.Helper()
	_, payloads := r.plantStepsRecording(steps, perSegment)
	return payloads
}

// TestScanSegmentZeroTailIsSearchedInWindows: a segment whose records are
// followed by a multi-megabyte zero tail — the shape of a stream's newest
// segment, allocated whole — is walked in reads of the window's size, not one
// small read per 8 bytes. The recorded reads, passed through to the real
// bytes, are bounded by a small constant times the window count plus the
// records stepped through.
func TestScanSegmentZeroTailIsSearchedInWindows(t *testing.T) {
	id := core.SegmentID(3)
	seg := buildSeg(id, rec(3, nil), rec(5, func(r *raw.Record) { r.Seq = 101; r.Version = raw.Version{Counter: 201} }))
	records := 2
	tail := 4 << 20
	seg = append(seg, make([]byte, tail)...)

	r := newRig(t)
	r.plantSeg(id, seg)
	rec := newReadsOf(r.seam.FS())
	rd := newReader(context.Background(), rec, r.clk, nil)
	got, err := scanSegment(rd, journalID, SegmentInfo{ID: id, Header: format.SegmentHeader{Identity: journalID, ID: id}})
	if err != nil {
		t.Fatalf("scanSegment over a zero tail: %v", err)
	}
	if len(got.Records) != records {
		t.Fatalf("scanSegment attached %d records, want the %d planted", len(got.Records), records)
	}
	limit := 2*(len(seg)/(1<<20)+1) + records + 4
	if n := len(rec.saw()); n > limit {
		t.Errorf("scanSegment over a %d-byte zero tail made %d reads, want at most %d: one per window, not one per slot", tail, n, limit)
	}
}
