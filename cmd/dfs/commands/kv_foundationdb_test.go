//go:build foundationdb

package commands

import (
	"os"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
)

// foundationDBConfig is a bootstrap selecting FoundationDB on clusterFile,
// with a node and no Badger path.
func foundationDBConfig(clusterFile string) *bootstrap.Bootstrap {
	var cfg bootstrap.Bootstrap
	cfg.Metadata.Backend = "foundationdb"
	cfg.Metadata.FoundationDB.ClusterFile = clusterFile
	cfg.Node.ID = [16]byte{1}
	return &cfg
}

// A FoundationDB bootstrap names no Badger path, so checkKV accepts it
// without one; it refuses TLS files it would not pass to the client.
func TestCheckKVFoundationDBNeedsNoBadgerPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*bootstrap.Bootstrap)
		want string // the refusal's text; empty is none
	}{
		{"no badger path", func(*bootstrap.Bootstrap) {}, ""},
		{"tls files", func(cfg *bootstrap.Bootstrap) {
			cfg.Metadata.FoundationDB.TLS.CertFile = "cert.pem"
			cfg.Metadata.FoundationDB.TLS.KeyFile = "key.pem"
			cfg.Metadata.FoundationDB.TLS.CAFile = "ca.pem"
		}, "metadata.foundationdb.tls is not supported yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := foundationDBConfig("fdb.cluster")
			tc.edit(cfg)
			err := checkKV(cfg)
			if tc.want == "" && err != nil {
				t.Fatalf("checkKV(foundationdb, %s): %v, want nil", tc.name, err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("checkKV(foundationdb, %s): %v, want an error naming %q", tc.name, err, tc.want)
			}
		})
	}
}

// openKV opens the cluster a FoundationDB bootstrap names, not Badger.
func TestOpenKVOpensFoundationDB(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	// The open writes the node's clock ceiling: the test owns the cluster,
	// as an installation does, and clears it.
	t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
	db, err := openKV(foundationDBConfig(cf), newSystemClock())
	if err != nil {
		t.Fatalf("openKV(foundationdb on %s): %v", cf, err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("closing the store: %v", err)
	}
	if _, ok := db.(*fdbkv.Store); !ok {
		t.Fatalf("openKV(foundationdb) opened a %T, want *fdbkv.Store", db)
	}
}
