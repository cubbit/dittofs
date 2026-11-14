package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// friendlyLine turns err into the one line a presenter can act on: what
// happened and, where it is on the user to fix, what to do.
func friendlyLine(err error) string {
	var un *unreachableError
	if errors.As(err, &un) {
		return fmt.Sprintf("jdemo: daemon not reachable at %s — run 'jdemo start' first", un.path)
	}
	return fmt.Sprintf("jdemo: %v", err)
}

// panePrint swallows its write error: a demo's state reaching nowhere is
// the only failure it can report, and reporting it nowhere is that failure
// already. Every renderer prints through here, so one explicit discard
// stands for the whole file.
func panePrint(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// newTabWriter returns a tabwriter over w, flushable per view.
func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
}

// entryKindName is the one word an index entry's kind prints.
func entryKindName(k journal.EntryKind) string {
	switch k {
	case journal.EntryHeld:
		return "held"
	case journal.EntryRetained:
		return "retained"
	case journal.EntryMarker:
		return "marker"
	}
	return fmt.Sprintf("kind%d", uint8(k))
}

// indexRow is one index entry with the file it belongs to, so All's
// interleaved files stay distinguishable on screen.
type indexRow struct {
	file  string // the file's short hex, 8 chars
	entry journal.IndexEntry
}

// renderIndexRows prints one row per entry: file, kind, extent range,
// version, the backing record's segment and offset, the owning tag, the
// offloaded bit.
func renderIndexRows(w io.Writer, rows []indexRow) {
	if len(rows) == 0 {
		panePrint(w, "no index entries\n")
		return
	}
	tw := newTabWriter(w)
	panePrint(tw, "file\tkind\trange\tversion\trecord\tag\toffloaded\n")
	for _, r := range rows {
		e := r.entry
		offloaded := "no"
		if e.Offloaded {
			offloaded = "yes"
		}
		panePrint(tw, "%s\t%s\t%d–%d\tv%s\t%d/%d\t%d\t%s\n",
			r.file, entryKindName(e.Kind), e.Extent.Off, e.Extent.Off+e.Extent.Len,
			fmtVersion(e.Version), e.Location.Segment, e.Location.Offset,
			e.Tag, offloaded)
	}
	_ = tw.Flush()
}

// renderSegments prints one row per segment: id, stream, state flags, and the
// accounting the index behind `jdemo stats` sums.
func renderSegments(w io.Writer, segs []journal.SegmentInfo) {
	if len(segs) == 0 {
		panePrint(w, "no segments\n")
		return
	}
	tw := newTabWriter(w)
	panePrint(tw, "id\tstream\tallocated\theld\tretained\tstate\n")
	for _, s := range segs {
		flags := ""
		if s.Sealed {
			flags += "sealed "
		}
		if s.Failed {
			flags += "failed "
		}
		if s.Damaged {
			flags += "damaged"
		}
		if flags == "" {
			flags = "active"
		}
		panePrint(tw, "%d\t%d\t%d\t%d\t%d\t%s\n",
			s.ID, s.Stream, s.Allocated, s.HeldBytes, s.RetainedBytes, flags)
	}
	_ = tw.Flush()
}

// renderVerify prints what a verification pass found, counts first so a
// clean pass reads at a glance.
func renderVerify(w io.Writer, rep verifyJSON) {
	tw := newTabWriter(w)
	panePrint(tw, "%d unsupported entries\n", rep.Unsupported)
	panePrint(tw, "%d unindexed records\n", rep.Unindexed)
	panePrint(tw, "%d bad footers\n", rep.BadFooters)
	panePrint(tw, "footprint mismatch: %d bytes\n", rep.Mismatch)
	_ = tw.Flush()
}

// renderStats prints the journal's totals and the per-share breakdown behind
// them — the viewer answer for "what changed?".
func renderStats(w io.Writer, st journal.Stats) {
	tw := newTabWriter(w)
	panePrint(tw, "name\tvalue\n")
	panePrint(tw, "max bytes\t%d\n", st.MaxBytes)
	panePrint(tw, "used\t%d\n", st.UsedBytes)
	panePrint(tw, "reserved\t%d\n", st.ReservedBytes)
	panePrint(tw, "held\t%d\n", st.HeldBytes)
	panePrint(tw, "dirty\t%d\n", st.DirtyBytes)
	panePrint(tw, "files\t%d\n", st.Files)
	panePrint(tw, "index entries\t%d\n", st.ExtentCount)
	panePrint(tw, "segments\t%d (%d sealed)\n", st.Segments, st.Sealed)
	panePrint(tw, "spares\t%d\n", st.Spares)
	panePrint(tw, "unreclaimed\t%d\n", st.UnreclaimedBytes)
	panePrint(tw, "removal markers\t%d\n", st.RemovalMarkers)
	panePrint(tw, "versions left\t%d\n", st.VersionsLeft)
	if len(st.Losses) > 0 {
		panePrint(tw, "losses\t%d (generation %d)\n", len(st.Losses), st.LossGeneration)
	}
	if st.DamagedSegments > 0 {
		panePrint(tw, "damaged segments\t%d\n", st.DamagedSegments)
	}
	_ = tw.Flush()

	if len(st.Shares) > 0 {
		panePrint(w, "\n")
		// The rows are sorted by tag: a map's iteration is not an order,
		// and the panes must not reshuffle between refreshes.
		tags := make([]journal.ShareTag, 0, len(st.Shares))
		for tag := range st.Shares {
			tags = append(tags, tag)
		}
		slices.Sort(tags)
		tw := newTabWriter(w)
		panePrint(tw, "share\tlimit\tused\theld\tdirty\tunreclaimed\n")
		for _, tag := range tags {
			s := st.Shares[tag]
			panePrint(tw, "share %d\t%d\t%d\t%d\t%d\t%d\n",
				tag, s.Limit, s.UsedBytes, s.HeldBytes, s.DirtyBytes, s.UnreclaimedBytes)
		}
		_ = tw.Flush()
	}
}
