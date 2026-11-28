package kvstore_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/transform/keystest"
)

// storeOn opens a store over the backend open builds and returns it, and
// the backend beneath it.
func storeOn(t *testing.T, open kvstoretest.Opener) (*kvstore.Store, kv.KV) {
	t.Helper()

	db := open(t)
	if err := kvstore.Init(t.Context(), db, uuid.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	s, err := kvstore.Open(t.Context(), db)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return s, db
}

// keyRecordsOn opens a store over the backend open builds and returns its
// key-record view, and the backend beneath it.
func keyRecordsOn(t *testing.T, open kvstoretest.Opener) (kvstore.KeyRecords, kv.KV) {
	t.Helper()

	s, db := storeOn(t, open)

	return s.KeyRecords(), db
}

// runKeyRecords runs the view suite on the store's view, each view a fresh
// one from the store, and each namespace created as the control plane
// creates one, its record and its key in the store index together.
func runKeyRecords(t *testing.T, open kvstoretest.Opener) {
	keystest.RunKeyRecords(t, func(t *testing.T) keystest.RecordStore[kvstore.KeyRecords] {
		s, _ := storeOn(t, open)

		return keystest.RecordStore[kvstore.KeyRecords]{
			View: s.KeyRecords,
			AddNamespace: func(ctx context.Context, ns entity.NamespaceID, store [16]byte) error {
				_, err := s.ControlPlane().PutNamespace(ctx, entity.Namespace{ID: ns, Store: store}, 0)
				return err
			},
		}
	})
}

// The store's key-record view passes the view suite on Badger.
func TestKeyRecordsConformance(t *testing.T) {
	prof.Watch(t, prof.Options{})
	runKeyRecords(t, badger(badgerkv.Options{}))
}

type keyRow = struct {
	Kind, ID, State string
	Record          []byte
}

// keyRecordKey is NS‖ns‖key‖kind‖keyID as RFC 16 §4.2 and §4.2.1 lay it out,
// built here rather than by the store so a layout the store got wrong is seen.
func keyRecordKey(ns entity.NamespaceID, kind byte, id string) []byte {
	return keys.String(append(keys.ID([]byte{keys.KindNS}, ns[:]), keys.NSKey, kind), []byte(id))
}

func set(t *testing.T, db kv.KV, key, value []byte) {
	t.Helper()

	if err := db.Update(t.Context(), func(tx kv.Txn) error { return tx.Set(key, value) }); err != nil {
		t.Fatalf("Set(%s): %v", keys.Describe(key), err)
	}
}

// A record's state is the byte after the value's version, apart from the
// record, so a commit reads a destroyed state without the provider's codec;
// and each kind is stored as RFC 16 §4.2.1's byte.
func TestKeyRecordStateIsReadableWithoutTheRecord(t *testing.T) {
	prof.Watch(t, prof.Options{})

	v, db := keyRecordsOn(t, badger(badgerkv.Options{}))
	ns := entity.NamespaceID{15: 7}
	// A record no codec decodes, and holding a zero byte.
	opaque := []byte{0xff, 0x00, 0x01}

	err := v.Update(t.Context(), ns, func(seq uint64, _ []keyRow) (uint64, []keyRow, error) {
		return seq, []keyRow{
			{"data-key", "d\x00", "destroyed", opaque},
			{"chunk-id-key", "c", "current", nil},
			{"chunking-key", "k", "retired", nil},
			{"export-key", "e", "retiring", nil},
		}, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, c := range []struct {
		kind  byte
		id    string
		value []byte
	}{
		{keys.KeyData, "d\x00", append([]byte{1, 0x04}, opaque...)},
		{keys.KeyChunkID, "c", []byte{1, 0x01}},
		{keys.KeyChunking, "k", []byte{1, 0x03}},
		{keys.KeyExport, "e", []byte{1, 0x02}},
	} {
		key := keyRecordKey(ns, c.kind, c.id)

		var got []byte

		err := db.View(t.Context(), func(r kv.Reader) error {
			var err error
			got, err = r.Get(key)
			return err
		})
		if err != nil {
			t.Fatalf("Get(%s): %v", keys.Describe(key), err)
		}

		if !bytes.Equal(got, c.value) {
			t.Errorf("value at %s = %x, want %x", keys.Describe(key), got, c.value)
		}
	}
}

// Load refuses a stored record of an unknown value version, kind byte or
// state byte, rather than read it as some other record.
func TestKeyRecordsRefuseUnknownStoredValues(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		name       string
		key, value []byte
	}{
		{"an unknown value version", keyRecordKey(entity.NamespaceID{15: 1}, keys.KeyData, "d"), []byte{2, 0x01}},
		{"an unknown kind byte", keyRecordKey(entity.NamespaceID{15: 1}, 0x03, "h"), []byte{1, 0x01}},
		{"an unknown state byte", keyRecordKey(entity.NamespaceID{15: 1}, keys.KeyData, "d"), []byte{1, 0x05}},
		{"a value without its state", keyRecordKey(entity.NamespaceID{15: 1}, keys.KeyData, "d"), []byte{1}},
		{"an ID without its terminator", append(keyRecordKey(entity.NamespaceID{15: 1}, keys.KeyData, "")[:19], 'd'), []byte{1, 0x01}},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, db := keyRecordsOn(t, badger(badgerkv.Options{}))
			set(t, db, c.key, c.value)

			if rows, err := v.Load(t.Context(), entity.NamespaceID{15: 1}); err == nil {
				t.Errorf("Load over %s = %q, want an error", c.name, rows)
			}

			err := v.Update(t.Context(), entity.NamespaceID{15: 1}, func(seq uint64, rows []keyRow) (uint64, []keyRow, error) {
				t.Errorf("Update over %s ran fn on %q, want it refused before", c.name, rows)
				return seq, nil, nil
			})
			if err == nil {
				t.Errorf("Update over %s succeeded, want an error", c.name)
			}
		})
	}
}
