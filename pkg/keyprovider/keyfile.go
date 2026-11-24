package keyprovider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cubbit/dittofs/pkg/internal/dirsync"
)

// CreateKeyFile writes a new key file at path, a master key wrapped under
// passphrase, with mode 0600, unless a file is already there. It reports
// whether it wrote one. An empty passphrase is refused with
// ErrPassphraseMissing before anything is written. Once a file is at path,
// it removes the temporaries earlier stopped runs left beside it.
//
// decision: the key is written to a temporary file in path's directory,
// synced, and published by a hard link rather than a rename. A link fails
// when path exists, so a file another run created meanwhile is kept and
// used, never replaced, and a crash leaves at most the temporary, never a
// half-written key file at path. A filesystem without hard links refuses
// the creation; the operator then writes the file with the Go helper.
// Overturned if dfs must create the key on a filesystem without hard links:
// then an O_EXCL write and a sync of the file itself.
func CreateKeyFile(path, passphrase string) (bool, error) {
	created, err := createKeyFile(path, passphrase)
	if err == nil {
		removeTemporaries(path)
	}
	return created, err
}

// tmpPrefix is how the names of path's temporaries begin.
func tmpPrefix(path string) string { return "." + filepath.Base(path) + ".tmp" }

func createKeyFile(path, passphrase string) (bool, error) {
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return false, nil // an existing file, or one the provider's read reports
	}
	raw, err := GenerateKeyFile(passphrase)
	if err != nil {
		return false, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPrefix(path)+"*") // mode 0600
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
	if err := os.Link(tmp.Name(), path); err != nil {
		// A file at path, by EEXIST or by another run removing this run's
		// temporary after linking its own, is used.
		if _, serr := os.Lstat(path); serr == nil {
			return false, nil
		}
		return false, fmt.Errorf("creating the key file %s: %w", path, err)
	}
	// The temporary's own name goes with the deferred remove; only the new
	// entry must survive a power loss.
	if err := dirsync.Sync(dir); err != nil {
		return true, fmt.Errorf("creating the key file %s: sync %s: %w", path, dir, err)
	}
	return true, nil
}

// removeTemporaries removes the temporaries beside path that a stopped run
// left, each holding a wrapped master key. It runs only once a file is at
// path, so a temporary another run is still writing can only lose its link,
// which then finds that file. A removal that fails is left for the next run.
func removeTemporaries(path string) {
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), tmpPrefix(path)) {
			_ = os.Remove(filepath.Join(filepath.Dir(path), e.Name()))
		}
	}
}
