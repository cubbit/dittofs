package durability

import (
	"context"
	"slices"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// TestFailedForAnswersBeforeAnyDurabilityIsBuilt pins the registry read the
// open wires: the failed list a read-only window left on the directory
// answers through FailedFor, on the directory alone before any Durability
// is built, and a directory the process has not touched answers none.
func TestFailedForAnswersBeforeAnyDurabilityIsBuilt(t *testing.T) {
	prof.Watch(t, prof.Options{})
	dir := t.TempDir()
	r, _ := newRORig(t, 1, dir)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	seqW := core.Seq(r.lastSeq.Load())
	_, seqH := r.appendHeader(t, f, format.KindTruncate, format.RecordBody{})

	r.d.Enter(syscall.EROFS)
	_ = mustReturn(t, "Sync", r.syncAsync(context.Background(), f))

	got, err := FailedFor(dir)
	if err != nil {
		t.Fatalf("FailedFor: %v", err)
	}
	for _, seq := range []core.Seq{seqW, seqH} {
		if !slices.Contains(got, seq) {
			t.Errorf("FailedFor = %v, missing seq %d", got, seq)
		}
	}
	if other, err := FailedFor(t.TempDir()); err != nil || len(other) != 0 {
		t.Errorf("FailedFor on an untouched directory = %v, %v, want none and no error", other, err)
	}
}
