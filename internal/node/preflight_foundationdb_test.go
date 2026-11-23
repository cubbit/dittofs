//go:build foundationdb

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The start check over a store with no directory: the KV a FoundationDB host
// opened, with no key prefix, over a cluster the installation owns whole.
// Each row is an installation on that cluster, cleared when it ends.
func TestPreflightOnFoundationDB(t *testing.T) {
	cf := os.Getenv("DITTOFS_FDB_CLUSTER_FILE")
	if cf == "" {
		t.Skip("no FoundationDB: run through test/fdb/cluster.sh test, which sets DITTOFS_FDB_CLUSTER_FILE")
	}
	if os.Getenv("DITTOFS_FDB_OWN_CLUSTER") == "" {
		t.Skip("clears the whole cluster: set DITTOFS_FDB_OWN_CLUSTER=1 on a cluster of its own, with -p 1")
	}
	instA := journal.InstallationID{0xAA, 0x01}
	for _, r := range []struct {
		name string
		// store writes the row's keys into the opened cluster.
		store func(t *testing.T, db kv.KV)
		// journal makes the row's device hold a journal of instA.
		journal bool
		want    error
	}{
		{name: "an empty cluster is not initialised", want: errNotInitialised},
		{name: "a journal over an empty cluster is the store lost", journal: true, want: errStoreLost},
		{
			name: "a key that is not the installation's is unidentifiable",
			store: func(t *testing.T, db kv.KV) {
				if err := db.Update(t.Context(), func(tx kv.Txn) error { return tx.Set([]byte("foreign"), []byte("v")) }); err != nil {
					t.Fatalf("write the foreign key: %v", err)
				}
			},
			want: errUnidentifiable,
		},
		{
			name: "an initialised cluster passes",
			store: func(t *testing.T, db kv.KV) {
				if err := kvstore.Init(t.Context(), db, [16]byte(instA)); err != nil {
					t.Fatalf("initialise the store: %v", err)
				}
			},
		},
	} {
		t.Run(r.name, func(t *testing.T) {
			t.Cleanup(func() { fdbkvtest.ClearAll(t, cf) })
			db, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: cf, NodeID: [16]byte{1}})
			if err != nil {
				t.Fatalf("open the cluster in %s: %v", cf, err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if r.store != nil {
				r.store(t, db)
			}
			dir := filepath.Join(t.TempDir(), "device")
			if r.journal {
				initJournal(t, dir, instA)
			} else {
				makeDir(t, dir)
			}
			got := Preflight(context.Background(), "", db, filepath.Join(t.TempDir(), "blocks"), []string{dir},
				sim.NewTestClock(time.Unix(0, 0)), journaltest.SuitableMount())
			if !errors.Is(got, r.want) {
				t.Fatalf("Preflight over the cluster: got %v, want %v", got, r.want)
			}
			// Preflight never closes the host's KV: the node goes on to use it.
			if err := db.View(t.Context(), func(kv.Reader) error { return nil }); err != nil {
				t.Errorf("the KV after the check: %v, want it still open", err)
			}
		})
	}
}
