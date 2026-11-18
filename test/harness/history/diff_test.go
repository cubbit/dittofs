package history

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/test/harness/report"
)

func tst(name string, d time.Duration) report.Test {
	return report.Test{Name: name, Status: report.Passed, Duration: d}
}

func cell(name string, took time.Duration, tests ...report.Test) report.Cell {
	return report.Cell{Name: name, Result: report.Passed, Took: took, Tests: tests}
}

func run(cells ...report.Cell) []report.Run { return []report.Run{{Cells: cells}} }

func TestDiff(t *testing.T) {
	const floor = 100 * time.Millisecond
	s := time.Second
	tests := []struct {
		name string
		a, b []report.Run
		top  int
		// want are the test rows' key (cell + " " + test), A, B, InA, InB, in order.
		want []Change
	}{
		{
			name: "grew",
			a:    run(cell("unit", 0, tst("TestX", 1*s))),
			b:    run(cell("unit", 0, tst("TestX", 3*s))),
			want: []Change{{Cell: "unit", Test: "TestX", A: 1 * s, B: 3 * s, InA: true, InB: true}},
		},
		{
			name: "shrank sorts after grown",
			a:    run(cell("unit", 0, tst("Shrank", 3*s), tst("Grew", 1*s))),
			b:    run(cell("unit", 0, tst("Shrank", 1*s), tst("Grew", 2*s))),
			want: []Change{
				{Cell: "unit", Test: "Grew", A: 1 * s, B: 2 * s, InA: true, InB: true},
				{Cell: "unit", Test: "Shrank", A: 3 * s, B: 1 * s, InA: true, InB: true},
			},
		},
		{
			name: "new and gone",
			a:    run(cell("unit", 0, tst("Gone", 5*s))),
			b:    run(cell("unit", 0, tst("New", 5*s))),
			want: []Change{
				{Cell: "unit", Test: "New", B: 5 * s, InB: true},
				{Cell: "unit", Test: "Gone", A: 5 * s, InA: true},
			},
		},
		{
			name: "below floor",
			a:    run(cell("unit", 0, tst("Small", 40*time.Millisecond), tst("Gone", 50*time.Millisecond))),
			b:    run(cell("unit", 0, tst("Small", 90*time.Millisecond))),
		},
		{
			name: "two cells one name",
			a:    run(cell("unit/a", 0, tst("TestX", 1*s)), cell("unit/b", 0, tst("TestX", 5*s))),
			b:    run(cell("unit/a", 0, tst("TestX", 2*s)), cell("unit/b", 0, tst("TestX", 5*s))),
			want: []Change{{Cell: "unit/a", Test: "TestX", A: 1 * s, B: 2 * s, InA: true, InB: true}},
		},
		{
			name: "several results in one cell are summed",
			a:    run(cell("unit", 0, tst("TestY", 1*s), tst("TestY", 2*s))),
			b:    run(cell("unit", 0, tst("TestY", 4*s))),
			want: []Change{{Cell: "unit", Test: "TestY", A: 3 * s, B: 4 * s, InA: true, InB: true}},
		},
		{
			name: "top keeps the largest",
			a:    run(cell("unit", 0, tst("A", 1*s), tst("B", 1*s), tst("C", 1*s), tst("D", 1*s), tst("E", 1*s))),
			b:    run(cell("unit", 0, tst("A", 2*s), tst("B", 3*s), tst("C", 4*s), tst("D", 5*s), tst("E", 6*s))),
			top:  2,
			want: []Change{
				{Cell: "unit", Test: "E", A: 1 * s, B: 6 * s, InA: true, InB: true},
				{Cell: "unit", Test: "D", A: 1 * s, B: 5 * s, InA: true, InB: true},
			},
		},
		{
			name: "same run twice",
			a:    run(cell("unit", 10*s, tst("TestX", 1*s))),
			b:    run(cell("unit", 10*s, tst("TestX", 1*s))),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Diff(tc.a, tc.b, floor, tc.top).Tests
			if len(got) != len(tc.want) {
				t.Fatalf("test rows: got %d %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("test row %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDiffCells(t *testing.T) {
	s := time.Second
	skipped := report.Cell{Name: "needs", Result: report.Skipped, Took: 9 * s}
	d := Diff(
		run(cell("unit", 60*s), cell("gone", 5*s)),
		run(cell("unit", 90*s), cell("new", 7*s), skipped),
		100*time.Millisecond, 0,
	)
	want := []Change{
		{Cell: "unit", A: 60 * s, B: 90 * s, InA: true, InB: true},
		{Cell: "new", B: 7 * s, InB: true},
		{Cell: "gone", A: 5 * s, InA: true},
	}
	if len(d.Cells) != len(want) {
		t.Fatalf("cell rows: got %+v, want %+v", d.Cells, want)
	}
	for i := range want {
		if d.Cells[i] != want[i] {
			t.Errorf("cell row %d: got %+v, want %+v", i, d.Cells[i], want[i])
		}
	}
	if got := d.Cells[0].Delta(); got != 30*s {
		t.Errorf("unit cell Delta: got %v, want 30s", got)
	}
}
