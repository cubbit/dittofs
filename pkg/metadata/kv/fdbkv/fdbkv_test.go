//go:build foundationdb

package fdbkv_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
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

// tlsFiles writes a certificate, a key and a CA file under dir, as paths;
// the client is never asked to read them.
func tlsFiles(t *testing.T) fdbkv.TLS {
	dir := t.TempDir()
	tls := fdbkv.TLS{CertFile: dir + "/cert.pem", KeyFile: dir + "/key.pem", CAFile: dir + "/ca.pem"}
	for _, f := range []string{tls.CertFile, tls.KeyFile, tls.CAFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return tls
}

// A missing TLS file is refused, named, before the client is touched, so no
// cluster is needed: the client itself takes any path and fails only on
// connecting.
func TestOpenRefusesAMissingTLSFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(*fdbkv.TLS) *string
	}{
		{"cert_file", func(tls *fdbkv.TLS) *string { return &tls.CertFile }},
		{"key_file", func(tls *fdbkv.TLS) *string { return &tls.KeyFile }},
		{"ca_file", func(tls *fdbkv.TLS) *string { return &tls.CAFile }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tls := tlsFiles(t)
			missing := tc.path(&tls)
			*missing += ".missing"
			s, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: t.TempDir() + "/fdb.cluster", TLS: tls})
			if err == nil {
				_ = s.Close()
				t.Fatalf("Open with a missing %s: nil error, want one naming %s", tc.name, *missing)
			}
			if !strings.Contains(err.Error(), *missing) {
				t.Fatalf("Open with a missing %s: %v, want an error naming %s", tc.name, err, *missing)
			}
		})
	}
}

// The client reads its TLS files once, when its network starts, and ignores
// any given later, so an Open naming other files than the process's first is
// refused rather than connected with the first's. No cluster is needed: the
// first Open gives the client its files before it finds no cluster file.
func TestOpenRefusesOtherTLSFiles(t *testing.T) {
	if s, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: t.TempDir() + "/fdb.cluster"}); err == nil {
		_ = s.Close()
		t.Fatal("Open on a missing cluster file: nil error, want the client's refusal")
	}
	s, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: t.TempDir() + "/fdb.cluster", TLS: tlsFiles(t)})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open with TLS files after one without: nil error, want a refusal")
	}
	if want := "TLS files differ"; !strings.Contains(err.Error(), want) {
		t.Fatalf("Open with TLS files after one without: %v, want an error naming %q", err, want)
	}
}
