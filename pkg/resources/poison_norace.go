//go:build !race

package resources

// poison does nothing without the race detector.
func poison([]byte) {}
