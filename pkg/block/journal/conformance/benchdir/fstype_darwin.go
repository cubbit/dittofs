package benchdir

import "golang.org/x/sys/unix"

// FSType is the name of the filesystem holding dir, as statfs reports it.
func FSType(dir string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(st.Fstypename[:]), nil
}
