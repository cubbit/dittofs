//go:build engine_conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// §9.1 clone, the rows a clone's reservation, journal step, copies and phase
// 1 answer for. A clone keeps the destination's write outside its extent,
// staged before it, and one staged while it runs lands without a stall; it
// never waits, no remote being composed, answering within a bound no clock
// moves, with the destination reading the source's journal-held bytes; and a
// clone whose reservation the destination's journal refuses fails before its
// journal step, the destination reading its prior content and both extents
// writable again, their registrations undone.
func TestRFC8A_Clone(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	const n = 64 << 10
	_, src := f.File("src")
	_, dst := f.File("dst")
	s := bytes.Repeat([]byte{'s'}, n)
	write(t, f, src, 0, s)
	write(t, f, dst, 1<<20, []byte("outside"))
	// The test clock never moves, so a clone that waited on it, or on
	// anything else, would answer the bound's deadline.
	bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cloned := make(chan error, 1)
	go func() { cloned <- f.E.Clone(bctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: n}) }()
	if _, err := f.E.Write(bctx, f.Share.ID, dst, 2<<20, []byte("during"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write outside the extent while the clone runs: %v, want it to land without a stall", err)
	}
	if err := <-cloned; err != nil {
		t.Fatalf("Clone: %v, want an answer without a wait", err)
	}
	if got := read(t, f, dst, 2<<20, 6); string(got) != "during" {
		t.Errorf("the write outside the extent staged during the clone reads %q", got)
	}
	if got := read(t, f, dst, 0, n); !bytes.Equal(got, s) {
		t.Error("the destination extent does not read the source")
	}
	if got := read(t, f, dst, 1<<20, 7); string(got) != "outside" {
		t.Errorf("the write outside the extent reads %q after the clone", got)
	}

	// The second share's journal limit is below what the copies need.
	second := f.Second
	second.Limit = 16 << 10
	if err := f.E.Bind(ctx, second); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	full := secondFile(t, f, "full")
	prior := []byte("prior content")
	if _, err := f.E.Write(ctx, second.ID, full, 0, prior, engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := f.E.Clone(ctx, f.Share.ID, src, second.ID, full, vfserr.Extent{Len: n})
	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayCapacity {
		t.Fatalf("a clone its destination's journal cannot reserve: %v, want ErrDelay(DelayCapacity)", err)
	}
	got := make([]byte, len(prior))
	if _, err := f.E.Read(ctx, second.ID, full, 0, got); err != nil || !bytes.Equal(got, prior) {
		t.Errorf("the refused clone's destination reads %q, %v; want its prior content", got, err)
	}
	if _, err := f.E.Write(bctx, second.ID, full, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the refused clone's destination extent: %v, want its registration undone", err)
	}
	if _, err := f.E.Write(bctx, f.Share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the refused clone's source extent: %v, want its registration undone", err)
	}
}

// §9.1 removals of a cloning destination, its last assertion: once the
// clone is answered, its last batch has deleted the source's clone-kind
// open, and Release has closed open state's hold, reporting nothing of a
// source still linked.
func TestRFC8A_RemovalsOfACloningDestination(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	const n = 64 << 10
	_, src := f.File("src")
	_, dst := f.File("dst")
	write(t, f, src, 0, bytes.Repeat([]byte{'s'}, n))
	if err := f.E.Clone(ctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: n}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	opens, err := f.FS.Store.Opens().Of(ctx, f.Share.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opens {
		if o.Kind == entity.OpenCloneSource {
			t.Errorf("the source's clone-kind open %+v outlived the clone", o)
		}
	}
	for _, c := range f.Sink.ClosedCalls() {
		if c.File == src && c.Result.Orphan {
			t.Errorf("the linked source was reported an orphan: %+v", c)
		}
	}
}
