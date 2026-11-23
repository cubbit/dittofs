//go:build foundationdb

package commands

import (
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
)

// foundationDB reports whether this build opens metadata.backend foundationdb.
const foundationDB = true

// openFoundationDB opens the cluster metadata.foundationdb.cluster_file names,
// with no key prefix: an installation owns its whole cluster.
func openFoundationDB(cfg *bootstrap.Bootstrap, clock systemClock) (kv.KV, error) {
	db, err := fdbkv.Open(fdbkv.Options{
		ClusterFile: cfg.Metadata.FoundationDB.ClusterFile,
		NodeID:      cfg.Node.ID,
		Clock:       clock.Now,
	})
	if err != nil {
		return nil, err
	}
	return db, nil
}
