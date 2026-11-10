package datapath

import (
	"bytes"
	"context"
	"runtime"
	"testing"
)

// TestReadOfALargeRecordAllocatesFlat reads 4 KiB of a 1 MiB record over and
// over: once warm, a read reuses its record buffer, so the bytes a read
// allocates do not grow with the record it verifies.
func TestReadOfALargeRecordAllocatesFlat(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector inflates allocations and drops pooled buffers")
	}
	c, _ := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
	s := mustShare(t, c, 1)
	f := testFile(1)
	data := pattern(1<<20, 23)
	if _, err := c.WriteAt(context.Background(), s, f, 0, data, 1); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 4<<10)
	read := func() {
		if n, _, _, _, err := c.ReadAt(context.Background(), s, f, 1000, p); err != nil || n != len(p) {
			t.Fatalf("ReadAt = %d, %v", n, err)
		}
	}
	read()

	const reads = 50
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range reads {
		read()
	}
	runtime.ReadMemStats(&after)
	// The record is 1 MiB; a read that allocated its buffer would allocate at
	// least that. What is left is the read's bookkeeping, and whatever the
	// core's own goroutines allocate meanwhile.
	if per := (after.TotalAlloc - before.TotalAlloc) / reads; per > 64<<10 {
		t.Fatalf("a warm read allocates %d bytes, want the record buffer reused", per)
	}
	if !bytes.Equal(p, data[1000:1000+len(p)]) {
		t.Fatal("the reads served the wrong bytes")
	}
}

// TestReadThroughARecycledBufferServesOnlyItsRecord reads a large record, then
// a small one and a large one again, through the record buffer the first read
// leaves in the pool: what the larger record left in the buffer must never
// reach p from the smaller one's read.
func TestReadThroughARecycledBufferServesOnlyItsRecord(t *testing.T) {
	c, _ := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
	s := mustShare(t, c, 1)
	big, small := testFile(1), testFile(2)
	bigData, smallData := pattern(1<<20, 24), pattern(4<<10, 25)
	if _, err := c.WriteAt(context.Background(), s, big, 0, bigData, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteAt(context.Background(), s, small, 0, smallData, 1); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		p := sentinels(8 << 10)
		n, missing, _, _, err := c.ReadAt(context.Background(), s, small, 0, p)
		if err != nil || n != 4<<10 || len(missing) != 1 {
			t.Fatalf("round %d: small read = %d %v %v", i, n, missing, err)
		}
		if !bytes.Equal(p[:4<<10], smallData) || !bytes.Equal(p[4<<10:], sentinels(4<<10)) {
			t.Fatalf("round %d: the small read served bytes that are not its record's", i)
		}
		q := make([]byte, 64<<10)
		if _, _, _, _, err := c.ReadAt(context.Background(), s, big, 900<<10, q); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(q, bigData[900<<10:900<<10+64<<10]) {
			t.Fatalf("round %d: the large read served the wrong bytes", i)
		}
	}
}
