//go:build foundationdb

package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
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
// without one; it refuses an unset cluster file, which would open the
// client's default cluster, and each TLS file it would not pass to the client.
func TestCheckKVFoundationDBNeedsNoBadgerPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*bootstrap.Bootstrap)
		want string // the refusal's text; empty is none
	}{
		{"no badger path", func(*bootstrap.Bootstrap) {}, ""},
		{"no cluster file", func(cfg *bootstrap.Bootstrap) {
			cfg.Metadata.FoundationDB.ClusterFile = ""
		}, "metadata.foundationdb.cluster_file is not set"},
		{"tls cert file", func(cfg *bootstrap.Bootstrap) {
			cfg.Metadata.FoundationDB.TLS.CertFile = "cert.pem"
		}, "metadata.foundationdb.tls is not supported yet"},
		{"tls key file", func(cfg *bootstrap.Bootstrap) {
			cfg.Metadata.FoundationDB.TLS.KeyFile = "key.pem"
		}, "metadata.foundationdb.tls is not supported yet"},
		{"tls ca file", func(cfg *bootstrap.Bootstrap) {
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

// openKV gives up on a cluster it cannot reach when its context ends,
// naming the cluster file, rather than waiting for the cluster forever. No
// cluster listens on port 1.
func TestOpenKVGivesUpOnAnUnreachableCluster(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(cf, []byte("test:test@127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type result struct {
		db  kv.KV
		err error
	}
	done := make(chan result, 1)
	go func() {
		db, err := openKV(ctx, foundationDBConfig(cf), newSystemClock())
		done <- result{db, err}
	}()
	var r result
	select {
	case r = <-done:
	// A bounded wait is the assertion: well past ctx's deadline, and short of
	// openKV's own, so an open that drops ctx fails here too.
	case <-time.After(15 * time.Second):
		t.Fatalf("openKV(foundationdb on an unreachable %s) did not return 13s after its context ended", cf)
	}
	db, err := r.db, r.err
	if err == nil {
		_ = db.Close()
		t.Fatalf("openKV(foundationdb on an unreachable %s): nil error, want one", cf)
	}
	if !strings.Contains(err.Error(), cf) {
		t.Fatalf("openKV(foundationdb on an unreachable %s): %v, want an error naming the cluster file", cf, err)
	}
}

// openKV opens the cluster a FoundationDB bootstrap names, not Badger.
func TestOpenKVOpensFoundationDB(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	if os.Getenv("DITTOFS_FDB_OWN_CLUSTER") == "" {
		t.Skip("clears the whole cluster: set DITTOFS_FDB_OWN_CLUSTER=1 on a cluster of its own, with -p 1")
	}
	// The open writes the node's clock ceiling: the test owns the cluster,
	// as an installation does, and clears it.
	t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
	db, err := openKV(t.Context(), foundationDBConfig(cf), newSystemClock())
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
