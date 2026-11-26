package workqueue_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
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

// TestExports compares the package's surface with frozenExports, each
// declaration on one line: a function or method with its signature, a
// constant with its value, a struct with its exported fields only, since an
// opaque struct gains unexported ones as its behaviour lands. The package is
// type-checked from every non-test file, whatever its build tags, so a file
// built only for another platform cannot hide an export.
func TestExports(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()

	var files []*ast.File

	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}

		files = append(files, f)
	}

	conf := types.Config{Importer: archtest.ExportImporter(".")}

	pkg, err := conf.Check("github.com/cubbit/dittofs/pkg/workqueue", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check the package's files: %v", err)
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
			if obj.IsAlias() {
				// An alias declares no type of its own, so neither methods.
				got = append(got, "type "+name+" = "+types.TypeString(types.Unalias(obj.Type()), qualify))

				continue
			}

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
