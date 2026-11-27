//go:build linux

package platform

// checkDirOS decides whether dir is suitable for a journal by the
// platform's own probe: the real mount table, statfs and the ioctl answers
// for dir, run through the verdict. Its inputs are swapped for a recorded
// table and recorded answers in the platform's own tests, so no answer
// there depends on the host's mounts.
func checkDirOS(dir string) error {
	return Check(dir)
}
