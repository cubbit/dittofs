//go:build !linux && !darwin

package benchdir

// FSType names no filesystem on a platform the suite cannot read: the
// refusal does not apply there.
func FSType(string) (string, error) { return "", nil }
