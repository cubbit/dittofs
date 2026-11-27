package recovery

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
)

// predFate is one predecessor segment's verdict for this open: what became
// of a segment another's header names, decided before the spares phase runs
// so the take naming it is adopted or held on the predecessor's own state
// and not on evidence the phase would have to re-derive.
type predFate uint8

const (
	// predSealed is a predecessor on disk whose end its own evidence
	// proves — the marker the scan read, or the trailer naming a footer
	// past a rotted one, through the classing's marker-alone exemption.
	// Its take is re-adopted at this open.
	predSealed predFate = iota
	// predEnded is a predecessor the open ends — still on disk, its end
	// unproven, ended by the classing's fate or by this phase over a mark's
	// record-less segment. Its take is withheld: a predecessor the open
	// ends frees its take only once it is gone with a synced unlink, so the
	// emptying that unlinks it hands the take to the pool once the segment
	// is gone, and a take adopted before that unlink would zero the only
	// header naming the predecessor, leaving the next open reading its
	// predecessor with less evidence than this one had.
	predEnded
	// predGone is a predecessor no own, unverified or zero-header segment
	// on disk holds: its name answers nothing, so its take is the pool's
	// plain spare. Only a verified own header's claim counts — a foreign
	// segment is nobody's predecessor here.
	predGone
)

// String names the verdict as the rules above spell it.
func (f predFate) String() string {
	switch f {
	case predSealed:
		return "sealed"
	case predEnded:
		return "ended"
	case predGone:
		return "gone"
	}
	return "unknown"
}

// marksResults is the marks raise phase's answer: the marks themselves, one
// entry per named segment; the predecessor verdicts the spares phase holds
// takes by; and the segments withheld from the spare test — every segment
// a mark names, kept on disk for the marks steps that read what it holds.
type marksResults struct {
	set      map[core.SegmentID]core.SyncMark
	fates    map[core.SegmentID]predFate
	withheld map[core.SegmentID]bool
}

// runMarksRaise is the marks raise phase: the caller's sync marks read into
// the state every later phase consumes, raising and withholding before the
// tail's proof runs and before anything is appended, zeroed or taken from
// the pool, so a refused open changes nothing. For each mark, four jobs:
//
// The mark's Through joins the proofs the tail classing reads. A refused
// range — a torn or a zero slot included — below the Through in the mark's
// segment, whose catalog does not verify, is a range that names no record
// and refuses the open: the mark is the caller's durable record that its
// stream synced through the range, so its bytes held a record nothing on
// disk names, and the open never cuts it as the torn tail of a crash. A
// record the classing left unreconciled below the Through — a clone target
// whose body failed — folds into the corrupt drops, and a torn or zero
// header slot the Through covers counts as synced damage: the damaged set
// carries the segment until the journal closes, and the classing reports
// none of these where the mark is the only proof, for its probes read the
// device, and the mark is the caller's.
//
// The mark's segment is withheld from the spare test wherever it stands,
// and where it holds no verifying record it is ended — the fate that takes
// it no append — for a re-append landing in it would hide a lost tail it
// guards. The other fates stay as the classing wrote them, and a mark whose
// segment is not on disk is decided by the lost-tail check that runs after
// the tail's proof, which reads the absence as a retirement: the counters
// and the id seed below are raised for every mark regardless.
//
// The next sequence number the counters issue and the id seed the attach
// pass drew both end above the mark's own Sequence and Segment, so a
// restored or drained directory never reissues a number the caller's
// metadata still names.
//
// Every predecessor an own header names gets its verdict, decided here
// before the spares phase runs: gone where no disk segment holds the id,
// ended where the segment's fate ends it, sealed otherwise. The verdict is
// what the spares phase holds a take by — a take naming an ended
// predecessor stays with it until the emptying has unlinked it, and one
// naming a sealed predecessor is re-adopted at once.
func runMarksRaise(ctx context.Context, _ Deps, _ *reader, st *State, _ core.JournalID, _ core.Version, marks []core.SyncMark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	res := marksResults{
		set:      make(map[core.SegmentID]core.SyncMark, len(marks)),
		fates:    make(map[core.SegmentID]predFate, len(st.Attached.Own)),
		withheld: make(map[core.SegmentID]bool, len(marks)),
	}
	named := successorSet(st.Attached.Own)
	unverified := unverifiedSet(st.Attached)

	for _, m := range marks {
		s := core.SegmentID(m.Segment)
		res.set[s] = m
		// Every segment a mark names is withheld from the spare test and kept
		// on disk whatever its own bytes say: the marks steps that follow the
		// tail's proof read what it holds.
		res.withheld[s] = true
		// The counters and the id seed rise above every mark, whatever the
		// device returned of the segment it names: a drained directory never
		// reissues a number or an id the caller's metadata still names.
		if err := st.Rebuilt.Counters.Observe(index.Numbers{Seq: core.Seq(m.Sequence)}); err != nil {
			return err
		}
		if s > st.Attached.MaxID {
			st.Attached.MaxID = s
		}
		sc, ok := markScan(st, s)
		if !ok {
			// A mark whose segment is not on disk, or whose read no bounded
			// attempt completed: the first is a retirement the lost-tail
			// check reads after the tail's proof, the second a range the open
			// refuses on before any phase adopts. Neither is decided here.
			continue
		}
		// A segment the classing judged through its named or sealed arms
		// confined or refused its bad ranges by its own catalog already, so
		// the mark's Through owes those segments nothing; the range below is
		// the classing's torn half alone, the newest segment of a stream
		// whose catalog does not verify.
		if !named[s] && !sc.Sealed {
			for _, r := range sc.Ranges {
				if r.Off < m.Through {
					return refuseMarkedRange(s, r)
				}
			}
		}
		// A record whose header verified and whose body the classing left
		// unreconciled, below the Through: the sync covered it, so it is
		// corruption with a name its header gives it — a corrupt drop, its
		// own error carried, never a refusal. The classing arms the fold
		// complements are the torn ones alone, so each drop one of its own
		// proofs already corrupted is skipped here rather than counted
		// twice, and every drop below the Through it left torn folds.
		for _, d := range sc.Dropped {
			if d.Synced || sc.Sealed || named[s] || d.Header.Offset < sc.SyncedThrough {
				continue // the classing corrupted this one from its own proofs
			}
			if d.Header.Offset >= m.Through {
				continue // above the mark's Through, never proven synced
			}
			d.Synced = true
			st.Tails.Corrupt = append(st.Tails.Corrupt, corruptRecord{Segment: s, Drop: d})
			markDamaged(st, s)
			// The classing floors every cut a segment's own facts prove —
			// never below a synced-through its records name — and a mark's
			// Through is one more such sync, this one the caller's: the torn
			// verdict this fold reclassifies as corruption was cut below it,
			// and a cut below a mark's Through would seal a synced record
			// away.
			floorFate(st, s, min(m.Through, sc.Size))
		}
		// The header slot of a segment whose header did not verify: the
		// classing calls it torn where no proof covers it, damaged where one
		// does, and the mark's Through is one more proof — a sync that
		// reached past the slot makes what it holds synced damage.
		if unverified[s] && m.Through >= format.SegmentHeaderSize {
			markDamaged(st, s)
		}
		// The mark's record-less segment: no record header verifies
		// anywhere in it — the record-less test the spare phase runs,
		// RecordsEnd at the first record slot, a clone target whose header
		// verified and whose body failed counted with the records — so
		// nothing in it serves, and the emptying unlinks it. Offset zero
		// keeps the cutting phases off it — there is no record to cut
		// behind. A catalogued scan leaves RecordsEnd zero, naming a
		// segment the footer read builds only over records.
		if sc.RecordsEnd == format.SegmentHeaderSize {
			endSegment(st, s)
		}
	}

	// The verdict, after every mark is folded: a predecessor a mark ends
	// — the record-less segment above — keeps its fate once decided, and
	// the set holds one entry per named predecessor.
	onDisk := make(map[core.SegmentID]bool, len(st.Attached.Own)+len(st.Attached.Unverified)+len(st.Attached.ZeroHeader))
	for _, seg := range st.Attached.Own {
		onDisk[seg.ID] = true
	}
	for _, id := range slices.Concat(st.Attached.Unverified, st.Attached.ZeroHeader) {
		onDisk[id] = true
	}
	for _, seg := range st.Attached.Own {
		p := seg.Header.Predecessor
		if p == 0 || p == seg.ID {
			continue
		}
		// The map names every predecessor the topology did, the gone ones
		// too: a name no file holds is part of the answer the emptying
		// reads when it frees takes, and its entry frees the take by what
		// it says, not by being absent.
		if !onDisk[p] {
			res.fates[p] = predGone
			continue
		}
		res.fates[p] = onDiskVerdict(st, p)
	}

	sortTails(&st.Tails)
	st.Marks = res
	return nil
}

// runMarksLostTail is the marks phase's second step and the open's gate over
// a sync the device acknowledged and then lost: a lost tail is never read as
// a torn tail, for the mark is the caller's durable record that its stream
// synced through the range it names, and cutting what the device dropped as
// a crash's torn end would cut a record the sync answered for. The check
// runs after the tail's proof has verified every unproven tail and before
// the spares phase — the open's first writing one — so a refused open has
// written, zeroed and taken nothing, and every retry meets the same loss
// over the same directory.
//
// A mark's tail is lost when its segment is on disk with the last record
// that still held after the proof — the walk's last verifying record where
// the proof reached no tail, its own held end where it did — ending below
// the mark's Offset, and no copy of the record
// carrying the mark's Sequence verifies anywhere — seqVerifies answers that
// one question over the feed's map, the aside headers the scans keep and
// the records the tail's proof handed to the re-append from first reads
// that verified — and each of the three is checked, for a record about to be
// re-appended is not lost whatever the device holds. A lost tail refuses the
// open with ErrOpenRefused naming the mark's segment, its Offset and its
// Sequence. A mark whose segment is absent is decided first, and decided by
// nothing here: the retirement reading below owns it.
func runMarksLostTail(ctx context.Context, _ Deps, _ *reader, st *State, _ core.JournalID, _ core.Version, marks []core.SyncMark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The caller's own order, the one the raise judged the marks in: the
	// refusal names the first lost tail that order reaches, as the raise's
	// own refusals do.
	for _, m := range marks {
		sc, onDisk := markScan(st, core.SegmentID(m.Segment))
		if !onDisk {
			// ponytail: a segment's absence is taken as its retirement, so a
			// device that loses both a synced tail and the directory entry of
			// the segment holding it hides the loss. Upgrade to recording each
			// retirement where the marks can see it, a metadata write per
			// retired segment, when a lost directory entry is seen in the field.
			continue
		}
		// The end of the last record that still held, judged over the
		// post-proof state: the walk's own answer where the proof reached no
		// tail of the segment, and the proof's where it did — a record it
		// tore or judged corrupt holds nothing, whatever the walk attached.
		// A catalogue-built scan leaves RecordsEnd zero with its End at the
		// seal marker, where the records of the segment it proved ended; no
		// scan End lies below the header's own size.
		end := sc.RecordsEnd
		if end == 0 {
			end = sc.End
		}
		if held, proved := st.Proof.HeldEnd[core.SegmentID(m.Segment)]; proved {
			end = held
		}
		if end >= m.Offset {
			continue // the sync's last record is where the mark says it ended
		}
		if seqVerifies(st, core.Seq(m.Sequence)) {
			continue // a copy of the sync's record verifies, or the re-append carries one
		}
		return refuseLostTail(m)
	}
	return nil
}

// seqVerifies answers whether a copy of the record carrying seq verifies
// anywhere: the feed's map, one count per walk-attached copy save one the
// remainder pass confined, whose kept run failed the one read an open makes
// of a payload, and less one copy whose first read the tail's proof judged
// bad, for a copy judged rotten or torn holds nothing and its placement
// verifies the record nowhere — the aside headers the scans keep, for an
// aside kind never feeds, and the records the tail's proof verified at
// their first reads and handed to the re-append, which the re-append phase
// will write back.
func seqVerifies(st *State, seq core.Seq) bool {
	if st.Kinds.MarkedOnDisk[seq] > 0 {
		return true
	}
	for i := range st.Rebuilt.Scans {
		for _, a := range st.Rebuilt.Scans[i].Aside {
			if a.Header.Seq == seq {
				return true
			}
		}
	}
	for _, rec := range st.Proof.Reappends {
		if rec.Header.Seq == seq {
			return true
		}
	}
	return false
}

// refuseLostTail is the open's refusal of a sync the device acknowledged and
// then lost: the mark's segment is on disk, its last verifying record ends
// below the mark's Offset, and no copy of the record carrying the mark's
// Sequence verifies anywhere.
//
// decision: a device that loses an acknowledged sync keeps its journal closed
// until the reconcile's journal half exists — the lost record's extent may
// have superseded content a serving read would then serve stale, and the
// reconcile that would recover what it held is not built — so every retry
// meets the same refusal over an unchanged directory. Withdrawn when the
// reconcile's journal half lands.
func refuseLostTail(m core.SyncMark) error {
	return fmt.Errorf("%w: segment %d: the sync's acknowledged record, sequence %d ending at %d, verifies nowhere",
		core.ErrOpenRefused, m.Segment, m.Sequence, m.Offset)
}

// markedSeqs seeds the map the feed raises: one entry per sequence number a
// caller's sync mark names, left zero until a walk-attached copy carrying the
// number raises it one. Seeded by the marks' own numbers so the feed's
// per-record lookup costs one miss over the marks alone, never a write per
// record of the journal.
func markedSeqs(marks []core.SyncMark) map[core.Seq]int {
	if len(marks) == 0 {
		return nil
	}
	m := make(map[core.Seq]int, len(marks))
	for _, mk := range marks {
		m[core.Seq(mk.Sequence)] = 0
	}
	return m
}

// markScan finds the mark's segment's scan in the rebuild's answer,
// reporting whether the segment was attached and scanned at all.
func markScan(st *State, id core.SegmentID) (segmentScan, bool) {
	for i := range st.Rebuilt.Scans {
		if st.Rebuilt.Scans[i].ID == id {
			return st.Rebuilt.Scans[i], true
		}
	}
	return segmentScan{}, false
}

// onDiskVerdict answers the verdict a predecessor the directory still holds
// carries: ended where its classed fate ends it, sealed otherwise. A
// predecessor the open could not class — no fate entry, a read the bound
// did not clear — holds its take, the conservative answer, for the open
// refuses on its ranges before the spares phase adopts anything and no
// verdict may release a take over evidence nobody read.
func onDiskVerdict(st *State, p core.SegmentID) predFate {
	for _, f := range st.Tails.Fates {
		if f.ID == p {
			if f.Kind == fateEnded {
				return predEnded
			}
			return predSealed
		}
	}
	return predEnded
}

// endSegment folds the ended fate over segment id into the state's fates, in
// place: an entry the classing already gave keeps its id and takes the ended
// kind with nothing to cut, and a segment the classing gave no entry — the
// record-less spare shapes it hands to the spare test — gains one. Offset
// zero is the ending whole: the segment holds no record to cut behind, so
// its removal is the emptying's unlink.
func endSegment(st *State, id core.SegmentID) {
	for i := range st.Tails.Fates {
		if st.Tails.Fates[i].ID == id {
			st.Tails.Fates[i].Kind = fateEnded
			st.Tails.Fates[i].Offset = 0
			return
		}
	}
	st.Tails.Fates = append(st.Tails.Fates, segmentFate{ID: id, Kind: fateEnded})
	slices.SortFunc(st.Tails.Fates, func(a, b segmentFate) int { return cmp.Compare(a.ID, b.ID) })
}

// floorFate raises segment id's cut to at least off, never lowering one and
// never cutting into an ended fate that owns no cut at all — its zero
// offset is the ending whole, not a cut to raise. The classing floors every
// cut at the synced-through a segment's own records name; this answers the
// same fold for a proof the classing never read, so a cut never crosses
// below bytes a sync answered for.
func floorFate(st *State, id core.SegmentID, off int64) {
	for i := range st.Tails.Fates {
		if f := &st.Tails.Fates[i]; f.ID == id && f.Offset != 0 && f.Offset < off {
			f.Offset = off
		}
	}
}

// markDamaged folds segment id into the damaged set, once: a torn fact a
// mark proved synced is reported damage, and the open keeps counting the
// segment until the journal closes, whatever later phases do with it.
func markDamaged(st *State, id core.SegmentID) {
	if !slices.Contains(st.Tails.Damaged, id) {
		st.Tails.Damaged = append(st.Tails.Damaged, id)
	}
}

// refuseMarkedRange is the open's refusal of a byte range below a sync
// mark's Through in the mark's own segment, whose catalog does not verify:
// a torn slot, a zero slot or a rotted record the walk refused.
//
// decision: the range is the whole open's refusal rather than a dropped
// record, because the mark proves its bytes held a record no catalog names
// — the segment is one no successor names, so nothing on disk holds its
// boundaries — and the reconcile that would recover what it held is not
// built, so serving past it could serve without knowing what was lost. The
// ceiling is a journal that stays closed over a rotted record header below
// a mark's Through, over a truncated named predecessor and over a refusal
// no catalog names, until the reconcile work lands, which withdraws this
// marker.
func refuseMarkedRange(id core.SegmentID, r format.ScanRange) error {
	return fmt.Errorf("%w: segment %d: a refused range at [%d,%d) below a mark's Through that names no record",
		core.ErrOpenRefused, id, r.Off, r.End)
}
