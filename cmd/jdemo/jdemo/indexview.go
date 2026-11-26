package jdemo

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/pkg/journal"
)

// The index view draws a file's placement index the way the journal keeps
// it: three separate sets — held pieces (a B-tree on start offset, disjoint,
// each one slice of one record), retained pieces (superseded, never served)
// and unsettled removal markers — over a ruler of the file's byte offsets.

const (
	rulerMaxWidth = 118 // columns the ruler may take before the view falls back to rows alone
	rulerMinCell  = 7
	rulerMaxCell  = 12
	lanePrefix    = 10
)

// indexRuler draws entries on a byte ruler, one lane per kind present. The
// ruler is ordinal, not proportional: every stretch between two boundaries
// gets one equal cell, so a 300 MB write and an 11-byte one are both
// readable, and a hole between pieces is one cell marked as a hole. It
// returns nil when the pieces are too many to fit a screen.
func indexRuler(es []journal.IndexEntry) []string {
	var held, retained, markers []journal.IndexEntry
	for _, e := range es {
		switch e.Kind {
		case journal.EntryHeld:
			held = append(held, e)
		case journal.EntryRetained:
			retained = append(retained, e)
		case journal.EntryMarker:
			markers = append(markers, e)
		}
	}
	if len(es) == 0 {
		return nil
	}
	end := func(e journal.IndexEntry) int64 {
		if e.Kind == journal.EntryMarker {
			return math.MaxInt64
		}
		return e.Extent.Off + e.Extent.Len
	}
	bset := map[int64]bool{0: true}
	for _, e := range es {
		bset[e.Extent.Off] = true
		bset[end(e)] = true
	}
	bounds := make([]int64, 0, len(bset))
	for b := range bset {
		bounds = append(bounds, b)
	}
	slices.Sort(bounds)
	cells := len(bounds) - 1
	if cells < 1 {
		return nil
	}
	w := min(rulerMaxCell, rulerMaxWidth/cells)
	if w < rulerMinCell {
		return nil
	}
	at := make(map[int64]int, len(bounds))
	for i, b := range bounds {
		at[b] = i
	}
	width := cells*w + rulerMaxCell

	offsets := []rune(strings.Repeat(" ", width))
	for i, b := range bounds {
		label := "end"
		if b != math.MaxInt64 {
			label = boundaryLabel(b)
		}
		if i < len(bounds)-1 && len([]rune(label)) > w-1 {
			label = string([]rune(label)[:w-1])
		}
		copy(offsets[i*w:], []rune(label))
	}

	lane := func(name string, pieces []journal.IndexEntry, text func(journal.IndexEntry) string, holes bool) string {
		g := []rune(strings.Repeat(" ", width))
		lo, hi := cells, 0
		for _, e := range pieces {
			s, t := at[e.Extent.Off], at[end(e)]
			lo, hi = min(lo, s), max(hi, t)
			span := (t-s)*w - 1
			g[s*w] = '├'
			for x := s*w + 1; x < t*w; x++ {
				g[x] = '─'
			}
			if e.Kind == journal.EntryMarker {
				g[t*w-1] = '▶'
			} else {
				g[t*w-1] = '┤'
			}
			if label := []rune(text(e)); len(label) <= span-1 {
				copy(g[s*w+1+(span-1-len(label))/2:], label)
			}
		}
		if holes {
			covered := make([]bool, cells)
			for _, e := range pieces {
				for c := at[e.Extent.Off]; c < at[end(e)]; c++ {
					covered[c] = true
				}
			}
			for c := lo; c < hi; c++ {
				if !covered[c] {
					g[c*w+w/2] = '·'
				}
			}
		}
		return fmt.Sprintf("%-*s%s", lanePrefix, name, strings.TrimRight(string(g), " "))
	}

	lines := []string{fmt.Sprintf("%-*s%s", lanePrefix, "offset", strings.TrimRight(string(offsets), " "))}
	if len(held) > 0 {
		lines = append(lines, lane("held", held, func(e journal.IndexEntry) string { return fmt.Sprintf("v%d", e.Version.Counter) }, true))
	}
	if len(retained) > 0 {
		lines = append(lines, lane("retained", retained, func(e journal.IndexEntry) string {
			return fmt.Sprintf("v%d by v%d", e.Version.Counter, e.By.Counter)
		}, false))
	}
	if len(markers) > 0 {
		lines = append(lines, lane("marker", markers, func(e journal.IndexEntry) string { return fmt.Sprintf("v%d", e.Version.Counter) }, false))
	}
	return lines
}

// boundaryLabel names a byte offset at the width a ruler column carries.
func boundaryLabel(n int64) string {
	if n < 1<<10 {
		return fmt.Sprint(n)
	}
	return HumanSize(n)
}

// indexRows lays the entries out as the index keeps them: three sets, each
// under a heading that names its structure and totals. Every row is
// diffable; a heading changes when its set's count or bytes do.
func indexRows(es []journal.IndexEntry) []viewRow {
	var held, retained, markers []journal.IndexEntry
	for _, e := range es {
		switch e.Kind {
		case journal.EntryHeld:
			held = append(held, e)
		case journal.EntryRetained:
			retained = append(retained, e)
		case journal.EntryMarker:
			markers = append(markers, e)
		}
	}
	rows := []viewRow{
		{key: "set:held", text: fmt.Sprintf("held       a B-tree on start offset, disjoint pieces of one record each · %s", setTotal(held))},
	}
	for _, e := range held {
		rows = append(rows, entryRow("h", e, fmt.Sprintf("  %s  v%-3d record %d@%d%s",
			extentText(e), e.Version.Counter, e.Location.Segment, e.Location.Offset, offloadedText(e))))
	}
	rows = append(rows, viewRow{key: "set:retained", text: fmt.Sprintf("retained   superseded, kept until the new write is synced, never served · %s", setTotal(retained))})
	for _, e := range retained {
		rows = append(rows, entryRow("r", e, fmt.Sprintf("  %s  v%-3d record %d@%d  superseded by v%d%s",
			extentText(e), e.Version.Counter, e.Location.Segment, e.Location.Offset, e.By.Counter, offloadedText(e))))
	}
	head := "markers    unsettled removals, in version order · none"
	if len(markers) > 0 {
		head = fmt.Sprintf("markers    unsettled removals, in version order · %d", len(markers))
	}
	rows = append(rows, viewRow{key: "set:markers", text: head})
	for _, e := range markers {
		rows = append(rows, entryRow("m", e, fmt.Sprintf("  %s  v%-3d removed from %d to the end of the file", extentText(e), e.Version.Counter, e.Extent.Off)))
	}
	return rows
}

func setTotal(es []journal.IndexEntry) string {
	var n int64
	for _, e := range es {
		n += e.Extent.Len
	}
	switch len(es) {
	case 0:
		return "none"
	case 1:
		return fmt.Sprintf("1 piece, %s", HumanSize(n))
	}
	return fmt.Sprintf("%d pieces, %s", len(es), HumanSize(n))
}

func extentText(e journal.IndexEntry) string {
	if e.Kind == journal.EntryMarker {
		return fmt.Sprintf("%d ─ end", e.Extent.Off)
	}
	return fmt.Sprintf("%d ─ %d", e.Extent.Off, e.Extent.Off+e.Extent.Len)
}

func offloadedText(e journal.IndexEntry) string {
	if e.Offloaded {
		return "  offloaded"
	}
	return ""
}

func entryRow(set string, e journal.IndexEntry, text string) viewRow {
	return viewRow{key: fmt.Sprintf("%s:%d-%d@v%d", set, e.Extent.Off, e.Extent.Off+e.Extent.Len, e.Version.Counter), text: text}
}
