package engine_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A clone of journal-held bytes reserves their record bytes, not their
// payload. The unaligned length needs both a record header and padding.
func TestACloneReservesItsCopiesRecordBytes(t *testing.T) {
	// The source's first write takes the pool's opening spare, kept beside
	// the target until a stream takes its first segment, so no refill follows
	// it; the clone's reservation takes two more, and one spare of tolerance
	// beside them: a journal working in real
	// time can roll one more of its own records onto a spare in the window
	// between the write and the reservation, and with the refill held the
	// pool can never make that spare up before the reservation asks — the
	// engine answers a refusal at the reservation step, changing nothing, so
	// the pool the row sizes must cover the journal's own rolls too, not only
	// the write and the copies.
	f := enginetest.New(t, enginetest.WithSpares(4))
	const n = 64<<10 + 3
	_, src := f.File("src")
	_, dst := f.File("dst")
	want := bytes.Repeat([]byte{'s'}, n)
	// Stop the refill so the clone has to finish with the spares it started
	// with: the first make, the one the clone's hold kicks, parks here, and
	// the pool's one worker makes nothing past it.
	f.Seam.HoldFirst(t, "allocate", "*")
	if _, err := f.E.Write(t.Context(), f.Share.ID, src, 0, want, engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatalf("write the source: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := f.E.Clone(ctx, f.Share.ID, src, f.Share.ID, dst, vfserr.Extent{Len: n}); err != nil {
		t.Fatalf("Clone of %d journal-held bytes: %v, want nil", n, err)
	}
	got := make([]byte, n)
	if read, err := f.E.Read(ctx, f.Share.ID, dst, 0, got); read != n || err != nil {
		t.Fatalf("read the clone: got %d bytes, %v, want %d bytes, nil", read, err, n)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the clone's bytes differ from the source")
	}
}
