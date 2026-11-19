package recovery

import (
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// catalogueMarksRig plants a sealed segment 1 holding two write records, a
// then b, and a newest segment 2 that names it as predecessor and holds no
// record, so the open reads segment 1 by its catalogue. It returns the rig
// and the offset b starts at.
func catalogueMarksRig(t *testing.T) (*rig, raw.Record, raw.Record, int64) {
	t.Helper()
	a := rec(1, func(r *raw.Record) {
		r.Seq, r.Version = 10, raw.Version{Counter: 11}
		r.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
	b := rec(1, func(r *raw.Record) {
		r.Seq, r.Version = 11, raw.Version{Counter: 12}
		r.Offset = 0x8100
		r.Write = &raw.WriteBody{MTime: 1, Payload: bytes16()}
	})
	sealed, _ := buildSealed(1, a, b)
	r := newRig(t)
	r.writeFormat()
	r.plantSeg(1, sealed)
	r.plantSeg(2, raw.SegmentHeader{Identity: journalID, ID: 2, Predecessor: 1}.Bytes())
	return r, a, b, endAfter(a)
}

// TestSyncMarksThroughACatalogue opens a sealed segment by its catalogue and
// checks the stream's mark names the last record the catalogue kept, its
// sequence number and its end; and, where that record's own header rotted, the
// record before it, never a boundary its bytes never proved.
func TestSyncMarksThroughACatalogue(t *testing.T) {
	for _, row := range []struct {
		name    string
		rot     bool
		wantSeq uint64
		end     func(a, b raw.Record) int64
	}{
		{"every header verifies", false, 11, func(a, b raw.Record) int64 { return endAfter(a, b) }},
		{"the last record's header rotted", true, 10, func(a, b raw.Record) int64 { return endAfter(a) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, a, b, bAt := catalogueMarksRig(t)
			if row.rot {
				name := format.SegmentName(1)
				bs := r.fileBytes(name)
				bs[bAt+2] ^= 0xff // inside the header's checksummed bytes
				r.rewrite(name, bs)
			}
			st, err := Run(context.Background(), r.runDeps(), core.Version{}, journalID, nil)
			if err != nil {
				t.Fatalf("Run over the catalogued segment: %v", err)
			}
			if st.Rebuilt.ByCatalog != 1 {
				t.Fatalf("Run rebuilt %d segments by catalog, want segment 1 alone", st.Rebuilt.ByCatalog)
			}
			want := []core.SyncMark{{Stream: 0, Segment: 1, Through: row.end(a, b), Offset: row.end(a, b), Sequence: row.wantSeq}}
			if got := st.SyncMarks(); !slices.Equal(got, want) {
				t.Errorf("SyncMarks = %+v, want %+v", got, want)
			}
		})
	}
}

// TestSyncMarksPreferTheCopyOverItsSource holds the Replaced source out of
// the scans: a source whose own scan reads a higher sequence number than the
// copy's, the placement the device lost, still yields the copy's entry. The
// state is the pure function's input, built by hand, for a real tail whose
// source outranks its copy cannot be planted.
func TestSyncMarksPreferTheCopyOverItsSource(t *testing.T) {
	st := State{}
	st.Attached.Own = []SegmentInfo{{ID: 2}}
	st.Rebuilt.Scans = []segmentScan{{ID: 2, LastSeq: 105, LastEnd: 500}}
	st.Proof.Replaced = []replacedRecord{{Source: 2, Target: 9, End: 168, Seq: 100}}
	want := []core.SyncMark{{Stream: 0, Segment: 9, Through: 168, Offset: 168, Sequence: 100}}
	if got := st.SyncMarks(); !slices.Equal(got, want) {
		t.Errorf("SyncMarks = %+v, want %+v: the source's own scan must not outrank its copy", got, want)
	}
}
