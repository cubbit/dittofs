//go:build linux

package platform

// CheckDir decides whether dir is suitable for a journal. It returns an
// error matching core.ErrUnsuitableStorage for the first rule of the
// platform profile a directory refuses — a mount without cache flushes, a
// Btrfs directory that is not a subvolume root or keeps copy-on-write —
// with its message naming the rule, the directory and what to change; any
// other error as itself, which says nothing about whether the directory is
// suitable; and nil for a suitable one.
func CheckDir(dir string) error {
	return Check(dir)
}
