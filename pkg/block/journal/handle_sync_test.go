package journal

import (
	"context"
	"errors"
	"testing"
)

// TestHandleSync_PublicSyncsItsOwnAndRefusesAnotherShares syncs a written
// file through its handle, and the same file through another share's handle,
// which is refused with an OtherShareError.
func TestHandleSync_PublicSyncsItsOwnAndRefusesAnotherShares(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	a, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share 1: %v", err)
	}
	b, err := j.Share(2, 0, Version{})
	if err != nil {
		t.Fatalf("Share 2: %v", err)
	}
	id := fileOf(7)
	writeReady(t, a, id, "hello")

	if err := a.Sync(context.Background(), id); err != nil {
		t.Fatalf("Sync through the owner = %v, want nil", err)
	}
	var other *OtherShareError
	if err := b.Sync(context.Background(), id); !errors.As(err, &other) {
		t.Fatalf("Sync through another share = %v, want an OtherShareError", err)
	}
}
