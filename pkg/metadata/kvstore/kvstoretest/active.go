package kvstoretest

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// WriteActive sets the installation record's active versions to active, as
// raw bytes through db. No call writes them, since only the control plane's
// gate raises a version (RFC 13 §5.5), so a test raises one by writing the
// record. Unlike the suite's cases, it writes a key directly.
func WriteActive(tb testing.TB, db kv.KV, active map[entity.Format]uint32) {
	tb.Helper()

	w := codec.NewWriter(0).Len(len(active))
	for _, f := range slices.Sorted(maps.Keys(active)) {
		w.Uint8(uint8(f)).Uint32(active[f])
	}

	WriteActiveRaw(tb, db, w.Encode()[1:])
}

// WriteActiveRaw replaces the installation record's active versions with
// raw, which need not decode. The record must hold none yet, as Init writes
// it: the active versions are its last field, and their empty count its last
// byte.
func WriteActiveRaw(tb testing.TB, db kv.KV, raw []byte) {
	tb.Helper()

	key := []byte{keys.KindIN}

	err := db.Update(context.Background(), func(tx kv.Txn) error {
		b, err := tx.Get(key)
		if err != nil {
			return err
		}

		if len(b) == 0 || b[len(b)-1] != 0 {
			return errors.New("the installation record holds active versions already")
		}

		return tx.Set(key, append(slices.Clip(b[:len(b)-1]), raw...))
	})
	if err != nil {
		tb.Fatal(err)
	}
}
