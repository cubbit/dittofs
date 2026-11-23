//go:build foundationdb

// Package fdbkvtest opens fdbkv stores for tests, each under a prefix of its
// own on a local cluster.
package fdbkvtest

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
)

var run atomic.Int64

// Prefix is a fresh prefix for a test's store; stores opened with the same one
// share their data.
func Prefix() []byte { return fmt.Appendf(nil, "test/%d-%d/", time.Now().UnixNano(), run.Add(1)) }

// Open is a store under a prefix of its own, or opts.Prefix, on the cluster
// DITTOFS_FDB_CLUSTER_FILE names, cleared when tb ends: test/fdb/cluster.sh
// starts one and sets it. Without it the test is skipped.
func Open(tb testing.TB, opts fdbkv.Options) *fdbkv.Store {
	tb.Helper()
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		tb.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	opts.ClusterFile = cf
	if opts.Prefix == nil {
		opts.Prefix = Prefix()
	}
	s, err := fdbkv.Open(tb.Context(), opts)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
		clearPrefix(tb, opts)
	})
	return s
}

// ClearAll removes every key below the system keys on the cluster clusterFile
// names, for a test that, as an installation does, owns the whole cluster.
func ClearAll(tb testing.TB, clusterFile string) {
	clearRange(tb, clusterFile, nil, []byte{0xff})
}

// Keys returns every key below the system keys on the cluster clusterFile
// names, in order, as they stand: fdbkv's own rows included, no prefix
// stripped.
func Keys(tb testing.TB, clusterFile string) [][]byte {
	tb.Helper()
	db, err := database(clusterFile)
	if err != nil {
		tb.Fatal(err)
	}
	ks, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		kvs, err := tr.GetRange(fdb.KeyRange{Begin: fdb.Key(nil), End: fdb.Key{0xff}}, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		ks := make([][]byte, len(kvs))
		for i, kv := range kvs {
			ks[i] = kv.Key
		}
		return ks, nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return ks.([][]byte)
}

// clearPrefix removes everything under the test's prefix.
func clearPrefix(tb testing.TB, opts fdbkv.Options) {
	end, _ := fdb.Strinc(opts.Prefix)
	clearRange(tb, opts.ClusterFile, opts.Prefix, end)
}

// clearRange removes the keys from begin up to end.
func clearRange(tb testing.TB, clusterFile string, begin, end []byte) {
	db, err := database(clusterFile)
	if err != nil {
		tb.Error(err)
		return
	}
	// The client's handle for this cluster file is shared; it stays open.
	if _, err := db.Transact(func(tr fdb.Transaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)})
		return nil, nil
	}); err != nil {
		tb.Error(err)
	}
}

// database opens the cluster clusterFile names, first selecting fdbkv's
// client API version when nothing in this process has: a test may read or
// write the cluster before it opens a store.
func database(clusterFile string) (fdb.Database, error) {
	if _, err := fdb.GetAPIVersion(); err != nil {
		if err := fdb.APIVersion(fdbkv.APIVersion); err != nil {
			return fdb.Database{}, err
		}
	}
	return fdb.OpenDatabase(clusterFile)
}
