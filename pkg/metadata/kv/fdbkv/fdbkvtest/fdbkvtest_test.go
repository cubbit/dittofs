//go:build foundationdb

package fdbkvtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/kv/fdbkv"
)

// Reading or clearing the cluster before any store opens starts the client
// without TLS, so a later Open naming TLS files is refused rather than run
// without them. No cluster is needed: the client takes the files' paths
// without opening them, and the refusal comes before any connection.
func TestDatabaseBeforeOpenFixesTheClientWithoutTLS(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "fdb.cluster")
	_, _ = database(cf) // no cluster file: only the client setup matters
	tls := fdbkv.TLS{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"), CAFile: filepath.Join(dir, "ca.pem")}
	for _, f := range []string{tls.CertFile, tls.KeyFile, tls.CAFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := fdbkv.Open(t.Context(), fdbkv.Options{ClusterFile: cf, TLS: tls})
	if err == nil {
		_ = s.Close()
		t.Fatal("fdbkv.Open with TLS files after fdbkvtest read the cluster without: nil error, want a refusal")
	}
	if want := "TLS files differ"; !strings.Contains(err.Error(), want) {
		t.Fatalf("fdbkv.Open with TLS files after fdbkvtest read the cluster without: %v, want an error naming %q", err, want)
	}
}
