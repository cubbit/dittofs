package main

import (
	"fmt"
	"io"

	"text/tabwriter"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// friendlyLine turns err into the one line a presenter can act on. The
// in-process driver has nobody else's reachability to diagnose, so the
// journal's own words are the whole story.
func friendlyLine(err error) string {
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
