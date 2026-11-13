package recovery

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// spareCandidates returns the segments the spares phase may re-adopt,
// ascending by id: the segments of the Own, Unverified and ZeroHeader classes
// — a spare no stream took, a segment whose header slot does not verify, and
// a recycle a crash interrupted after its rename, whose old header names
// this journal under another id — in which no record header verifies anywhere
// (the scan's VerifiedEnd sits at the first record slot) and no seal marker
// verifies (the scan is not sealed). Nothing durable was ever written into
// such a segment — a take is made durable only with the records after it — so
// re-adopting one loses nothing. A segment a successor names is not among
// them whatever its records hold: it was sealed before its successor was
// made.
//
// The identity is checked before any record-less test, so a segment whose
// header verifies naming another journal is never a candidate: no record of
// another journal verifies under this identity, so the record-less test
// alone would take a foreign segment for a spare, and re-adopting it would
// zero what may be the only copy of another journal's content. A segment with
// no scan cannot be a candidate either: its read failed, and the open refuses
// on the range the reader collected before any phase re-adopts — unreadable
// is never read as empty.
func spareCandidates(att Attached, scans []segmentScan, named map[core.SegmentID]bool) []core.SegmentID {
	scanned := make(map[core.SegmentID]segmentScan, len(scans))
	for _, sc := range scans {
		scanned[sc.ID] = sc
	}

	// The classes that name a segment of this journal; the foreign class is
	// excluded here, before any record-less test.
	ids := make([]core.SegmentID, 0, len(att.Own)+len(att.Unverified)+len(att.ZeroHeader))
	for _, s := range att.Own {
		ids = append(ids, s.ID)
	}
	ids = append(ids, att.Unverified...)
	ids = append(ids, att.ZeroHeader...)
	slices.Sort(ids)

	var out []core.SegmentID
	for _, id := range ids {
		sc, ok := scanned[id]
		switch {
		case !ok, named[id], sc.Sealed, sc.VerifiedEnd != format.SegmentHeaderSize:
			// Unreadable, a sealed predecessor, or a segment holding a
			// record: nothing the spares phase may take.
		default:
			out = append(out, id)
		}
	}
	return out
}

// Spares is what the spares phase did with the candidates it re-adopted:
// each adoption, naming the id the allocator owns again, and each id the
// phase unlinked. Both lists are ascending by id.
type Spares struct {
	// Adopted holds one entry per re-adopted spare, ascending by id.
	Adopted []AdoptedSpare
	// Unlinked holds each id the phase unlinked, ascending by id.
	Unlinked []core.SegmentID
}

// AdoptedSpare is one re-adopted spare: the id the allocator may take again,
// and the class of the pool's thirds a take draws it from. The class is the
// classing step's to fill; an adoption not yet classed carries the zero
// class, which no class of the pool is.
type AdoptedSpare struct {
	ID    core.SegmentID
	Class segment.Class // segment.Content | segment.Headroom | segment.Reserve
}

// runSpares re-adopts the segments no verifying record of this journal
// names — the spares a crash left untaken and the recycles it interrupted
// after the rename — zeroing the two slots a take writes before its
// records: the header slot the old life named itself in and the trailer
// slot at the file's end. Nothing durable ever reached a candidate, so the
// old life's bytes become unreadable and each id is the allocator's again.
//
// The adoptions are recorded only after the one directory sync the open
// spends has made the spares' entries durable; its failure is the open's —
// no spare is taken and nothing is unlinked over a name no sync proved.
//
// The phase is the open's first writing one, so it refuses first, before
// any candidate is touched; each slot is read through the open's reader, so
// a read the bound did not clear refuses rather than zeroes, and the
// adoption takes the segment's tail fate with it: a re-adopted spare's tail
// is nobody's to cut, and the phases that cut find no fate for it.
//
// The unlinking of a re-adopted id the pool's sizes refuse and the class each
// adoption takes under are the later steps of this open's own list, not this
// phase's.
func runSpares(_ context.Context, deps Deps, rd *reader, st *State, _ core.JournalID, _ core.Version) error {
	// decision: this phase is the open's first writing one, so the refusal
	// runs before any candidate work rather than at the run's end: zeroing
	// a slot — or writing anything at all — over a range no bounded read
	// completed would judge bytes the device never returned, and no phase
	// after this one may write while a range stays unread. The ceiling is
	// an open that stays closed over a whole directory one unread range
	// keeps shut; withdrawn when an unread record, the suspect segment and
	// the unread file can be held until the device reads again, as the
	// later unread set plans.
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return &core.UnreadableRangesError{Ranges: ranges}
	}

	cand := spareCandidates(st.Attached, st.Rebuilt.Scans, namedBySuccessor(st.Attached))
	sizes := make(map[core.SegmentID]int64, len(st.Rebuilt.Scans))
	for _, sc := range st.Rebuilt.Scans {
		sizes[sc.ID] = sc.Size
	}
	adopted := make([]AdoptedSpare, 0, len(cand))
	for _, id := range cand {
		if err := zeroSpareSlots(deps, rd, id, sizes[id]); err != nil {
			return err
		}
		adopted = append(adopted, AdoptedSpare{ID: id})
	}
	if len(adopted) > 0 {
		// A process that died may have left a spare's directory entry without
		// the sync its own rename was owed: a spare taken before this sync
		// could lose its name — and the records a later open re-appends into
		// it — to a power cut, so the one sync precedes every recorded
		// adoption and its failure answers the open taking nothing.
		if err := deps.FS.SyncDir(); err != nil {
			return err
		}
	}
	st.Spares.Adopted = adopted

	reAdopted := make(map[core.SegmentID]bool, len(adopted))
	for _, a := range adopted {
		reAdopted[a.ID] = true
	}
	st.Tails.Fates = slices.DeleteFunc(st.Tails.Fates, func(f segmentFate) bool {
		return reAdopted[f.ID]
	})
	return nil
}

// zeroSpareSlots zeroes the two slots of the candidate id, file size bytes
// long: the header slot of the first SegmentHeaderSize bytes and the
// trailer slot of the last SealMarkerSize. Each slot is read through the
// open's reader first, so a read the bound did not clear refuses the open
// rather than zeroing what was never read back, and a candidate whose slots
// are already clean is never opened: nothing is written for it and no file
// sync is spent; the adoption still owes the phase's one directory sync.
// What is dirty is opened once, zeroed and synced once, so a
// crash under the phase leaves a half-zeroed slot the next open zeroes the
// same way.
func zeroSpareSlots(deps Deps, rd *reader, id core.SegmentID, size int64) error {
	name := format.SegmentName(id)

	hdr := make([]byte, format.SegmentHeaderSize)
	n, rerr := rd.ReadAt(name, hdr, 0)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return unreadRefusal(rd, rerr)
	}
	dirtyHdr := anyHeaderByte(hdr[:n])

	// A file shorter than a trailer holds none: its whole self is the
	// header slot the read above already covered.
	dirtyTail, tailOff := false, int64(0)
	if size >= format.SealMarkerSize {
		tail := make([]byte, format.SealMarkerSize)
		tn, terr := rd.ReadAt(name, tail, size-format.SealMarkerSize)
		if terr != nil && !errors.Is(terr, io.EOF) {
			return unreadRefusal(rd, terr)
		}
		if tn > 0 && anyHeaderByte(tail[:tn]) {
			dirtyTail, tailOff = true, size-format.SealMarkerSize
		}
	}
	if !dirtyHdr && !dirtyTail {
		return nil
	}

	f, oerr := deps.FS.Open(name)
	if oerr != nil {
		return oerr
	}
	werr := error(nil)
	if dirtyHdr {
		_, werr = f.WriteAt(make([]byte, n), 0) // the slot's own bytes: a short file is never grown
	}
	if werr == nil && dirtyTail {
		_, werr = f.WriteAt(make([]byte, format.SealMarkerSize), tailOff)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		return cerr
	}
	return werr
}

// unreadRefusal answers a read of the spares phase the bound did not clear:
// the ranges the reader collected, the same refusal the open answers — or,
// where none is because the open's own context ended the retries, the
// error itself, which says nothing of the device and is collected nowhere.
func unreadRefusal(rd *reader, err error) error {
	if ranges := rd.Unreadable(); len(ranges) > 0 {
		return &core.UnreadableRangesError{Ranges: ranges}
	}
	return err
}
