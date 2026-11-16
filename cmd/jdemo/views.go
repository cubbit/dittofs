package main

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// viewFS shows the journal's directory as it is: format file, segments,
// spares — the on-disk story the index, once it lands, names by id.
func (d *demo) viewFS() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		d.printf("filesystem: %v\n", err)
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
			text: fmt.Sprintf("%-32s %9s  %s", e.Name(), humanSize(size), note),
		})
	}
	// ReadDir's ascending names leave growth reading downward: the newest
	// (highest-id) segment is the last row.
	d.renderView("filesystem", rows)
	d.printf("state file (a sibling, outside the journal's dir): %s\n", statePath(d.dir))
}

// humanSize renders bytes at the scale a screen can carry: at distance,
// "256 MiB" reads and "268435456" does not.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatInt(n>>30, 10) + " GiB"
	case n >= 1<<20:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// viewStats shows the journal's totals and the per-share breakdown as
// diffable rows — the measured view, honest even where the read-outs
// behind other views are still stubs.
func (d *demo) viewStats() {
	st := d.j.Stats()
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
