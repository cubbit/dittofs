package recovery

import (
	"cmp"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// SyncMarks answers, one per append stream whose records the run recovered,
// the mark of what it recovered: the sequence number of the stream's last
// verifying record and the place that record verifies now, the segment
// holding it and the end of its whole on-disk bytes — a mark's Through and
// Offset both name that end — with a record the re-append phase wrote into
// its target counted at the copy, for the copy is where it verifies and the
// source is the placement the device lost or the emptying unlinks. A stream
// whose records verify nowhere has no entry, and none joins from a segment
// whose stream nothing the run reads can name.
//
// The answer is computed once, over the state the phases already collected —
// the re-append's answers and the scans' own last-kept pairs, never a walk
// of anything — so the caller reads it at the open's settle and serves it as
// a figure. What it holds is the recovered position, not the running
// journal's own marks: a stream the caller writes through the journal this
// open assembled keeps its entry until the group commit records whatever the
// next sync answers.
//
// decision: a segment a caller's mark names is attributed to the mark's
// stream over its own header's word — the only stream a copy target has at
// all, for the copier writes its header with no stream number and the
// recorded mark is what a crash between the copy and its record leaves
// behind. A copy target no mark was recorded for yet, or a copy batch holding
// several streams' records, reports under the number its header names: the
// open cannot name a stream the directory's own bytes do not. Withdrawn when
// the open records each copy's stream durably beside it, a metadata write per
// copy, or by a copy header that names the source's stream.
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
	sources := make(map[core.SegmentID]bool, len(st.Proof.Replaced))
	for _, r := range st.Proof.Replaced {
		sources[r.Source] = true
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

	for i := range st.Rebuilt.Scans {
		sc := &st.Rebuilt.Scans[i]
		if sc.LastSeq == 0 || sources[sc.ID] {
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

// streamSteads answers the append stream each segment's bytes name: the one
// a caller's recorded mark answers first — the only word on a copy target,
// whose header the copier writes with no stream number, and a torn header
// slot's recovery beside it — and the one the segment's own verified header
// carries otherwise. A segment the map leaves out is one whose stream nothing
// the run reads can name.
func (st State) streamSteads() map[core.SegmentID]uint32 {
	out := make(map[core.SegmentID]uint32, len(st.Attached.Own)+len(st.Marks.set))
	for _, seg := range st.Attached.Own {
		out[seg.ID] = seg.Header.Stream
	}
	for id, m := range st.Marks.set {
		out[id] = m.Stream
	}
	return out
}
