package bootstrap

import (
	"errors"
	"reflect"
	"time"

	"github.com/cubbit/dittofs/pkg/keyprovider"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/go-viper/mapstructure/v2"
	"github.com/google/uuid"
)

// Journal is one journal device: the directory its journal lives in.
type Journal struct {
	Path string `mapstructure:"path" yaml:"path"`
}

// WrappingKey is one role's wrapping key, under the keys RFC 13 Appendix C
// gives it, nested by provider. It is not keyprovider.Config, whose flat JSON
// shape is the stored per-remote encryption policy's.
type WrappingKey struct {
	Kind  keyprovider.Kind `mapstructure:"kind"  yaml:"kind"`
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

// decodeHook is Load's decode hook: a UUID string into entity.NodeID, and a
// path string into a Journal, composed with viper's own default hooks, a
// duration and a comma-separated slice, which a hook of Load's own would
// otherwise replace. With the slice hook, DITTOFS_JOURNALS=/a,/b becomes one
// Journal per path.
func decodeHook() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		stringToNodeID,
		stringToJournal,
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToWeakSliceHookFunc(","),
	)
}

// stringToNodeID parses a node.id written as a UUID.
func stringToNodeID(from, to reflect.Type, data any) (any, error) {
	if from.Kind() != reflect.String || to != reflect.TypeFor[entity.NodeID]() {
		return data, nil
	}

	// uuid's own error is left out: one form of it quotes part of the value,
	// which may come from the environment.
	id, err := uuid.Parse(data.(string))
	if err != nil {
		return nil, errors.New("node.id is not a UUID")
	}

	return entity.NodeID(id), nil
}

// stringToJournal reads a journal given as its path alone, the form each
// entry of DITTOFS_JOURNALS takes.
func stringToJournal(from, to reflect.Type, data any) (any, error) {
	if from.Kind() != reflect.String || to != reflect.TypeFor[Journal]() {
		return data, nil
	}

	return Journal{Path: data.(string)}, nil
}
