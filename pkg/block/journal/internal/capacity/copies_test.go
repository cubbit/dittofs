package capacity

import (
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// TestCopies_TransferKeepsTheShareFigureWhole follows a repack copy through
// the share's tally. The tag holds 1000 record bytes in a source segment and
// a pass has copied 600 of them into another: the set's settled figure reads
// 1600, the tag's own is 1000 while the source remains (the first take seeds
// it so), and 600 once the source retires and its copies are handed over.
// Without the handover the retirement lowers a tally that never held the
// copies, and the share's limit admits a write the copies' bytes leave no
// room for.
func TestCopies_TransferKeepsTheShareFigureWhole(t *testing.T) {
	r := newRig(t, std())
	src := r.seed(t, 1000-segment.RecordBytes(0))
	r.seal(t)
	r.seed(t, 600-segment.RecordBytes(0))
	r.c.CopyLanded(src, tag1, 600)
	if got := r.c.CopyHeldBytes(); got != 600 {
		t.Fatalf("CopyHeldBytes = %d with 600 counted, want 600", got)
	}

	const limit = 1500
	ask := func(payload int64) error {
		q := req()
		q.Payload = payload
		q.ShareLimit = limit
		g, err := r.c.Take(context.Background(), q)
		if err == nil {
			g.Release()
		}
		return err
	}
	// 1000 beside the source plus a 400-byte ask fits the limit; the settled
	// 1600 the copies are in would not: the seed leaves them out.
	if err := ask(400 - segment.RecordBytes(0)); err != nil {
		t.Fatalf("a 400-byte ask beside a tally of 1000 against a limit of %d: %v", limit, err)
	}

	r.retire(t, src)
	r.c.CopiesTransfer(src)
	r.c.Retired(tag1)
	if got := r.c.CopyHeldBytes(); got != 0 {
		t.Fatalf("CopyHeldBytes = %d once the source's copies were handed over, want 0", got)
	}
	// 600 beside the copies plus a 1100-byte ask is past the limit.
	var ns *core.NoSpaceError
	if err := ask(1100 - segment.RecordBytes(0)); !errors.As(err, &ns) || ns.Limit != core.LimitShare {
		t.Fatalf("a 1100-byte ask beside the copies' 600 against a limit of %d = %v, want the share's refusal", limit, err)
	}
}
