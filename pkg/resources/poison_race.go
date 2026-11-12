//go:build race

package resources

// poison fills a released buffer, so a borrower that reads after its release
// sees garbage under the race detector.
func poison(b []byte) {
	for i := range b {
		b[i] = poisonByte
	}
}
