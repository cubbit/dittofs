//go:build foundationdb

package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
)

// foundationDB reports whether this build opens metadata.backend foundationdb.
const foundationDB = true

// foundationDBOpenTimeout bounds connecting to the cluster: the client retries
// an unreachable one forever, so a wrong cluster file would hang the command
// with no output.
const foundationDBOpenTimeout = 30 * time.Second

// openFoundationDB opens the cluster metadata.foundationdb.cluster_file names,
// over the TLS files metadata.foundationdb.tls names, with no key prefix: an
// installation owns its whole cluster.
func openFoundationDB(ctx context.Context, cfg *bootstrap.Bootstrap, clock systemClock) (kv.KV, error) {
	ctx, cancel := context.WithTimeout(ctx, foundationDBOpenTimeout)
	defer cancel()
	cf := cfg.Metadata.FoundationDB.ClusterFile
	db, err := fdbkv.Open(ctx, fdbkv.Options{
		ClusterFile: cf,
		TLS:         fdbkv.TLS(cfg.Metadata.FoundationDB.TLS),
		NodeID:      cfg.Node.ID,
		Clock:       clock.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to the FoundationDB cluster in %s: %w", cf, err)
	}
	return db, nil
}
