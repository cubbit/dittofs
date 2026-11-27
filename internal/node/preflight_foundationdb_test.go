//go:build foundationdb

package node

import (
	"os"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
)

// The start check's KV rows over the backend they exist for: the KV a
// FoundationDB host opened, with no key prefix, over a cluster the
// installation owns whole. Each row is an installation on that cluster,
// cleared when it ends.
func TestPreflightOnFoundationDB(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	if os.Getenv("DITTOFS_FDB_OWN_CLUSTER") == "" {
		t.Skip("clears the whole cluster: set DITTOFS_FDB_OWN_CLUSTER=1 on a cluster of its own, with -p 1")
	}
	for _, r := range kvRows() {
		t.Run(r.name, func(t *testing.T) {
			t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
			db, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: cf, NodeID: [16]byte{1}})
			if err != nil {
				t.Fatalf("open the cluster in %s: %v", cf, err)
			}
			t.Cleanup(func() { _ = db.Close() })
			r.run(t, db)
		})
	}
}
