package kvstore

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/settings"
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

// settingDelete deletes one setting under expect, failing the test on an
// error it does not allow.
func settingDelete(t *testing.T, s *Store, scope entity.SettingScope, key string, expect uint64) {
	t.Helper()

	if err := s.ControlPlane().DeleteSetting(settingCtx, scope, key, expect); err != nil {
		t.Fatalf("delete %s at %v under %d: %v", key, scope.Kind, expect, err)
	}
}

// settingGen reads one stored setting's generation, zero when its row is
// gone.
func settingGen(t *testing.T, s *Store, scope entity.SettingScope, key string) uint64 {
	t.Helper()

	for got, err := range s.ControlPlane().Settings(settingCtx, scope) {
		if err != nil {
			t.Fatalf("list settings at %v: %v", scope.Kind, err)
		}

		if got.Key == key {
			return got.Generation
		}
	}

	return 0
}

// A delete raises the settings generation in its own transaction, once for
// the one row it removes: a delete that moves no generation is a delete no
// node ever re-reads, so every node keeps serving the override it removed.
func TestDeleteSettingRaisesGeneration(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)

	if g := settingPut(t, s, settingInst, "k", 0); g != 1 {
		t.Errorf("first generation = %d, want 1", g)
	}

	if got := cfgGen(t, db); got != 1 {
		t.Errorf("generation after the first put = %d, want 1", got)
	}

	settingDelete(t, s, settingInst, "k", 1)

	if got := cfgGen(t, db); got != 2 {
		t.Errorf("generation after the delete = %d, want 2", got)
	}
}

// A delete removes only the row its scope names, so the wider scope's value
// is effective again while its own row keeps the generation and value its
// last change gave it.
func TestDeleteSettingRemovesOverride(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)
	node := entity.SettingScope{Kind: entity.ScopeNode, ID: uuid.New()}

	if g := settingPut(t, s, settingInst, "k", 0); g != 1 {
		t.Errorf("installation generation = %d, want 1", g)
	}

	if g := settingPut(t, s, node, "k", 0); g != 2 {
		t.Errorf("node generation = %d, want 2", g)
	}

	if got := cfgGen(t, db); got != 2 {
		t.Errorf("generation after two puts = %d, want 2", got)
	}

	settingDelete(t, s, node, "k", 2)

	if got := cfgGen(t, db); got != 3 {
		t.Errorf("generation after the delete = %d, want 3", got)
	}

	if g := settingGen(t, s, node, "k"); g != 0 {
		t.Errorf("node row after its delete = %d, want gone", g)
	}

	rows := 0

	for got, err := range s.ControlPlane().Settings(settingCtx, settingInst) {
		if err != nil {
			t.Fatalf("list the installation settings: %v", err)
		}

		rows++

		if got.Key != "k" || got.Generation != 1 || string(got.Value) != "k" {
			t.Errorf(
				"installation row = key %q generation %d value %q, want k, 1, k",
				got.Key,
				got.Generation,
				got.Value,
			)
		}
	}

	if rows != 1 {
		t.Errorf("installation rows after the node delete = %d, want 1", rows)
	}
}

// A delete under the row's own generation removes it, and a row created
// again over nothing succeeds.
func TestDeleteSettingThenRecreate(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)

	if g := settingPut(t, s, settingInst, "k", 0); g != 1 {
		t.Errorf("first generation = %d, want 1", g)
	}

	if g := settingPut(t, s, settingInst, "k", 1); g != 2 {
		t.Errorf("second generation = %d, want 2", g)
	}

	if got := cfgGen(t, db); got != 2 {
		t.Errorf("generation after two puts = %d, want 2", got)
	}

	settingDelete(t, s, settingInst, "k", 2)

	if got := cfgGen(t, db); got != 3 {
		t.Errorf("generation after the delete = %d, want 3", got)
	}

	// The recreate lands on the settings generation's fourth position:
	// two puts and the delete moved the counter three times, and the row
	// it replaces is gone, so no older generation outranks it.
	settingPut(t, s, settingInst, "k", 0)

	if g := settingGen(t, s, settingInst, "k"); g != 4 {
		t.Errorf("recreated generation = %d, want 4", g)
	}
}

// A row created again after its delete never takes back a tag a writer
// before the delete already holds: a put under that tag is refused, so it
// never overwrites the new row.
func TestSettingRecreatedRefusesOldGeneration(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)
	cp := s.ControlPlane()

	if g := settingPut(t, s, settingInst, "k", 0); g != 1 {
		t.Errorf("first generation = %d, want 1", g)
	}

	settingDelete(t, s, settingInst, "k", 1)

	if g := settingPut(t, s, settingInst, "k", 0); g != 3 {
		t.Errorf("recreated generation = %d, want 3", g)
	}

	if got := cfgGen(t, db); got != 3 {
		t.Errorf("generation after the recreate = %d, want 3", got)
	}

	if _, err := cp.PutSetting(
		settingCtx,
		entity.Setting{Scope: settingInst, Key: "k", Value: []byte("k")},
		1,
	); !errors.Is(err, entity.ErrGenerationMismatch) {
		t.Errorf(
			"recreated row written under the pre-delete tag: %v, want %v",
			err,
			entity.ErrGenerationMismatch,
		)
	}

	if g := settingGen(t, s, settingInst, "k"); g != 3 {
		t.Errorf("generation after the refused write = %d, want 3", g)
	}
}

// A delete of a row that is not there, expect zero, changes nothing: an
// absent row has no tag, so zero names it and any other expect is a refused
// tag, and neither moves the settings generation a node re-reads.
func TestDeleteSettingNoRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)
	cp := s.ControlPlane()

	if err := cp.DeleteSetting(settingCtx, settingInst, "none", 0); err != nil {
		t.Fatalf("absent row deleted under 0: %v", err)
	}

	if got := cfgGen(t, db); got != 0 {
		t.Errorf("generation after a no-row delete under 0 = %d, want 0", got)
	}

	if err := cp.DeleteSetting(settingCtx, settingInst, "none", 1); !errors.Is(
		err,
		entity.ErrGenerationMismatch,
	) {
		t.Errorf("absent row deleted under 1: %v, want %v", err, entity.ErrGenerationMismatch)
	}

	if got := cfgGen(t, db); got != 0 {
		t.Errorf("generation after a no-row delete under 1 = %d, want 0", got)
	}
}

// A delete at a key or scope a put refuses is refused as the put is, with
// the same error, before any read or write, so a delete never names a row
// no put could have written.
func TestDeleteSettingRefusesWhatPutRefuses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)
	cp := s.ControlPlane()

	for _, tc := range []struct {
		name  string
		scope entity.SettingScope
		key   string
	}{
		{
			name: "installation scope with an ID",
			scope: entity.SettingScope{
				Kind: entity.ScopeInstallation,
				ID:   [16]byte{1},
			},
			key: "k",
		},
		{
			name: "node scope without an ID",
			scope: entity.SettingScope{
				Kind: entity.ScopeNode,
			},
			key: "k",
		},
		{
			name: "scope with no kind and an ID",
			scope: entity.SettingScope{
				ID: [16]byte{1},
			},
			key: "k",
		},
		{
			name:  "empty key",
			scope: settingInst,
			key:   "",
		},
	} {
		_, putErr := cp.PutSetting(settingCtx, entity.Setting{
			Scope: tc.scope,
			Key:   tc.key,
		}, 0)
		if !isInvalidArgument(putErr) {
			t.Fatalf("%s: put %v, want an invalid argument", tc.name, putErr)
		}

		err := cp.DeleteSetting(settingCtx, tc.scope, tc.key, 0)
		if !isInvalidArgument(err) {
			t.Errorf("%s: delete %v, want an invalid argument", tc.name, err)

			continue
		}

		if err.Error() != putErr.Error() {
			t.Errorf("%s: delete refusal = %q, want the put's %q", tc.name, err, putErr)
		}
	}

	if got := cfgGen(t, db); got != 0 {
		t.Errorf("generation after the refused deletes = %d, want 0", got)
	}
}

// A delete over a row that does not decode returns the decode error and
// repairs nothing, so a corrupt row stays for its owner to repair instead of
// disappearing under a delete that hid the corruption.
func TestDeleteSettingUndecodableRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)

	if err := db.Update(settingCtx, func(tx kv.Txn) error {
		return tx.Set(settingKey(settingInst, "k"), []byte{0xFF})
	}); err != nil {
		t.Fatal(err)
	}

	err := s.ControlPlane().DeleteSetting(settingCtx, settingInst, "k", 0)
	if !errors.Is(err, codec.ErrVersion) {
		t.Errorf(
			"undecodable row deleted: %v, want the decode error wrapping %v",
			err,
			codec.ErrVersion,
		)
	}

	var stored []byte

	if err := db.View(settingCtx, func(r kv.Reader) error {
		b, ok, err := get(r, settingKey(settingInst, "k"))
		if err != nil {
			return err
		}

		if !ok {
			t.Fatal("undecodable row gone after the refused delete")
		}

		stored = b

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(stored, []byte{0xFF}) {
		t.Errorf("undecodable row = %v, want stored as it was", stored)
	}
}

// A put of a schema above the binary's registered one is refused whole, so
// neither the row nor the settings generation moves: a record this
// store wrote would be a record every reader refuses, so it never lands.
// A put at the registered schema still writes, under the same generation
// the refused one named.
func TestPutSettingRefusesNewerSchema(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)
	cp := s.ControlPlane()

	top := settings.Versions()[entity.FormatSettings].Max

	got, err := cp.PutSetting(
		settingCtx,
		entity.Setting{
			Scope:  settingInst,
			Key:    "k",
			Value:  []byte("k"),
			Schema: top,
		},
		0,
	)
	if err != nil {
		t.Fatalf("put at the registered schema under 0: %v", err)
	}

	if got != 1 {
		t.Errorf("generation of the schema-max put = %d, want 1", got)
	}

	if _, err := cp.PutSetting(
		settingCtx,
		entity.Setting{
			Scope:  settingInst,
			Key:    "k",
			Value:  []byte("newer"),
			Schema: top + 1,
		},
		1,
	); !errors.Is(err, settings.ErrNewerSchema) {
		t.Errorf(
			"newer schema written under 1: %v, want %v",
			err,
			settings.ErrNewerSchema,
		)
	}

	if got := cfgGen(t, db); got != 1 {
		t.Errorf("generation after the refused put = %d, want 1", got)
	}

	rows := 0

	for row, err := range cp.Settings(settingCtx, settingInst) {
		if err != nil {
			t.Fatalf("list the installation settings: %v", err)
		}

		rows++

		if row.Key != "k" || row.Schema != top || row.Generation != 1 ||
			!slices.Equal(row.Value, []byte("k")) {
			t.Errorf(
				"row after the refused put = key %q schema %d generation %d value %q, "+
					"want k, %d, 1, k",
				row.Key,
				row.Schema,
				row.Generation,
				row.Value,
				top,
			)
		}
	}

	if rows != 1 {
		t.Errorf("rows at the installation scope = %d, want 1", rows)
	}

	got, err = cp.PutSetting(
		settingCtx,
		entity.Setting{
			Scope:  settingInst,
			Key:    "k",
			Value:  []byte("k2"),
			Schema: top,
		},
		1,
	)
	if err != nil {
		t.Fatalf("put at the registered schema under 1: %v", err)
	}

	if got != 2 {
		t.Errorf("generation of the second schema-max put = %d, want 2", got)
	}

	if got := cfgGen(t, db); got != 2 {
		t.Errorf("generation after the second schema-max put = %d, want 2", got)
	}
}

// One scan lists a row at each of the five scopes, each once, and a row of a
// schema the binary does not read comes back unchanged, for its own reader
// to refuse.
func TestAllSettingsListsEveryScopeWithANewerSchemaRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s, db := settingStore(t)

	scopes := []entity.SettingScope{
		{Kind: entity.ScopeInstallation},
		{Kind: entity.ScopeNode, ID: uuid.New()},
		{Kind: entity.ScopeNamespace, ID: uuid.New()},
		{Kind: entity.ScopeRemote, ID: uuid.New()},
		{Kind: entity.ScopeShare, ID: uuid.New()},
	}

	// The middle row, the namespace's, carries a schema one above the
	// binary's registered one.
	want := make([]entity.Setting, len(scopes))
	for i, scope := range scopes {
		schema := uint32(1)
		if scope.Kind == entity.ScopeNamespace {
			schema = settings.Versions()[entity.FormatSettings].Max + 1
		}

		want[i] = entity.Setting{
			Scope:      scope,
			Key:        "k",
			Value:      []byte("k"),
			Generation: 1,
			Schema:     schema,
		}
	}

	if err := db.Update(settingCtx, func(tx kv.Txn) error {
		for _, w := range want {
			if err := tx.Set(settingKey(w.Scope, w.Key), encodeSetting(w)); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	i := 0

	for got, err := range s.Nodes().AllSettings(settingCtx, entity.SettingKey{}) {
		if err != nil {
			t.Fatalf("pair %d: %v", i, err)
		}

		if i >= len(want) {
			t.Fatalf("pair %d = %+v, want only %d pairs", i, got, len(want))
		}

		w := want[i]
		if got.Scope != w.Scope || got.Key != w.Key || !slices.Equal(got.Value, w.Value) ||
			got.Generation != w.Generation || got.Schema != w.Schema || got.Position != w.Position {
			t.Errorf("pair %d = %+v, want %+v", i, got, w)
		}

		i++
	}

	if i != len(want) {
		t.Errorf("pairs = %d, want %d", i, len(want))
	}
}
