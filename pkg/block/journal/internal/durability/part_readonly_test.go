package durability

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestPartReportsAFailedSyncFollowedByAGoodSpareAsWritable covers the two
// refusals rule at the report: a failed sync alone, then a spare whose first
// sync succeeds, leave the report writable.
func TestPartReportsAFailedSyncFollowedByAGoodSpareAsWritable(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigConfigured(t, 1, func(*Config) {})
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	r.leaveAFailedSegment(t, f, g)
	if got := r.d.Stats().ReadOnly; got.ReadOnly {
		t.Fatalf("Stats().ReadOnly after one failed sync = %+v, want writable", got)
	}
	if err := mustReturn(t, "Sync in the spare", r.syncAsync(context.Background(), g)); err != nil {
		t.Fatalf("Sync in the spare = %v, want nil", err)
	}
	if got := r.d.Stats().ReadOnly; got.ReadOnly || got.Err != nil {
		t.Fatalf("Stats().ReadOnly after a good first sync in the spare = %+v, want writable", got)
	}
}

// TestPartReportsTwoRefusalsAsReadOnlyNamingTheSecond covers the same rule's
// other branch: the failed sync, then a failed first sync in the spare.
func TestPartReportsTwoRefusalsAsReadOnlyNamingTheSecond(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRigConfigured(t, 1, func(*Config) {})
	r.d.Start(r.streams)
	f, g := file(1), file(2)
	_, spare := r.leaveAFailedSegment(t, f, g)
	if got := r.d.Stats().ReadOnly; got.ReadOnly {
		t.Fatalf("Stats().ReadOnly after the first refusal = %+v, want writable", got)
	}
	second := errors.New("second boom")
	r.failSyncOf(t, spare.Segment, second)
	r.settlePool(t)
	_ = mustReturn(t, "Sync in the spare", r.syncAsync(context.Background(), g))
	eventually(t, "the report to turn read-only", func() bool { return r.d.Stats().ReadOnly.ReadOnly })
	got := r.d.Stats().ReadOnly
	if !errors.Is(got.Err, second) || got.SpareMade || got.Check != nil {
		t.Fatalf("Stats().ReadOnly = %+v, want read-only naming the second failure", got)
	}
}
