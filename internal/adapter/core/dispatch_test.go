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
func TestNewOpTableChecksItsRows(t *testing.T) {
	plain := core.NewErrorTable(fault, core.ErrorRow{Err: vfs.ErrDelay, Status: delayed})
	hold := func(error, time.Time) time.Time { return time.Time{} }
	holding := core.NewErrorTable(fault, core.ErrorRow{Err: vfs.ErrDelay, Hold: hold, Status: delayed})
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
