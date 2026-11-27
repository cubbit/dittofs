//go:build jdemo

package jdemo

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/journal"
)

// viewFS shows the journal's directory as it is: format file, segments,
// spares — the on-disk story the index, once it lands, names by id.
func (d *Demo) viewFS() {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		d.Printf("filesystem: %v\n", err)
		return
	}
	rows := make([]viewRow, 0, len(entries))
	for _, e := range entries {
		var note string
		switch {
		case strings.HasSuffix(e.Name(), ".seg"):
			note = "segment"
		case strings.HasSuffix(e.Name(), ".spare.tmp"):
			note = "spare (spooling)"
		case e.Name() == "format":
			note = "the journal's identity, version, settings"
		default:
			note = "unknown to the demo"
		}
		size := int64(0)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		rows = append(rows, viewRow{
			key:  e.Name(),
			text: fmt.Sprintf("%-32s %9s  %s", e.Name(), HumanSize(size), note),
		})
	}
	// ReadDir's ascending names leave growth reading downward: the newest
	// (highest-id) segment is the last row.
	d.renderView("filesystem", rows)
	d.Printf("state file (a sibling, outside the journal's dir): %s\n", StatePath(d.Dir))
}

// humanSize renders bytes at the scale a screen can carry: at distance,
// "256 MiB" reads and "268435456" does not.
func HumanSize(n int64) string {
	for _, u := range []struct {
		shift uint
		name  string
	}{{30, "GiB"}, {20, "MiB"}, {10, "KiB"}} {
		if n >= 1<<u.shift {
			// Two decimals, trailing zeros trimmed: 1.25 GiB must not read
			// as "1 GiB", and a whole number stays whole.
			t := strconv.FormatFloat(float64(n)/float64(int64(1)<<u.shift), 'f', 2, 64)
			t = strings.TrimSuffix(strings.TrimRight(t, "0"), ".")
			return t + " " + u.name
		}
	}
	return strconv.FormatInt(n, 10) + " B"
}

// Bytes is HumanSize with no space ("80MiB"), the way the demo's call lines
// write a size argument.
func Bytes(n int64) string { return strings.ReplaceAll(HumanSize(n), " ", "") }

// journalClosed answers plainly when a view runs with no live journal:
// a story that closes mid-run still offers the chooser, and the view names
// the closure rather than dereferencing what it would read. The journal
// itself answers "journal: closed" on a closed handle; nil is the demo's
// own state, and the message stays in the same voice.
func (d *Demo) journalClosed() bool {
	if d.J == nil {
		d.Printf("the journal is closed — this view lives while a journal does\n")
		return true
	}
	return false
}

// viewStats shows the journal's totals and the per-share breakdown as
// diffable rows — the measured view, honest even where the read-outs
// behind other views are still stubs.
func (d *Demo) viewStats() {
	if d.journalClosed() {
		return
	}
	st := d.J.Stats()
	rows := []viewRow{
		{key: "max bytes", text: fmt.Sprintf("max bytes          %12d", st.MaxBytes)},
		{key: "used", text: fmt.Sprintf("used               %12d", st.UsedBytes)},
		{key: "reserved", text: fmt.Sprintf("reserved           %12d", st.ReservedBytes)},
		{key: "held", text: fmt.Sprintf("held               %12d", st.HeldBytes)},
		{key: "dirty", text: fmt.Sprintf("dirty              %12d", st.DirtyBytes)},
		{key: "files", text: fmt.Sprintf("files              %12d", st.Files)},
		{key: "index entries", text: fmt.Sprintf("index entries      %12d", st.ExtentCount)},
		{key: "segments", text: fmt.Sprintf("segments           %12d (%d sealed)", st.Segments, st.Sealed)},
		{key: "spares", text: fmt.Sprintf("spares             %12d", st.Spares)},
		{key: "unreclaimed", text: fmt.Sprintf("unreclaimed        %12d", st.UnreclaimedBytes)},
		{key: "removal markers", text: fmt.Sprintf("removal markers    %12d", st.RemovalMarkers)},
		{key: "versions left", text: fmt.Sprintf("versions left      %12d", st.VersionsLeft)},
	}
	// The rows are sorted by tag below the totals: a map's iteration is
	// not an order, and screens must not reshuffle between looks.
	tags := make([]journal.ShareTag, 0, len(st.Shares))
	for tag := range st.Shares {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	for _, tag := range tags {
		s := st.Shares[tag]
		rows = append(rows, viewRow{
			key:  fmt.Sprintf("share %d", tag),
			text: fmt.Sprintf("share %-12d %9d used %7d held %7d dirty", tag, s.UsedBytes, s.HeldBytes, s.DirtyBytes),
		})
	}
	d.renderView("stats", rows)
}

// viewIndex is the (i) view: the story file's placement entries, per the
// index the journal reports. The read-out's honesty rule: the journal's
// own answer prints verbatim, today's refusal included, and the view never
// invents rows.
func (d *Demo) viewIndex() {
	if d.journalClosed() {
		return
	}
	es, err := d.J.Inspect().File(StoryFile)
	if err != nil {
		d.Printf("index: %v\n", err)
		return
	}
	if !d.recording {
		if len(es) > 0 {
			d.Printf("FILE · owner tag %d\n", es[0].Tag)
		}
		for _, line := range indexRuler(es) {
			d.Printf("%s\n", line)
		}
		d.Printf("\n")
	}
	rows := indexRows(es)
	d.renderView("index", rows)
}

// viewSegments is the (s) view: per-segment accounting.
func (d *Demo) viewSegments() {
	if d.journalClosed() {
		return
	}
	segs := d.J.Inspect().Segments()
	if len(segs) == 0 {
		if d.recording {
			d.renderView("segments", nil) // an empty baseline: the first segment shows as new, not as a first look
			return
		}
		d.Printf("(the journal reports no segments)\n")
		return
	}
	rows := make([]viewRow, 0, len(segs))
	for _, s := range segs {
		rows = append(rows, viewRow{
			key:  fmt.Sprintf("seg %d", s.ID),
			text: fmt.Sprintf("seg %-3d stream %d  allocated %9d  held %9d  retained %9d  %s", s.ID, s.Stream, s.Allocated, s.HeldBytes, s.RetainedBytes, segState(s)),
		})
	}
	d.renderView("segments", rows)
}

func segState(s journal.SegmentInfo) string {
	var st string
	if s.Sealed {
		st += "sealed "
	}
	if s.Failed {
		st += "failed "
	}
	if s.Damaged {
		st += "damaged"
	}
	if st == "" {
		st = "active"
	}
	return st
}
