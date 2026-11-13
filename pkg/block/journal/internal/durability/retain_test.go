package durability

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// publish appends a write of payload at off to f and publishes it through the
// index with retain, as a writer does: both under the file's append domain.
// It returns where the record landed and its version.
func (r *rig) publish(t *testing.T, f core.FileID, off int64, payload string) (segment.Location, core.Version) {
	t.Helper()
	return r.publishGap(t, f, off, payload, nil)
}

// publishGap is publish with gap run between the append and the publish, still
// under the file's append domain.
func (r *rig) publishGap(t *testing.T, f core.FileID, off int64, payload string, gap func()) (segment.Location, core.Version) {
	t.Helper()
	v, err := r.d.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	return r.publishAs(t, f, off, payload, v, gap)
}

// publishAs is publishGap with the version given, so that several records can
// carry the one version of a write split across segments.
func (r *rig) publishAs(t *testing.T, f core.FileID, off int64, payload string, v core.Version, gap func()) (segment.Location, core.Version) {
	t.Helper()
	seq, err := r.d.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	x := core.Extent{Off: off, Len: int64(len(payload))}
	r.lastSeq.Store(uint64(seq))
	unlock := r.domains.Lock(f)
	defer unlock()
	p, err := r.streams.For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: x, Seq: seq, Version: v, Tag: 1,
	}, format.RecordBody{Payload: []byte(payload), MTime: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if gap != nil {
		gap()
	}
	placed := index.Placed{Extent: x, Record: index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}, Version: v}
	if _, err := r.ix.WriteRetain(1, f, placed, seq); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	return p.Loc, v
}

// retainedBy returns the retained pieces of f superseded by v.
func (r *rig) retainedBy(f core.FileID, v core.Version) []index.Retained {
	var out []index.Retained
	for _, p := range r.ix.Retained(f) {
		if p.By == v {
			out = append(out, p)
		}
	}
	return out
}

// TestSyncLiftsWhatTheSyncedRecordSuperseded pins that a superseded extent is
// retained until the record that superseded it is synced, and not after.
func TestSyncLiftsWhatTheSyncedRecordSuperseded(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	_, v1 := r.publish(t, f, 0, "aaaa")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	_, v2 := r.publish(t, f, 0, "bbbb")

	if got := r.retainedBy(f, v2); len(got) != 1 || got[0].Version != v1 {
		t.Fatalf("before the sync the retained pieces superseded by v2 are %+v, want v1's piece", got)
	}
	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v2 {
		t.Fatalf("before the sync Lookup serves %+v, %v, want v2", p, ok)
	}

	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("after the sync %+v is still retained", got)
	}
	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v2 {
		t.Fatalf("after the sync Lookup serves %+v, %v, want v2", p, ok)
	}
}

// TestSyncOfARecordThatSupersededNothingAccountsNothing pins that a lift with
// nothing to drop changes no retained piece and reaches no account.
func TestSyncOfARecordThatSupersededNothingAccountsNothing(t *testing.T) {
	r := newRig(t, 1)
	f, g := file(1), file(2)
	_, v1 := r.publish(t, f, 0, "aaaa")
	r.publish(t, g, 0, "gggg")
	if _, err := r.d.Sync(context.Background(), f, g); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("retained %+v, want none", got)
	}
	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v1 {
		t.Fatalf("Lookup serves %+v, %v, want v1", p, ok)
	}
	if got := r.acct.all(); len(got) != 0 {
		t.Fatalf("the account received %+v for records that superseded nothing", got)
	}
}

// TestSyncReleasesTheSegmentsRetainedBytes pins that the segment of the
// superseded record counts its bytes as retained until the superseding record
// syncs.
func TestSyncReleasesTheSegmentsRetainedBytes(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	loc1, _ := r.publish(t, f, 0, "aaaa")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	r.publish(t, f, 0, "bbbb")
	if got := r.ix.SegmentBytes(loc1.Segment).Retained; got != 4 {
		t.Fatalf("before the sync the segment retains %d bytes, want 4", got)
	}
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	if got := r.ix.SegmentBytes(loc1.Segment).Retained; got != 0 {
		t.Fatalf("after the sync the segment retains %d bytes, want 0", got)
	}
}

// TestSyncAccountsTheLift pins that the lift's delta reaches Config.Account,
// so the counters an account feeds see the retained bytes leave.
func TestSyncAccountsTheLift(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	r.publish(t, f, 0, "bbbb")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	var retained int64
	for _, hd := range r.acct.all() {
		retained += hd.RetainedBytes
		if hd.File != f {
			t.Fatalf("a delta names %v, want %v", hd.File, f)
		}
	}
	if retained != -4 {
		t.Fatalf("the account saw %d retained bytes change, want -4", retained)
	}
}

// TestSyncInTheGapLiftsAfterThePublish pins that a sync which completes
// between a writer's append and its publish lifts only after the publish: the
// lift waits for the file's append domain, so what the writer retains is not
// left retained for good.
func TestSyncInTheGapLiftsAfterThePublish(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}

	appended := make(chan segment.Location, 1)
	release := make(chan struct{})
	done := make(chan core.Version, 1)
	go func() {
		_, v2 := r.publishGap(t, f, 0, "bbbb", func() {
			appended <- segment.Location{}
			<-release
		})
		done <- v2
	}()
	<-appended

	// The stream syncs while the writer holds f's domain: v2's record is
	// covered before it is published.
	r.domains.mu.Lock()
	taken := r.domains.taken
	r.domains.mu.Unlock()
	r.syncStream(t, f)
	// The worker has completed v2's entry up to the file's domain, which it
	// asks for and the writer holds.
	eventually(t, "the worker to ask for the file's domain", func() bool {
		r.domains.mu.Lock()
		defer r.domains.mu.Unlock()
		return r.domains.taken > taken
	})

	close(release)
	v2 := <-done
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	eventually(t, "v1 to leave the retained set", func() bool { return len(r.retainedBy(f, v2)) == 0 })
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("%+v is retained after the sync that covered v2", got)
	}
}

// emptied is one report a move made through Config.Emptied: the segments it
// named and the causality location a retirement of them waits on.
type emptied struct {
	segs []core.SegmentID
	at   segment.Location
}

// newMovedRig reports a started one-stream rig whose Config.Emptied records
// every report its moves make, the account log's own pattern over the real
// paths. It installs the real index marker when mark is set, as the datapath
// wires it, instead of the harness's zero marker. One rig per test, so the
// one recorder never serves two.
func newMovedRig(t *testing.T, mark bool) (*recorder[emptied], *rig) {
	t.Helper()
	log := &recorder[emptied]{}
	r := newRigConfigured(t, 1, func(c *Config) {
		c.Emptied = func(segs []core.SegmentID, at segment.Location) {
			log.add(emptied{segs: slices.Clone(segs), at: at})
		}
		if mark {
			c.MarkLoss = c.Index.Lose
		}
	})
	r.d.Start(r.streams)
	return log, r
}

// lossPlaces reads the durable loss records off the rig's seam, with the
// places they sit at, the way the harness walks the directory's segments.
func (r *rig) lossPlaces(t *testing.T) []segment.Location {
	t.Helper()
	var out []segment.Location
	for id := core.SegmentID(1); id < 64; id++ {
		raw, err := r.seam.Durable(format.SegmentName(id))
		if err != nil {
			continue
		}
		off := int64(format.SegmentHeaderSize)
		for off+format.RecordHeaderSize <= int64(len(raw)) {
			h, err := format.DecodeRecordHeader(raw[off:off+format.RecordHeaderSize], rigIdentity, id, off)
			if err != nil {
				break // the zero tail of a segment ends the scan
			}
			end := off + format.RecordHeaderSize + h.BodySize()
			if end > int64(len(raw)) {
				t.Fatalf("record at %d of segment %d runs past the durable bytes", off, id)
			}
			if _, err := format.VerifyRecordBody(raw[off+format.RecordHeaderSize:end], rigIdentity, h); err != nil {
				t.Fatalf("record at %d of segment %d: %v", off, id, err)
			}
			if h.Kind == format.KindLoss {
				out = append(out, segment.Location{Segment: id, Off: off, Len: h.Size()})
			}
			off += h.Size()
		}
	}
	return out
}

// TestTheCompletedSyncsLiftReportsTheTouchedSegments pins that the lift a
// completed sync makes reports, through Config.Emptied, the segments whose
// retained counters it lowered — the superseded records' own — gated on the
// superseding record's place, the causality record a retirement of those
// segments waits on.
func TestTheCompletedSyncsLiftReportsTheTouchedSegments(t *testing.T) {
	prof.Watch(t, prof.Options{})
	moved, r := newMovedRig(t, false)
	f := file(1)
	loc1, _ := r.publish(t, f, 0, "aaaa")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	loc2, _ := r.publish(t, f, 0, "bbbb")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	// The Sync that completed v2 has lifted it: the lift the report names has
	// run by the time the syncing record is no longer pending.

	got := moved.all()
	if len(got) != 1 {
		t.Fatalf("the lift reported %d moves, want 1: v2's record superseded one piece", len(got))
	}
	if want := []core.SegmentID{loc1.Segment}; !slices.Equal(got[0].segs, want) {
		t.Errorf("the lift named the segments %v, want %v: the superseded record's own", got[0].segs, want)
	}
	if got[0].at != loc2 {
		t.Errorf("the lift gated its report on %+v, want the superseding record's place %+v", got[0].at, loc2)
	}
}

// TestASyncedLossReportsTheSegmentsItsMarkerEmptied pins that a synced loss's
// report is gated on its own record's place: the segments whose held counters
// its marker lowered are named, through Config.Emptied, with the location of
// the loss record the call appended, the causality record a retirement of
// them waits on.
func TestASyncedLossReportsTheSegmentsItsMarkerEmptied(t *testing.T) {
	prof.Watch(t, prof.Options{})
	moved, r := newMovedRig(t, true)
	f := file(1)
	loc1, _ := r.publish(t, f, 0, "aaaa")
	if _, err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}

	l := Loss{Share: 1, File: f, Extent: core.Extent{Off: 0, Len: 4},
		Version: core.Version{Counter: 5000}, Seq: core.Seq(7), Reason: core.LossStale, Synced: true}
	if _, err := r.d.Lose(context.Background(), l); err != nil {
		t.Fatalf("Lose: %v", err)
	}

	got := moved.all()
	if len(got) != 1 {
		t.Fatalf("the synced loss reported %d moves, want 1: its marker cut one held piece", len(got))
	}
	if want := []core.SegmentID{loc1.Segment}; !slices.Equal(got[0].segs, want) {
		t.Errorf("the loss named the segments %v, want %v: the cut piece's record's own segment", got[0].segs, want)
	}
	places := r.lossPlaces(t)
	if len(places) != 1 {
		t.Fatalf("the journal holds %d durable loss records, want the one the Lose appended", len(places))
	}
	if got[0].at != places[0] {
		t.Errorf("the loss gated its report on %+v, want its own record's place %+v", got[0].at, places[0])
	}
	if h := r.ix.SegmentBytes(loc1.Segment).Held; h != 0 {
		t.Errorf("segment %d still holds %d bytes the marker should have cut, so the report named a segment it did not empty", loc1.Segment, h)
	}
}

// TestAFailedWindowReportsItsLostRecordsSegmentWithItsLossRecordsLocation
// pins that a failed window's loss, which leaves no marker of its own, still
// reports: the lost record's own segment is named, through Config.Emptied,
// gated on the loss record the worker appends for it — the causality record
// a retirement of the failed segment waits on, so a crash that loses the
// record cannot leave the segment retired with its loss unrecorded.
func TestAFailedWindowReportsItsLostRecordsSegmentWithItsLossRecordsLocation(t *testing.T) {
	prof.Watch(t, prof.Options{})
	moved, r := newMovedRig(t, false)
	f := file(1)
	loc := r.appendWrite(t, f, 0, "a")

	r.failSyncOf(t, loc.Segment, errors.New("sync boom"))
	if err := mustReturn(t, "Sync", r.syncAsync(context.Background(), f)); !errors.Is(err, core.ErrSyncFailed) {
		t.Fatalf("Sync = %v, want ErrSyncFailed", err)
	}

	eventually(t, "the failed window to report its lost record's segment", func() bool {
		return len(moved.all()) != 0
	})
	got := moved.all()
	if len(got) != 1 {
		t.Fatalf("the failed window reported %d moves, want 1", len(got))
	}
	if want := []core.SegmentID{loc.Segment}; !slices.Equal(got[0].segs, want) {
		t.Errorf("the failed window named the segments %v, want %v: the lost record's own", got[0].segs, want)
	}
	places := r.lossPlaces(t)
	if len(places) != 1 {
		t.Fatalf("the journal holds %d durable loss records, want the one the worker appended for the lost record", len(places))
	}
	if got[0].at != places[0] {
		t.Errorf("the failed window gated its report on %+v, want its loss record's place %+v", got[0].at, places[0])
	}
}
