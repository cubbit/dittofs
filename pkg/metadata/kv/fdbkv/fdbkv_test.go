//go:build linux && cgo

package fdbkv_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv/fdbkvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
)

// Two nodes over one prefix, one with its clock ahead.
func TestNowAcrossNodes(t *testing.T) {
	prefix, node := fdbkvtest.Prefix(), byte(0)
	kvtest.NowAcrossStores(t, func(t *testing.T, skew time.Duration) kv.KV {
		node++
		opts := fdbkv.Options{Prefix: prefix, Clock: func() time.Time { return time.Now().Add(skew) }}
		opts.NodeID[15] = node
		return fdbkvtest.Open(t, opts)
	})
}

func TestConformance(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.KV { return fdbkvtest.Open(t, fdbkv.Options{}) })
}

// A scan yields stored values only: the backend's own unstamped rows — the
// commit mark a committing Update writes at zero bytes, and the store time's
// eight-byte atomic maximum a transaction that asks Now raises — are
// skipped by their keys, so no scan whose range covers them errors on a
// row no API read can decode, while any other short row still does.
func TestScanSkipsTheBackendsOwnRows(t *testing.T) {
	s := fdbkvtest.Open(t, fdbkv.Options{})
	api := []byte{0x01, 'k'} // an API key: its kind byte is one the store writes
	if err := s.Update(context.Background(), func(tx kv.Txn) error {
		tx.Now() // the commit writes the mark and the store time's maximum
		return tx.Set(api, []byte("value"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(context.Background(), func(r kv.Reader) error {
		apiRows := 0
		for kvs, err := range r.Scan(nil, nil) {
			if err != nil {
				return fmt.Errorf("Scan(nil, nil): %w, want the backend's own rows skipped", err)
			}
			switch {
			case bytes.Equal(kvs.Key, api):
				apiRows++
			case bytes.HasPrefix(kvs.Key, keys.Commit) || bytes.HasPrefix(kvs.Key, keys.Time):
				t.Errorf("the whole-store scan yielded the backend's own key %x, want only stored values", kvs.Key)
			}
		}
		if apiRows != 1 {
			t.Errorf("the whole-store scan yielded the API key %x %d times, want exactly one", api, apiRows)
		}
		for _, prefix := range [][]byte{keys.Commit, keys.Time} {
			rows := 0
			for kvs, err := range r.Scan(prefix, nil) {
				if err != nil {
					t.Fatalf("Scan(%q): %v, want the backend's own rows skipped", prefix, err)
				}
				if !bytes.HasPrefix(kvs.Key, prefix) {
					t.Errorf("Scan(%q) yielded key %x outside the prefix", prefix, kvs.Key)
				}
				rows++
			}
			if rows != 0 {
				t.Errorf("Scan(%q) yielded %d rows, want none: the backend's own rows are skipped", prefix, rows)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
