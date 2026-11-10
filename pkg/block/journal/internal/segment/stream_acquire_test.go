package segment_test

import (
	"bytes"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// TestStreamAcquireReadHoldsTheSegmentUntilRelease checks the reference a
// reader takes to read one record in more than one read: a closed stream
// refuses it with ErrStreamClosed, reads through it return the segment's
// bytes, and a retirement's drain of the segment waits until it is released.
func TestStreamAcquireReadHoldsTheSegmentUntilRelease(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(streamEnv) {}).built(t)
	defer func() { _ = e.s.Close() }()

	payload := bytes.Repeat([]byte{0x5a}, 64)
	h, body := streamWriteHeader(streamFileID(1), 0, payload, 7, core.Version{Counter: 1})
	p := streamAppendWrite(t, e, h, body)
	want := make([]byte, 128)
	if _, err := e.s.ReadAt(1, want, p.Loc.Off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	// The stream's own reference to its active segment goes first, or the
	// drain below would wait the stream out rather than the reader.
	if err := e.s.Close(); err != nil {
		t.Fatalf("the stream's Close: %v", err)
	}
	if ref, err := e.s.AcquireRead(1); !errors.Is(err, segment.ErrStreamClosed) {
		if err == nil {
			ref.Release()
		}
		t.Fatalf("AcquireRead through a closed stream = %v, want ErrStreamClosed", err)
	}

	reader := segment.NewStream(2, streamIdentity, e.set, e.pool, e.cache, nopSink())
	ref, err := reader.AcquireRead(1)
	if err != nil {
		t.Fatalf("AcquireRead: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- e.cache.Close(1) }()

	// The drain has begun once a new reference is refused.
	for deadline := time.Now().Add(10 * time.Second); ; runtime.Gosched() {
		other, err := reader.AcquireRead(1)
		if errors.Is(err, segment.ErrSegmentGone) {
			break
		}
		if err == nil {
			other.Release()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache's Close never began its drain: %v", err)
		}
	}
	select {
	case err := <-closed:
		t.Fatalf("the drain returned (%v) while a read reference was held", err)
	default:
	}

	// Two reads through the one reference, as a header then a body.
	got := make([]byte, 128)
	if _, err := ref.ReadAt(got[:16], p.Loc.Off); err != nil {
		t.Fatalf("the first read through the reference: %v", err)
	}
	if _, err := ref.ReadAt(got[16:], p.Loc.Off+16); err != nil {
		t.Fatalf("the second read through the reference: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the reads through the reference do not return the record's bytes")
	}

	ref.Release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("the cache's Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not return once the read reference was released")
	}
}
