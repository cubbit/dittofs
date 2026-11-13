package recovery

import (
	"context"
	"errors"
	"io"
	"io/fs"
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

// allocProber is the platform's optional allocation query, asserted on the
// file the open holds of a candidate: where the candidate's platform files
// report one, the phase asks whether the device holds the file whole before
// taking it. The interface is the consumer's own declaration, not the
// platform's export, so it stays optional: on a platform that does not
// report allocation the assertion finds nothing and the size alone decides.
type allocProber interface{ Allocated() (bool, error) }

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
// The same one sync covers an open that only unlinked: a removal its sync
// never proved could come back after a power cut.
//
// The phase is the open's first writing one, so it refuses first, before
// any candidate is touched; each slot is read through the open's reader, so
// a read the bound did not clear refuses rather than zeroes, and the
// adoption takes the segment's tail fate with it: a re-adopted spare's tail
// is nobody's to cut, and the phases that cut find no fate for it.
//
// A candidate whose file does not hold exactly the segment cap — or whose
// body the device does not hold allocated whole, where the platform reports
// the allocation of its files — is unlinked rather than adopted: the pool
// takes whole cap-sized segments only, and a hole no byte of its size can
// name would take the writes of a whole segment and lose a crash's worth of
// them. Each is left for the pool to make again; the class each adoption
// takes under is a later step of this open's own list, not this phase's.
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
	var unlinked []core.SegmentID
	for _, id := range cand {
		// The size is the scan's own answer, read through the open's retry
		// bound: a spare the pool could take must hold exactly the cap, so
		// a candidate whose file does not — a crash's empty leftover, a
		// recycle interrupted short, a file grown past the cap — is
		// unlinked and made again, never zeroed: no byte of a file about
		// to be removed is read into a slot or written. decision: a
		// SegmentCap of zero or less unlinks every candidate rather than
		// skipping the check — under no cap no spare can be
		// right-sized, and not an empty one by its size 0 equaling the
		// cap; the open fills the field from the journal's own cap, so no
		// open that can adopt a spare passes zero.
		if deps.SegmentCap <= 0 || sizes[id] != deps.SegmentCap {
			if err := unlinkSpare(deps, id); err != nil {
				return err
			}
			unlinked = append(unlinked, id) // candidates ascend, so unlinked does too
			continue
		}
		whole, qerr := spareWholeOnDisk(deps, id)
		if qerr != nil {
			return qerr
		}
		if !whole {
			if err := unlinkSpare(deps, id); err != nil {
				return err
			}
			unlinked = append(unlinked, id)
			continue
		}
		if err := zeroSpareSlots(deps, rd, id, sizes[id]); err != nil {
			return err
		}
		adopted = append(adopted, AdoptedSpare{ID: id})
	}
	if len(adopted) > 0 || len(unlinked) > 0 {
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
	st.Spares.Unlinked = unlinked

	reAdopted := make(map[core.SegmentID]bool, len(adopted))
	for _, a := range adopted {
		reAdopted[a.ID] = true
	}
	st.Tails.Fates = slices.DeleteFunc(st.Tails.Fates, func(f segmentFate) bool {
		return reAdopted[f.ID]
	})
	return nil
}

// unlinkSpare removes the candidate id's segment file, answering the error a
// removal the device refused — the open takes and unlinks nothing over a name
// it could not clear. A name already gone is the unlink done.
func unlinkSpare(deps Deps, id core.SegmentID) error {
	if err := deps.FS.Remove(format.SegmentName(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A name already gone is the unlink done; anything else the device
		// said of the removal fails the open.
		return err
	}
	return nil
}

// spareWholeOnDisk asks, where the platform reports the allocation of its
// files, whether the device holds every byte of the candidate id's file
// allocated and written: a cap-sized file whose middle a recycle's punch left
// a hole in reads back zeros no size can tell from a made spare, and adopting
// it would take writes a crash would lose into the hole. The file is opened
// only for the query and no byte of it is read, written or zeroed: what it
// fails the unlink answers, never a slot.
func spareWholeOnDisk(deps Deps, id core.SegmentID) (bool, error) {
	f, err := deps.FS.Open(format.SegmentName(id))
	if err != nil {
		return false, err
	}

	// ponytail: only the Linux platform's file answers the allocation query,
	// so on macOS and Windows the assertion finds nothing and the size alone
	// decides — a spare a punch holed passes there unseen. Give the darwin
	// and windows files their own Allocated, over F_LOG2PHYS_EXT and
	// FSCTL_QUERY_ALLOCATED_RANGES, when a short spare is seen on either;
	// with it, the same assertion takes them.
	whole := true
	if p, ok := f.(allocProber); ok {
		whole, err = p.Allocated()
	}

	if err == nil {
		err = f.Close()
	} else {
		_ = f.Close() // the query's own error is the one the open answers
	}

	// A query the device refused fails the open naming the spare, as its
	// own reads elsewhere do: adopting over a file whose allocation the
	// device never answered would take a hole the open chose not to see.
	return whole, err
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
