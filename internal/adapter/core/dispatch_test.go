package core_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// opTablePanic is the panic NewOpTable raised, or nil.
func opTablePanic(errs *core.ErrorTable, holds bool, ops ...*core.Op) (p any) {
	defer func() { p = recover() }()
	core.NewOpTable(errs, holds, ops...)
	return nil
}

// NewOpTable refuses a table with two rows under one code, a row whose
// versions run backwards, or a row of its error table that holds when the
// table does not, each naming the row; a table that holds takes the row.
// A table that holds also answers ErrCancelled for every operation and
// version: one with no row for it, or whose only row for it names operations
// or versions, holds, sends no reply or answers the fault status, is refused,
// naming it; a table that does not hold needs none.
func TestNewOpTableChecksItsRows(t *testing.T) {
	plain := table(core.ErrorRow{Err: vfs.ErrDelay, Status: delayed})
	hold := func(error, time.Time) time.Time { return time.Time{} }
	cancel := core.ErrorRow{Err: core.ErrCancelled, Status: cancelled}
	holding := table(core.ErrorRow{Err: vfs.ErrDelay, Hold: hold, Status: delayed}, cancel)
	one := core.Range{First: 1, Last: 1}
	for _, c := range []struct {
		name  string
		errs  *core.ErrorTable
		holds bool
		ops   []*core.Op
		names string // what the panic names; empty: no panic
	}{
		{"one code twice", plain, false, []*core.Op{{Code: 7, Name: "Read", Versions: one}, {Code: 7, Name: "Write", Versions: one}}, "Write"},
		{"versions backwards", plain, false, []*core.Op{{Code: 7, Name: "Read", Versions: core.Range{First: 2, Last: 1}}}, "Read"},
		{"a hold where the table does not hold", holding, false, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, vfs.ErrDelay.Error()},
		{"a hold where the table holds", holding, true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, ""},
		{"a table that holds with no ErrCancelled row", table(core.ErrorRow{Err: vfs.ErrDelay, Hold: hold, Status: delayed}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "ErrCancelled"},
		{"a table that holds whose ErrCancelled row names an operation", table(core.ErrorRow{Err: core.ErrCancelled, Ops: []uint32{7}, Status: cancelled}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "ErrCancelled"},
		{"a table that holds whose ErrCancelled row names versions", table(core.ErrorRow{Err: core.ErrCancelled, Versions: &one, Status: cancelled}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "ErrCancelled"},
		{"a table that holds whose ErrCancelled row sends no reply", table(core.ErrorRow{Err: core.ErrCancelled, NoReply: true}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "ErrCancelled"},
		{"a table that holds whose ErrCancelled row holds", table(core.ErrorRow{Err: core.ErrCancelled, Hold: hold, Status: cancelled}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "ErrCancelled"},
		{"a table that holds whose ErrCancelled row answers the fault status", table(core.ErrorRow{Err: core.ErrCancelled, Status: fault}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "ErrCancelled"},
		{"a table that does not hold, with no ErrCancelled row", plain, false, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := opTablePanic(c.errs, c.holds, c.ops...)
			switch {
			case c.names == "" && p != nil:
				t.Fatalf("NewOpTable refused a valid table: %v", p)
			case c.names != "" && (p == nil || !strings.Contains(fmt.Sprint(p), c.names)):
				t.Errorf("NewOpTable panicked with %v, want a panic naming %s", p, c.names)
			}
		})
	}
	tb := core.NewOpTable(plain, false, &core.Op{Code: 7, Name: "Read", Versions: one}, &core.Op{Code: 9, Name: "Write", Versions: one})
	if tb.Ops[7].Name != "Read" || tb.Ops[9].Name != "Write" || tb.Errors != plain || tb.Holds {
		t.Errorf("the table built is %+v", tb)
	}
}
