//go:build !windows

package credentials

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an exclusive lock on f without waiting, and reports whether
// another holds it. Closing f, or the process ending, releases it.
func tryLock(f *os.File) (held bool, err error) {
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// readFile is os.ReadFile: a rename replaces a file another process reads.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// restrict keeps the directory dir to its owner, 0700, an existing one wider
// than that included; a file's own mode is set as it is created.
func restrict(path string, dir bool) error {
	if !dir {
		return nil
	}
	return os.Chmod(path, DirPermissions)
}

// syncDir flushes dir's entries, so a rename into it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
