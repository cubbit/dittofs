package vfstest_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// backstop bounds every wait on an event, so a test that would hang fails.
const backstop = 10 * time.Second

var errFault = errors.New("armed fault")

// ops is every Service method's Op.
func ops() []vfs.Op {
	var all []vfs.Op
	for op := vfs.Op(1); op.String() != "Op(?)"; op++ {
		all = append(all, op)
	}
	return all
}

// call calls s's method of op with zero arguments, but one reference in each
// list, and answers its errors, nil included.
func call(ctx context.Context, s vfs.Service, op vfs.Op) []error {
	m := reflect.ValueOf(s).MethodByName(op.String())
	args := []reflect.Value{reflect.ValueOf(ctx)}
	for j := 1; j < m.Type().NumIn(); j++ {
		in := m.Type().In(j)
		a := reflect.Zero(in)
		if in.Kind() == reflect.Slice && in.Elem().Kind() == reflect.Struct {
			a = reflect.MakeSlice(in, 1, 1)
		}
		args = append(args, a)
	}
	var errs []error
	for _, r := range m.Call(args) {
		switch e := r.Interface().(type) {
		case []error:
			errs = append(errs, e...)
		default:
			if r.Type() == reflect.TypeFor[error]() {
				err, _ := e.(error)
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// failed reports whether a call answered errFault.
func failed(errs []error) bool {
	for _, err := range errs {
		if errors.Is(err, errFault) {
			return true
		}
	}
	return false
}

// reached reports whether h's call has arrived.
func reached(h *vfstest.Hold) bool {
	select {
	case <-h.Reached():
		return true
	default:
		return false
	}
}

// Every Service method of FaultService consults its own Op first and then
// returns inner's same method's results, its arguments passed in order and
// unchanged: read from the source, since a swapped argument changes nothing
// a test over a service answering zero values can see.
func TestFaultsWrapsEveryMethodAsWritten(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "faults.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string]*ast.FuncDecl{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv != nil && types.ExprString(fn.Recv.List[0].Type) == "*FaultService" {
			decls[fn.Name.Name] = fn
		}
	}
	it := reflect.TypeFor[vfs.Service]()
	for i := range it.NumMethod() {
		name := it.Method(i).Name
		fn := decls[name]
		if fn == nil {
			t.Errorf("%s: no method of *FaultService in faults.go", name)
			continue
		}
		var params []string
		for _, p := range fn.Type.Params.List {
			for _, n := range p.Names {
				params = append(params, n.Name)
			}
		}
		body := fn.Body.List
		var consult, pass string
		if s, ok := body[0].(*ast.IfStmt); ok && s.Init != nil {
			if a, ok := s.Init.(*ast.AssignStmt); ok && len(a.Rhs) == 1 {
				consult = types.ExprString(a.Rhs[0])
			}
		}
		if r, ok := body[len(body)-1].(*ast.ReturnStmt); ok && len(r.Results) == 1 {
			pass = types.ExprString(r.Results[0])
		}
		if want := "f.at(ctx, vfs.Op" + name + ")"; len(body) != 2 || consult != want {
			t.Errorf("%s first consults %q, want %q, then returns", name, consult, want)
		}
		if want := "f.inner." + name + "(" + strings.Join(params, ", ") + ")"; pass != want {
			t.Errorf("%s returns %q, want %q", name, pass, want)
		}
	}
}

// A fault armed on one method fires on that method's next call alone: every
// other method's call passes through to inner, the faulted call answers the
// fault without reaching inner, and the call after it passes through again.
func TestFailFiresOnItsMethodAlone(t *testing.T) {
	ctx := t.Context()
	for _, op := range ops() {
		inner := vfstest.Faults(vfstest.Null{})
		reach := inner.Hold(op) // released at once: it only shows whether inner was reached
		reach.Release()
		s := vfstest.Faults(inner)
		s.Fail(op, errFault)
		for _, other := range ops() {
			if other != op && failed(call(ctx, s, other)) {
				t.Errorf("%v answered the fault armed on %v", other, op)
			}
		}
		if !failed(call(ctx, s, op)) {
			t.Errorf("%v did not answer the fault armed on it", op)
		}
		if reached(reach) {
			t.Errorf("%v reached inner, though its fault answered it", op)
		}
		if failed(call(ctx, s, op)) {
			t.Errorf("%v answered its fault a second time", op)
		}
		if !reached(reach) {
			t.Errorf("%v did not reach inner once its fault had fired", op)
		}
	}
}

// Faults armed on one method fire one call each, the one armed first on the
// next call, and the call after them all passes through.
func TestFailsOnOneMethodFireInArmingOrder(t *testing.T) {
	errSecond := errors.New("second fault")
	s := vfstest.Faults(vfstest.Null{})
	s.Fail(vfs.OpWrite, errFault)
	s.Fail(vfs.OpWrite, errSecond)
	for i, want := range []error{errFault, errSecond, nil} {
		if _, got := s.Write(t.Context(), vfs.Call{}, vfs.OpenRef{}, 0, nil, vfs.Unstable); got != want {
			t.Errorf("Write %d: got %v, want %v", i+1, got, want)
		}
	}
}

// A fault on TestState answers its error for every reference, one each.
func TestFailOnTestStateAnswersEveryReference(t *testing.T) {
	s := vfstest.Faults(vfstest.Null{})
	s.Fail(vfs.OpTestState, errFault)
	errs := s.TestState(t.Context(), vfs.Call{}, 0, make([]vfs.StateRef, 2))
	if len(errs) != 2 {
		t.Fatalf("TestState of two references answered %d errors, want 2", len(errs))
	}
	for i, err := range errs {
		if err != errFault {
			t.Errorf("TestState reference %d: got %v, want %v", i, err, errFault)
		}
	}
}

// A held call waits until the test releases it and returns only after the
// release, while other calls pass meanwhile; it then passes through to inner.
func TestHoldKeepsTheCallUntilReleased(t *testing.T) {
	ctx := t.Context()
	inner := vfstest.Faults(vfstest.Null{})
	reach := inner.Hold(vfs.OpWrite)
	reach.Release()
	s := vfstest.Faults(inner)
	h := s.Hold(vfs.OpWrite)

	var released atomic.Bool
	early := make(chan bool, 1)
	go func() {
		_, _ = s.Write(ctx, vfs.Call{}, vfs.OpenRef{}, 0, []byte("x"), vfs.Unstable)
		early <- !released.Load()
	}()
	select {
	case <-h.Reached():
	case <-time.After(backstop):
		t.Fatal("the held Write never arrived")
	}
	other := make(chan error, 1)
	go func() {
		_, err := s.GetAttr(ctx, vfs.Call{}, nil)
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("GetAttr while a Write is held: %v", err)
		}
	case <-time.After(backstop):
		t.Fatal("GetAttr did not return while a Write was held")
	}
	select {
	case <-early:
		t.Fatal("the held Write returned before the release")
	default:
	}
	if reached(reach) {
		t.Fatal("the held Write reached inner before the release")
	}
	released.Store(true)
	h.Release()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("a second Release panicked: %v, want it to do nothing", r)
			}
		}()
		h.Release()
	}()
	select {
	case e := <-early:
		if e {
			t.Error("the held Write returned before the release")
		}
	case <-time.After(backstop):
		t.Fatal("the released Write never returned")
	}
	if !reached(reach) {
		t.Error("the released Write did not reach inner")
	}
}

// A held call whose context ends stops waiting and passes through to inner,
// with no release.
func TestHoldEndsWithTheCallsContext(t *testing.T) {
	inner := vfstest.Faults(vfstest.Null{})
	reach := inner.Hold(vfs.OpCreate)
	reach.Release()
	s := vfstest.Faults(inner)
	h := s.Hold(vfs.OpCreate)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_, _ = s.Create(ctx, vfs.Call{}, nil, []byte("f"), vfs.CreateArgs{})
		close(done)
	}()
	select {
	case <-h.Reached():
	case <-time.After(backstop):
		t.Fatal("the held Create never arrived")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(backstop):
		t.Fatal("the held Create outlived its context")
	}
	if !reached(reach) {
		t.Error("the Create whose context ended did not reach inner")
	}
}
