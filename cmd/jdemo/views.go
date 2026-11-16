package main

import (
	"os"
	"strconv"
	"strings"
)

// fileRow is one row of the filesystem view: a thing in the journal's
// directory, its size in bytes, and any note.
type fileRow struct {
	name string
	size int64
	note string
}

// viewFS shows the journal's directory as it is: format file, segments,
// spares — the on-disk story the index, once it lands, names by id.
func (d *demo) viewFS() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		d.printf("filesystem: %v\n", err)
		return
	}
	rows := make([]fileRow, 0, len(entries))
	for _, e := range entries {
		var row fileRow
		switch {
		case strings.HasSuffix(e.Name(), ".seg"):
			row = fileRow{name: e.Name(), note: "segment"}
		case strings.HasSuffix(e.Name(), ".spare.tmp"):
			row = fileRow{name: e.Name(), note: "spare (spooling)"}
		case e.Name() == "format":
			row = fileRow{name: e.Name(), note: "the journal's identity, version, settings"}
		default:
			row = fileRow{name: e.Name(), note: "unknown to the demo"}
		}
		if info, err := e.Info(); err == nil {
			row.size = info.Size()
		}
		rows = append(rows, row)
	}
	renderFiles(d, rows)
	d.printf("state file (a sibling, outside the journal's dir): %s\n", statePath(d.dir))
}

// renderFiles prints fileRows through the demo's screen. Sizes are human
// ones: at distance, "256 MiB" reads and "268435456" does not.
func renderFiles(d *demo, rows []fileRow) {
	d.printf("%-32s %-9s %s\n", "name", "size", "what")
	for _, r := range rows {
		d.printf("%-32s %-9s %s\n", r.name, humanSize(r.size), r.note)
	}
}

// humanSize renders bytes at the scale a screen can carry.
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

// viewStats shows the journal's totals and the per-share breakdown —
// the measured view, honest even where the read-outs behind other views
// are still stubs.
func (d *demo) viewStats() {
	renderStats(d.out, d.j.Stats())
}
