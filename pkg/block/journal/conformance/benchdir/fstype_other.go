//go:build !linux && !darwin

package benchdir

// FSType names no filesystem on a platform the suite cannot read, so the
// refusal does not apply there.
//
// decision: on a platform other than Linux and macOS the refusal fails open:
// a RAM-backed directory is accepted, because the suite reads no filesystem
// name there. The benchmarks are run by hand on the two platforms that
// can; withdraw this if the suite is ever run elsewhere.
func FSType(string) (string, error) { return "", nil }
