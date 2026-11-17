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

// renderView shows a view's rows, highlighted against the last time this
// view was shown: additions green-marked "+", changes yellow-marked "~"
// carrying the new text, removals red "- removed" naming what went. The
// first look at a view — and the first look after a mutating beat, which
// drops every snapshot — prints plain and says so, because diffing against
// nothing would mark every row new.
//
// The glyphs are text, not colour: a piped transcript keeps the "+ two"
// that said "this appeared" even where it keeps no escape bytes. Colours
// come from the writer's gate.
func (d *Demo) renderView(name string, rows []viewRow) string {
	c := coloursFor(d.out)
	if d.lastView == nil {
		d.lastView = make(map[string]map[string]string)
	}
	last := d.lastView[name]
	first := last == nil

	var sb strings.Builder
	cur := make(map[string]string, len(rows))
	if first {
		// The whole set is new information, not a diff.
		for _, r := range rows {
			cur[r.key] = r.text
			fmt.Fprintf(&sb, "%s\n", c.dim(r.text))
		}
		sb.WriteString("(first look)\n")
		d.lastView[name] = cur
		text := sb.String()
		d.Printf("%s", text)
		return text
	}
	for _, r := range rows {
		cur[r.key] = r.text
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
	d.lastView[name] = cur

	text := sb.String()
	d.Printf("%s", text)
	return text
}

// markStale is gone by design: the spec's diff is between one view's
// consecutive looks, snapshots survive mutating beats — that is when the
// diff is the point.
