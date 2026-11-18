package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
)

var instScope = entity.SettingScope{Kind: entity.ScopeInstallation}

// faultDiskStore is a store over Badger on disk under t.TempDir(), wrapped by a
// FaultKV that passes every call until the test sets a fault, and the raw KV
// under it.
func faultDiskStore(t *testing.T) (*Store, *kvtest.FaultKV, kv.KV) {
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

	faults := kvtest.NewFaultKV(db)
	ctx := context.Background()

	if err := Init(ctx, faults, uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, faults)
	if err != nil {
		t.Fatal(err)
	}

	return s, faults, db
}

// putRows writes every row in one commit, raw.
func putRows(t *testing.T, db kv.KV, rows []kv.KeyValue) {
	t.Helper()

	err := db.Update(context.Background(), func(tx kv.Txn) error {
		for _, r := range rows {
			if err := tx.Set(r.Key, r.Value); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// settingRow is the row a setting at instScope scope is stored as.
func settingRow(key string) kv.KeyValue {
	s := entity.Setting{
		Scope:      instScope,
		Key:        key,
		Value:      []byte(key),
		Generation: 1,
	}

	return kv.KeyValue{Key: settingKey(s.Scope, s.Key), Value: encodeSetting(s)}
}

// pair is one pair AllSettings yields.
type pair struct {
	s   entity.Setting
	err error
}

// allPairs is every pair AllSettings yields after after.
func allPairs(s *Store, after entity.SettingKey) []pair {
	var got []pair

	for st, err := range s.Nodes().AllSettings(context.Background(), after) {
		got = append(got, pair{st, err})
	}

	return got
}

// pairKeys is each pair's key, failing the test on an error.
func pairKeys(t *testing.T, got []pair) []string {
	t.Helper()

	var ks []string

	for _, p := range got {
		if p.err != nil {
			t.Fatalf("setting %q: %v", p.s.Key, p.err)
		}

		ks = append(ks, p.s.Key)
	}

	return ks
}

// A listing resumed after a key no longer stored starts at the next one.
func TestAllSettingsResumesAfterDeletedKey(t *testing.T) {
	s, _, db := faultDiskStore(t)
	rows := []kv.KeyValue{settingRow("a"), settingRow("b"), settingRow("c"), settingRow("d")}
	putRows(t, db, rows)

	err := db.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Delete(settingKey(instScope, "b"))
	})
	if err != nil {
		t.Fatal(err)
	}

	got := pairKeys(t, allPairs(s, entity.SettingKey{Scope: instScope, Key: "b"}))
	if want := []string{"c", "d"}; !slices.Equal(got, want) {
		t.Errorf("settings after the deleted b: got %q, want %q", got, want)
	}
}

// A row that does not decode, by its value or by its key, is yielded as a
// pair wrapping ErrBadRow and its cause, with the scope and key when the key
// parses, and every row after it still comes.
func TestAllSettingsYieldsBadRows(t *testing.T) {
	s, _, db := faultDiskStore(t)
	badKey := []byte{keys.KindCFG, byte(entity.ScopeInstallation), 'b', '2'} // no string terminator
	putRows(t, db, []kv.KeyValue{
		settingRow("a"),
		{Key: settingKey(instScope, "b"), Value: []byte{0xFF}},
		{Key: badKey, Value: settingRow("b2").Value},
		settingRow("c"),
	})

	got := allPairs(s, entity.SettingKey{})
	if len(got) != 4 {
		t.Fatalf("pairs: got %d %+v, want 4: a, the bad value, the bad key, c", len(got), got)
	}

	for i, key := range map[int]string{0: "a", 3: "c"} {
		p := got[i]
		if p.err != nil || p.s.Scope != instScope || p.s.Key != key || string(p.s.Value) != key {
			t.Errorf("pair %d: got %+v, want the valid setting %q and no error", i, p, key)
		}
	}

	for _, c := range []struct {
		i     int
		name  string
		s     entity.Setting
		cause error
	}{
		{1, "bad value", entity.Setting{Scope: instScope, Key: "b"}, codec.ErrVersion},
		{2, "bad key", entity.Setting{}, keys.ErrMalformed},
	} {
		p := got[c.i]
		if !errors.Is(p.err, entity.ErrBadRow) || !errors.Is(p.err, c.cause) {
			t.Errorf("%s: error %v, want ErrBadRow wrapping %v", c.name, p.err, c.cause)
		}

		if !slices.Equal(p.s.Value, nil) || p.s.Scope != c.s.Scope || p.s.Key != c.s.Key {
			t.Errorf("%s: setting %+v, want %+v", c.name, p.s, c.s)
		}
	}
}

// A scan failing past the first page ends the listing with that error, not
// with ErrBadRow, after the first page's rows.
func TestAllSettingsScanFaultEndsWalk(t *testing.T) {
	s, faults, db := faultDiskStore(t)

	var rows []kv.KeyValue
	for i := range 300 {
		rows = append(rows, settingRow(fmt.Sprintf("k%03d", i)))
	}

	putRows(t, db, rows)

	injected := errors.New("injected scan failure")

	sched, err := sim.NewSchedule(1, nil, sim.Rule{
		Op:     kvtest.OpScan,
		Key:    "*",
		From:   2,
		Count:  1,
		Action: sim.FaultAction{Err: injected},
	})
	if err != nil {
		t.Fatal(err)
	}

	faults.SetFault(sched)

	got := allPairs(s, entity.SettingKey{})
	if len(got) != pageSize+1 {
		t.Fatalf("pairs: got %d, want %d: the first page, then the error", len(got), pageSize+1)
	}

	if ks := pairKeys(t, got[:pageSize]); ks[0] != "k000" || ks[pageSize-1] != "k255" {
		t.Errorf("first page: got %q to %q, want k000 to k255", ks[0], ks[pageSize-1])
	}

	if last := got[pageSize].err; !errors.Is(last, injected) || errors.Is(last, entity.ErrBadRow) {
		t.Errorf("last pair: error %v, want the injected scan failure, not ErrBadRow", last)
	}
}
