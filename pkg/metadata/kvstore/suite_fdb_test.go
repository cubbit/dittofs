//go:build foundationdb

package kvstore_test

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
)

func fdbStore(opts fdbkv.Options) kvstoretest.Opener {
	return func(tb testing.TB) kv.KV { return fdbkvtest.Open(tb, opts) }
}

// The suite on FoundationDB, the replicated backend.
func TestStoreConformanceFDB(t *testing.T) {
	kvstoretest.Run(t, fdbStore(fdbkv.Options{}))
}

// The suite on FoundationDB under a clock that never moves.
func TestStoreConformanceFDBFrozenClock(t *testing.T) {
	frozen := time.Unix(1_800_000_000, 0)
	kvstoretest.Run(t, fdbStore(fdbkv.Options{Clock: func() time.Time { return frozen }}))
}

// The reopen and clock cases on FoundationDB, each reopen over the case's
// key prefix.
func TestStoreDurableFDB(t *testing.T) {
	kvstoretest.RunDurable(t, func(tb testing.TB, clock func() time.Time) func() kv.KV {
		prefix := fdbkvtest.Prefix()
		var db *fdbkv.Store
		return func() kv.KV {
			if db != nil {
				if err := db.Close(); err != nil {
					tb.Fatal(err)
				}
			}
			db = fdbkvtest.Open(tb, fdbkv.Options{Prefix: prefix, Clock: clock})
			return db
		}
	})
}

func FuzzNamespaceFDB(f *testing.F) { kvstoretest.FuzzNamespace(f, fdbStore(fdbkv.Options{})) }

// BenchmarkStoreFDB runs the store's benchmarks on FoundationDB.
func BenchmarkStoreFDB(b *testing.B) { kvstoretest.Bench(b, fdbStore(fdbkv.Options{})) }
