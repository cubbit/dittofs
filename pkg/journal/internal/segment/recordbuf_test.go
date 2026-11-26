package segment

import "testing"

// A record lands in the smallest class whose buffers hold it, so a
// power-of-two payload with its overhead keeps its own class rather than
// doubling, and a buffer handed back by its capacity returns to the class
// it came from. A record above the largest class has none and draws no
// buffer.
func TestRecordClassIsTheSmallestThatHoldsTheRecord(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want int
		ok   bool
	}{
		{104, 0, true},
		{4096 + 104, 0, true},
		{4096 + 128, 0, true},
		{4096 + 129, 1, true},
		{65536 + 104, 4, true},
		{1<<20 + 104, 8, true},
		{maxPooledRecord, 8, true},
		{maxPooledRecord + 1, 0, false},
		{1 << 30, 0, false},
	} {
		c, ok := recordClass(tc.size)
		if c != tc.want || ok != tc.ok {
			t.Errorf("recordClass(%d) = %d, %v, want %d, %v", tc.size, c, ok, tc.want, tc.ok)
			continue
		}
		p := getRecordBuf(tc.size)
		if !ok {
			if p != nil {
				t.Errorf("getRecordBuf(%d) above the largest class = a buffer of capacity %d, want nil", tc.size, cap(*p))
			}
			continue
		}
		if int64(cap(*p)) < tc.size {
			t.Errorf("getRecordBuf(%d) has capacity %d, below the record", tc.size, cap(*p))
		}
		if got, _ := recordClass(int64(cap(*p))); got != c {
			t.Errorf("a class %d buffer of capacity %d returns to class %d", c, cap(*p), got)
		}
		putRecordBuf(p)
	}
}

// TestPutRecordBufDropsABufferOffItsClass checks that a buffer whose capacity
// is not exactly its class's never comes back from the pool: a 5000-byte one,
// below class 1's 8320, would otherwise be handed out for a record it cannot
// hold.
func TestPutRecordBufDropsABufferOffItsClass(t *testing.T) {
	b := make([]byte, 0, 5000)
	putRecordBuf(&b)
	if p := getRecordBuf(8000); cap(*p) < 8000 {
		t.Fatalf("getRecordBuf(8000) after a 5000-byte put = capacity %d, want at least 8000", cap(*p))
	}
}
