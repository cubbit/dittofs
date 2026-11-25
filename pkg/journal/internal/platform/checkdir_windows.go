//go:build windows

package platform

// checkDirOS decides whether dir is suitable for a journal by the
// platform's own probe: the mount point and volume of dir and its write
// cache property answer, gathered and run through the verdict. Its inputs
// are swapped for a recorded table and recorded answers in the platform's
// own tests, so no answer there depends on the host's volumes.
func checkDirOS(dir string) error {
	return checkDirVolume(realTable, dir)
}
