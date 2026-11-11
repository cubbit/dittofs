package durability

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestReappendIntoAPartFullSegmentKeepsEveryHeldByteReadable covers a failed
// window's content record re-appended into a segment that no longer has room
// for it whole (another file's record landed there first): the stream splits
// it, placing only part of its payload in the new record. Every held piece
// the index names at the new place must still be backed by bytes the record
// holds, and what is read back through the index's own extent must equal what
// was written.
func TestReappendIntoAPartFullSegmentKeepsEveryHeldByteReadable(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	st := r.d.made[0]
	st.s = r.streams.Stream(0)
	r.streams.OnEnd(r.d.onEnd)
	r.d.BindStreams(r.streams)
	f, g := file(1), file(2)

	want := strings.Repeat("a", 30000)
	a, _ := r.publish(t, f, 0, want)
	r.keptPages.Store(a.Segment, struct{}{})
	if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(a.Segment), 1, errors.New("seal boom"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.streams.For(f).Seal(false); err == nil {
		t.Fatal("the seal was expected to fail")
	}
	eventually(t, "the end to be queued", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.ends) == 1
	})
	// Another file's write lands in the stream's next segment before the
	// worker re-appends the window, leaving less room there than A needs.
	c, _ := r.publish(t, g, 0, strings.Repeat("c", 40000))
	if c.Segment == a.Segment {
		t.Fatalf("C in segment %d, want a segment after A's", c.Segment)
	}
	r.d.takeWindows(st)
	r.d.resolveWindows(st)
	if ws := r.windowsOf(0); len(ws) != 0 {
		t.Fatalf("windows = %+v, want the window resolved", ws)
	}

	checkHeldReadable(t, r, f, want)
}

// checkHeldReadable reads every piece the index holds for f at its first
// offset's record, as a read would, and checks the record backs the piece's
// extent whole and the bytes equal what was written.
func checkHeldReadable(t *testing.T, r *rig, f core.FileID, want string) {
	t.Helper()
	pieces := r.ix.HeldAt(f, r.placedAt(t, f, 0))
	if len(pieces) == 0 {
		t.Fatalf("the index holds no piece of file f at the re-appended record")
	}
	var heldLen int64
	for _, p := range pieces {
		heldLen += p.Extent.Len
		// Read the record the piece points at, as a read would.
		buf := make([]byte, 1<<16)
		n, _ := r.streams.Stream(0).ReadAt(p.Record.Segment, buf, int64(p.Record.Offset))
		hdr, err := format.DecodeRecordHeader(buf[:format.RecordHeaderSize], rigIdentity, p.Record.Segment, int64(p.Record.Offset))
		if err != nil || n < int(hdr.Size()) {
			t.Fatalf("record at %+v: decode %v, read %d", p.Record, err, n)
		}
		body, err := format.VerifyRecordBody(buf[format.RecordHeaderSize:format.RecordHeaderSize+hdr.BodySize()], rigIdentity, hdr)
		if err != nil {
			t.Fatalf("record at %+v: %v", p.Record, err)
		}
		lo := int64(p.Skip)
		hi := lo + p.Extent.Len
		if hi > int64(len(body.Payload)) {
			t.Errorf("piece %+v claims payload bytes [%d,%d) but the record at %+v holds only %d payload bytes (record extent len %d): the extent outruns its record",
				p.Extent, lo, hi, p.Record, len(body.Payload), hdr.Extent.Len)
			continue
		}
		if !bytes.Equal(body.Payload[lo:hi], []byte(want)[p.Extent.Off:p.Extent.Off+p.Extent.Len]) {
			t.Errorf("piece %+v reads back other bytes than were written", p.Extent)
		}
	}
	if heldLen != int64(len(want)) {
		t.Errorf("held pieces of f total %d bytes, want the %d written", heldLen, len(want))
	}
}

// TestTwoFailedWindowsReappendedIntoOneNearFullSegmentEachLandWhole fails two
// segments' seals and re-appends both records into a stream whose current
// segment is part full: the first does not fit and rolls, the second goes
// after it, and each file's bytes read back whole.
func TestTwoFailedWindowsReappendedIntoOneNearFullSegmentEachLandWhole(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigUnstarted(t, 1)
	st := r.d.made[0]
	st.s = r.streams.Stream(0)
	r.streams.OnEnd(r.d.onEnd)
	r.d.BindStreams(r.streams)
	f, g, h := file(1), file(2), file(3)

	failSeal := func(fid core.FileID, payload string, ends int) {
		loc, _ := r.publish(t, fid, 0, payload)
		r.keptPages.Store(loc.Segment, struct{}{})
		if err := r.seam.Schedule(kit.Failing(kit.OpSync, format.SegmentName(loc.Segment), 1, errors.New("seal boom"))); err != nil {
			t.Fatal(err)
		}
		if _, err := r.streams.For(fid).Seal(false); err == nil {
			t.Fatal("the seal was expected to fail")
		}
		eventually(t, "the end to be queued", func() bool {
			st.mu.Lock()
			defer st.mu.Unlock()
			return len(st.ends) == ends
		})
	}
	wantF, wantG := strings.Repeat("a", 30000), strings.Repeat("b", 30000)
	failSeal(f, wantF, 1)
	// A healthy segment between the two, so the stream is not stopped by a
	// second failure in a row.
	r.publish(t, file(4), 0, "ok")
	if _, err := r.streams.For(file(4)).Seal(false); err != nil {
		t.Fatalf("the healthy seal: %v", err)
	}
	failSeal(g, wantG, 2)
	// The stream's current segment is now part full.
	r.publish(t, h, 0, strings.Repeat("c", 40000))

	r.d.takeWindows(st)
	r.d.resolveWindows(st)
	if ws := r.windowsOf(0); len(ws) != 0 {
		t.Fatalf("windows = %+v, want both resolved", ws)
	}
	checkHeldReadable(t, r, f, wantF)
	checkHeldReadable(t, r, g, wantG)
}
