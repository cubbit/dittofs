package jdemo

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
)

func held(off, n int64, v uint64) journal.IndexEntry {
	return journal.IndexEntry{Kind: journal.EntryHeld, Extent: journal.Extent{Off: off, Len: n}, Version: journal.Version{Counter: v}}
}

func TestRulerPutsARetainedPieceUnderTheHeldOneThatSupersededIt(t *testing.T) {
	es := []journal.IndexEntry{
		held(0, 2, 1), held(2, 2, 2), held(4, 7, 1),
		{Kind: journal.EntryRetained, Extent: journal.Extent{Off: 2, Len: 2}, Version: journal.Version{Counter: 1}, By: journal.Version{Counter: 2}},
	}
	lines := indexRuler(es)
	var heldLine, retainedLine string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "held"):
			heldLine = l
		case strings.HasPrefix(l, "retained"):
			retainedLine = l
		}
	}
	if !strings.Contains(retainedLine, "v1 by v2") {
		t.Errorf("retained lane does not name what superseded the piece: %q", retainedLine)
	}
	// The second held piece (v2) and the retained piece share a start column.
	if got, want := runeCol(retainedLine, 0), runeCol(heldLine, 1); got != want {
		t.Errorf("retained piece starts at column %d, the superseding held piece at %d:\n%s", got, want, strings.Join(lines, "\n"))
	}
}

func TestRulerMarksAHoleAndAnOpenEndedMarker(t *testing.T) {
	es := []journal.IndexEntry{
		held(0, 11, 1), held(4096, 100, 3),
		{Kind: journal.EntryMarker, Extent: journal.Extent{Off: 5000}, Version: journal.Version{Counter: 4}},
	}
	lines := strings.Join(indexRuler(es), "\n")
	if !strings.Contains(lines, "·") {
		t.Errorf("the hole between two held pieces is not marked:\n%s", lines)
	}
	if !strings.Contains(lines, "▶") || !strings.Contains(lines, "end") {
		t.Errorf("an open-ended removal marker is not drawn running to the end:\n%s", lines)
	}
}

func TestRulerGivesWayToRowsWhenTheFileHasTooManyPieces(t *testing.T) {
	var es []journal.IndexEntry
	for i := int64(0); i < 40; i++ {
		es = append(es, held(i*10, 5, uint64(i+1)))
	}
	if got := indexRuler(es); got != nil {
		t.Errorf("a file of 40 separate pieces drew a ruler of %d lines; the rows alone should carry it", len(got))
	}
}

func TestIndexRowsKeepTheThreeSetsApart(t *testing.T) {
	es := []journal.IndexEntry{
		held(0, 4, 2),
		{Kind: journal.EntryRetained, Extent: journal.Extent{Off: 0, Len: 4}, Version: journal.Version{Counter: 1}, By: journal.Version{Counter: 2}},
	}
	var text []string
	keys := map[string]bool{}
	for _, r := range indexRows(es) {
		text = append(text, r.text)
		if keys[r.key] {
			t.Errorf("two rows share the key %q: a held and a retained piece of the same extent must diff apart", r.key)
		}
		keys[r.key] = true
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{"held ", "retained ", "markers ", "superseded by v2", "1 piece, 4 B"} {
		if !strings.Contains(all, want) {
			t.Errorf("index rows lack %q:\n%s", want, all)
		}
	}
}

// runeCol is the column of the nth '├' in a line, counted in runes: the
// ruler is drawn in box characters that take more than one byte.
func runeCol(line string, nth int) int {
	for i, r := range []rune(line) {
		if r == '├' {
			if nth == 0 {
				return i
			}
			nth--
		}
	}
	return -1
}
