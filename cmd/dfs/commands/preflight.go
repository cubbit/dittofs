package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"syscall"
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
// file at beside or at one of fixed only stops the start, naming it. A path
// whose parent is missing or not a directory holds no database. Any other
// path it cannot check stops the start too, but for a fixed path it may not
// search, which it skips with a warning.
func refuseV1(beside string, fixed []string) error {
	for i, p := range append([]string{beside}, fixed...) {
		_, err := os.Stat(p)
		switch {
		case err == nil:
			return fmt.Errorf("refusing to start: %s is v1's control-plane database, which this version does not read; move it away to start", p)
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		case i > 0 && errors.Is(err, fs.ErrPermission):
			// decision: a fixed /data path the server's user may not search is
			// skipped, not refused: the operator did not name it, and refusing
			// would keep every host whose /data is closed to that user from
			// starting over a valid installation. The path beside the bootstrap,
			// which the operator chose, still fails closed on any error, and a
			// fixed path fails closed on any other. Overturned if v1 ever ran as
			// a user other than the one starting this version over the same
			// /data.
			slog.Warn("dfs: not checking for v1's control-plane database at a path this user may not search", "path", p, "err", err)
		default:
			return fmt.Errorf("refusing to start: checking for v1's control-plane database at %s: %w", p, err)
		}
	}
	return nil
}
