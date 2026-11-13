//go:build engine_conformance

package conformance

import (
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// awaitShut returns once a Write on the fixture's share answers
// vfserr.ErrStale, the engine's answer once the Unbind has shut the share, and
// fails the test if that does not happen. The check after it can miss a
// regression, but it never fails a correct engine: a correct Unbind is still
// waiting for the held batch.
func awaitShut(t *testing.T, f *enginetest.Fixture, file entity.FileID) {
	t.Helper()

	for range 10000 {
		_, err := f.E.Write(t.Context(), f.Share.ID, file, 0, []byte("x"), engine.WriteOpts{})
		if errors.Is(err, vfserr.ErrStale) {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("the share never refused a Write with ErrStale while Unbind waited")
}

// §2.3 unbind and drop, adapted (no offload pass): a share with pending writes
// and a removal batch in flight is unbound, which returns once the batch has,
// and dropped, after which the journal's figures name no entry for the share
// and each accepted write had its Done exactly once.
func TestRFC8A_UnbindThenDropLeavesNoEntryForTheTag(t *testing.T) {
	f := enginetest.New(t)
	ctx := t.Context()
	res := enginetest.NewReservations(t)
	_, removed := f.File("removed")
	_, pending := f.File("pending")

	write(t, f, removed, 0, make([]byte, 4096))
	f.HoldBatches()

	if err := f.E.Truncate(ctx, f.Share.ID, removed, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	opts, answered := res.Opts()
	_, err := f.E.Write(ctx, f.Share.ID, pending, 0, []byte("pending"), opts)

	answered(err)

	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	unbound := make(chan error, 1)

	go func() {
		unbound <- f.E.Unbind(ctx, f.Share.ID, false)
	}()

	awaitShut(t, f, pending)

	select {
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a removal batch held, want it to wait for the batch", err)
	default:
	}

	f.ReleaseBatches()

	if err := <-unbound; err != nil {
		t.Fatalf("Unbind: %v", err)
	}

	if err := f.E.Drop(ctx, f.Share.ID); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	if _, ok := f.E.Stats().Journals[f.Dir].Shares[f.Share.ID]; ok {
		t.Error("Stats names an entry for the dropped share's tag")
	}

	if got := res.Outstanding(); got != 0 {
		t.Errorf("%d accepted writes without their Done after Drop, want 0", got)
	}

	f.Restart()

	for change, err := range f.HandleOf(f.Share.Tag).Since(
		ctx, journal.NewFileID(pending), journal.Version{},
	) {
		if err != nil {
			t.Fatal(err)
		}

		t.Errorf(
			"after a clean restart the journal holds a change of the dropped share's file: %v",
			change,
		)
	}
}
