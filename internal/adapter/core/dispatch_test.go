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

// protoCancel is a protocol's own cancel value, matching ErrCancelled as
// protoDecode matches ErrDecode.
type protoCancel struct{}

func (protoCancel) Error() string        { return "proto: cancelled" }
func (protoCancel) Is(target error) bool { return target == core.ErrCancelled }

// NewOpTable refuses a table with two rows under one code or two names that
// are one once lower-cased, a row whose versions run backwards, or a row of
// its error table that holds when the table does not or that holds and sends
// no reply, each naming the row; a table that holds takes a row that holds
// and replies.
// A table that holds also answers ErrCancelled for every operation and
// version: one with no row for it naming neither operations nor versions is
// refused, and so is one with a row for it, whatever it names, that holds,
// sends no reply or answers the fault or success, the panic naming that row;
// a protocol's typed row for its own cancel value is not such a row, and a
// table that does not hold needs none. A table that holds answers ErrDelay of
// DelayResources the same way, by a typed row: the holding ErrDelay row,
// which the value also matches, is not read.
func TestNewOpTableChecksItsRows(t *testing.T) {
	plain := table(core.ErrorRow{Err: vfs.ErrDelay, Status: delayed})
	hold := func(error, time.Time) time.Time { return time.Time{} }
	cancel := core.ErrorRow{Err: core.ErrCancelled, Status: cancelled}
	resources := &vfs.Delay{Cause: vfs.DelayResources}
	spent := core.ErrorRow{Err: resources, Status: delayed}
	delay := core.ErrorRow{Err: vfs.ErrDelay, Hold: hold, Status: delayed}
	holding := table(delay, cancel, spent)
	one := core.Range{First: 1, Last: 1}
	missing := "no row of its error table answers ErrCancelled"
	bad := func(ops string) string {
		return fmt.Sprintf("error row {%v%s} matches ErrCancelled", core.ErrCancelled, ops)
	}
	missingSpent := "no typed row of its error table answers ErrDelay of DelayResources"
	badSpent := func(ops string) string {
		return fmt.Sprintf("error row {%v%s} matches ErrDelay of DelayResources", resources, ops)
	}
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
		{"a hold where the table holds, an ErrDelay row beside the typed DelayResources row", holding, true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, ""},
		{"a table that holds whose row holds and sends no reply", table(delay, cancel, spent, core.ErrorRow{Err: core.ErrDraining, NoReply: true, Hold: hold}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, "holds and sends no reply"},
		{"a table that holds with no ErrCancelled row", table(delay, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, missing},
		{"a table that holds whose ErrCancelled row names an operation", table(core.ErrorRow{Err: core.ErrCancelled, Ops: []uint32{7}, Status: cancelled}, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, missing},
		{"a table that holds whose ErrCancelled row names versions", table(core.ErrorRow{Err: core.ErrCancelled, Versions: &one, Status: cancelled}, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, missing},
		{"a table that holds whose ErrCancelled row sends no reply", table(core.ErrorRow{Err: core.ErrCancelled, NoReply: true, Status: cancelled}, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, bad("")},
		{"a table that holds whose ErrCancelled row holds", table(core.ErrorRow{Err: core.ErrCancelled, Hold: hold, Status: cancelled}, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, bad("")},
		{"a table that holds whose ErrCancelled row answers success", table(core.ErrorRow{Err: core.ErrCancelled}, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, bad("")},
		{"a table that holds whose ErrCancelled row answers the fault status", table(core.ErrorRow{Err: core.ErrCancelled, Status: fault}, spent), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, bad("")},
		{"a table that holds whose ErrCancelled row naming an operation sends no reply, beside one naming neither", table(cancel, spent, core.ErrorRow{Err: core.ErrCancelled, Ops: []uint32{7}, NoReply: true}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, bad(" ops [7]")},
		{"a table that holds with a protocol's typed cancel row that sends no reply, beside one naming neither", table(cancel, spent, core.ErrorRow{Err: protoCancel{}, NoReply: true}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, ""},
		{"a table that holds with no typed DelayResources row", table(delay, cancel), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, missingSpent},
		{"a table that holds whose DelayResources row names an operation", table(delay, cancel, core.ErrorRow{Err: resources, Ops: []uint32{7}, Status: delayed}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, missingSpent},
		{"a table that holds whose DelayResources row names versions", table(delay, cancel, core.ErrorRow{Err: resources, Versions: &one, Status: delayed}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, missingSpent},
		{"a table that holds whose DelayResources row holds", table(delay, cancel, core.ErrorRow{Err: resources, Hold: hold, Status: delayed}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, badSpent("")},
		{"a table that holds whose DelayResources row sends no reply", table(delay, cancel, core.ErrorRow{Err: resources, NoReply: true, Status: delayed}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, badSpent("")},
		{"a table that holds whose DelayResources row answers success", table(delay, cancel, core.ErrorRow{Err: resources}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, badSpent("")},
		{"a table that holds whose DelayResources row answers the fault status", table(delay, cancel, core.ErrorRow{Err: resources, Status: fault}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, badSpent("")},
		{"a table that holds whose DelayResources row naming an operation holds, beside one naming neither", table(delay, cancel, spent, core.ErrorRow{Err: resources, Ops: []uint32{7}, Hold: hold, Status: delayed}), true, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, badSpent(" ops [7]")},
		{"a table that does not hold, with no ErrCancelled or DelayResources row", plain, false, []*core.Op{{Code: 7, Name: "Read", Versions: one}}, ""},
		{"a name in another case, lower-cased to a label value", plain, false, []*core.Op{{Code: 7, Name: "Create_Session", Versions: one}}, ""},
		{"a name with a space", plain, false, []*core.Op{{Code: 7, Name: "Read All", Versions: one}}, "is not lower-case letters, digits and underscores alone"},
		{"a name that is empty", plain, false, []*core.Op{{Code: 7, Name: "", Versions: one}}, "is not lower-case letters, digits and underscores alone"},
		{"two names one once lower-cased", plain, false, []*core.Op{{Code: 7, Name: "Read", Versions: one}, {Code: 9, Name: "READ", Versions: one}}, "Read and READ are one name once lower-cased"},
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
