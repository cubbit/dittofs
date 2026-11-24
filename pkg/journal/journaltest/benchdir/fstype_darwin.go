package benchdir

import "syscall"

// cString is the text of a NUL-terminated C array of signed bytes.
func cString(a []int8) string {
	b := make([]byte, 0, len(a))
	for _, c := range a {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// FSType is the name of the filesystem holding dir, as statfs reports it.
func FSType(dir string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}
	return cString(st.Fstypename[:]), nil
}
