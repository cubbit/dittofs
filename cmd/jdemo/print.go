package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// friendly prints err as one line a presenter can act on: what happened and,
// where it is on the user to fix, what to do.
func friendly(w io.Writer, err error) {
	var un *unreachableError
	if errors.As(err, &un) {
		fmt.Fprintf(w, "jdemo: daemon not reachable at %s — run 'jdemo start' first\n", un.path)
		return
	}
	fmt.Fprintf(w, "jdemo: %v\n", err)
}

// fail is friendly for a fatal CLI error: print and exit non-zero.
func fail(err error) {
	friendly(os.Stderr, err)
	os.Exit(1)
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

// renderIndexEntries prints one row per entry: kind, extent range, version,
// the backing record's segment and offset, the owning tag, the offloaded bit.
func renderIndexEntries(w io.Writer, es []journal.IndexEntry) {
	if len(es) == 0 {
		fmt.Fprintln(w, "no index entries")
		return
	}
	tw := newTabWriter(w)
	fmt.Fprint(tw, "kind\trange\tversion\trecord\tag\toffloaded\n")
	for _, e := range es {
		offloaded := "no"
		if e.Offloaded {
			offloaded = "yes"
		}
		fmt.Fprintf(tw, "%s\t%d–%d\tv%s\t%d/%d\t%d\t%s\n",
			entryKindName(e.Kind), e.Extent.Off, e.Extent.Off+e.Extent.Len,
			fmtVersion(e.Version), e.Location.Segment, e.Location.Offset,
			e.Tag, offloaded)
	}
	tw.Flush()
}

// renderSegments prints one row per segment: id, stream, state flags, and the
// accounting the index behind `jdemo stats` sums.
func renderSegments(w io.Writer, segs []journal.SegmentInfo) {
	if len(segs) == 0 {
		fmt.Fprintln(w, "no segments")
		return
	}
	tw := newTabWriter(w)
	fmt.Fprint(tw, "id\tstream\tallocated\theld\tretained\tstate\n")
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
		fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%s\n",
			s.ID, s.Stream, s.Allocated, s.HeldBytes, s.RetainedBytes, flags)
	}
	tw.Flush()
}

// renderVerify prints what a verification pass found, counts first so a
// clean pass reads at a glance.
func renderVerify(w io.Writer, rep verifyJSON) {
	tw := newTabWriter(w)
	fmt.Fprintf(tw, "%d unsupported entries\n", rep.Unsupported)
	fmt.Fprintf(tw, "%d unindexed records\n", rep.Unindexed)
	fmt.Fprintf(tw, "%d bad footers\n", rep.BadFooters)
	fmt.Fprintf(tw, "footprint mismatch: %d bytes\n", rep.Mismatch)
	tw.Flush()
}

// renderStats prints the journal's totals and the per-share breakdown behind
// them — the viewer answer for "what changed?".
func renderStats(w io.Writer, st journal.Stats) {
	tw := newTabWriter(w)
	fmt.Fprint(tw, "name\tvalue\n")
	fmt.Fprintf(tw, "max bytes\t%d\n", st.MaxBytes)
	fmt.Fprintf(tw, "used\t%d\n", st.UsedBytes)
	fmt.Fprintf(tw, "reserved\t%d\n", st.ReservedBytes)
	fmt.Fprintf(tw, "held\t%d\n", st.HeldBytes)
	fmt.Fprintf(tw, "dirty\t%d\n", st.DirtyBytes)
	fmt.Fprintf(tw, "files\t%d\n", st.Files)
	fmt.Fprintf(tw, "index entries\t%d\n", st.ExtentCount)
	fmt.Fprintf(tw, "segments\t%d (%d sealed)\n", st.Segments, st.Sealed)
	fmt.Fprintf(tw, "spares\t%d\n", st.Spares)
	fmt.Fprintf(tw, "unreclaimed\t%d\n", st.UnreclaimedBytes)
	fmt.Fprintf(tw, "removal markers\t%d\n", st.RemovalMarkers)
	fmt.Fprintf(tw, "versions left\t%d\n", st.VersionsLeft)
	if len(st.Losses) > 0 {
		fmt.Fprintf(tw, "losses\t%d (generation %d)\n", len(st.Losses), st.LossGeneration)
	}
	if st.DamagedSegments > 0 {
		fmt.Fprintf(tw, "damaged segments\t%d\n", st.DamagedSegments)
	}
	tw.Flush()

	if len(st.Shares) > 0 {
		fmt.Fprintln(w)
		tw := newTabWriter(w)
		fmt.Fprint(tw, "share\tlimit\tused\theld\tdirty\tunreclaimed\n")
		for tag, s := range st.Shares {
			fmt.Fprintf(tw, "share %d\t%d\t%d\t%d\t%d\t%d\n",
				tag, s.Limit, s.UsedBytes, s.HeldBytes, s.DirtyBytes, s.UnreclaimedBytes)
		}
		tw.Flush()
	}
}
