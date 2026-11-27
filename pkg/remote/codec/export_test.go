package codec

// allocRecord is a decoder's allocator that records each size it is asked
// for, so a test asserts a bound held before anything was allocated.
type allocRecord struct{ sizes []int }

func (a *allocRecord) alloc(n int) []byte {
	a.sizes = append(a.sizes, n)
	return make([]byte, n)
}
