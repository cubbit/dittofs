package kvstore

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
)

// settingCtx is the context the settings tests call the store under.
var settingCtx = context.Background()

// settingInst is the installation scope every settings test writes at.
var settingInst = entity.SettingScope{Kind: entity.ScopeInstallation}

// settingStore is a store over its own Badger directory, its KV beside it
// for reads a test makes beneath the store, such as the settings generation.
func settingStore(t *testing.T) (*Store, kv.KV) {
	t.Helper()

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := Init(settingCtx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, err := Open(settingCtx, db)
	if err != nil {
		t.Fatal(err)
	}

	return s, db
}

// settingPut puts one setting under expect and fails the test on its error.
func settingPut(
	t *testing.T,
	s *Store,
	scope entity.SettingScope,
	key string,
	expect uint64,
) uint64 {
	t.Helper()

	g, err := s.ControlPlane().PutSetting(
		settingCtx,
		entity.Setting{Scope: scope, Key: key, Value: []byte(key)},
		expect,
	)
	if err != nil {
		t.Fatalf("put %s under %d: %v", key, expect, err)
	}

	return g
}

// cfgGen reads the settings generation, the counter every settings write
// raises, so a test asserts its exact value.
func cfgGen(t *testing.T, db kv.KV) uint64 {
	t.Helper()

	v, err := counter(settingCtx, db, cfgGenKey)
	if err != nil {
		t.Fatal(err)
	}

	return v
}
