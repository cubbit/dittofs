package recovery

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// SyncMarks answers, one per append stream whose records the run recovered,
// the mark of what it recovered: the sequence number of the stream's last
// verifying record and the place that record verifies now, the segment
// holding it and the end of its whole on-disk bytes — a mark's Through and
// Offset both name that end — with a record the re-append phase wrote into
// its target counted at the copy, for the copy is where it verifies and the
// source is the placement the device lost or the emptying unlinks. A stream
// whose records verify nowhere has no entry, and none joins from a segment
// whose stream nothing the run reads can name. Only a segment that stands
// offers a mark.
//
// The answer is computed once, over the state the phases already collected —
// the re-append's answers and the scans' own last-kept pairs, never a walk
// of anything — so the caller reads it at the open's settle and serves it as
// a figure. What it holds is the recovered position, not the running
// journal's own marks: a stream the caller writes through the journal this
// open assembled keeps its entry until the group commit records whatever the
// next sync answers.
//
// decision: a Replaced entry, the re-append's and the emptying's alike, is
// attributed by its source segment's stream, and the copy target's own
// rebuild scan is attributed to no stream, for the copier writes the
// target's header with format.NoStream — its records come from several
// streams — which streamSteads leaves out unless a caller's mark names the
// copy. So the copy's records join through the Replaced entries of the open
// that wrote it. A later open that no recorded mark names the copy for
// answers the stream's older standing segment, a mark below the stream's
// last verifying record, which the copy holds; the open's caller hands back
// the mark it recorded, which raises it. A source the caller recorded no
// mark for and whose own header did not verify names no stream, so its
// stream gets no entry from the copy. A source whose records a sealed copy
// already held credits that copy's position to its stream. A catalogued
// forget record sets its segment's last sequence number from its catalogue
// entry, which no read of this open checks against the disk, so a stream's
// last verifying record may be named by an entry a record never verified,
// held to what the footer's word buys. A copy written before the copier
// named NoStream carries stream 0 and is read as stream 0's segment.
//
// The figure is also the open's alone: Stats.SyncMarks holds it fixed until
// close, though the mark of a stream's last completed sync moves with every
// later sync, so a caller that writes it back on each commit would overwrite
// a later sync's mark with this one. Withdrawn when the open records each
// copy's stream durably beside it, a metadata write per copy, or when a copy
// header names the source's stream; overturned by a field open that reports
// a stream's entry lower than its last record where a copy holds it.
func (st State) SyncMarks() []core.SyncMark {
	steads := st.streamSteads()

	// One mark per stream, the highest sequence number the stream's verifying
	// records carried.
	type entry struct {
		seq  core.Seq
		mark core.SyncMark
	}
	best := make(map[uint32]entry, len(st.Rebuilt.Scans))
	keep := func(stream uint32, seq core.Seq, mark core.SyncMark) {
		if b, ok := best[stream]; ok && b.seq >= seq {
			return
		}
		best[stream] = entry{seq: seq, mark: mark}
	}

	// A record the re-append replaced names the copy, not the placement the
	// device lost or failed: the source's own scan keeps reading the old
	// bytes, so the sources are left out of the scans below.
	sources := make(map[core.SegmentID]bool, len(st.Proof.Replaced)+len(st.Proof.Superseded))
	credit := func(r replacedRecord) {
		sources[r.Source] = true
		// A copy a later step retired is no placement to name.
		if !st.stands(r.Target) {
			return
		}
		if stream, ok := steads[r.Source]; ok {
			keep(stream, r.Seq, core.SyncMark{
				Stream:   stream,
				Segment:  uint64(r.Target),
				Through:  r.End,
				Offset:   r.End,
				Sequence: uint64(r.Seq),
			})
		}
	}
	for _, r := range st.Proof.Superseded {
		credit(r)
	}
	for _, r := range st.Proof.Replaced {
		credit(r)
	}

	for i := range st.Rebuilt.Scans {
		sc := &st.Rebuilt.Scans[i]
		if sc.LastSeq == 0 || sources[sc.ID] || !st.stands(sc.ID) {
			continue
		}
		stream, ok := steads[sc.ID]
		if !ok {
			continue
		}
		keep(stream, sc.LastSeq, core.SyncMark{
			Stream:   stream,
			Segment:  uint64(sc.ID),
			Through:  sc.LastEnd,
			Offset:   sc.LastEnd,
			Sequence: uint64(sc.LastSeq),
		})
	}

	out := make([]core.SyncMark, 0, len(best))
	for _, e := range best {
		out = append(out, e.mark)
	}
	slices.SortFunc(out, func(a, b core.SyncMark) int { return cmp.Compare(a.Stream, b.Stream) })
	return out
}

// stands answers whether segment id is still in the open's ledger: a segment
// the emptying copied out and unlinked, or the retire step retired, is gone
// from it, and no mark may name a file that is not on disk. A state without
// an assembled ledger, one no emptying ran over, answers true for every id.
func (st State) stands(id core.SegmentID) bool {
	if st.Assembled.Set == nil {
		return true
	}
	_, err := st.Assembled.Set.State(id)
	return err == nil
}

// streamSteads answers the append stream each segment's bytes name: the one
// a caller's recorded mark answers first — the only word on a copy target,
// whose header the copier writes with no stream number, and a torn header
// slot's recovery beside it — and the one the segment's own verified header
// carries otherwise. A segment the map leaves out is one whose stream nothing
// the run reads can name.
func (st State) streamSteads() map[core.SegmentID]uint32 {
	out := make(map[core.SegmentID]uint32, len(st.Attached.Own)+len(st.Marks.set))
	for _, seg := range st.Attached.Own {
		if seg.Header.Stream != format.NoStream {
			out[seg.ID] = seg.Header.Stream
		}
	}
	for id, m := range st.Marks.set {
		out[id] = m.Stream
	}
	return out
}
