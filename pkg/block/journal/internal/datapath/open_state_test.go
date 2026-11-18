package datapath

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/recovery"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// These rows plant a rebuilt journal directory through the real recovery run
// and open a core from what it assembled: the registrations, the dirty seed
// and the worker start below are the open payload's own wiring, checked over
// the same records a journal that crashed would leave.

// The tags of the planted directory: tag 7 carries a forget with the
// withheld write below it, tag 3 a release nothing on disk outranks, and tag 9
// a written dirty file beside a written file its offloaded record covers.
const (
	openForgetTag  = core.ShareTag(7)
	openReleaseTag = core.ShareTag(3)
	openDirtyTag   = core.ShareTag(9)
)

// The planted segments sit high, above every id the core's spare maker draws
// from zero, so a fresh spare never meets a set the rebuilt ones own.
const (
	openForgetSeg  = core.SegmentID(900) // active, carrying the forget alone
	openHeldSeg    = core.SegmentID(901) // sealed, holding the tag's withheld write
	openReleaseSeg = core.SegmentID(902) // sealed, holding the outranked release alone
	openDirtySeg   = core.SegmentID(903) // active, holding the dirty file's write
	openOffSeg     = core.SegmentID(904) // active, holding the offloaded file
)

// The files the planted records name, one per planted shape.
var (
	openForgottenFile = testFile(1) // the write below the forget
	openReleasedFile  = testFile(2) // the release alone holds
	openDirtyFile     = testFile(3) // the written, not-offloaded file
	openOffloadedFile = testFile(4) // the write its offloaded record covers
)

// openRecord builds one planted record of kind k under tag over [off, off+n)
// of f, at the numbers the plant chose, with a write's body carrying the
// payload its length spells.
func openRecord(k uint8, tag core.ShareTag, f core.FileID, seq, counter uint64, off, n int64) raw.Record {
	r := raw.Record{
		Kind:    k,
		FileID:  f.Bytes(),
		Seq:     seq,
		Version: raw.Version{Counter: counter},
		Tag:     uint32(tag),
		Offset:  uint64(off),
		Length:  uint64(n),
	}
	if k == 1 {
		r.Write = &raw.WriteBody{MTime: 1, Payload: make([]byte, n)}
	}
	return r
}

// openForget builds one forget record: it names its tag and its sequence
// number alone — no file, no version, no extent.
func openForget(tag core.ShareTag, seq uint64) raw.Record {
	return raw.Record{Kind: 14, Tag: uint32(tag), Seq: seq}
}

// openPlant writes one planted journal file as a synced writer would have
// left it: the records in their order at their own slots, and for a sealed
// segment the seal the journal's seal writer leaves — the marker, the footer
// of one entry per record and the trailer — then the directory synced.
func openPlant(t *testing.T, seam *kit.Seam, name string, sealed bool, id core.SegmentID, recs ...raw.Record) {
	t.Helper()
	b := raw.SegmentHeader{Identity: testIdentity, ID: uint64(id)}.Bytes()
	for i := range recs {
		recs[i].Identity = testIdentity
		recs[i].Segment = uint64(id)
		recs[i].RecordOffset = uint32(len(b))
		b = append(b, recs[i].Bytes()...)
	}
	if sealed {
		markerOff := len(b)
		b = append(b, raw.SealMarker{Identity: testIdentity, Segment: uint64(id), Offset: uint64(markerOff)}.Bytes()...)
		var entries [][raw.CatalogEntrySize]byte
		for i := range recs {
			entries = append(entries, raw.Entry{
				FileID:       recs[i].FileID,
				Offset:       recs[i].Offset,
				RecordOffset: recs[i].RecordOffset,
				Length:       recs[i].Length,
				Seq:          recs[i].Seq,
				Version:      recs[i].Version,
				Kind:         recs[i].Kind,
				Tag:          recs[i].Tag,
			}.Bytes())
		}
		var footer []byte
		for _, e := range entries {
			footer = append(footer, e[:]...)
		}
		// 1 is the catalog layout version the format writes: the entries go
		// first, and the trailer checksums them and appends its own 32 bytes.
		b = append(append(b, footer...), raw.Trailer(1, uint64(markerOff+raw.SealMarkerSize), entries...)...)
	}
	f, err := seam.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	if err := seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory after %s: %v", name, err)
	}
}

// openedCore replays one open over the planted directory: recovery rebuilds
// the whole state, and the core opens from what it assembled, carrying the
// dirty seed an open derives from the rebuilt index — one became-dirty delta
// per file holding content whose offloaded bit is unset. The fixture checks
// its own collected facts, so a plant the rebuild read differently from what
// the rows expect fails naming the shape.
func openedCore(t *testing.T) *Core {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	t.Cleanup(seam.Abandon)

	ff, err := seam.Create(format.FormatFileName)
	if err != nil {
		t.Fatalf("create the format file: %v", err)
	}
	if _, err := ff.WriteAt(format.AppendFormatFile(nil, format.FormatFile{
		Identity: testIdentity, Installation: core.InstallationID{'t', '7', '6'},
	}), 0); err != nil {
		t.Fatalf("write the format file: %v", err)
	}
	if err := ff.Sync(); err != nil {
		t.Fatalf("sync the format file: %v", err)
	}
	if err := ff.Close(); err != nil {
		t.Fatalf("close the format file: %v", err)
	}
	if err := seam.SyncDir(); err != nil {
		t.Fatalf("sync the directory after the format file: %v", err)
	}

	// Tag 7's forget, and the write its tag left below it in a sealed
	// segment: the rebuild withholds the write whole — forgotten content —
	// so nothing counts on the segment beside the forget's snapshot.
	openPlant(t, seam, format.SegmentName(openForgetSeg), false, openForgetSeg, openForget(openForgetTag, 30))
	openPlant(t, seam, format.SegmentName(openHeldSeg), true, openHeldSeg,
		openRecord(1, openForgetTag, openForgottenFile, 10, 10, 0, 64))
	// The release nothing on disk outranks, the only record of its file,
	// sealed alone in its segment.
	openPlant(t, seam, format.SegmentName(openReleaseSeg), true, openReleaseSeg,
		openRecord(3, openReleaseTag, openReleasedFile, 2, 2, 0, 16))
	// The dirty file's write, and beside it a file whose write its offloaded
	// record covers, so the rebuilt index answers one dirty and one offloaded.
	openPlant(t, seam, format.SegmentName(openDirtySeg), false, openDirtySeg,
		openRecord(1, openDirtyTag, openDirtyFile, 40, 40, 0, 64))
	openPlant(t, seam, format.SegmentName(openOffSeg), false, openOffSeg,
		openRecord(1, openDirtyTag, openOffloadedFile, 50, 50, 0, 64),
		openRecord(8, openDirtyTag, openOffloadedFile, 51, 51, 0, 64))

	st, err := recovery.Run(context.Background(), recovery.Deps{
		FS:         seam.FS(),
		Clock:      kit.NewStopwatchClock(),
		SegmentCap: testSegmentCap,
	}, core.Version{}, testIdentity, nil)
	if err != nil {
		t.Fatalf("recovery.Run over the planted directory: %v", err)
	}
	if st.Assembled.Set == nil || st.Rebuilt.Index == nil || st.Rebuilt.Counters == nil {
		t.Fatal("open fixture: the rebuild left the assembled state half built")
	}
	if len(st.Assembled.Forgets) != 1 || st.Assembled.Forgets[0].Rec.Seg != openForgetSeg {
		t.Fatalf("open fixture: the rebuild collected forgets %+v, want the one carried in segment %d", st.Assembled.Forgets, openForgetSeg)
	}
	if segs := st.Assembled.Forgets[0].Segs; len(segs) != 1 || segs[0] != openHeldSeg {
		t.Fatalf("open fixture: the forget's snapshot got %v, want [%d]: the one segment holding the tag's records below it", segs, openHeldSeg)
	}
	var releases int
	for _, r := range st.Assembled.Live {
		if r.Kind != format.KindRelease {
			continue
		}
		releases++
		if r.Seg != openReleaseSeg {
			t.Fatalf("open fixture: a release record landed in segment %d, want the one planted release in %d", r.Seg, openReleaseSeg)
		}
	}
	if releases != 1 {
		t.Fatalf("open fixture: the rebuild collected %d release records into the open's live list, want the one planted in %d", releases, openReleaseSeg)
	}

	// This fixture's rebuild owes no loss records, so the hook Continue runs
	// before the retirement worker starts has nothing to land.
	c, err := Continue(Deps{
		FS:          seam.FS(),
		Clock:       kit.NewStopwatchClock(),
		Dir:         dir,
		Identity:    testIdentity,
		Streams:     testStreams,
		SegmentCap:  testSegmentCap,
		ExtentLimit: 1 << 20,
		Descriptors: core.NewDescriptorBudget(64),
		Targets:     segment.Targets{Content: 2 * testStreams},
	}, State{
		Index:    st.Rebuilt.Index,
		Counters: st.Rebuilt.Counters,
		Set:      st.Assembled.Set,
		Open: &OpenState{
			Live:     st.Assembled.Live,
			Forgets:  st.Assembled.Forgets,
			Unsynced: st.Assembled.Unsynced,
			// The dirty seed the open derives from the rebuilt index: the
			// written file's delta alone, at the zero write time restored
			// content carries; the offloaded file holds nothing the seed
			// lists.
			Dirty: []index.HeldDelta{{
				Tag:   openDirtyTag,
				File:  openDirtyFile,
				Dirty: index.BecameDirty,
			}},
		},
	}, func(*Core) error { return nil })
	if err != nil {
		t.Fatalf("Continue over the rebuilt state: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

// TestTheOpenPinsForgetsAndSpendsItsDroppableRegistrations holds the open's
// own answers, read the moment the open built them — before the retirement
// worker's own rhythm can claim the segments it answer-pins: the forget
// record's registration keeps its segment answering held while its snapshot
// names a segment of the set; the one OpenDone that closes the registration
// window spends the release nothing on disk outranks, whose file the open
// itself resolved, so its segment answers not held at once rather than
// waiting for an event a file whose records all settled may never see; and
// the dirty seed lists the written file the rebuild restored dirty with the
// zero write time restored content carries, while the offloaded file never
// joins it. The bound the read runs under is the worker's own: the withheld
// write's segment — the forget's one holder — answers held until the
// worker's claim of it retires durable, which its first tick alone reaches,
// and the read stays ahead of the tick.
func TestTheOpenPinsForgetsAndSpendsItsDroppableRegistrations(t *testing.T) {
	c := openedCore(t)

	if !c.liveness.Held(openForgetSeg) {
		t.Errorf("Held(%d) after the open = false, want true: the forget record's registration keeps its own segment held while its snapshot names %d, still a member of the set", openForgetSeg, openHeldSeg)
	}
	if c.liveness.Held(openReleaseSeg) {
		t.Errorf("Held(%d) after the open = true, want false: the one OpenDone of the registration window spent the release nothing on disk outranks", openReleaseSeg)
	}

	s := mustShare(t, c, openDirtyTag)
	got := c.DirtyFiles(s)
	if len(got) != 1 || got[0].File != openDirtyFile || !got[0].Oldest.IsZero() {
		t.Fatalf("DirtyFiles after the open got %+v, want only the dirty file %v at the zero time restored content carries, excluding the offloaded %v", got, openDirtyFile, openOffloadedFile)
	}
}

// TestTheOpenStartsTheRetirementWorkerOfAContinuedSet holds the worker the
// open starts over its rebuilt set, once the registration above pinned its
// records: the sealed segment holding the forget's withheld write answers
// held by nothing — the rebuild withheld the write whole — and the worker's
// own tick claims and retires it; the release OpenDone spent retires with
// the crossing its registration's release fired. The forget's registration
// outlives both claims: its segment keeps answering held until its
// snapshot's one segment leaves the set with the durable retirement the
// worker ran, and only then answers not held — while staying in the set
// itself, for the worker never claims a segment the stream still owns.
func TestTheOpenStartsTheRetirementWorkerOfAContinuedSet(t *testing.T) {
	c := openedCore(t)

	// The pool's fill is the wait this row takes, beside the worker's own
	// claims below: the pool's spare making runs its own pace, and the row
	// that reads the open's answers the moment the open built them — the row
	// above — must not sit through it.
	awaitSpares(t, c, 0)

	gone(t, c, "the sealed segment the rebuild left held by nothing to be claimed by the worker", openHeldSeg)
	gone(t, c, "the spent release's sealed segment to be claimed by the worker", openReleaseSeg)
	waitIn(t, 8*time.Second, "the forget's registration to be released by its holder's durable retirement", func() bool {
		return !c.liveness.Held(openForgetSeg)
	})
	if !inSet(c, openForgetSeg) {
		t.Errorf("the forget's own segment %d left the set, want it to stay: the worker never claims a segment its stream still owns", openForgetSeg)
	}
	if got := c.set.Counters().DoubleRetires; got != 0 {
		t.Errorf("DoubleRetires after the open's retirements = %d, want 0: one claim per rebuilt segment", got)
	}
}

// TestContinueRefusesASetWithNoOpenPayload holds Continue's credential: a set
// that carried no open payload registered nothing with the liveness tracker,
// so Continue refuses it before the hook runs and returns no core.
func TestContinueRefusesASetWithNoOpenPayload(t *testing.T) {
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("the seam could not be made: %v", err)
	}
	ran := false
	c, err := Continue(Deps{
		FS:          seam.FS(),
		Clock:       kit.NewStopwatchClock(),
		Dir:         dir,
		Identity:    testIdentity,
		Streams:     testStreams,
		SegmentCap:  testSegmentCap,
		ExtentLimit: 1 << 20,
		Descriptors: core.NewDescriptorBudget(64),
		Targets:     segment.Targets{Content: 2 * testStreams},
	}, State{}, func(*Core) error { ran = true; return nil })
	if err == nil || c != nil {
		if c != nil {
			_ = c.Close()
		}
		t.Fatalf("Continue over a fresh set returned a core %t, err %v; want no core and the refusal", c != nil, err)
	}
	if ran {
		t.Errorf("Continue ran the hook over a set it refused, want it refused before the hook")
	}
}
