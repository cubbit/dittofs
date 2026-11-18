package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
)

// nodeUUID is the node identity these tests show, in RFC 13 Appendix C's form.
const nodeUUID = "6f1c2d3e-4b5a-4978-8a6b-1c2d3e4f5a6b"

// show prints node.id as the UUID RFC 13 Appendix C writes, not as the 16
// numbers of its array, in YAML and in JSON.
func TestConfigShowPrintsNodeIDAsUUID(t *testing.T) {
	cfg := bootstrap.GetDefaultConfig()
	cfg.Node.ID = entity.NodeID(uuid.MustParse(nodeUUID))

	for _, tc := range []struct {
		format output.Format
		want   string
	}{
		{output.FormatYAML, "\nnode:\n  id: " + nodeUUID + "\n"},
		{output.FormatJSON, "\"id\": \"" + nodeUUID + "\""},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			var out bytes.Buffer

			if err := printConfig(&out, cfg, tc.format); err != nil {
				t.Fatalf("printConfig: %v", err)
			}

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf(
					"show in %s: node.id not printed as %q; output:\n%s",
					tc.format,
					tc.want,
					out.String(),
				)
			}
		})
	}
}

// What show prints of a Bootstrap with every key set, in YAML and in JSON,
// Load reads back to the same Bootstrap: a key show spells differently from
// the one Load reads fails here.
func TestConfigShowRoundTripsThroughLoad(t *testing.T) {
	cfg := filledBootstrap()

	for _, tc := range []struct {
		format output.Format
		file   string
	}{
		{output.FormatYAML, "config.yaml"},
		{output.FormatJSON, "config.json"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			var out bytes.Buffer

			if err := printConfig(&out, cfg, tc.format); err != nil {
				t.Fatalf("printConfig: %v", err)
			}

			path := filepath.Join(t.TempDir(), tc.file)
			if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
				t.Fatalf("write the shown config: %v", err)
			}

			reloaded, err := bootstrap.Load(path, os.Environ())
			if err != nil {
				t.Fatalf("Load of the shown %s: %v\noutput:\n%s", tc.format, err, out.String())
			}

			if !reflect.DeepEqual(reloaded, cfg) {
				t.Errorf(
					"Load of the shown %s: got %+v, want %+v\noutput:\n%s",
					tc.format,
					reloaded,
					cfg,
					out.String(),
				)
			}
		})
	}
}

// filledBootstrap is the default Bootstrap with every RFC 13 Appendix C key
// set to a value of its own.
func filledBootstrap() *bootstrap.Bootstrap {
	b := bootstrap.GetDefaultConfig()

	b.Node.ID = entity.NodeID(uuid.MustParse(nodeUUID))
	b.Node.Roles = []string{"protocol", "storage"}
	b.Metadata.Backend = "foundationdb"
	b.Metadata.Badger.Path = "/m/badger"
	b.Metadata.FoundationDB.ClusterFile = "/m/fdb.cluster"
	b.Metadata.FoundationDB.TLS.CertFile = "/m/fdb.crt"
	b.Metadata.FoundationDB.TLS.KeyFile = "/m/fdb.key"
	b.Metadata.FoundationDB.TLS.CAFile = "/m/fdb-ca.crt"
	b.Journals = []bootstrap.Journal{{Path: "/j/1"}, {Path: "/j/2"}}
	b.Adapters.Listen = []string{"127.0.0.1", "::1"}
	b.API.Listen = []string{"127.0.0.1:8080", "[::1]:8080"}
	b.API.TLS.CertFile = "/a/api.crt"
	b.API.TLS.KeyFile = "/a/api.key"
	b.API.Socket = "/run/dittofs/dfs.sock"
	b.Metrics.Listen = "127.0.0.1:9090"
	b.Keys.Protocol = wrappingKey(keyprovider.KindLocal, "p")
	b.Keys.Storage = wrappingKey(keyprovider.KindKMIP, "s")

	return b
}

// wrappingKey is a wrapping key with every field of both blocks set, each
// value prefixed by p.
func wrappingKey(kind keyprovider.Kind, p string) bootstrap.WrappingKey {
	var k bootstrap.WrappingKey

	k.Kind = kind
	k.Local.File = "/" + p + "/file"
	k.Local.RetiredFiles = []string{"/" + p + "/old"}
	k.KMIP.Endpoint = p + ".kmip.example:5696"
	k.KMIP.ServerCA = "/" + p + "/server-ca"
	k.KMIP.ClientCert = "/" + p + "/client-cert"
	k.KMIP.ClientKey = "/" + p + "/client-key"
	k.KMIP.KeyUID = p + "-uid"
	k.KMIP.Timeout = 5 * time.Second
	k.KMIP.RetiredKeyUIDs = []string{p + "-old-uid"}

	return k
}
