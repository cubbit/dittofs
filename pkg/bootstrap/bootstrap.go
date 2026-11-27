package bootstrap

import (
	"time"

	"github.com/cubbit/dittofs/pkg/keyprovider"
)

// Journal is one journal device: the directory its journal lives in.
type Journal struct {
	Path string `mapstructure:"path" yaml:"path" jsonschema:"required"`
}

// WrappingKey is one role's wrapping key, under the keys RFC 13 Appendix C
// gives it, nested by provider. It is not keyprovider.Config, whose flat JSON
// shape is the stored per-remote encryption policy's.
type WrappingKey struct {
	Kind  keyprovider.Kind `mapstructure:"kind"  yaml:"kind"  jsonschema:"required,enum=local,enum=kmip"` //nolint:lll // a struct tag is one line
	Local struct {
		File         string   `mapstructure:"file"          yaml:"file"`
		RetiredFiles []string `mapstructure:"retired_files" yaml:"retired_files"`
	} `mapstructure:"local" yaml:"local"`
	KMIP struct {
		Endpoint       string        `mapstructure:"endpoint"         yaml:"endpoint"`
		ServerCA       string        `mapstructure:"server_ca"        yaml:"server_ca"`
		ClientCert     string        `mapstructure:"client_cert"      yaml:"client_cert"`
		ClientKey      string        `mapstructure:"client_key"       yaml:"client_key"`
		KeyUID         string        `mapstructure:"key_uid"          yaml:"key_uid"`
		Timeout        time.Duration `mapstructure:"timeout"          yaml:"timeout"`
		RetiredKeyUIDs []string      `mapstructure:"retired_key_uids" yaml:"retired_key_uids"`
	} `mapstructure:"kmip"  yaml:"kmip"`
}

// Provider converts to the keyprovider.Config NewProvider takes, with
// Timeout as TimeoutMS milliseconds.
//
// Both blocks are copied whatever Kind names: NewProvider reads only its
// Kind's fields, and a key set in the other block stays visible to the
// check that refuses it.
func (k WrappingKey) Provider() keyprovider.Config {
	return keyprovider.Config{
		Kind:           k.Kind,
		File:           k.Local.File,
		RetiredFiles:   k.Local.RetiredFiles,
		Endpoint:       k.KMIP.Endpoint,
		ServerCA:       k.KMIP.ServerCA,
		ClientCert:     k.KMIP.ClientCert,
		ClientKey:      k.KMIP.ClientKey,
		KeyUID:         k.KMIP.KeyUID,
		TimeoutMS:      int(k.KMIP.Timeout / time.Millisecond),
		RetiredKeyUIDs: k.KMIP.RetiredKeyUIDs,
	}
}
