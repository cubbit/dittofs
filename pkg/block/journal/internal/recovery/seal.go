package recovery

import (
	"context"
	"errors"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
