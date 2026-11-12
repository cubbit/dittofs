package engine_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A clone of journal-held bytes reserves their record bytes, not their
// payload, over the real journal (RFC 1 §6): a reservation short of each
// record's header and padding refuses the last copy, which the clone retries
// past its journal step for good. The length is no multiple of 8, so the
// padding counts too.
func TestACloneReservesItsCopiesRecordBytes(t *testing.T) {
	f := enginetest.New(t)
	const n = 64<<10 + 3
	_, src := f.File("src")
	_, dst := f.File("dst")
	if _, err := f.E.Write(t.Context(), f.Share.ID, src, 0, bytes.Repeat([]byte{'s'}, n), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatalf("write the source: %v", err)
	}
	// The bound is the assertion: a short reservation never fails the clone,
	// it retries the copy until the caller gives up.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := f.E.Clone(ctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: n}); err != nil {
		t.Fatalf("Clone of %d journal-held bytes: %v, want nil", n, err)
	}
}
