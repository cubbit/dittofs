//go:build !windows

package dirsync

import "os"

// Sync makes the entries of the directory dir durable.
func Sync(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}
