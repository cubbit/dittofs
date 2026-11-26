package segment

import (
	"math/bits"
	"sync"
)

// recordOverhead bounds what a content record takes besides its payload: the
// header, a write's modification time and the padding to the slot's grid,
// rounded up. Each class adds it to its power of two, so a power-of-two
// payload — the common write size — lands in its own class, not the next.
const recordOverhead = 128

// The record classes span 4 KiB to 1 MiB payloads; a smaller record takes
// the smallest class, and a larger one is not pooled.
const (
	minRecordClass  = 12
	maxRecordClass  = 20
	maxPooledRecord = 1<<maxRecordClass + recordOverhead
)

// recordBufs holds the encode buffers of content records, one pool per
// class: class k holds buffers of capacity 1<<k + recordOverhead. It is
// shared by every stream, for a buffer never outlives the append that drew
// it, and sync.Pool is already per-P; Close needs no teardown, since it
// waits out every append in flight, and idle buffers go back to the GC.
var recordBufs [maxRecordClass - minRecordClass + 1]sync.Pool

// recordClass is the index into recordBufs of the smallest class whose
// buffers hold a record of size bytes. It reports false for a record above
// maxPooledRecord, which no class holds, so no size indexes past the table.
func recordClass(size int64) (int, bool) {
	if size > maxPooledRecord {
		return 0, false
	}
	if size <= 1<<minRecordClass+recordOverhead {
		return 0, true
	}
	return bits.Len64(uint64(size-recordOverhead-1)) - minRecordClass, true
}

// getRecordBuf returns an empty buffer of at least size bytes' capacity, to
// be handed back through putRecordBuf, or nil for a record above
// maxPooledRecord, which is not pooled.
func getRecordBuf(size int64) *[]byte {
	c, ok := recordClass(size)
	if !ok {
		return nil
	}
	if p, ok := recordBufs[c].Get().(*[]byte); ok {
		return p
	}
	b := make([]byte, 0, 1<<(c+minRecordClass)+recordOverhead)
	return &b
}

// putRecordBuf hands a buffer from getRecordBuf back to its class, emptied.
// The caller keeps no slice of it.
func putRecordBuf(p *[]byte) {
	c, ok := recordClass(int64(cap(*p)))
	if !ok {
		return
	}
	*p = (*p)[:0]
	recordBufs[c].Put(p)
}
