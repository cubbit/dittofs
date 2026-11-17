package recovery

import (
	"context"
	"errors"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// runSeal is the open's cut-and-seal phase: each torn-tail fate the classing
// and the tail's proof answered is truncated at the fate's offset — never
// below a verifying record, below the highest synced-through a verifying
// record names or below a mark's own Through, the floors the classing, the
// marks raise and the proof already folded into the offset and the truncation
// here keeps once more — the truncation is made durable, and the segment is
// sealed through the seal writer with the catalog its own bytes answer: one
// entry per attached record that begins before the cut, and a marker that
// rises past a record the floor sliced in half (R50); each ended fate with a
// cut takes the same truncation and no seal marker, so the next open never
// reads the tear the ended verdict answered for as corruption behind a seal
// (R51). The ended fate with no cut at all — offset zero, the ending whole —
// is left standing for the emptying that unlinks it, and a sealed or intact
// fate is left exactly as it stands.
//
// Each stream then continues in a new segment: the cut-and-sealed one carries
// its own marker as proof, the ended one names no successor, and the row of
// each is what the assembled set holds — the torn tail sealed by the writer in
// the set the open continues from, the ended row ended since the assembly.
//
// The phase writes at the crash seam, each write on a doorstep context check:
// a cancellation stops no truncate or seal already under way — the next open
// re-reads what it left and settles it: a tail whose cut never landed is cut
// the same way, one whose cut landed without the seal answers a segment with
// no tail to name, and one settled whole stays settled — but no write begins
// past the cancellation.
//
// decision: a segment whose sync failed in this open — the tail proof's
// failed window — is never truncated, sealed or otherwise touched here: it is
// left standing whole, failed and unsealed, exactly as the proof's verdict
// left it (R52). A truncation is a write its own sync must prove, and over a
// window the device already refused a durable cut would claim bytes the fail
// never reached; the next open re-proves the segment and re-decides. The
// ceiling is the emptying phase's hands-off-the-link rule, which owns such a
// segment's unlink.
func runSeal(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version, _ []core.SyncMark) (err error) {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	set := st.Assembled.Set
	var cache *segment.Descriptors
	// The one descriptor cache of the phase, spent whatever the loop answers:
	// a shutdown that fails past successful work is the open's error, and a
	// failure before one does not displace the one it follows.
	defer func() {
		if cache != nil {
			if serr := cache.Shutdown(); err == nil {
				err = serr
			}
		}
	}()

	for i := range st.Tails.Fates {
		fate := st.Tails.Fates[i]
		if fate.Offset == 0 {
			continue // no cut behind a record exists: the emptying unlinks the segment whole
		}
		if fate.Kind != fateTornTail && fate.Kind != fateEnded {
			continue // sealed or intact: nothing to cut
		}
		// Each write's doorstep: the write this segment's cut is about to
		// start, so an already-landed cancellation answers here and the
		// segment stands as the phase read it.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// decision: the failed window's segment is left exactly as it stands
		// — failed, unsealed, its window unmeasured by any truncate — see the
		// phase's own marker above.
		//
		// decision: a take the spares phase held stays a pool spare, whole:
		// never truncated, never sealed. Its zeros are an untaken spare's
		// body, not a crash's tear — the classing names a tail only because
		// its record slots never held a record — and a cut-and-sealed take
		// could never re-adopt at the emptying's unlink of its predecessor,
		// for a sealed stub holds the marker no record-less candidate may.
		// The take ends where the predecessor does, the spares phase's own
		// rule; withdrawn with it.
		if slices.Contains(st.Proof.FailedWindows, fate.ID) || slices.Contains(st.Spares.Held, fate.ID) {
			continue
		}
		sc, ok := markScan(st, fate.ID)
		if !ok {
			// Cannot happen: every fate names a scanned segment, and nothing
			// between the classing and here removes a scan — kept as a guard
			// for the same reason the loop's own skips are.
			continue
		}
		cut := fate.Offset
		if m, here := st.Marks.set[fate.ID]; here {
			// The marks raise folded this floor into the fate and the
			// proof raised its own answers the same way; the cut keeps it
			// here too, so a fate an open earlier than this one settled
			// can never truncate below the mark's Through: a sealed end
			// below a surviving mark's Offset wedges every later open on
			// the lost-tail refusal.
			cut = max(cut, min(m.Through, sc.Size))
		}
		// The catalog is asked of the bytes as they stand, before anything
		// is written: the walk answers from the same file the phase is
		// about to truncate.
		entries, marker := []format.CatalogEntry(nil), cut
		if fate.Kind == fateTornTail {
			var aerr error
			entries, marker, aerr = sealAnswers(rd, expect, fate.ID, sc, cut)
			if aerr != nil {
				return aerr
			}
		}
		if cache == nil {
			cache = segment.NewDescriptors(deps.FS, format.SegmentName, core.NewDescriptorBudget(1))
		}
		ref, aerr := cache.Acquire(fate.ID)
		if aerr != nil {
			return aerr
		}
		cutErr := ref.Truncate(marker)
		if cutErr == nil {
			cutErr = ref.Sync()
		}
		if cutErr == nil && fate.Kind == fateTornTail {
			_, serr := segment.WriteSeal(set, ref, expect, fate.ID, marker, entries, true,
				func(core.SegmentID, int64) {})
			if serr != nil {
				if errors.Is(serr, segment.ErrSegmentEnded) {
					// The writer already ended the row after the failure of
					// its own write or sync: the device's arm of the swallow
					// below, which skips the row's end for the same reason.
					cutErr = serr
				} else {
					// A catalog the seal's own facts refuse, or the ledger
					// a durable seal: the open stops on the shape, never
					// serving a segment its seal could not honestly name.
					ref.Release()
					return serr
				}
			}
		}
		ref.Release()
		if cutErr == nil {
			if fate.Kind == fateTornTail {
				st.Tails.TornTails++
			}
			// decision: the ended cut leaves the row accounted at its cap, no
			// Shrink running, although the truncation took the file down. The
			// set's shrink bound reads the segment's charged record bytes, and
			// the record the proof judged torn or lost above the cut still
			// carries its charge — uncharging it here without charging the
			// re-append's target for it would undercount the share, and the
			// charge migration is the re-append and emptying's own to make.
			// The row therefore reads like a seal whose truncation sync
			// failed stays: conservative, accounted above a smaller file,
			// until the emptying unlinks the segment and its retirement
			// answers for the whole cap. Withdrawn when the retirement
			// recomputes what it frees or the charges migrate with their
			// records.
			continue
		}
		if isContextErr(cutErr) {
			return cutErr
		}
		// decision: a truncation, its sync or the seal writer's own writes
		// that fail leave the segment failed and unsealed for the open's
		// continuation, and the open goes on: the tail they would have cut
		// was never acknowledged — it is the tear the classing answered for
		// — and the records below it serve from wherever they verifiably
		// sit, so no served byte depends on the cut. The writer ends its
		// own failures' row in the set already, so the row's end below runs
		// only for a truncation that failed before the writer was reached —
		// for the continuation that reads rows must see a segment that
		// takes no append and no seal. The truncated bytes a failed sync
		// never made durable show nowhere: the next open re-reads the
		// segment whole, re-proves it and re-cuts it the same way. The
		// ceiling is a device that refuses the cut at every open, which
		// keeps counting the segment torn without ever sealing it — an
		// honest word for a device that will not close it.
		if fate.Kind == fateTornTail && !errors.Is(cutErr, segment.ErrSegmentEnded) {
			if uerr := set.End(fate.ID); uerr != nil {
				return errors.Join(cutErr, uerr)
			}
		}
	}
	return nil
}

// sealAnswers walks the segment's own bytes — as they stand before the cut —
// and answers the catalog the seal writes over them: one entry per record the
// walk attaches that begins before the cut, and the offset the marker takes.
// The marker is the cut itself, raised past the end of an attached record the
// cut would slice in half: the fold's one such shape is the synced-through
// that covers a rotted clone target's header while its record runs further,
// and a cut inside it would leave the next open a truncated record no
// catalog names and a half-file no walk can read past. The raised marker
// crosses no floor — a raise is above the cut, and the floors bound it from
// below — and no verifying record: the straddler's own header verified, its
// bytes stay whole below the marker, and the sealed segment's last entry
// ends exactly where its marker sits, the shape the seal writer demands and
// the footer's own reader computes.
func sealAnswers(rd *reader, identity core.JournalID, id core.SegmentID, sc segmentScan, cut int64) (entries []format.CatalogEntry, marker int64, err error) {
	walk, werr := format.ScanSegmentHeaders(readerAt{rd: rd, name: format.SegmentName(id)}, sc.Size, identity, id)
	if werr != nil {
		// The open's walk of these same bytes already succeeded, so an error
		// here is a read the bound did not clear.
		return nil, 0, refusedRead(rd, werr)
	}
	entries = make([]format.CatalogEntry, 0, len(walk.Records))
	marker = cut
	for _, r := range walk.Records {
		h := r.Header
		if h.Offset >= cut {
			break // the tail's own records begin where the cut answers
		}
		entries = append(entries, format.CatalogEntry{
			File:         h.File,
			Extent:       h.Extent,
			RecordOffset: h.Offset,
			Seq:          h.Seq,
			Version:      h.Version,
			Kind:         h.Kind,
			Tag:          h.Tag,
		})
		if end := h.Offset + h.Size(); end > marker {
			// decision: the raise can carry the seal's own footprint — the
			// marker, the footer and the trailer the capped segment must
			// hold after the raised end — past the segment cap, on the fold
			// shapes that floor the cut within that footprint of the cap, and
			// the seal writer then refuses its own catalog. That refusal is
			// the open's: every later open recomputes the same fold, the
			// same floors and the same raise over the same bytes, so a
			// spill into a second target or a sealed end below the floors
			// would each settle a question no asked rule answers, and the
			// journal that stays closed over a segment no seal fits is the
			// honest word until one does. Withdrawn by a packing rule that
			// says where such a tail's records go.
			marker = end
		}
	}
	return entries, marker, nil
}

// EndedSegments returns the ids of the segments the open this State came from
// ends — each fate the classing answered with an ending: a torn slot or a torn
// header the open truncates and the emptying unlinks, or a failed marker
// window the tail's proof left standing whole. Ascending, like the fates, so
// the first names the lowest. An open that ends no segment serves; one that
// ends any is the emptying's to finish, which is not built yet.
func (st State) EndedSegments() []core.SegmentID {
	var out []core.SegmentID
	for _, f := range st.Tails.Fates {
		if f.Kind == fateEnded {
			out = append(out, f.ID)
		}
	}
	return out
}

// corruptLoss spells the Loss the pass sends for the corrupt drop c: its
// columns from the record's own header where one verified and from the
// catalogue entry the confined drop carries where none did — a loss record
// built on zero columns would name no file and no extent, the shape the next
// open refuses at the record it reads back — with the synced flag the
// classing proved and the found-at-open mark of its leg. Deferred and its
// segment the caller names; both are the pass's own to decide on the
// reserve's answer.
func corruptLoss(c corruptRecord) durability.Loss {
	l := durability.Loss{
		Share:   c.Drop.Header.Tag,
		File:    c.Drop.Header.File,
		Extent:  c.Drop.Header.Extent,
		Version: c.Drop.Header.Version,
		Seq:     c.Drop.Header.Seq,
		Reason:  core.LossCorrupt,
		Synced:  c.Drop.Synced,
		AtOpen:  true,
	}
	if c.Drop.Entry.RecordOffset != 0 {
		h := headerFromEntry(c.Segment, c.Drop.Entry)
		l.Share, l.File, l.Extent, l.Version, l.Seq = h.Tag, h.File, h.Extent, h.Version, h.Seq
	}
	return l
}

// OpenLosses appends the open's own loss records through d, the layer the
// journal the recovery continued into was built over, once that journal
// exists and before the open returns: no phase of the recovery itself can
// run it, for the layer is not built until the open's continuation is.
//
// The records it owes, in the order it appends them: first every record a
// read-only journal failed earlier in this process and the rebuild found on
// disk — its eviction from the index the rebuild already made — each handed
// to Lose marked AtOpen, reported already — the read-only journal that
// failed the record published it when its window failed, so the open's own
// record appends and the ring holds no new event (RFC 1 section 5.4) — and
// counted against the open reserve, never deferred, with the sync the record
// never got; past the reserve the rest append nothing, for the copy that
// empties each segment still holding one leaves the failed records out, and
// no marker and no record names them. Then every corrupt drop no loss record on disk names — each one
// whose (seq, extent) no earlier Lose or Place of this journal took —
// handed to Lose marked AtOpen with the synced flag the classing proved,
// while the open reserve lasts; past the reserve each loss is deferred
// against the segment holding the dropped record, published marked deferred
// and named in the registry the copy that retires the segment reads. The
// reason of the deferred leg is the dedup's own: the piece of a corrupt
// drop already named is not handed over again, and no extent of a dropped
// record feeds twice.
//
// decision: R27's failed records take the reserve whole before any corrupt
// drop's draws on it — the failed records' loss records are the
// journal's own debt from the run that failed them, and past the reserve
// they gain nothing by deferral, for no copy writes their record, while a
// corrupt drop's deferral keeps its promise through the copy. The failed
// leg appends as many as the reserve holds and leaves the rest to the
// emptying (RFC 1 section 6); the corrupt leg defers everything past what
// the reserve has left. Withdrawn when the emptying writes the failed
// records' losses into its copies too.
//
// The found loss records themselves are placed first, into the layer's
// ring as found at open, taking their pieces once for whichever instance
// of the journal reads them: a loss record the disk already named is
// never appended again — the corrupt leg below hands the drop it names to
// no second Lose.
//
// The settle wait, when the caller passes one, precedes the first record any
// leg appends and nothing else: a pass whose every loss is named, or whose
// leftovers defer past the reserve, asks for no refill, so a clean reopen —
// and a name-only one over a device that cannot make a spare — block on
// nothing.
//
// Lose appends and syncs its record before it returns, so the open's caller
// holds loss records durable the moment each leg lands; a Lose that fails
// answers its error here, refusing the open — the piece it took stays
// taken, its record is owed to nothing, and its number is on the
// directory's failed list for the next open to answer. The generational
// raise, the marker and the counters are Lose's own, whichever call they
// came from: extension of the one rule, not a second one.
func OpenLosses(ctx context.Context, st State, d *durability.Durability, headroomRecords int, waitForSpares func() error) error {
	for _, lo := range st.Kinds.Losses {
		d.Place(lo.Ev, lo.Dropped)
	}

	// appends wait for the pool's settle before the first record: asked here
	// rather than ahead of the pass, for the placement above is what names
	// the pieces the found records hold — the first open of a process knows
	// a drop is named only once it has placed the record naming it. The wait
	// runs once, before the first record that appends — never for a
	// name-only pass over an open the disk answers whole, and never for a
	// deferred loss, whose record the copy writes. Nil waits on nothing, the
	// caller that has settled its pool already.
	settled := false
	settle := func() error {
		if settled || waitForSpares == nil {
			return nil
		}
		settled = true
		return waitForSpares()
	}

	// The open reserve: the records this pass may append before the journal
	// serves. The failed leg draws first and never defers; what it leaves,
	// decision: the open keeps the last records of the headroom for
	// itself, for the records an open must append before it serves are
	// few, bounded (RFC 1 section 6): a journal whose headroom ran out
	// before the crash still opens and still records what the crash
	// corrupted. Withdrawn when the open draws these records through the
	// capacity ledger instead of counting its own reserve.
	reserve := capacity.OpenReserveRecords(headroomRecords)

	for _, rd := range st.Kinds.ReadOnlyDrops {
		if reserve <= 0 {
			break // past the reserve nothing appends: the emptying takes the rest
		}
		l := durability.Loss{
			Share:    rd.Ev.Share,
			File:     rd.Ev.File,
			Extent:   rd.Ev.Extent,
			Version:  rd.Ev.Version,
			Seq:      rd.Dropped,
			Reason:   core.LossFailedWindow,
			AtOpen:   true,
			Reported: true,
		}
		// A record the disk already names draws none of the reserve: Lose
		// would answer the zero event and append nothing.
		if d.Named(l.Seq, l.Extent) {
			continue
		}
		reserve--
		if err := settle(); err != nil {
			return err
		}
		if _, err := d.Lose(ctx, l); err != nil {
			return err
		}
	}
	for _, c := range st.Tails.Corrupt {
		l := corruptLoss(c)
		// A drop the disk already names appends no second record and defers
		// against no segment; it draws none of the reserve either, for the
		// record Lose would answer the zero event and append nothing.
		if d.Named(l.Seq, l.Extent) {
			continue
		}
		if reserve <= 0 {
			// Past the reserve the loss defers: no record now, the piece
			// stays taken, and the copy that retires the segment owing the
			// dropped record writes the record from the registry entry.
			l.Deferred, l.Seg = true, c.Segment
		} else {
			reserve--
			if err := settle(); err != nil {
				return err
			}
		}
		if _, err := d.Lose(ctx, l); err != nil {
			return err
		}
	}
	return nil
}
