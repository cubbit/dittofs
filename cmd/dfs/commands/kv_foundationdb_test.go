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
// without one.
func TestCheckKVFoundationDBNeedsNoBadgerPath(t *testing.T) {
	if err := checkKV(foundationDBConfig("fdb.cluster")); err != nil {
		t.Fatalf("checkKV(foundationdb, no badger path): %v, want nil", err)
	}
}

// openKV gives the client the bootstrap's TLS files: a missing key file is
// refused naming it, which fdbkv does before it touches the client, so no
// cluster is needed. Dropped on the way, the open would reach the client and
// fail on the cluster file instead.
func TestOpenKVPassesTheTLSFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := foundationDBConfig(dir + "/fdb.cluster")
	tls := &cfg.Metadata.FoundationDB.TLS
	tls.CertFile, tls.KeyFile, tls.CAFile = dir+"/cert.pem", dir+"/key.pem", dir+"/ca.pem"
	for _, f := range []string{tls.CertFile, tls.CAFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openKV(cfg, newSystemClock())
	if err == nil {
		_ = db.Close()
		t.Fatalf("openKV(foundationdb, no %s): nil error, want one naming it", tls.KeyFile)
	}
	if !strings.Contains(err.Error(), tls.KeyFile) {
		t.Fatalf("openKV(foundationdb, no %s): %v, want an error naming it", tls.KeyFile, err)
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
