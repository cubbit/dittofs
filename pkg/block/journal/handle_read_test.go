package journal

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestHandleReadAt_PublicServesAWrite(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	id := fileOf(7)
	v := writeReady(t, h, id, "hello")

	p := []byte("XXXXX")
	n, missing, held, asOf, err := h.ReadAt(context.Background(), id, 0, p)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 5 || string(p) != "hello" || len(missing) != 0 {
		t.Fatalf("ReadAt = n %d, %q, missing %+v; want 5, \"hello\", none", n, p, missing)
	}
	if len(held) != 1 || held[0].Version != v {
		t.Fatalf("held = %+v, want one extent at %v", held, v)
	}
	if asOf == (Version{}) {
		t.Fatal("asOf is zero")
	}

	// A read spanning the write and a never-written gap reports the gap and
	// leaves its bytes untouched.
	q := bytes.Repeat([]byte{'#'}, 9)
	n, missing, _, _, err = h.ReadAt(context.Background(), id, 0, q)
	if err != nil {
		t.Fatalf("ReadAt spanning: %v", err)
	}
	if string(q[:5]) != "hello" || string(q[5:]) != "####" {
		t.Fatalf("buffer = %q, want the gap left untouched", q)
	}
	if len(missing) != 1 || missing[0].Off != 5 || missing[0].Len != 4 {
		t.Fatalf("missing = %+v, want [5,9)", missing)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5 bytes served", n)
	}
}

func TestHandleReadAt_PublicRefusesAClosedHandle(t *testing.T) {
	j := newTestJournal(t)
	withLimit(t, j)
	h, err := j.Share(1, 0, Version{})
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, _, _, err := h.ReadAt(context.Background(), fileOf(1), 0, make([]byte, 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadAt after Close = %v, want ErrClosed", err)
	}
}
