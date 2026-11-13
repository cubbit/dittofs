package core_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Errors lists every refusal core declares, once each: a sentinel added to
// core but not to Errors would reach a table no totality test checks it
// against. ErrParked is a handler's return, never a refusal a table maps.
func TestErrorsListsEverySentinelOnce(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sentinels int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if strings.HasPrefix(n.Name, "Err") && n.Name != "ErrParked" {
							sentinels++
						}
					}
				}
			}
		}
	}
	errs := core.Errors()
	if len(errs) != sentinels {
		t.Errorf("Errors lists %d errors; core declares %d refusals", len(errs), sentinels)
	}
	for i, e := range errs {
		if e == nil || e == core.ErrParked || slices.Contains(errs[:i], e) {
			t.Errorf("Errors()[%d] = %v: nil, ErrParked or listed twice", i, e)
		}
	}
}

const (
	fault core.Status = iota + 100
	delayed
	recalled
	both
	opOnly
	versionOnly
	neither
	quota
	garbage
	cancelled
)

var (
	read  = &core.Op{Code: 1, Name: "Read"}
	write = &core.Op{Code: 2, Name: "Write"}
)

func versions(first, last core.Version) *core.Range { return &core.Range{First: first, Last: last} }

// decodeRow is the row every table must carry: ErrDecode for every operation
// and version.
var decodeRow = core.ErrorRow{Err: core.ErrDecode, Status: garbage}

// table is NewErrorTable over rows and decodeRow.
func table(rows ...core.ErrorRow) *core.ErrorTable {
	return core.NewErrorTable(fault, append(rows, decodeRow)...)
}

// mapsTo checks one Map answer.
func mapsTo(t *testing.T, tb *core.ErrorTable, err error, op *core.Op, v core.Version, want core.Status) {
	t.Helper()
	if got, reply := tb.Map(err, op, v); got != want || !reply {
		t.Errorf("Map(%v, %v, %d) = %d, reply %v; want %d with a reply", err, op, v, got, reply, want)
	}
}

// A typed row wins over its sentinel's whatever the order the rows are
// declared in, matches a wrapped error as its cause, and only its own kind.
func TestMapTakesATypedRowBeforeItsSentinel(t *testing.T) {
	typed := core.ErrorRow{Err: &vfs.Delay{Cause: vfs.DelayRecall}, Status: recalled}
	sentinel := core.ErrorRow{Err: vfs.ErrDelay, Status: delayed}
	for name, rows := range map[string][]core.ErrorRow{
		"typed first":    {typed, sentinel},
		"sentinel first": {sentinel, typed},
	} {
		t.Run(name, func(t *testing.T) {
			tb := table(rows...)
			mapsTo(t, tb, &vfs.Delay{Cause: vfs.DelayRecall}, read, 1, recalled)
			mapsTo(t, tb, fmt.Errorf("open: %w", &vfs.Delay{Cause: vfs.DelayRecall}), read, 1, recalled)
			mapsTo(t, tb, &vfs.Delay{Cause: vfs.DelayClone}, read, 1, delayed)
			mapsTo(t, tb, vfs.ErrDelay, read, 1, delayed)
		})
	}
}

// Among rows of one error, the one naming operations and versions wins,
// then operations, then versions, then neither, declared least specific
// first; a request with no operation matches only rows naming none.
func TestMapTakesTheMostSpecificRow(t *testing.T) {
	tb := table(
		core.ErrorRow{Err: vfs.ErrNotFound, Status: neither},
		core.ErrorRow{Err: vfs.ErrNotFound, Versions: versions(2, 2), Status: versionOnly},
		core.ErrorRow{Err: vfs.ErrNotFound, Ops: []uint32{read.Code}, Status: opOnly},
		core.ErrorRow{Err: vfs.ErrNotFound, Ops: []uint32{read.Code}, Versions: versions(2, 2), Status: both},
	)
	mapsTo(t, tb, vfs.ErrNotFound, read, 2, both)
	mapsTo(t, tb, vfs.ErrNotFound, read, 1, opOnly)
	mapsTo(t, tb, vfs.ErrNotFound, write, 2, versionOnly)
	mapsTo(t, tb, vfs.ErrNotFound, write, 1, neither)
	mapsTo(t, tb, vfs.ErrNotFound, nil, 2, versionOnly)
	mapsTo(t, tb, vfs.ErrNotFound, nil, 1, neither)

	// With no row naming both, a row naming the operation wins over one
	// naming the version.
	tb = table(
		core.ErrorRow{Err: vfs.ErrNotFound, Versions: versions(2, 2), Status: versionOnly},
		core.ErrorRow{Err: vfs.ErrNotFound, Ops: []uint32{read.Code}, Status: opOnly},
	)
	mapsTo(t, tb, vfs.ErrNotFound, read, 2, opOnly)
}

// An error no row matches is answered the fault status, with a reply; a
// NoReply row answers none.
func TestMapAnswersTheFaultAndNoReply(t *testing.T) {
	tb := table(
		core.ErrorRow{Err: core.ErrDraining, NoReply: true},
		core.ErrorRow{Err: &vfs.QuotaError{Scope: vfs.QuotaUser}, Status: quota},
	)
	mapsTo(t, tb, vfs.ErrNotFound, read, 1, fault)
	mapsTo(t, tb, &vfs.QuotaError{Scope: vfs.QuotaShare}, read, 1, fault)
	if _, reply := tb.Map(fmt.Errorf("serve: %w", core.ErrDraining), read, 1); reply {
		t.Error("a NoReply row answered with a reply")
	}
}

// buildPanic is the panic NewErrorTable raised on rows and decodeRow, or nil.
func buildPanic(rows ...core.ErrorRow) (p any) {
	return bareBuildPanic(append(rows, decodeRow)...)
}

// bareBuildPanic is the panic NewErrorTable raised on rows alone, or nil.
func bareBuildPanic(rows ...core.ErrorRow) (p any) {
	defer func() { p = recover() }()
	core.NewErrorTable(fault, rows...)
	return nil
}

// Two rows of one rank that can both match one error, operation and version
// are refused at build, naming both; rows of one rank that cannot are not.
func TestNewErrorTableRefusesRowsThatOverlap(t *testing.T) {
	wrapped := fmt.Errorf("lookup: %w", vfs.ErrNotFound)
	for _, c := range []struct {
		name string
		rows []core.ErrorRow
		bad  bool
	}{
		{"one sentinel twice", []core.ErrorRow{{Err: vfs.ErrNotFound, Status: 1}, {Err: vfs.ErrNotFound, Status: 2}}, true},
		{"a sentinel and an error wrapping it", []core.ErrorRow{{Err: vfs.ErrNotFound, Status: 1}, {Err: wrapped, Status: 2}}, true},
		{"one kind twice", []core.ErrorRow{{Err: &vfs.Delay{Cause: vfs.DelayRecall}, Status: 1}, {Err: &vfs.Delay{Cause: vfs.DelayRecall}, Status: 2}}, true},
		{"operations that meet", []core.ErrorRow{{Err: vfs.ErrNotFound, Ops: []uint32{1, 2}, Status: 1}, {Err: vfs.ErrNotFound, Ops: []uint32{2, 3}, Status: 2}}, true},
		{"versions that meet", []core.ErrorRow{{Err: vfs.ErrNotFound, Versions: versions(1, 2), Status: 1}, {Err: vfs.ErrNotFound, Versions: versions(2, 3), Status: 2}}, true},
		{"two kinds of one class", []core.ErrorRow{{Err: &vfs.Delay{Cause: vfs.DelayRecall}, Status: 1}, {Err: &vfs.Delay{Cause: vfs.DelayClone}, Status: 2}}, false},
		{"disjoint operations", []core.ErrorRow{{Err: vfs.ErrNotFound, Ops: []uint32{1}, Status: 1}, {Err: vfs.ErrNotFound, Ops: []uint32{2}, Status: 2}}, false},
		{"disjoint versions", []core.ErrorRow{{Err: vfs.ErrNotFound, Versions: versions(1, 1), Status: 1}, {Err: vfs.ErrNotFound, Versions: versions(2, 3), Status: 2}}, false},
		{"two ranks", []core.ErrorRow{{Err: vfs.ErrNotFound, Status: 1}, {Err: vfs.ErrNotFound, Ops: []uint32{1}, Status: 2}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := buildPanic(c.rows...)
			if !c.bad {
				if p != nil {
					t.Fatalf("NewErrorTable refused rows that cannot overlap: %v", p)
				}
				return
			}
			msg := fmt.Sprint(p)
			if p == nil || !strings.Contains(msg, fmt.Sprint(c.rows[0].Err)) || !strings.Contains(msg, fmt.Sprint(c.rows[1].Err)) {
				t.Errorf("NewErrorTable over rows that overlap panicked with %q, want a panic naming both rows", msg)
			}
		})
	}
}

// Every table answers ErrDecode for every operation and version: a table with
// no row for it, or whose only row for it names operations or versions, holds,
// sends no reply or answers the fault status, is refused at build, naming it,
// since a header's decode error has no operation and must not pass for a
// fault; and a wrapped ErrDecode maps to its row.
func TestNewErrorTableDemandsAnErrDecodeRowForEveryOperation(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []core.ErrorRow
		bad  bool
	}{
		{"no row", []core.ErrorRow{{Err: vfs.ErrNotFound, Status: neither}}, true},
		{"a row naming an operation", []core.ErrorRow{{Err: core.ErrDecode, Ops: []uint32{read.Code}, Status: garbage}}, true},
		{"a row naming versions", []core.ErrorRow{{Err: core.ErrDecode, Versions: versions(1, 2), Status: garbage}}, true},
		{"a row that sends no reply", []core.ErrorRow{{Err: core.ErrDecode, NoReply: true}}, true},
		{"a row that holds", []core.ErrorRow{{Err: core.ErrDecode, Hold: func(error, time.Time) time.Time { return time.Time{} }, Status: garbage}}, true},
		{"a row answering the fault status", []core.ErrorRow{{Err: core.ErrDecode, Status: fault}}, true},
		{"a row naming neither", []core.ErrorRow{decodeRow}, false},
		{"one naming neither beside one naming an operation", []core.ErrorRow{decodeRow, {Err: core.ErrDecode, Ops: []uint32{read.Code}, Status: opOnly}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := bareBuildPanic(c.rows...)
			switch {
			case !c.bad && p != nil:
				t.Fatalf("NewErrorTable refused a table that maps ErrDecode everywhere: %v", p)
			case c.bad && (p == nil || !strings.Contains(fmt.Sprint(p), "ErrDecode")):
				t.Errorf("NewErrorTable panicked with %v, want a panic naming ErrDecode", p)
			}
		})
	}
	tb := table()
	mapsTo(t, tb, fmt.Errorf("header: %w", core.ErrDecode), nil, 7, garbage)
	mapsTo(t, tb, core.ErrDecode, write, 1, garbage)
}
