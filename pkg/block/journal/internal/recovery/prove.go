package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
)

// tailRecord is one record the tail proof hands to the re-append phase:
// Segment and At name where the record sits, Header the header the walk
// verified, and First the whole record — header and body both — exactly as
// the first read served it, so the re-append writes back the bytes the
// journal wrote.
type tailRecord struct {
	Segment core.SegmentID
	At      int64
	Header  format.RecordHeader
	First   []byte
}

// proof is the tail proof's answer: the records the re-append phase takes
// and the segments whose proof sync failed. The marks phase reads Reappends
// for its lost-tail check.
type proof struct {
	Reappends     []tailRecord
	FailedWindows []core.SegmentID
}

// runTailProof proves on the device every seal marker no successor names.
// The classing answered fateSealToProve for each such segment, its walk
// having seen the marker through the reader, in the bytes a live process
// serves; the proof takes the file, syncs it, and reads the marker's 32
// bytes directly — what reached the device, never what the reader collected.
// A marker that verifies there seals the segment as a successor's name
// would; one that disagrees — short, rotted or written somewhere else — is
// treated as absent, and its segment carries the torn tail the classing
// gives an unsealed one. Nothing is appended here: the phase's only reach
// into storage is the sync before the read.
func runTailProof(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version) error {
	// One scan per own segment, by id: every fate the loop proves owns one,
	// the classing having answered a fate per scan it was handed.
	scans := make(map[core.SegmentID]segmentScan, len(st.Rebuilt.Scans))
	for _, sc := range st.Rebuilt.Scans {
		scans[sc.ID] = sc
	}

	// The scans whose markers were treated as absent, held whole for the
	// re-append flow that grows in this file: the fate each was left is
	// that flow's to keep or deepen, never to clear.
	var unsealed []segmentScan

	for i := range st.Tails.Fates {
		fate := &st.Tails.Fates[i]
		if fate.Kind != fateSealToProve {
			continue // sealed by a successor's name, or no seal to prove
		}
		sc, ok := scans[fate.ID]
		if !ok {
			// The classing writes one fate per scan, so a fate with no
			// scan names a segment whose seal nothing may speak for.
			return fmt.Errorf("%w: segment %d holds a seal-to-prove fate no scan answers", core.ErrCorrupt, fate.ID)
		}

		f, err := deps.FS.Open(format.SegmentName(fate.ID))
		if err != nil {
			if isContextErr(err) {
				return err
			}
			// The house read rule, as readFooter takes it: a context error
			// answers as itself, then the ranges the reader collected,
			// then the open's own error.
			if ranges := rd.Unreadable(); len(ranges) > 0 {
				return &core.UnreadableRangesError{Ranges: ranges}
			}
			return err
		}

		// decision: bytes that read and disagree are judged by the marker's
		// decoder and never reported — the marker is treated as absent and
		// its segment carries the torn tail the classing gives an unsealed
		// one, never damaged. The one thing this phase refuses is a read
		// the bound did not complete, and that refusal keeps read.go's own
		// ceiling: a filesystem that refuses direct reads holds its journal
		// closed at an unnamed marker, withdrawn only when an unread
		// record can be held instead of refusing, as read.go's read
		// refusals are.
		unseal := func() {
			fate.Kind, fate.Offset = fateTornTail, min(max(sc.RecordsEnd, sc.SyncedThrough), sc.Size)
			unsealed = append(unsealed, sc)
		}

		// The sync carries the segment's bytes to the device before the
		// marker is read there. A context error answers the open; any
		// other failure is the marker's absence — the marker is never
		// re-read after a failed sync, since a later one can succeed
		// without writing the pages the failed one dropped.
		if serr := f.Sync(); serr != nil {
			_ = f.Close()
			if isContextErr(serr) {
				return serr
			}
			st.Proof.FailedWindows = append(st.Proof.FailedWindows, fate.ID)
			unseal()
			continue
		}

		// The marker read, taken straight off the device and under the
		// read bound. It never goes through the reader — a range the
		// reader would collect here names the marker, and the refusal it
		// answers below names it by hand — and its bytes are never classed:
		// a short file is its own length, and the bytes either decode or
		// they do not.
		buf := make([]byte, format.SealMarkerSize)
		var n int
		_, rerr := readretry.Do(rd.ctx, rd.clk, readretry.Attempts, func() error {
			var e error
			n, e = f.ReadAtDirect(buf, sc.End)
			if errors.Is(e, io.EOF) {
				return nil // a read to the file's own end completed
			}
			return e
		})
		_ = f.Close()
		switch {
		case isContextErr(rerr):
			return rerr
		case rerr != nil:
			// A read no bounded attempt completed refuses the open on the
			// marker's own range, as read.go's read refusals refuse on
			// what the reader collected: the marker names a seal no
			// successor proves, so no marker the device never showed may
			// seal one.
			return &core.UnreadableRangesError{Ranges: []core.SegmentRange{{
				Segment: fate.ID, Off: sc.End, Len: format.SealMarkerSize,
			}}}
		}

		if _, derr := format.DecodeSealMarker(buf[:n], expect, fate.ID, sc.End); derr == nil {
			fate.Kind = fateSealed // a marker proven on the device seals as a successor's name does
			continue
		}
		unseal()
	}
	return nil
}
