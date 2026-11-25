//go:build engine_conformance

package integration

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// fileKeyOf builds F‖share‖file the way the metadata store lays it out
// (pkg/metadata/kvstore keys.go), from the key package's own pieces, so a
// change of KindF or IDLen moves the test with them.
func fileKeyOf(sub ...byte) []byte {
	key := []byte{keys.KindF}
	key = keys.ID(key, make([]byte, keys.IDLen))
	key = keys.ID(key, make([]byte, keys.IDLen))

	return append(key, sub...)
}

// shareKeyOf builds S‖share‖sub.
func shareKeyOf(sub byte) []byte {
	key := keys.ID([]byte{keys.KindS}, make([]byte, keys.IDLen))

	return append(key, sub)
}

// syncMarkKeyOf builds J‖journal‖sm‖stream, the stream 4 bytes big-endian, a
// key the group commit writes on every fsync's path.
func syncMarkKeyOf() []byte {
	key := keys.ID([]byte{keys.KindJ}, make([]byte, keys.IDLen))
	key = append(key, keys.JSm)

	return binary.BigEndian.AppendUint32(key, 1)
}

func TestKeyClassifiers(t *testing.T) {
	for _, tc := range []struct {
		name            string
		key             []byte
		existence, size bool
	}{
		{"overwrite record", keys.Uint64(fileKeyOf(keys.FOv), 4096), true, false},
		{"overwrite range prefix", fileKeyOf(keys.FOv), true, false},
		{"file record", fileKeyOf(), false, true},
		{"acl", fileKeyOf(keys.FAcl), false, false},
		{"streams", fileKeyOf(keys.FS), false, false},
		{"hole", keys.Uint64(fileKeyOf(keys.FH), 0), false, false},
		{"history", fileKeyOf(keys.FHis), false, false},
		{"share version floor", shareKeyOf(keys.SVf), false, false},
		{"sync mark", syncMarkKeyOf(), false, false},
		{"format record", keys.Format, false, false},
		{"empty", nil, false, false},
		{"file kind alone", []byte{keys.KindF}, false, false},
		{"one byte short of a file record", fileKeyOf()[:keys.IDLen*2], false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := existenceKey(tc.key); got != tc.existence {
				t.Errorf("existenceKey = %v, want %v", got, tc.existence)
			}

			if got := sizeKey(tc.key); got != tc.size {
				t.Errorf("sizeKey = %v, want %v", got, tc.size)
			}
		})
	}
}

func TestTxnRecordClassifiesCommittedTransactions(t *testing.T) {
	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	rec := &txnRecord{KV: db}
	ctx := context.Background()

	set := func(keys ...[]byte) error {
		return rec.Update(ctx, func(tx kv.Txn) error {
			for _, key := range keys {
				if err := tx.Set(key, []byte("v")); err != nil {
					return err
				}
			}

			return nil
		})
	}

	if err := set(fileKeyOf()); err != nil {
		t.Fatal(err)
	}

	if err := set(syncMarkKeyOf()); err != nil {
		t.Fatal(err)
	}

	if err := set(keys.Uint64(fileKeyOf(keys.FOv), 0), fileKeyOf()); err != nil {
		t.Fatal(err)
	}

	errAbort := errors.New("abort")
	if err := rec.Update(ctx, func(tx kv.Txn) error {
		if err := tx.Set(fileKeyOf(), []byte("v")); err != nil {
			return err
		}

		return errAbort
	}); !errors.Is(err, errAbort) {
		t.Fatalf("an aborted update returned %v, want %v", err, errAbort)
	}

	want := []txnKeys{
		{size: true, other: true},
		{other: true},
		{existence: true, size: true, other: true},
	}
	if got := rec.takeTxns(); !slices.Equal(got, want) {
		t.Fatalf("takeTxns = %+v, want %+v", got, want)
	}

	if got := rec.takeTxns(); len(got) != 0 {
		t.Fatalf("takeTxns after a take = %+v, want none", got)
	}
}

func TestTakeAndTakeTxnsShareOneReset(t *testing.T) {
	rec := &txnRecord{
		recorded: []txnKeys{
			{existence: true, other: true},
			{existence: true},
			{other: true},
		},
	}

	if engine, mixed := rec.take(); engine != 2 || mixed != 1 {
		t.Fatalf("take = %d existence, %d mixed, want 2 and 1", engine, mixed)
	}

	if got := rec.takeTxns(); len(got) != 0 {
		t.Fatalf("takeTxns after take = %+v, want none: both read one record", got)
	}
}

// TestTxnRecordClassifiesTheStoresOwnKeys builds the store as the bench does,
// over a record around a real badger, and classifies what the store itself
// writes: a create writes a file record and an append's existence commit
// does too, and only an overwrite's existence commit writes an overwrite
// record. A drift of the store's key layout from existenceKey and sizeKey
// fails here, which TestKeyClassifiers' own key builders cannot show.
func TestTxnRecordClassifiesTheStoresOwnKeys(t *testing.T) {
	rec := &txnRecord{}
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{Dir: tb.TempDir()})
		if err != nil {
			tb.Fatalf("open the store: %v", err)
		}

		tb.Cleanup(func() {
			_ = db.Close()
		})

		rec.KV = db

		return rec
	})

	rec.takeTxns()

	_, made := fs.Create(t, fs.Admin, fs.Root, "f", kvstore.Regular(0o644))
	if got := countOf(rec.takeTxns()); got.size != 1 || got.existence != 0 {
		t.Fatalf(
			"create: %d size, %d existence transactions, want 1 and 0",
			got.size,
			got.existence,
		)
	}

	version := made.Version.Counter()
	commit := func(counter uint64) {
		t.Helper()

		op := kvstore.PendingOp{
			Version: entity.VersionOf(counter),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Length: 4096},
			Mtime:   time.Now(),
			Ctime:   time.Now(),
		}

		failed, err := fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{
			Share: fs.Share.ID,
			File:  made.ID,
			Ops:   []kvstore.PendingOp{op},
		}}, nil)
		if err != nil || failed[made.ID] != nil {
			t.Fatalf("commit existence: %v, %v", err, failed[made.ID])
		}
	}

	commit(version + 1)

	if got := countOf(rec.takeTxns()); got.size != 1 || got.existence != 0 {
		t.Fatalf(
			"append: %d size, %d existence transactions, want 1 and 0",
			got.size,
			got.existence,
		)
	}

	commit(version + 2)

	if got := countOf(rec.takeTxns()); got.size != 1 || got.existence != 1 {
		t.Fatalf(
			"overwrite: %d size, %d existence transactions, want 1 and 1",
			got.size,
			got.existence,
		)
	}
}

type txnCounts struct {
	size, existence int
}

func countOf(txns []txnKeys) txnCounts {
	var c txnCounts

	for _, t := range txns {
		if t.size {
			c.size++
		}

		if t.existence {
			c.existence++
		}
	}

	return c
}

func TestAppendBurstError(t *testing.T) {
	marks := txnKeys{other: true}
	size := txnKeys{size: true, other: true}

	for _, tc := range []struct {
		name    string
		txns    []txnKeys
		wantErr bool
	}{
		{"nothing committed", nil, false},
		{"one marks transaction", []txnKeys{marks}, false},
		{"two marks transactions", []txnKeys{marks, marks}, true},
		{"one grouped existence commit", []txnKeys{size}, true},
		{"a marks transaction and a grouped commit", []txnKeys{marks, size}, true},
		{"a commit per file", slices.Repeat([]txnKeys{size}, 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := appendBurstError(tc.txns)
			if (err != nil) != tc.wantErr {
				t.Fatalf("appendBurstError = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}
