package core_test

import (
	"context"
	"errors"
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
	warned
	failedIO
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
	if got, reply, _ := tb.Map(err, op, v); got != want || !reply {
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
	if _, reply, _ := tb.Map(fmt.Errorf("serve: %w", core.ErrDraining), read, 1); reply {
		t.Error("a NoReply row answered with a reply")
	}
}

// errPair is a NotFailure row's error, its pair's own.
var errPair = errors.New("read: a warning of its own")

// typedPair is a NotFailure row's error that is a typed value: it matches
// itself alone.
type typedPair struct{}

func (typedPair) Error() string        { return "read: a typed warning" }
func (typedPair) Is(target error) bool { return target == typedPair{} }

// A NotFailure row's status comes back with notFailure true, for the
// operations it names alone; an error that also matches another row, joined
// or wrapped with the pair's, maps to that row, a sentinel's before a typed
// NotFailure row's, with notFailure false.
func TestMapTakesANotFailureRowOnlyWhereNoOtherMatches(t *testing.T) {
	tb := table(
		core.ErrorRow{Err: errPair, Ops: []uint32{read.Code}, NotFailure: true, Status: warned},
		core.ErrorRow{Err: typedPair{}, Ops: []uint32{read.Code}, NotFailure: true, Status: warned},
		core.ErrorRow{Err: vfs.ErrIO, Status: failedIO},
	)
	for _, c := range []struct {
		err        error
		op         *core.Op
		want       core.Status
		notFailure bool
	}{
		{errPair, read, warned, true},
		{typedPair{}, read, warned, true},
		{errPair, write, fault, false},
		{errors.Join(errPair, vfs.ErrIO), read, failedIO, false},
		{fmt.Errorf("%w: %w", typedPair{}, vfs.ErrIO), read, failedIO, false},
	} {
		if s, reply, notFailure := tb.Map(c.err, c.op, 1); s != c.want || !reply || notFailure != c.notFailure {
			t.Errorf("Map(%v, %s) = %d, reply %v, notFailure %v; want %d with a reply, notFailure %v", c.err, c.op.Name, s, reply, notFailure, c.want, c.notFailure)
		}
	}
}

// A NotFailure row that names no operation, answers success or the fault,
// sends no reply, holds, or is keyed on an error every operation shares is
// refused at build, naming the row: it would let the members after any
// failure run. A row of the pair's own error naming its operation builds.
func TestNewErrorTableRefusesANotFailureRowThatCouldHideAFailure(t *testing.T) {
	ok := core.ErrorRow{Err: errPair, Ops: []uint32{read.Code}, NotFailure: true, Status: warned}
	for _, c := range []struct {
		name string
		edit func(*core.ErrorRow)
	}{
		{"no operation", func(r *core.ErrorRow) { r.Ops = nil }},
		{"success", func(r *core.ErrorRow) { r.Status = 0 }},
		{"the fault", func(r *core.ErrorRow) { r.Status = fault }},
		{"no reply", func(r *core.ErrorRow) { r.NoReply = true }},
		{"a hold", func(r *core.ErrorRow) { r.Hold = func(error, time.Time) time.Time { return time.Time{} } }},
		{"a service sentinel", func(r *core.ErrorRow) { r.Err = vfs.ErrNotFound }},
		{"a service typed value", func(r *core.ErrorRow) { r.Err = &vfs.Delay{Cause: vfs.DelayRecall} }},
		{"core's refusal", func(r *core.ErrorRow) { r.Err = core.ErrDecode }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := ok
			c.edit(&r)
			if p := buildPanic(r); p == nil || !strings.Contains(fmt.Sprint(p), "NotFailure row {"+fmt.Sprint(r.Err)) {
				t.Errorf("NewErrorTable over a NotFailure row with %s panicked with %v, want a panic naming the row", c.name, p)
			}
		})
	}
	if p := buildPanic(ok); p != nil {
		t.Errorf("NewErrorTable refused a NotFailure row of its pair's own error: %v", p)
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

// A row that can never match is refused at build, naming the row: nil Ops
// and Versions name every operation and version, so an empty Ops matches no
// operation and a Versions of First above Last matches no version; each
// would leave its error to the fault status with no row saying why.
func TestNewErrorTableRefusesARowThatCanNeverMatch(t *testing.T) {
	for _, c := range []struct {
		name string
		row  core.ErrorRow
		want string
	}{
		{"an empty Ops", core.ErrorRow{Err: vfs.ErrNotFound, Ops: []uint32{}, Status: 1}, "names an empty Ops"},
		{"a Versions of First above Last", core.ErrorRow{Err: vfs.ErrNotFound, Versions: versions(2, 1), Status: 1}, "match no version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := buildPanic(c.row)
			msg := fmt.Sprint(p)
			if p == nil || !strings.Contains(msg, c.want) || !strings.Contains(msg, fmt.Sprint(vfs.ErrNotFound)) {
				t.Fatalf("NewErrorTable over a row that never matches returned %q, want a panic naming the row and %v", msg, vfs.ErrNotFound)
			}
		})
	}

	// A row naming operations and a range that holds a version still builds.
	if p := buildPanic(core.ErrorRow{Err: vfs.ErrNotFound, Ops: []uint32{1}, Versions: versions(1, 2), Status: 3}); p != nil {
		t.Fatalf("NewErrorTable refused a row that can match: %v", p)
	}
}

// protoDecode is a protocol's own decode error, matching ErrDecode as the
// service's typed values match their sentinels.
type protoDecode struct{}

func (protoDecode) Error() string        { return "proto: cannot decode" }
func (protoDecode) Is(target error) bool { return target == core.ErrDecode }

// Every table answers ErrDecode for every operation and version: a table with
// no row for it naming neither operations nor versions is refused at build,
// and so is one with a row for it, whatever it names, that holds, sends no
// reply or answers the fault or success, the panic naming that row, since a
// header's decode error has no operation and must pass neither for a fault
// nor for a request served. A
// protocol's typed row for its own value is not such a row, and a wrapped
// ErrDecode maps to its row.
func TestNewErrorTableDemandsAnErrDecodeRowForEveryOperation(t *testing.T) {
	missing := "no row answers ErrDecode"
	bad := func(ops string) string {
		return fmt.Sprintf("row {%v%s} matches ErrDecode", core.ErrDecode, ops)
	}
	for _, c := range []struct {
		name string
		rows []core.ErrorRow
		want string // a substring of the panic; "" for none
	}{
		{"no row", []core.ErrorRow{{Err: vfs.ErrNotFound, Status: neither}}, missing},
		{"a row naming an operation", []core.ErrorRow{{Err: core.ErrDecode, Ops: []uint32{read.Code}, Status: garbage}}, missing},
		{"a row naming versions", []core.ErrorRow{{Err: core.ErrDecode, Versions: versions(1, 2), Status: garbage}}, missing},
		{"a row that sends no reply", []core.ErrorRow{{Err: core.ErrDecode, NoReply: true, Status: garbage}}, bad("")},
		{"a row that holds", []core.ErrorRow{{Err: core.ErrDecode, Hold: func(error, time.Time) time.Time { return time.Time{} }, Status: garbage}}, bad("")},
		{"a row answering the fault status", []core.ErrorRow{{Err: core.ErrDecode, Status: fault}}, bad("")},
		{"a row answering success", []core.ErrorRow{{Err: core.ErrDecode}}, bad("")},
		{"a row naming an operation that answers success, beside one naming neither", []core.ErrorRow{decodeRow, {Err: core.ErrDecode, Ops: []uint32{read.Code}}}, bad(" ops [1]")},
		{"a row naming an operation that answers the fault status, beside one naming neither", []core.ErrorRow{decodeRow, {Err: core.ErrDecode, Ops: []uint32{read.Code}, Status: fault}}, bad(" ops [1]")},
		{"a row naming neither", []core.ErrorRow{decodeRow}, ""},
		{"one naming neither beside one naming an operation", []core.ErrorRow{decodeRow, {Err: core.ErrDecode, Ops: []uint32{read.Code}, Status: opOnly}}, ""},
		{"a protocol's typed row that sends no reply, beside one naming neither", []core.ErrorRow{decodeRow, {Err: protoDecode{}, NoReply: true}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := bareBuildPanic(c.rows...)
			switch {
			case c.want == "" && p != nil:
				t.Fatalf("NewErrorTable refused a table that maps ErrDecode everywhere: %v", p)
			case c.want != "" && (p == nil || !strings.Contains(fmt.Sprint(p), c.want)):
				t.Errorf("NewErrorTable panicked with %v, want a panic with %q", p, c.want)
			}
		})
	}
	tb := table()
	mapsTo(t, tb, fmt.Errorf("header: %w", core.ErrDecode), nil, 7, garbage)
	mapsTo(t, tb, core.ErrDecode, write, 1, garbage)
}

// ErrCancelled wraps nothing: a row for context.Canceled, which a protocol
// may keep for its own deadline, never answers a cancel that ended a hold or
// a park, and ErrCancelled's own row is the one that does (§6.5).
func TestErrCancelledWrapsNothing(t *testing.T) {
	if errors.Is(core.ErrCancelled, context.Canceled) || errors.Unwrap(core.ErrCancelled) != nil {
		t.Errorf("ErrCancelled %q matches context.Canceled or wraps an error, want a plain sentinel", core.ErrCancelled)
	}
}
