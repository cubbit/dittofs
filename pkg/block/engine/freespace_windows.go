package engine

import "golang.org/x/sys/windows"

// freeSpace is the free space, in bytes, the caller has on the volume
// holding dir.
func freeSpace(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, nil, nil); err != nil {
		return 0, err
	}
	return int64(avail), nil
}
