package workqueue_test

import (
	"go/types"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/archtest"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/workqueue"
)

// RFC 25's watchdog loop and the set's test clock satisfy the interfaces the
// scheduler declares, so the node hands them over as they are. A method whose
// signature drifts from theirs fails the build.
var (
	_ workqueue.Loop  = (*health.Loop)(nil)
	_ workqueue.Clock = (*sim.TestClock)(nil)
)

// frozenExports is the scheduler's surface as RFC 8 §6.1.1 writes it. A change
// here is a change to every component that runs an instance.
var frozenExports = []string{
	"type Key struct{Tenant TenantID; Kind uint8; ID [16]byte}",
	"type Item struct{Key Key; NotBefore time.Time; Cost int64; Expired bool}",
	"type Outcome uint8",
	"const Done Outcome = 0",
	"const Retry Outcome = 1",
	"const Throttled Outcome = 2",
	"const StopTenant Outcome = 3",
	"type Result struct{Outcome Outcome; Err error}",
	"type Source interface{Rebuild(ctx context.Context, t TenantID, post func(Item)) error; " +
		"Run(ctx context.Context, batch []Item, done func(i int, r Result))}",
	"type Limits struct{Batch int; Concurrency int; Rate float64; Weight int; Total int; " +
		"RunTimeout time.Duration; Deadline time.Duration}",
	"type Tenant struct{Weight int; BackoffMin time.Duration; BackoffMax time.Duration; Paused bool}",
	"type Stats struct{Pending int; Backoff int; Running int; Overdue int; OldestPending time.Duration; " +
		"Dropped uint64; Expired uint64; LastDone time.Time}",
	"type TenantID [16]byte",
	"type SourceID int",
	"type Clock interface{After(d time.Duration) <-chan struct{}; Elapsed() time.Duration; Now() time.Time}",
	"type Loop interface{Idle(); Turn()}",
	"type Scheduler struct{}",
	"func New(clk Clock, loop Loop) *Scheduler",
	"func (*Scheduler).Register(src Source, l Limits) SourceID",
	"func (*Scheduler).Start()",
	"func (*Scheduler).SetTenant(ctx context.Context, src SourceID, t TenantID, c Tenant) error",
	"func (*Scheduler).Post(src SourceID, it Item) bool",
	"func (*Scheduler).CancelTenant(src SourceID, t TenantID)",
	"func (*Scheduler).WaitTenant(ctx context.Context, src SourceID, t TenantID) error",
	"func (*Scheduler).Stats(src SourceID) map[TenantID]Stats",
	"func (*Scheduler).Stop(ctx context.Context) error",
}

// TestExports compares the package's compiled surface with frozenExports, each
// declaration on one line: a function or method with its signature, a
// constant with its value, a struct with its exported fields only, since an
// opaque struct gains unexported ones as its behaviour lands.
func TestExports(t *testing.T) {
	pkg, err := archtest.ExportImporter(".").Import("github.com/cubbit/dittofs/pkg/workqueue")
	if err != nil {
		t.Fatalf("import the package's export data: %v", err)
	}

	got := exports(pkg)

	for _, g := range got {
		if !slices.Contains(frozenExports, g) {
			t.Errorf("extra export: %s", g)
		}
	}

	for _, w := range frozenExports {
		if !slices.Contains(got, w) {
			t.Errorf("missing export: %s", w)
		}
	}
}

func exports(pkg *types.Package) []string {
	qualify := types.RelativeTo(pkg)

	var got []string

	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}

		switch obj := obj.(type) {
		case *types.Const:
			got = append(got, types.ObjectString(obj, qualify)+" = "+obj.Val().ExactString())
		case *types.TypeName:
			got = append(got, "type "+name+" "+typeString(obj.Type().Underlying(), qualify))

			if _, isIface := obj.Type().Underlying().(*types.Interface); !isIface {
				for sel := range types.NewMethodSet(types.NewPointer(obj.Type())).Methods() {
					if sel.Obj().Exported() {
						got = append(got, types.ObjectString(sel.Obj(), qualify))
					}
				}
			}
		default:
			got = append(got, types.ObjectString(obj, qualify))
		}
	}

	return got
}

// typeString prints t as types.TypeString does, but a struct with its exported
// fields only.
func typeString(t types.Type, qualify types.Qualifier) string {
	st, ok := t.(*types.Struct)
	if !ok {
		return types.TypeString(t, qualify)
	}

	var fields []string

	for f := range st.Fields() {
		if f.Exported() || f.Embedded() {
			fields = append(fields, f.Name()+" "+typeString(f.Type(), qualify))
		}
	}

	return "struct{" + strings.Join(fields, "; ") + "}"
}
