package bootstrap

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// nodeUUID is the node identity the decoding tests write, in RFC 13
// Appendix C's form.
const nodeUUID = "6f1c2d3e-4b5a-4978-8a6b-1c2d3e4f5a6b"

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

// Every key of the table is a field of Bootstrap and the reverse, and every
// key has its own environment name: two keys on one name would let one
// silently set the other.
func TestKeySpecsMatchBootstrap(t *testing.T) {
	t.Parallel()

	var fields []string
	collectKeys(reflect.TypeFor[Bootstrap](), "", &fields)

	var keys []string

	seen := map[string]string{}

	for _, s := range keySpecs {
		keys = append(keys, s.key)

		if other, ok := seen[s.envName()]; ok {
			t.Errorf("keys %s and %s share the environment name %s", other, s.key, s.envName())
		}

		seen[s.envName()] = s.key
	}

	slices.Sort(fields)
	slices.Sort(keys)

	if !slices.Equal(fields, keys) {
		t.Errorf("key table:\ngot  %q\nwant %q (Bootstrap's fields)", keys, fields)
	}

	if len(keys) != 42 {
		t.Errorf("bootstrap keys: got %d, want 42 (RFC 13 Appendix C)", len(keys))
	}
}

// collectKeys appends the dotted mapstructure key of every leaf field of t.
func collectKeys(t reflect.Type, prefix string, out *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		key := joinKey(prefix, f.Tag.Get("mapstructure"))

		if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeFor[entity.NodeID]() &&
			f.Type != reflect.TypeFor[time.Duration]() {
			collectKeys(f.Type, key, out)

			continue
		}

		*out = append(*out, key)
	}
}
