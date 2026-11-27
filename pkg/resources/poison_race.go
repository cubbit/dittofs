//go:build race

package resources

// poison fills a released buffer, so a borrower that reads after its release
// sees garbage under the race detector.
func poison(b []byte) {
	if len(b) == 0 {
		return
	}
	// A doubling copy: the race detector instruments each byte a loop writes,
	// and a copy as one range.
	b[0] = poisonByte
	for i := 1; i < len(b); i *= 2 {
		copy(b[i:], b[:i])
	}
}
