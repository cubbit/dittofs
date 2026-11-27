//go:build foundationdb

package kvstore_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
)

// The store's key-record view passes the view suite on FoundationDB.
func TestKeyRecordsConformanceFDB(t *testing.T) {
	prof.Watch(t, prof.Options{})
	runKeyRecords(t, fdbStore(fdbkv.Options{}))
}
