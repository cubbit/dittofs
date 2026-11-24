package recovery

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
)

// spareCandidates returns the segments the spares phase may re-adopt,
// ascending by id: the segments of the Own, Unverified and ZeroHeader classes
// — a spare no stream took, a segment whose header slot does not verify, and
// a recycle a crash interrupted after its rename, whose old header names
// this journal under another id — in which no record header verifies anywhere
// (the scan's RecordsEnd sits at the first record slot: the walk raises it
// past every record whose header verified, a clone target whose body failed
// among them, so its lower VerifiedEnd alone would take such a segment for a
// spare) and no seal marker verifies (the scan is not sealed). Nothing
// durable was ever written into such a segment — a take is made durable only
// with the records after it — so re-adopting one loses nothing. A segment a
// successor names is not among them whatever its records hold: it was sealed
// before its successor was made. The naming successor itself is withheld
// while the verdict the raise computed ends the predecessor it names, as
// the decision below says; a predecessor whose end its own evidence seals
// releases the take at this open, and one the open ends releases it only
// once the emptying has unlinked it, so the re-adoption never leaves the
// predecessor unnamed while its claim still judges it: the unnamed path
// the classing keeps
// safe, a predecessor nothing names being one that could have been open at a
// crash, its tail proven on the device rather than trusted, its synced drops
// still corrupt by their own synced-through, and a marker rotted into the
// bytes the successor's claim covered cutting as a torn tail below which no
// acknowledged record sits.
//
// The identity is checked before any record-less test, so a segment whose
// header verifies naming another journal is never a candidate: no record of
// another journal verifies under this identity, so the record-less test
// alone would take a foreign segment for a spare, and re-adopting it would
// zero what may be the only copy of another journal's content. A segment with
// no scan cannot be a candidate either: its read failed, and the open refuses
// on the range the reader collected before any phase re-adopts — unreadable
// is never read as empty.
//
// decision: a candidate a sync mark names is withheld, never re-adopted,
// zeroed or unlinked whatever its own bytes say, and the mark's own work —
// its contribution to the counters, the proof below its Through, the
// withheld record-less segment the raise ends, the lost-tail check — is
// the marks phases' own, must they find the segment as it stands. The
// ceiling is the withholding alone, a filter at this one choke point: the
// raise ends the mark's record-less segment and the later emptying unlinks
// it, and a mark naming a segment holding records keeps them from the
// record-less test alone, never naming a fate of its own here.
//
// decision: a record-less take whose verified header names a predecessor
// this open ends — a predecessor still on disk whose end neither its own
// marker nor its trailer proved, so the open ends it rather than trusting
// it — is withheld with it, never re-adopted, zeroed or unlinked, for the
// take's naming header is evidence the tail classing reasons from — its
// claim is what the marker-alone-rotted and missing-end checks judge the
// predecessor by — and nothing that reasons from evidence releases it
// before the conclusion is durable. The taking ends where the predecessor
// does: one whose end its own evidence seals leaves the pool its take at
// this very open, and so does one unlinked, its claim answering nothing on
// disk. A predecessor the open ends releases its take only once it is gone
// with a synced unlink — where the emptying empties it — for a take
// adopted before that unlink is durable would zero the only header naming
// the predecessor, and the next open would read its predecessor with less
// evidence than the one that took it had. A held take leaks no cap: it
// counts toward the headroom for as long as it is held. A predecessor on
// disk with no verdict entry keeps its take held — the verdict the raise
// writes one entry per named predecessor, so a missing entry names a
// predecessor the open could not class, an open the unreadable-range
// refusal answers before this phase adopts anything.
//
// spareCandidates takes the verdict the marks raise computed; its own
// reads stay the record-less test.
func spareCandidates(att Attached, scans []segmentScan, named map[core.SegmentID]bool, marks []core.SyncMark, fates map[core.SegmentID]predFate) (cands []core.SegmentID, held []core.SegmentID) {
	scanned := make(map[core.SegmentID]segmentScan, len(scans))
	for _, sc := range scans {
		scanned[sc.ID] = sc
	}

	// The segments the caller's sync marks name: each stays on disk, whatever
	// its bytes say, for the marks phases to read.
	marked := make(map[core.SegmentID]bool, len(marks))
	for _, m := range marks {
		marked[core.SegmentID(m.Segment)] = true
	}

	// Every segment the attach pass found on disk: the own, unverified and
	// zero-header classes, the ones this open writes its verdicts on. A
	// foreign segment is nobody's predecessor here, and a predecessor with
	// no class is one a header read failed on, an open this phase never
	// starts over.
	onDisk := make(map[core.SegmentID]bool, len(att.Own)+len(att.Unverified)+len(att.ZeroHeader))
	for _, s := range att.Own {
		onDisk[s.ID] = true
	}
	for _, id := range slices.Concat(att.Unverified, att.ZeroHeader) {
		onDisk[id] = true
	}

	// The takes naming a predecessor the verdict ends: each stays with the
	// predecessor it names, withheld by the decision above, until the
	// emptying unlinks that predecessor with a synced unlink; a predecessor
	// gone answers nothing on disk, its take the pool's plain spare.
	naming := make(map[core.SegmentID]bool, len(att.Own))
	for _, s := range att.Own {
		p := s.Header.Predecessor
		if p == 0 || p == s.ID || !onDisk[p] {
			continue
		}
		if f, pok := fates[p]; !pok || f == predEnded {
			naming[s.ID] = true
		}
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
	var heldOut []core.SegmentID
	for _, id := range ids {
		sc, ok := scanned[id]
		switch {
		case !ok, named[id], marked[id], sc.Sealed, sc.RecordsEnd != format.SegmentHeaderSize:
			// Unreadable, a sealed predecessor, a segment a sync mark names,
			// or a segment in which a record header verified: nothing the
			// spares phase may take.
		case naming[id]:
			// The take naming a predecessor whose end is not yet proven: it
			// stays with the predecessor it names, and the phase records it
			// as held rather than taking it.
			heldOut = append(heldOut, id) // ids ascend, so heldOut does too
		default:
			out = append(out, id)
		}
	}
	return out, heldOut
}

// allocProber is the platform's optional allocation query, asserted on the
// file the open holds of a candidate: where the candidate's platform files
// report one, the phase asks whether the device holds the file whole before
// taking it. The interface is the consumer's own declaration, not the
// platform's export, so it stays optional. The Linux platform's own file is
// the one implementation that answers of the device — FIEMAP's extent
// report, or the hole seek its fallback runs where the filesystem serves
// none — so a holed cap-sized spare is caught there whatever its filesystem
// answers. The crash seam's file answers too, whole by default with a query
// the kit's faults can fail or answer not whole — the seam models crash
// durability, not allocation, so a hole it holds reads as zeros exactly as
// on the device and its own answer could never see one; the answer is what
// a fault names, the shape a check of the not-whole unlink needs. The darwin
// and Windows platform files answer nothing, deliberately: their queries are
// unmade (the marker in spareWholeOnDisk names the platform calls that would
// make them). Over those two the
// assertion finds nothing and the size alone decides: a holed spare passes
// unseen.
type allocProber interface{ Allocated() (bool, error) }

// Spares is what the spares phase did with the candidates it re-adopted:
// each adoption, naming the id the allocator owns again and the class of the
// pool's thirds a take draws it from, each id the phase unlinked, and each
// record-less take it held for its predecessor's sake. All three lists are
// ascending by id.
//
// This state is what the pool gets: the open's own later step hands each
// adoption to the pool — id and class together — and starts the pool after
// them, standing on the one directory sync Run spent inside the phase
// before any adoption was recorded, the sync whose proof the pool's gate on
// adoption and start holds every re-adopted spare to.
type Spares struct {
	// Adopted holds one entry per re-adopted spare, ascending by id, each
	// with the class the pool's targets' adoption order gave its id.
	Adopted []AdoptedSpare
	// Held holds each id the phase withheld because its header names a
	// predecessor whose end is not yet proven, ascending by id: the take is
	// left exactly as it stands — not adopted, not unlinked, no slot of it
	// touched. Held takes count in two places and stand in the headroom
	// both times: the adoption order leaves the headroom slots they take
	// out of what the adopted spares draw, and the pool's HoldTaken
	// count — which nothing hands this list to yet — is what would keep the
	// refill from making a headroom spare over each: until it is wired, a
	// hold costs one headroom cap past the targets while it stands.
	Held []core.SegmentID
	// Unlinked holds each id the phase unlinked, ascending by id.
	Unlinked []core.SegmentID
	// Borrowed counts the headroom spares the re-append phase drew for its
	// copies: the headroom is that many short until the pool makes them
	// again.
	Borrowed int
}

// AdoptedSpare is one re-adopted spare: the id the allocator may take again,
// and the class of the pool's thirds a take draws it from, recorded by the
// spares phase from the pool's own targets.
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
// The phase decides first — staging each whole candidate and collecting each
// short one, writes none — then, only where a spare is to be taken, spends
// its one directory sync, and only past that sync zeroes the staged slots,
// recording each adoption as its zeroing succeeds, and performs the
// removals. The sync's failure is the open's, and it changes nothing: no
// slot was zeroed and nothing unlinked, every candidate keeping its name
// and its bytes over a sync that proved none. A zeroing that fails past the
// sync is the one place the phase leaves a half-done state behind: the
// candidates zeroed before it keep their zeroed slots on disk, but the
// failed open's State is discarded and the next open re-reads their bytes
// and zeroes them the same way, idempotently, so nothing incorrect
// survives it. Each removal runs unproved by the sync: a power cut may
// resurrect a short spare, which is harmless — the next open re-reads the
// same bytes, decides the same and unlinks it again.
//
// The phase checks its context before its first write and again at each
// write's doorstep — each zeroing and each removal before it starts, past
// the check the staging's end already made. A cancellation stops no write
// already under way: the one in flight finishes, and the next open settles
// what it left idempotently — a half-zeroed slot zeroed the same way, a
// removal that never became durable re-decided — but no write begins past
// the cancellation: no further candidate is zeroed, taken or unlinked.
//
// The phase is the open's first writing one, so it refuses first, before
// any candidate is touched; each slot is read through the open's reader, so
// a read the bound did not clear refuses rather than zeroes, and the
// adoption or unlink takes the segment's tail fate with it: a re-adopted
// spare's tail is nobody's to cut, and an unlinked one would name a cut at
// a file that no longer exists.
//
// A candidate whose file does not hold exactly the segment cap — or whose
// body the device does not hold allocated whole, where the platform reports
// the allocation of its files — is unlinked rather than adopted: the pool
// takes whole cap-sized segments only, and a hole no byte of its size can
// name would take the writes of a whole segment and lose a crash's worth of
// them. Each is left for the pool to make again; every adoption is recorded
// with the class the pool's own targets' adoption order gives its id.
func runSpares(ctx context.Context, deps Deps, rd *reader, st *State, _ core.JournalID, _ core.Version, marks []core.SyncMark) error {
	// decision: a zero SegmentCap — a Deps literal that forgot the cap —
	// refuses the phase naming it, rather than unlinking every candidate
	// under "no cap right-sizes any spare": nothing distinguishes that
	// refusal from a whole directory of short candidates, and a caller
	// that reads a fixture "the way an open does" must not lose files to a
	// literal omitted. The open fills the field from the journal's own
	// configured cap, so no open that can take a spare passes zero here.
	if deps.SegmentCap <= 0 {
		return errors.New("recovery: a Deps without SegmentCap reached the spares phase: fill it with the journal's own configured cap")
	}

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

	// The predecessor verdicts the marks raise computed: each named
	// predecessor's sealed, ended or gone, the answer the naming hold keys
	// on — a predecessor the open ends keeps its take held, one whose end
	// its own evidence seals releases it at this open.
	cand, held := spareCandidates(st.Attached, st.Rebuilt.Scans, successorSet(st.Attached.Own), marks, st.Marks.fates)
	sizes := make(map[core.SegmentID]int64, len(st.Rebuilt.Scans))
	for _, sc := range st.Rebuilt.Scans {
		sizes[sc.ID] = sc.Size
	}
	// The context is checked before any write: the staging below reads and
	// asks, but writes nothing, so the open a caller that cancelled before
	// the phase reaches answers here, taking and unlinking nothing.
	if err := ctx.Err(); err != nil {
		return err
	}
	take := make([]core.SegmentID, 0, len(cand)) // the whole candidates staged for zeroing
	adopted := make([]AdoptedSpare, 0, len(cand))
	var unlinked []core.SegmentID
	for _, id := range cand {
		// The size is the scan's own answer, read through the open's retry
		// bound: a spare the pool could take must hold exactly the cap, so
		// a candidate whose file does not — a crash's empty leftover, a
		// recycle interrupted short, a file grown past the cap — is collected
		// for unlinking and made again, never zeroed: no byte of a file
		// about to be removed is read into a slot or written. The cap itself
		// never reaches zero here: the phase above refused a Deps without
		// one, so the comparison sizes every candidate.
		if sizes[id] != deps.SegmentCap {
			unlinked = append(unlinked, id) // candidates ascend, so unlinked does too
			continue
		}
		whole, qerr := spareWholeOnDisk(deps, id)
		if qerr != nil {
			return qerr
		}
		if !whole {
			unlinked = append(unlinked, id)
			continue
		}
		// candidates ascend, so take does too, and the i-th class is the
		// i-th id's below.
		take = append(take, id)
	}
	// The context is checked again where the phase begins to act, and then at
	// each write's doorstep below — each zeroing and each removal checks
	// before it starts: a cancellation lands only on a write under way, which
	// finishes, while no new write begins past it.
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(take) > 0 {
		// A process that died may have left a spare's directory entry without
		// the sync its own rename was owed: a spare taken before this sync
		// could lose its name — and the records a later open re-appends into
		// it — to a power cut, so the sync precedes every zeroed slot and
		// every recorded adoption, and its failure answers the open taking
		// and unlinking nothing, every candidate's bytes as they began. It
		// proves only the names of the spares to be taken: a removal is not
		// among what it proves, so a phase that unlinks without adopting
		// spends no sync and a resurrected short spare is idempotently
		// re-unlinked at the next open, as the marker at the unlink below
		// says.
		if err := deps.FS.SyncDir(); err != nil {
			return err
		}
		for _, id := range take {
			// Each write's doorstep: the write this spare's zeroing is
			// about to start, so an already-landed cancellation answers here
			// and the spare's bytes stand as the phase read them.
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := zeroSpareSlots(deps, rd, id, sizes[id]); err != nil {
				// decision: a zeroing that fails here leaves the candidates
				// zeroed before it holding zeroed slots on disk and none
				// adopted, for the open's State is discarded over the
				// failure and the next open re-reads their bytes and zeroes
				// them the same way. Withdraw only if a later phase ever
				// reads what this phase staged rather than the bytes
				// themselves.
				return err
			}
			adopted = append(adopted, AdoptedSpare{ID: id})
		}
	}
	// The removals run unproved by the sync: a power cut may resurrect a
	// short spare, which is harmless — the next open re-reads it, re-decides
	// and unlinks it again.
	for _, id := range unlinked {
		// Each write's doorstep, the removals' own: a cancellation that
		// landed while an earlier removal ran stops the rest here, leaving
		// the candidate for the next open to unlink, which re-decides it the
		// same way.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := unlinkSpare(deps, id); err != nil {
			return err
		}
	}
	// The held takes stand in the headroom's slots, which the re-adopted
	// spares then do not fill — bounded by the headroom's own target, for
	// the pool makes and takes no slot a hold stands in: a hold past the
	// headroom's slots consumes no other class, exactly as the refill
	// would make none there.
	left := deps.SpareTargets
	left.Headroom -= min(len(held), left.Headroom)
	classes := left.AdoptClasses(len(adopted))
	for i := range adopted {
		adopted[i].Class = classes[i] // candidates ascend, so the i-th class is the i-th id's
	}
	st.Spares.Adopted = adopted
	st.Spares.Held = held
	st.Spares.Unlinked = unlinked

	taken := make(map[core.SegmentID]bool, len(adopted)+len(unlinked))
	for _, a := range adopted {
		taken[a.ID] = true
	}
	for _, id := range unlinked {
		taken[id] = true
	}
	st.Tails.Fates = slices.DeleteFunc(st.Tails.Fates, func(f segmentFate) bool {
		return taken[f.ID]
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

	// ponytail: the Linux platform's file answers the allocation query by
	// FIEMAP or the hole seek it falls back to, and the crash seam's answers
	// whole by default with a faultable not-whole, so over darwin's and
	// Windows's files alone the size decides — a spare a punch holed passes
	// on those two unseen. Give the darwin and windows files their own
	// Allocated, over F_LOG2PHYS_EXT and
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
// trailer slot of the last TrailerSize. Each slot is read through the
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
	if size >= format.TrailerSize {
		tail := make([]byte, format.TrailerSize)
		tn, terr := rd.ReadAt(name, tail, size-format.TrailerSize)
		if terr != nil && !errors.Is(terr, io.EOF) {
			return unreadRefusal(rd, terr)
		}
		if tn > 0 && anyHeaderByte(tail[:tn]) {
			dirtyTail, tailOff = true, size-format.TrailerSize
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
		_, werr = f.WriteAt(make([]byte, format.TrailerSize), tailOff)
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
