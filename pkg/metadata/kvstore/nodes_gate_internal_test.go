package kvstore

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
)

// raiseOnRead wraps a KV so that the first read of key inside an Update
// runs raise, between the transaction's read and its commit.
type raiseOnRead struct {
	kv.KV
	key   []byte
	raise func()
	fired int
}

func (r *raiseOnRead) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return r.KV.Update(ctx, func(tx kv.Txn) error {
		return fn(raiseTxn{Txn: tx, r: r})
	})
}

type raiseTxn struct {
	kv.Txn
	r *raiseOnRead
}

func (t raiseTxn) Get(key []byte) ([]byte, error) {
	b, err := t.Txn.Get(key)
	if bytes.Equal(key, t.r.key) && t.r.fired == 0 {
		t.r.fired++
		t.r.raise()
	}

	return b, err
}

// An active version raised between the registration's read of the
// installation and its commit is seen by the registration's retry, which
// refuses it: the comparison and the record are one transaction.
func TestRegisterNodeSeesConcurrentRaise(t *testing.T) {
	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	hook := &raiseOnRead{KV: db, key: installationKey}
	hook.raise = func() {
		err := db.Update(ctx, func(tx kv.Txn) error {
			b, err := tx.Get(installationKey)
			if err != nil {
				return err
			}

			in, err := decodeInstallation(b)
			if err != nil {
				return err
			}

			in.Active = map[entity.Format]uint32{entity.FormatJournal: 2}

			return tx.Set(installationKey, encodeInstallation(in))
		})
		if err != nil {
			t.Error(err)
		}
	}

	s, err := Open(ctx, hook)
	if err != nil {
		t.Fatal(err)
	}

	node := entity.Node{
		ID:      entity.NodeID{15: 1},
		Address: "10.0.0.1:12049",
		Roles:   entity.NodeRoleProtocol | entity.NodeRoleStorage,
		Formats: map[entity.Format]entity.VersionRange{entity.FormatJournal: {Min: 1, Max: 1}},
	}

	_, err = s.Nodes().RegisterNode(ctx, node)

	want := "journal format 2 is active, this node reads 1 to 1"
	if err == nil || err.Error() != want {
		t.Errorf("registration under a concurrent raise: error %v, want %q", err, want)
	}

	if hook.fired != 1 {
		t.Errorf("raise fired %d times, want 1", hook.fired)
	}

	rec, found, err := s.Nodes().Node(ctx, node.ID)
	if err != nil || found {
		t.Errorf("node record after the refusal: %+v, found %v, %v; want none", rec, found, err)
	}

	if !reflect.DeepEqual(rec, entity.Node{ID: node.ID}) {
		t.Errorf("node record after the refusal: %+v, want only its ID", rec)
	}
}

// openActive initialises and opens a store on Badger under a test directory,
// its installation recording active.
func openActive(t *testing.T, active map[entity.Format]uint32) *Store {
	t.Helper()

	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	err = db.Update(ctx, func(tx kv.Txn) error {
		b, err := tx.Get(installationKey)
		if err != nil {
			return err
		}

		in, err := decodeInstallation(b)
		if err != nil {
			return err
		}

		in.Active = active

		return tx.Set(installationKey, encodeInstallation(in))
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// A registration with no ranges is refused once any active version is
// recorded, naming the format, and writes no record.
func TestRegisterNodeRefusesActiveWithoutRanges(t *testing.T) {
	ctx := context.Background()
	s := openActive(t, map[entity.Format]uint32{entity.FormatJournal: 1})

	node := entity.Node{
		ID:    entity.NodeID{15: 2},
		Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage,
	}

	_, err := s.Nodes().RegisterNode(ctx, node)

	want := "format journal is active at version 1, this node registers no range for it"
	if err == nil || err.Error() != want {
		t.Errorf("registration with no ranges: error %v, want %q", err, want)
	}

	if _, found, err := s.Nodes().Node(ctx, node.ID); err != nil || found {
		t.Errorf("node record after the refusal: found %v, %v; want none", found, err)
	}
}

// With two active versions out of range the refusal names the lower format,
// so its text does not depend on map order.
func TestRegisterNodeNamesLowerFormatFirst(t *testing.T) {
	s := openActive(t, map[entity.Format]uint32{entity.FormatJournal: 3, entity.FormatBlock: 5})

	node := entity.Node{
		ID:    entity.NodeID{15: 3},
		Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage,
		Formats: map[entity.Format]entity.VersionRange{
			entity.FormatJournal: {Min: 1, Max: 1},
			entity.FormatBlock:   {Min: 1, Max: 1},
		},
	}

	want := "journal format 3 is active, this node reads 1 to 1"

	for range 20 {
		_, err := s.Nodes().RegisterNode(context.Background(), node)
		if err == nil || err.Error() != want {
			t.Fatalf("registration with two formats out of range: error %v, want %q", err, want)
		}
	}
}
