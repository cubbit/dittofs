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
// installation owns its whole cluster. dfs start opens it before the start
// check, which reads the store through it.
func openFoundationDB(ctx context.Context, cfg *bootstrap.Bootstrap, clock systemClock) (kv.KV, error) {
	ctx, cancel := context.WithTimeout(ctx, foundationDBOpenTimeout)
	defer cancel()
	cf := cfg.Metadata.FoundationDB.ClusterFile
	// decision: the open writes this node's clock ceiling before it returns,
	// so before the start check reads the cluster: over a cluster another
	// installation or program owns, the ceiling lands in its keyspace before
	// the check refuses it. The ceiling is the one key the store's Init
	// tolerates, so on the installation's own cluster it is harmless, and
	// fdbkv has no open that skips it: no Now may precede a durable ceiling
	// (RFC 16 §4.1). Overturned when an operator reports the stray key, or
	// when a cluster can hold several installations under prefixes: then the
	// check needs a read-only path through fdbkv.
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
