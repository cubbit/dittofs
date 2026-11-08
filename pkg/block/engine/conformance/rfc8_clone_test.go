//go:build engine_conformance

package conformance

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// §9.1 clone, the rows a clone's reservation, journal step, copies and phase
// 1 answer for. A clone keeps the destination's write outside its extent,
// staged before it; it never waits, no remote being composed, and answers
// with the destination reading the source's journal-held bytes; and a clone
// whose reservation the destination's journal refuses fails before its
// journal step, the destination reading its prior content.
func TestRFC8A_Clone(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	const n = 64 << 10
	_, src := f.File("src")
	_, dst := f.File("dst")
	s := bytes.Repeat([]byte{'s'}, n)
	write(t, f, src, 0, s)
	write(t, f, dst, 1<<20, []byte("outside"))
	if err := f.E.Clone(ctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: n}); err != nil {
		t.Fatalf("Clone: %v", err)
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
}
