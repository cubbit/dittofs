package kvstore

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// expiryRows is every row of the expiry index.
func expiryRows(t *testing.T, s *Store) []string {
	t.Helper()
	var rows []string
	if err := s.kv.View(context.Background(), func(r kv.Reader) error {
		for row, err := range r.Scan([]byte{keys.KindXP}, nil) {
			if err != nil {
				return err
			}
			rows = append(rows, string(row.Key))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// A running operation has no expiry-index row; the write that ends it
// writes one, 7 days after that write's store time.
func TestOperationExpiryRow(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	op := entity.Operation{ID: entity.OperationID{15: 1}, Kind: "share.delete",
		Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte{1}}, State: entity.OpRunning}
	gen, err := cp.PutOperation(ctx, op, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows := expiryRows(t, s); len(rows) != 0 {
		t.Errorf("expiry rows of a running operation: got %x, want none", rows)
	}
	op.State = entity.OpSucceeded
	if _, err := cp.PutOperation(ctx, op, gen); err != nil {
		t.Fatal(err)
	}
	ended, _, err := cp.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := operationExpiryKey(entity.Operation{ID: op.ID, Updated: ended.Updated})
	if rows := expiryRows(t, s); len(rows) != 1 || rows[0] != string(want) {
		t.Errorf("expiry rows of an operation ended at %v: got %x, want only %x", ended.Updated, rows, want)
	}
}
