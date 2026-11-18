package bootstrap

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/google/uuid"
)

// nodeUUID is the node identity the decoding tests write, in RFC 13
// Appendix C's form.
const nodeUUID = "6f1c2d3e-4b5a-4978-8a6b-1c2d3e4f5a6b"

// The file's node.id, a UUID, reaches Bootstrap.Node.ID; a node.id that is
// not a UUID refuses the load; a file with none leaves the identity zero,
// since the journals' node-id copies give it.
func TestLoadDecodesNodeID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    entity.NodeID
		fails   bool
	}{
		{"uuid", "node:\n  id: " + nodeUUID + "\n", entity.NodeID(uuid.MustParse(nodeUUID)), false},
		{"not a uuid", "node:\n  id: lixo\n", entity.NodeID{}, true},
		{"absent", "logging:\n  format: text\n", entity.NodeID{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, tc.content), os.Environ())

			if tc.fails {
				want := "failed to unmarshal config: " +
					"decoding failed due to the following error(s):" +
					"\n\n'node.id' node.id is not a UUID"
				if err == nil || err.Error() != want {
					t.Fatalf("Load of node.id %q: got %v, want %q", "lixo", err, want)
				}

				if strings.Contains(err.Error(), "lixo") {
					t.Errorf(
						"the refusal %q prints the value, which may come from the environment",
						err,
					)
				}

				return
			}

			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if cfg.Node.ID != tc.want {
				t.Errorf("Node.ID after Load: got %x, want %x", cfg.Node.ID, tc.want)
			}
		})
	}
}

// A KMIP timeout written as a duration decodes: the decode hook keeps
// viper's own duration hook beside the node identity's.
func TestLoadDecodesKMIPTimeout(t *testing.T) {
	path := writeConfigFile(t, "keys:\n  protocol:\n    kmip:\n      timeout: \"5s\"\n")

	cfg, err := Load(path, os.Environ())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Keys.Protocol.KMIP.Timeout; got != 5*time.Second {
		t.Errorf("keys.protocol.kmip.timeout: got %v, want 5s", got)
	}
}

// The file's journal entries and the environment's DITTOFS_JOURNALS give the
// same entries, and the environment's list replaces the file's.
func TestLoadDecodesJournals(t *testing.T) {
	want := []Journal{{Path: "/a"}, {Path: "/b"}}

	for _, tc := range []struct {
		name    string
		content string
		env     string
	}{
		{"file", "journals:\n  - path: /a\n  - path: /b\n", ""},
		{"environment", "logging:\n  format: text\n", "/a,/b"},
		{"environment over file", "journals:\n  - path: /f\n", "/a,/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("DITTOFS_JOURNALS", tc.env)
			}

			cfg, err := Load(writeConfigFile(t, tc.content), os.Environ())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if !reflect.DeepEqual(cfg.Journals, want) {
				t.Errorf("journals: got %+v, want %+v", cfg.Journals, want)
			}
		})
	}
}

// api.listen holds more than one address.
func TestLoadDecodesTwoAPIAddresses(t *testing.T) {
	path := writeConfigFile(t, "api:\n  listen: [\"127.0.0.1:8080\", \"[::1]:8080\"]\n")

	cfg, err := Load(path, os.Environ())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"127.0.0.1:8080", "[::1]:8080"}
	if !reflect.DeepEqual(cfg.API.Listen, want) {
		t.Errorf("api.listen: got %q, want %q", cfg.API.Listen, want)
	}
}

// A role's retired key files decode into the per-role struct.
func TestLoadDecodesRetiredFiles(t *testing.T) {
	path := writeConfigFile(
		t,
		"keys:\n  storage:\n    local:\n      retired_files: [/k/old1, /k/old2]\n",
	)

	cfg, err := Load(path, os.Environ())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"/k/old1", "/k/old2"}
	if got := cfg.Keys.Storage.Local.RetiredFiles; !reflect.DeepEqual(got, want) {
		t.Errorf("keys.storage.local.retired_files: got %q, want %q", got, want)
	}
}

// Provider carries every field of both blocks into keyprovider.Config, the
// timeout as milliseconds.
func TestWrappingKeyProvider(t *testing.T) {
	var k WrappingKey

	k.Local.File = "/k/file"
	k.Local.RetiredFiles = []string{"/k/old"}
	k.KMIP.Endpoint = "kmip.example:5696"
	k.KMIP.ServerCA = "/k/server-ca"
	k.KMIP.ClientCert = "/k/client-cert"
	k.KMIP.ClientKey = "/k/client-key"
	k.KMIP.KeyUID = "uid-new"
	k.KMIP.Timeout = 1500 * time.Millisecond
	k.KMIP.RetiredKeyUIDs = []string{"uid-old"}

	for _, kind := range []keyprovider.Kind{keyprovider.KindLocal, keyprovider.KindKMIP} {
		t.Run(string(kind), func(t *testing.T) {
			k.Kind = kind

			got := k.Provider()

			want := keyprovider.Config{
				Kind:           kind,
				File:           "/k/file",
				RetiredFiles:   []string{"/k/old"},
				Endpoint:       "kmip.example:5696",
				ServerCA:       "/k/server-ca",
				ClientCert:     "/k/client-cert",
				ClientKey:      "/k/client-key",
				KeyUID:         "uid-new",
				TimeoutMS:      1500,
				RetiredKeyUIDs: []string{"uid-old"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Provider(): got %+v, want %+v", got, want)
			}
		})
	}
}

// Every key has its own environment name: two keys on one name would let one
// silently set the other.
func TestBootstrapEnvNamesAreDistinct(t *testing.T) {
	keys := configEnvKeys()

	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		name := "DITTOFS_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		if other, ok := seen[name]; ok {
			t.Errorf("keys %s and %s share the environment name %s", other, key, name)
		}

		seen[name] = key
	}

	if len(keys) != 43 {
		t.Errorf(
			"bootstrap keys: got %d, want 43 (RFC 13 Appendix C's 42 and blockstore.journal.path)",
			len(keys),
		)
	}
}
