package vfstest_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// Each Service method, called once in Op order, is recorded as its own op
// exactly when that op may change state.
func TestMutationsRecordsEveryMethodAsItsOp(t *testing.T) {
	rec := vfstest.RecordMutations(vfstest.Null{})
	v := reflect.ValueOf(rec)
	var want []vfs.Op
	for op := vfs.Op(1); op.String() != "Op(?)"; op++ {
		m := v.MethodByName(op.String())
		if !m.IsValid() {
			t.Fatalf("Mutations has no method %s", op)
		}
		args := make([]reflect.Value, m.Type().NumIn())
		for i := range args {
			args[i] = reflect.Zero(m.Type().In(i))
		}
		args[0] = reflect.ValueOf(context.Background())
		if m.Type().IsVariadic() {
			m.CallSlice(args)
		} else {
			m.Call(args)
		}
		if vfs.Mutates(op) {
			want = append(want, op)
		}
	}
	if got := rec.Ops(); !slices.Equal(got, want) {
		t.Errorf("Ops = %v, want %v", got, want)
	}
}

// The wrapper records each call that may change state, a refused one
// included, and none that cannot; every answer, and every change, is the
// filesystem's it passes the call to.
func TestMutationsRecordsEachMutatingCallItPasses(t *testing.T) {
	s := memory.New(t)
	ctx, admin := t.Context(), s.Admin()
	h, _ := s.Store.Create(t, s.Store.Admin, s.Store.Root, "f", kvstore.Regular(0o644))
	if w, err := s.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("hello"), vfs.Unstable); err != nil || w.N != 5 {
		t.Fatalf("Write: %d bytes, %v; want 5", w.N, err)
	}
	buf := make([]byte, 8)
	if n, _, err := s.Read(ctx, admin, vfs.Anonymous(h), 0, buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read: %q, %v; want %q", buf[:n], err, "hello")
	}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(2)}); err != nil {
		t.Fatalf("SetAttr of size 2: %v", err)
	}
	if f, err := s.GetAttr(ctx, admin, h); err != nil || f.Size != 2 {
		t.Fatalf("GetAttr: size %d, %v; want 2", f.Size, err)
	}
	if _, err := s.Unlink(ctx, admin, vfs.Target{H: s.Store.Root}, []byte("missing")); !errors.Is(err, vfs.ErrNotFound) {
		t.Fatalf("Unlink of a name never made: %v, want ErrNotFound", err)
	}
	want := []vfs.Op{vfs.OpWrite, vfs.OpSetAttr, vfs.OpUnlink}
	if got := s.Ops(); !slices.Equal(got, want) {
		t.Errorf("Ops = %v, want %v", got, want)
	}
	s.Reset()
	if got := s.Ops(); len(got) != 0 {
		t.Errorf("Ops after Reset = %v, want none", got)
	}
}
