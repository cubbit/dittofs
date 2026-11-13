package recovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
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

// runTailProof proves on the device the two things a crash could leave
// unproven: every seal marker no successor names, and the tail of every
// segment no header, seal or sync proves.
//
// The marker half: the classing answered fateSealToProve for each unnamed
// sealed segment, its walk having seen the marker through the reader, in the
// bytes a live process serves; the proof takes the file, syncs it, and reads
// the marker's 32 bytes directly — what reached the device, never what the
// reader collected. A marker that verifies there seals the segment as a
// successor's name would; one that disagrees — short, rotted or written
// somewhere else — is treated as absent, and its segment carries the torn
// tail the classing gives an unsealed one.
//
// The tail half: the newest segment of each stream — no own header names it
// as its predecessor, and no seal proves it — has each record past the
// highest synced-through its verifying records named read through the reader
// and checked whole, both checksums, its bytes kept as the first copy; the
// segment is then synced and each record that verified re-read directly and
// checked again. A record that verifies there stays where it is; one the
// device does not hold — its re-read short, rotted, or refused — is lost by
// the device, handed to the re-append with its first copy, and its segment
// ended; one whose first read does not verify is torn, re-appended nowhere
// and reported nowhere. A sync that fails hands every record that verified
// to the re-append the same way, as a failed window, and the open still
// succeeds: the re-append resolves it.
//
// Nothing is appended here: the phase's only reach into storage is the sync
// before each direct read, and the cut its fates name is a later phase's.
func runTailProof(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version) error {
	// One scan per own segment, by id: every fate the loop proves owns one,
	// the classing having answered a fate per scan it was handed.
	scans := make(map[core.SegmentID]segmentScan, len(st.Rebuilt.Scans))
	for _, sc := range st.Rebuilt.Scans {
		scans[sc.ID] = sc
	}

	// The scans whose markers were treated as absent, held whole for the
	// tail flow below: the fate each was left is that flow's to keep or
	// deepen, never to clear.
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

	// The tail-proof set: the newest segment of each stream — one no own
	// header names as a predecessor — that no seal proves. The marker flow
	// above queued its own unsealed scans here; every fate the classing left
	// short of fateSealed names the same segments, so the queue joins them
	// without a second pass. A zero-header segment holds no records, so its
	// fate is the classing's alone.
	named := namedPredecessors(st.Attached)
	zeroHeader := make(map[core.SegmentID]bool, len(st.Attached.ZeroHeader))
	for _, id := range st.Attached.ZeroHeader {
		zeroHeader[id] = true
	}
	queued := make(map[core.SegmentID]bool, len(unsealed))
	var pending []core.SegmentID
	for _, sc := range unsealed {
		if !queued[sc.ID] {
			queued[sc.ID] = true
			pending = append(pending, sc.ID)
		}
	}
	fateByID := make(map[core.SegmentID]*segmentFate, len(st.Tails.Fates))
	for i := range st.Tails.Fates {
		f := &st.Tails.Fates[i]
		fateByID[f.ID] = f
		if named[f.ID] || f.Kind == fateSealed || queued[f.ID] || zeroHeader[f.ID] {
			continue
		}
		pending = append(pending, f.ID)
	}
	slices.Sort(pending)

	for _, id := range pending {
		sc, ok := scans[id]
		if !ok {
			// The classing writes one fate per scan, so a fate with no
			// scan names a segment whose tail nothing may speak for.
			return fmt.Errorf("%w: segment %d holds a fate no scan answers", core.ErrCorrupt, id)
		}
		if err := proveTail(deps.FS, rd, st, expect, sc, fateByID[id]); err != nil {
			return err
		}
	}

	// The re-append takes the records in order, and the phases that re-prove
	// a failed window its segments in order, whatever order the fates were
	// visited in.
	slices.SortStableFunc(st.Proof.Reappends, func(a, b tailRecord) int {
		return cmp.Or(cmp.Compare(a.Segment, b.Segment), cmp.Compare(a.At, b.At))
	})
	slices.Sort(st.Proof.FailedWindows)
	return nil
}

// proveTail proves on the device every record of one segment the classing
// left unproven, and ends the segment's fate where a record did not prove.
// Nothing is appended: the flow's only reaches into storage are its reads
// and the one sync between the two of them.
func proveTail(fs storage.FS, rd *reader, st *State, expect core.JournalID, sc segmentScan, fate *segmentFate) error {
	// No record can end past the synced-through the segment's own records
	// name when the point reaches the last record's end, and a segment with
	// no verifying record at all holds nothing to prove: the classing's
	// answer covers the segment, and the flow reaches no file.
	if sc.SyncedThrough >= sc.RecordsEnd || sc.RecordsEnd <= format.SegmentHeaderSize {
		return nil
	}
	cands, err := tailCandidates(rd, expect, sc)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		// A segment with no proof records takes no second sync: the marker
		// flow's sync, where any, is the segment's sync.
		return nil
	}

	// The first read of each proof record, payload included: the bytes the
	// reader serves are the only copy the re-append may keep a record's
	// numbers from, so they are read and verified whole and kept. Every read
	// goes through the reader, so a read the bound did not clear refuses the
	// open as the open's other reads do.
	name := format.SegmentName(sc.ID)
	var verified []tailRecord
	// cut is the start of the first record the flow did not find on the
	// device, torn or lost both, when one exists.
	cut := int64(math.MaxInt64)
	for _, h := range cands {
		rec, ok, err := tailFirst(rd, expect, sc.ID, name, h.Offset)
		if err != nil {
			return err
		}
		if !ok {
			// A record whose bytes read and do not verify is torn: the
			// classing's torn or torn-slot rule owns it — never re-appended,
			// never reported, never damage — wherever the flow lands.
			cut = min(cut, h.Offset)
			continue
		}
		verified = append(verified, rec)
	}

	f, ferr := fs.Open(name)
	if ferr != nil {
		// The house read rule, as the marker flow takes it: a context error
		// answers as itself, then the ranges the reader collected, then the
		// open's own error.
		return refusedRead(rd, ferr)
	}
	serr := f.Sync()
	if serr != nil && isContextErr(serr) {
		_ = f.Close()
		return serr
	}
	if serr != nil {
		_ = f.Close()
		// decision: a sync that fails hands the window over only through the
		// records whose first read verified — a record that failed
		// verification there has no copy the re-append could keep its
		// numbers from, so it is torn wherever the window lands, and only
		// the verified ones re-enter the journal as re-appends. Overturned
		// if the failed-window rule is ever ruled to re-append from an
		// unverifiable copy.
		if !slices.Contains(st.Proof.FailedWindows, sc.ID) {
			// The marker flow may have named the segment already, when the
			// sync failed its marker first: the window is one, not two.
			st.Proof.FailedWindows = append(st.Proof.FailedWindows, sc.ID)
		}
		for _, rec := range verified {
			st.Proof.Reappends = append(st.Proof.Reappends, rec)
			cut = min(cut, rec.At)
		}
	} else {
		// The sync carried the tail's bytes to the device: each record that
		// verified at the first read is re-read there and checked again.
		for _, rec := range verified {
			holds, rerr := tailOnDevice(f, rd, expect, rec)
			if rerr != nil {
				_ = f.Close()
				return rerr
			}
			if !holds {
				// Lost by the device: the re-append writes the record back
				// from the copy its first read verified, and the segment
				// ends at the record.
				st.Proof.Reappends = append(st.Proof.Reappends, rec)
				cut = min(cut, rec.At)
			}
		}
		_ = f.Close()
	}

	// The fate, once per segment after the flow: a record the device did not
	// hold ends the segment at the record's own start, never above a record
	// the classing named as the tail's start, and never below the
	// synced-through the segment's own records proved whole. Everything clean
	// leaves the fate as the classing wrote it, and Damaged gains nothing
	// either way.
	if cut != math.MaxInt64 {
		off := cut
		if fate.Offset != 0 {
			off = min(off, fate.Offset)
		}
		fate.Kind, fate.Offset = fateEnded, max(off, sc.SyncedThrough)
	}
	return nil
}

// tailCandidates walks the segment's record headers again and returns the
// records the proof reads: each of the scan's content kinds whose end — the
// header's own size of itself and its body, never the index's — is past the
// highest synced-through the segment's verifying records named, a record that
// straddles the point included. A record the classing already dropped is out
// — confined, or a clone target whose body failed: its torn-or-corrupt
// verdict is judged, not judged twice — and the aside kinds are out too:
// the proof reads what the scan fed the index.
//
// ponytail: the rebuild holds nothing of the records it fed the index — the
// scans it keeps drop them, so a journal of millions of records does not
// carry an index record twice — so the proof walks the segment's headers
// again, one bounded read per record, on the one segment per stream whose
// tail is unproven. Keep the unproven tail's record offsets in the scan, set
// where the scan walks, when an open whose newest segments are long shows
// the re-walk in a profile.
func tailCandidates(rd *reader, expect core.JournalID, sc segmentScan) ([]format.RecordHeader, error) {
	walk, werr := format.ScanSegmentHeaders(
		readerAt{rd: rd, name: format.SegmentName(sc.ID)}, sc.Size, expect, sc.ID)
	if werr != nil {
		// The open's walk of these same bytes already succeeded, so a walk
		// erroring here is a read the bound did not clear, and it refuses
		// the open as the open's own reads do.
		return nil, refusedRead(rd, werr)
	}
	judged := make(map[int64]bool, len(sc.Dropped))
	for _, d := range sc.Dropped {
		judged[d.Header.Offset] = true
		if d.Entry.RecordOffset != 0 {
			judged[d.Entry.RecordOffset] = true
		}
	}
	var out []format.RecordHeader
	for _, r := range walk.Records {
		h := r.Header
		if h.Kind > format.KindUnmark || judged[h.Offset] {
			continue
		}
		if h.Offset+format.RecordHeaderSize+h.BodySize() > sc.SyncedThrough {
			out = append(out, h)
		}
	}
	return out, nil
}

// tailFirst reads one proof record's whole bytes through the open's reader —
// the header at the record's own offset to learn the body's length, then the
// body, both checksums checked — and keeps them as the copy a later re-append
// writes back from. It reports whether the record verified whole (a record
// whose bytes read and do not verify is torn); a read the bound did not clear
// refuses the open, exactly as tailCandidates' walk answers its own.
func tailFirst(rd *reader, expect core.JournalID, seg core.SegmentID, name string, at int64) (rec tailRecord, verified bool, err error) {
	head := make([]byte, format.RecordHeaderSize)
	n, rerr := rd.ReadAt(name, head, at)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// A short read is a read to the file's own end: its bytes go to the
		// decoders, which judge them; every other error is the open's.
		return tailRecord{}, false, refusedRead(rd, rerr)
	}
	h, derr := format.DecodeRecordHeader(head[:n], expect, seg, at)
	if derr != nil {
		return tailRecord{}, false, nil
	}
	body := make([]byte, h.BodySize())
	nb, rerr := rd.ReadAt(name, body, at+format.RecordHeaderSize)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return tailRecord{}, false, refusedRead(rd, rerr)
	}
	if _, verr := format.VerifyRecordBody(body[:nb], expect, h); verr != nil {
		return tailRecord{}, false, nil
	}
	first := make([]byte, 0, format.RecordHeaderSize+h.BodySize())
	first = append(first, head[:n]...)
	first = append(first, body[:nb]...)
	return tailRecord{Segment: seg, At: at, Header: h, First: first}, true, nil
}

// tailOnDevice re-reads one record the first read verified — whole, in one
// direct read of the bytes the first read measured, for the record's length
// is known and no blind block is opened — through the open file's direct
// read, the device's own bytes and never the page the cache holds, under the
// read bound. It reports whether the device holds the record's verifying
// bytes.
//
// decision: a re-read that fails — a filesystem's refusal included — is not
// a refusal of the open, for the record below it already verified at its
// first read and its copy is kept: the record is lost by the device, and the
// re-append writes it back. This is the one read at open whose refusal does
// not keep the journal closed — the ceiling read.go's refusal decisions
// name, withdrawn with theirs when an unread record can be held instead of
// refused. A short read is the same verdict as disagreeing bytes: the device
// answered with what it holds. Only the context's error comes back as
// itself, answered by the caller, so a cancelled open is never taken for a
// lost record.
func tailOnDevice(f storage.File, rd *reader, expect core.JournalID, rec tailRecord) (holds bool, err error) {
	// The whole record, header and body, at the offset its header named: one
	// read answers the device's copy of the record the first read kept.
	whole := make([]byte, int64(len(rec.First)))
	var n int
	_, rerr := readretry.Do(rd.ctx, rd.clk, readretry.Attempts, func() error {
		var e error
		n, e = f.ReadAtDirect(whole, rec.At)
		if errors.Is(e, io.EOF) {
			return nil // a read to the file's own end completed, short
		}
		return e
	})
	if isContextErr(rerr) {
		return false, rerr
	}
	if rerr != nil {
		return false, nil // the device refused the read: the record is lost, never a verdict
	}
	if n < format.RecordHeaderSize {
		return false, nil // the device answered short of the record's own header
	}
	h, derr := format.DecodeRecordHeader(whole[:format.RecordHeaderSize], expect, rec.Segment, rec.At)
	if derr != nil {
		return false, nil // bytes that read and do not agree are lost with the record whole
	}
	if _, verr := format.VerifyRecordBody(whole[format.RecordHeaderSize:n], expect, h); verr != nil {
		return false, nil
	}
	return true, nil
}

// refusedRead answers an error the open did not clear, as the open's own
// house read rule takes it: the context's error as itself first, then the
// ranges the reader collected as the refusal that names what could not be
// read, then the error as its own.
func refusedRead(rd *reader, err error) error {
	if isContextErr(err) {
		return err
	}
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return &core.UnreadableRangesError{Ranges: ranges}
	}
	return err
}
