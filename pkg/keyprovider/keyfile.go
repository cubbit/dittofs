package keyprovider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cubbit/dittofs/pkg/internal/dirsync"
)

// CreateKeyFile writes a new key file at path, a master key wrapped under
// passphrase, with mode 0600, unless a file is already there. It reports
// whether it wrote one. An empty passphrase is refused with
// ErrPassphraseMissing before anything is written.
//
// decision: the key is written to a temporary file in path's directory,
// synced, and published by a hard link rather than a rename. A link fails
// when path exists, so a file another run created meanwhile is kept and
// used, never replaced, and a crash leaves at most the temporary, never a
// half-written key file at path. A filesystem without hard links refuses
// the creation; the operator then writes the file with the Go helper.
func CreateKeyFile(path, passphrase string) (bool, error) {
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return false, nil // an existing file, or one the provider's read reports
	}
	raw, err := GenerateKeyFile(passphrase)
	if err != nil {
		return false, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*") // mode 0600
	if err != nil {
		return false, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(raw)
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return false, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	if err := os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	if err := os.Remove(tmp.Name()); err != nil {
		return true, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	if err := dirsync.Sync(dir); err != nil {
		return true, fmt.Errorf("creating the key file %s: sync %s: %w", path, dir, err)
	}
	return true, nil
}
