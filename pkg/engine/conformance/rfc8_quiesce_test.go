//go:build engine_conformance

package conformance

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// TestRFC8_AQuiescedShareAdmitsNothingNew: once a Bind quiesces a served
// share, every content call that admits work answers ErrDelay with
// DelayFrozen, which the caller retries, and changes nothing. A Bind back to
// Serving admits writes again.
func TestRFC8_AQuiescedShareAdmitsNothingNew(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("f")
	_, dst := f.File("g")
	write(t, f, id, 0, []byte("abc"))

	bind(t, f, engine.Quiesced)

	ctx := t.Context()
	s := f.Share.ID
	x := vfserr.Extent{Len: 3}

	for _, c := range []struct {
		name string
		call func() error
	}{
		{"Write", func() error {
			_, err := f.E.Write(ctx, s, id, 3, []byte("XYZ"), engine.WriteOpts{Done: func() {}})
			return err
		}},
		{"Read", func() error { _, err := f.E.Read(ctx, s, id, 0, make([]byte, 3)); return err }},
		{"Draw", func() error { _, err := f.E.Draw(ctx, s, id); return err }},
		{"Commit", func() error { _, _, err := f.E.Commit(ctx, s, id, 0, 0); return err }},
		{"CommitExistence", func() error { return f.E.CommitExistence(ctx, s, id) }},
		{"Truncate", func() error { return f.E.Truncate(ctx, s, id, 1) }},
		{"Deallocate", func() error { return f.E.Deallocate(ctx, s, id, 0, 1) }},
		{"Delete", func() error { _, err := f.E.Delete(ctx, s, id); return err }},
		{"Overlay", func() error { _, _, err := f.E.Overlay(ctx, s, id); return err }},
		{"Allocation", func() error { _, err := f.E.Allocation(ctx, s, id, 0); return err }},
		{"Clone", func() error { return f.E.Clone(ctx, s, id, s, dst, x) }},
		{"Copy", func() error { _, err := f.E.Copy(ctx, s, id, s, dst, []vfserr.Extent{x}); return err }},
	} {
		var d *vfserr.Delay
		if err := c.call(); !errors.As(err, &d) || d.Cause != vfserr.DelayFrozen {
			t.Errorf("%s on a quiesced share = %v, want ErrDelay (DelayFrozen)", c.name, err)
		}
	}

	bind(t, f, engine.Serving)

	// The file is "abc" unless the refused write landed: a short read is fine.
	p := make([]byte, 6)
	if n, err := f.E.Read(ctx, s, id, 0, p); err != nil || bytes.Contains(p[:n], []byte("XYZ")) {
		t.Errorf("read after the resume = %q, %v, want the refused write's %q absent", p[:n], err, "XYZ")
	}

	write(t, f, id, 3, []byte("def"))

	if got := read(t, f, id, 0, 6); !bytes.Equal(got, []byte("abcdef")) {
		t.Errorf("read after the resume = %q, want %q", got, "abcdef")
	}
}

// TestRFC8_AQuiescedShareKeepsItsLossSeq: LossSeq admits nothing, so a
// quiesced share answers a file's raised sequence, not the 0 of a share with
// no binding.
func TestRFC8_AQuiescedShareKeepsItsLossSeq(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	write(t, f, id, 0, []byte("in a failed window"))
	f.FailNextWindow()

	_, seq, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0)
	if err != nil || seq == 0 {
		t.Fatalf("Commit over a failed window: sequence %d, %v; want it raised", seq, err)
	}

	bind(t, f, engine.Quiesced)

	if got := f.E.LossSeq(f.Share.ID, id); got != seq {
		t.Errorf("LossSeq on a quiesced share = %d, want the file's %d", got, seq)
	}
}

// bind applies state to the fixture's served share through Bind.
func bind(t *testing.T, f *enginetest.Fixture, state engine.ShareState) {
	t.Helper()

	s := f.Share
	s.State = state

	if err := f.E.Bind(t.Context(), s); err != nil {
		t.Fatalf("Bind with state %v: %v", state, err)
	}
}
