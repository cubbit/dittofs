package history

import (
	"cmp"
	"slices"
	"time"

	"github.com/cubbit/dittofs/test/harness/report"
)

// Change is one cell's or one test's time in two sets of runs. Test is ""
// for a cell.
type Change struct {
	Cell, Test string
	A, B       time.Duration
	InA, InB   bool
}

// Delta is how much longer the change took in B than in A; a side it isn't in
// counts as 0.
func (c Change) Delta() time.Duration { return c.B - c.A }

// Delta is what Diff found: the cells and the tests, each by how much longer
// they took in B, the largest increase first.
type Delta struct{ Cells, Tests []Change }

// Diff compares what a and b ran. A test is the one of its cell with its
// title, and several results of it in one cell add up. Tests holds those
// that took at least floor longer or shorter, and those in one side only that
// took at least floor there, at most top of them, the largest increase first;
// top <= 0 keeps them all. Cells holds every cell either side ran, which one
// skipped for needs its machine didn't meet didn't. A test is compared only
// in a cell both sides ran; a cell in one side only is one row of Cells, not a
// row for each of its tests, which would fill the list when an artifact was
// skipped.
func Diff(a, b []report.Run, floor time.Duration, top int) Delta {
	ca, ta := times(a)
	cb, tb := times(b)
	both := func(k key) bool {
		_, inA := ca[key{cell: k.cell}]
		_, inB := cb[key{cell: k.cell}]
		return inA && inB
	}
	d := Delta{Cells: changes(ca, cb, 0, func(key) bool { return true }), Tests: changes(ta, tb, floor, both)}
	if top > 0 && len(d.Tests) > top {
		d.Tests = d.Tests[:top]
	}
	return d
}

type key struct{ cell, test string }

// times are the time of each cell and of each test of each cell in runs.
func times(runs []report.Run) (cells, tests map[key]time.Duration) {
	cells, tests = map[key]time.Duration{}, map[key]time.Duration{}
	for _, r := range runs {
		for _, c := range r.Cells {
			if c.Result == report.Skipped {
				continue
			}
			cells[key{cell: c.Name}] += c.Took
			for _, t := range c.Tests {
				tests[key{c.Name, t.Title()}] += t.Duration
			}
		}
	}
	return cells, tests
}

// changes are the keys of a and b that moved by at least floor, sorted by Delta, largest first.
func changes(a, b map[key]time.Duration, floor time.Duration, keep func(key) bool) []Change {
	var out []Change
	add := func(k key) {
		ta, inA := a[k]
		tb, inB := b[k]
		c := Change{Cell: k.cell, Test: k.test, A: ta, B: tb, InA: inA, InB: inB}
		// A side the change isn't in counts 0, so a row in one side only is
		// held to the floor by the time it took there.
		if keep(k) && abs(c.Delta()) >= floor {
			out = append(out, c)
		}
	}
	for k := range a {
		add(k)
	}
	for k := range b {
		if _, in := a[k]; !in {
			add(k)
		}
	}
	slices.SortFunc(out, func(x, y Change) int {
		return cmp.Or(cmp.Compare(y.Delta(), x.Delta()), cmp.Compare(x.Cell, y.Cell), cmp.Compare(x.Test, y.Test))
	})
	return out
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
