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
