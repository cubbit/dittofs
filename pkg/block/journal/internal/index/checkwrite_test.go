package index

import (
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestIndexCheckWrite checks the exported non-reserving pre-check against the
// index's own admission: a write the bound would refuse is refused with the
// same NoSpaceError naming the index — the shortfall in entries — the refusal
// counted by the index's existing refusals counter, exactly once each; a
// write the bound would admit passes and reserves nothing, so the count a
// later admission stands on is the one the check left.
func TestIndexCheckWrite(t *testing.T) {
	// A bound of two entries, one write already committed: the count stands
	// at the limit less one, the room exactly one more write has.
	ix := New(2)
	id := fid(1)
	if _, err := ix.fs.admitWrite(id, ext(0, 4096), false); err != nil {
		t.Fatalf("the first write at a limit of 2: %v", err)
	}
	if c := ix.fs.extentCount(); c != 1 {
		t.Fatalf("after the committed write, the count = %d, want 1", c)
	}

	// A write on a new file adds an entry the bound still has room for:
	// the check passes, and reserves nothing — the count is unmoved, so the
	// admission the caller runs later finds all the room it was told.
	if err := ix.CheckWrite(fid(2), ext(0, 4096)); err != nil {
		t.Fatalf("CheckWrite one below the bound = %v, want nil", err)
	}
	if c := ix.fs.extentCount(); c != 1 {
		t.Fatalf("after a passed CheckWrite, the count = %d, want 1: the check reserves nothing", c)
	}
	if r := ix.fs.indexRefusals(); r != 0 {
		t.Fatalf("after a passed CheckWrite, refusals = %d, want 0", r)
	}

	// A whole-piece overwrite adds nothing, so it passes even at the bound:
	// the check refuses only what the bound would.
	if err := ix.CheckWrite(id, ext(0, 4096)); err != nil {
		t.Fatalf("a whole-piece overwrite = %v, want nil", err)
	}
	if c := ix.fs.extentCount(); c != 1 {
		t.Fatalf("after the overwrite's CheckWrite, the count = %d, want 1", c)
	}

	// The room the check said was there admits: the caller commits its
	// write, and the count stands at the limit.
	if a, err := ix.fs.admitWrite(fid(2), ext(0, 4096), false); err != nil || a != 1 {
		t.Fatalf("the write the check passed = (%d, %v), want (1, nil)", a, err)
	}

	// A third write adds an entry the bound has no room for: refused naming
	// the index, one entry missing, in entries — the same refusal the
	// admission itself returns — and the refusal is counted, once per check.
	err := ix.CheckWrite(fid(3), ext(0, 4070))
	assertIndexRefusal(t, err)
	var ns *core.NoSpaceError
	if !errors.As(err, &ns) {
		t.Fatalf("CheckWrite refused with %T, want *core.NoSpaceError", err)
	}
	if ns.Limit != core.LimitIndex || ns.Unit != core.UnitEntries || ns.Missing != 1 {
		t.Fatalf("CheckWrite's refusal = %+v, want the index, 1 missing, in entries", ns)
	}
	if r := ix.fs.indexRefusals(); r != 1 {
		t.Fatalf("after one refused CheckWrite, refusals = %d, want 1", r)
	}
	if err := ix.CheckWrite(fid(3), ext(0, 4096)); err == nil {
		t.Fatal("CheckWrite at the bound was not refused")
	}
	if r := ix.fs.indexRefusals(); r != 2 {
		t.Fatalf("after the same refused check twice, refusals = %d, want 2", r)
	}

	// The refused check reserved nothing: the count stands at the limit the
	// commit left it at, so a later write that adds nothing is still
	// admitted by the whole-piece rule the check forwards.
	if c := ix.fs.extentCount(); c != 2 {
		t.Fatalf("after a refused CheckWrite, the count = %d, want the 2 the commits stand at", c)
	}
}
