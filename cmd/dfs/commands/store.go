package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
)

// v1Databases are where v1 kept its control-plane database by default: the
// Kubernetes operator's path and the Docker image's state volume. refuseV1
// looks for a file at each, wherever the bootstrap file is.
func v1Databases() []string {
	return []string{"/data/controlplane/controlplane.db", "/data/state/controlplane.db"}
}

// openKV opens the metadata store's KV the bootstrap names, after refusing a
// backend this build does not open, a store directory that is not set or
// does not exist, so that a typo creates no empty store, a bootstrap naming
// no node, and, as refuseV1's preflight check, a v1 deployment's database at
// any of v1. It opens nothing
// before every refusal has passed.
func openKV(cfg *bootstrap.Bootstrap, clock systemClock, v1 ...string) (*badgerkv.Store, error) {
	if b := cfg.Metadata.Backend; b != "" && b != "badger" {
		return nil, fmt.Errorf("metadata.backend %q is not one this build opens; it opens badger", b)
	}
	dir := cfg.Metadata.Badger.Path
	if dir == "" {
		return nil, errors.New("metadata.badger.path is not set")
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("metadata.badger.path: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("metadata.badger.path %s is not a directory", dir)
	}
	// ponytail: node.id must be written in the bootstrap, as nothing else
	// names the node yet. The node's identity read at start, from the
	// journal directories' node-id copies and node.id, replaces this check.
	if cfg.Node.ID == [16]byte{} {
		return nil, errors.New("node.id is not set")
	}
	if err := refuseV1(v1...); err != nil {
		return nil, err
	}
	return badgerkv.Open(badgerkv.Options{Dir: dir, NodeID: cfg.Node.ID, Clock: clock.Now})
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
