package vfstest_test

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// Null answers within each call's contract: one answer per reference
// tested, an end to what a client pages through, and no short write.
func TestNullKeepsTheContract(t *testing.T) {
	ctx := context.Background()
	refs := make([]vfs.StateRef, 3)
	var svc vfs.Service = vfstest.Null{}
	if got := svc.TestState(ctx, vfs.Call{}, 1, refs); len(got) != len(refs) {
		t.Errorf("TestState answered %d, want %d", len(got), len(refs))
	}
	if p, _ := svc.ReadDir(ctx, vfs.Call{}, nil, 0, 10, 0); !p.EOF {
		t.Error("ReadDir answered no end")
	}
	if _, eof, _ := svc.Read(ctx, vfs.Call{}, vfs.Anonymous(nil), 0, nil); !eof {
		t.Error("Read answered no end")
	}
	if w, _ := svc.Write(ctx, vfs.Call{}, vfs.Anonymous(nil), 0, make([]byte, 3), vfs.Unstable); w.N != 3 {
		t.Errorf("Write of 3 bytes answered N %d", w.N)
	}
}
