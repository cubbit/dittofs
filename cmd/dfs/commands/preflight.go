package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// v1Databases are where v1 kept its control-plane database by default: the
// Kubernetes operator's path and the Docker image's state volume. refuseV1
// looks for a file at each, wherever the bootstrap file is.
func v1Databases() []string {
	return []string{"/data/controlplane/controlplane.db", "/data/state/controlplane.db"}
}

// refuseV1 is a preflight check only: it keeps this version from starting
// over a v1 deployment's data directory, where it would open a new, empty
// metadata store beside v1's configuration and serve none of it. It reads,
// opens and migrates nothing: this version never reads v1's database, and a
// file at one of paths only stops the start, naming it, as does a path it
// cannot check.
func refuseV1(paths ...string) error {
	for _, p := range paths {
		_, err := os.Stat(p)
		switch {
		case err == nil:
			return fmt.Errorf("refusing to start: %s is v1's control-plane database, which this version does not read; move it away to start", p)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("refusing to start: checking for v1's control-plane database at %s: %w", p, err)
		}
	}
	return nil
}
