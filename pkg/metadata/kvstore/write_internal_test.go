package kvstore

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// An idempotency record is written with its one expiry-index row, both
// naming the request's store time plus 24 hours.
func TestIdempotencyRecordAndExpiryTogether(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	i := &entity.IdempotencyRecord{Account: entity.NewUser(), Key: "k", Digest: [32]byte{1}}
	var now time.Time
	if err := s.ControlPlane().Write(ctx, entity.WriteMeta{Idempotency: i}, func(tx entity.ControlPlaneTx) error {
		now = tx.Now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(24 * time.Hour); !i.Expires.Equal(want) {
		t.Errorf("record's expiry: got %v, want %v", i.Expires, want)
	}
	if err := s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(idempotencyKey(i.Account, i.Key))
		if err != nil {
			t.Fatalf("idempotency record: %v, want written", err)
		}
		if rec, err := decodeIdempotency(i.Account, i.Key, b); err != nil || !rec.Expires.Equal(i.Expires) {
			t.Errorf("stored record's expiry: got %v, %v; want %v", rec.Expires, err, i.Expires)
		}
		var rows [][]byte
		for row, err := range r.Scan([]byte{keys.KindXP}, nil) {
			if err != nil {
				return err
			}
			rows = append(rows, append([]byte(nil), row.Key...))
		}
		if want := idempotencyExpiryKey(*i); len(rows) != 1 || string(rows[0]) != string(want) {
			t.Errorf("expiry rows: got %x, want only %x", rows, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// An index row whose record retention has deleted is skipped: a read by
// actor lists the records still kept and fails on none.
func TestAuditIndexRowWithoutRecordSkipped(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	cp := s.ControlPlane()
	alice := entity.NewUser()
	var written []entity.AuditRecord
	for _, src := range []string{"cut", "kept"} {
		a := &entity.AuditRecord{Actor: alice, Source: src, Target: entity.ResourceRef{Kind: entity.ResourceUser}}
		if err := cp.Write(ctx, entity.WriteMeta{Audit: a}, func(entity.ControlPlaneTx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		written = append(written, *a)
	}
	if err := s.kv.Update(ctx, func(tx kv.Txn) error { return tx.Delete(auditKey(written[0].Time, written[0].ID)) }); err != nil {
		t.Fatal(err)
	}
	var got []string
	for a, err := range cp.AuditRecords(ctx, entity.AuditFilter{Actor: &alice}, nil) {
		if err != nil {
			t.Fatalf("audit by actor over a deleted record's index row: %v, want it skipped", err)
		}
		got = append(got, a.Source)
	}
	if len(got) != 1 || got[0] != "kept" {
		t.Errorf("audit by actor: got %q, want only %q", got, "kept")
	}
}
