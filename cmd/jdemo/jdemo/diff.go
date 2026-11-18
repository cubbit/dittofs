package jdemo

import (
	"fmt"
	"slices"
	"strings"
)

// viewRow is one row of any view, with the stable key the diff layer
// tracks it by — an index entry's extent, a segment's id, a file's name:
// whatever the row *is*, not how it prints.
type viewRow struct {
	key  string
	text string
}

// renderView shows a view's rows, highlighted against the state the view had
// just before the beat that ran: additions green-marked "+", changes
// yellow-marked "~" carrying the new text, removals red "- removed" naming
// what went. The baseline is the beat's, not the viewer's: a view skipped for
// two beats shows only the second beat's changes, and a second look in the
// same beat shows the same diff. A view with no baseline yet — the beat that
// opens the journal has nothing before it — prints plain and says so, because
// diffing against nothing would mark every row new.
//
// While snapshotViews records, renderView stores the rows as the baseline and
// prints nothing.
//
// The glyphs are text, not colour: a piped transcript keeps the "+ two"
// that said "this appeared" even where it keeps no escape bytes. Colours
// come from the writer's gate.
func (d *Demo) renderView(name string, rows []viewRow) string {
	c := coloursFor(d.out)
	if d.base == nil {
		d.base = make(map[string]map[string]string)
	}
	cur := make(map[string]string, len(rows))
	for _, r := range rows {
		cur[r.key] = r.text
	}
	if d.recording {
		d.base[name] = cur
		return ""
	}
	last := d.base[name]

	var sb strings.Builder
	if last == nil {
		// The whole set is new information, not a diff.
		for _, r := range rows {
			fmt.Fprintf(&sb, "%s\n", c.dim(r.text))
		}
		sb.WriteString("(first look)\n")
		text := sb.String()
		d.Printf("%s", text)
		return text
	}
	for _, r := range rows {
		old, seen := last[r.key]
		switch {
		case !seen:
			fmt.Fprintf(&sb, "%s\n", c.green("+ "+r.text))
		case old == r.text:
			fmt.Fprintf(&sb, "%s\n", c.dim(r.text))
		default:
			fmt.Fprintf(&sb, "%s\n", c.yellow("~ "+r.text))
		}
	}
	// Removals print sorted, so the same state shows the same screen twice.
	var gone []string
	for k, old := range last {
		if _, still := cur[k]; !still {
			gone = append(gone, c.red("- removed "+old))
		}
	}
	slices.Sort(gone)
	for _, g := range gone {
		fmt.Fprintf(&sb, "%s\n", g)
	}

	text := sb.String()
	d.Printf("%s", text)
	return text
}

// snapshotViews records every diffed view's current rows as the baseline the
// coming beat is diffed against, printing nothing. The dump view prints its
// whole walk and keeps no baseline, so it is not recorded.
func (d *Demo) snapshotViews() {
	d.recording = true
	defer func() { d.recording = false }()
	d.viewFS()
	d.viewStats()
	d.viewIndex()
	d.viewSegments()
}
