package settings_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/settings"
)

// perJournal is a per-journal section of two fields, so a sibling field of
// one journal sits beside the same field of another.
type perJournal struct {
	valid
	A uint32 `json:"a" class:"live"`
	B uint32 `json:"b" class:"live"`
}

// attempts runs the put of s as the API's route does, Check then the put,
// and on the first attempt only, between the two, runs hook; it returns how
// many times the write's function ran.
func (st store) attempts(t *testing.T, tb settings.Table, s entity.Setting, hook func()) int {
	t.Helper()

	n := 0

	err := st.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		n++

		if err := tb.Check(tx, entity.RoleOperator, settings.Put(s), nil); err != nil {
			return err
		}

		if n == 1 {
			hook()
		}

		cur, _, err := tx.Setting(s.Scope, s.Key)
		if err != nil {
			return err
		}

		_, err = tx.PutSetting(s, cur.Generation)

		return err
	})
	if err != nil {
		t.Fatalf("put %s: %v", s.Key, err)
	}

	return n
}

// Check reads every row it judges tracked, and no other: a row written
// straight to the Badger between Check and the commit, with no settings
// generation raised, makes the write retry when Check read it, a sibling's
// at either scope of its chain, and never when it is another section's,
// another share's or another journal's. The put's own row is not a case: the
// route reads it too, so its retry would not show Check's read.
func TestCheckConflicts(t *testing.T) {
	prof.Watch(t, prof.Options{})

	shareB := entity.SettingScope{
		Kind: entity.ScopeShare,
		ID:   [16]byte{0x5b, 15: 1},
	}
	otherJournal := "3f2a00000000000000000000000000b2"

	tb := append(table(), settings.Of(
		"perjournal",
		settings.PerThing(entity.ScopeNode, "journal"),
		perJournal{},
	))

	cases := []struct {
		name  string
		put   entity.Setting
		scope entity.SettingScope
		key   string
		value string
		want  int
	}{
		{
			"a sibling at the installation", setting(installation, "test.port.port", "1"),
			installation, "test.port.host", `"h"`, 2,
		},
		{
			"a sibling at the installation under a node put", setting(nodeA, "test.port.port", "1"),
			installation, "test.port.host", `"h"`, 2,
		},
		{
			"a sibling at the node", setting(nodeA, "test.port.port", "1"),
			nodeA, "test.port.host", `"h"`, 2,
		},
		{
			"another section's row", setting(installation, "test.port.port", "1"),
			installation, "identity.mode", `"x"`, 1,
		},
		{
			"the same key for another share", setting(shareA, "rehome.rate", "1"),
			shareB, "rehome.rate", "2", 1,
		},
		{
			"the same field of another journal",
			setting(nodeA, "perjournal."+journalID+".a", "1"),
			nodeA, "perjournal." + otherJournal + ".a", "2", 1,
		},
		{
			"a sibling field of the same journal",
			setting(nodeA, "perjournal."+journalID+".a", "1"),
			nodeA, "perjournal." + journalID + ".b", "2", 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t)

			got := s.attempts(t, tb, c.put, func() {
				s.raw(t, c.scope, c.key, rowValue(c.value, 1, 1))
			})
			if got != c.want {
				t.Errorf("attempts = %d, want %d", got, c.want)
			}
		})
	}
}

// Check reads by point reads only: one per declared field of the section at
// each scope of the key's chain, the put's own key among them, plus the row
// an undeclared key's delete removes, and never a scan or a range.
func TestCheckPointReadCount(t *testing.T) {
	prof.Watch(t, prof.Options{})

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	counting := &kv.Counting{KV: db}
	s := openOver(t, counting)
	tb := table()

	for _, c := range []struct {
		name string
		w    settings.Write
		gets int
	}{
		{"installation put", settings.Put(setting(installation, "test.port.port", "1")), 2},
		{"node put", settings.Put(setting(nodeA, "test.port.port", "1")), 4},
		{"node delete", settings.Delete(nodeA, "test.port.port"), 4},
		{"undeclared delete", settings.Delete(installation, "identity.newer"), 2},
		{"per-journal delete", settings.Delete(nodeA, "journal."+journalID+".spare_target"), 1},
	} {
		counting.Take()

		passed(t, c.name, s.check(tb, entity.RoleAdmin, c.w))

		got := counting.Take()
		if got.Gets != c.gets || got.Scanned != 0 || got.RangeGuards != 0 || got.Befores != 0 {
			t.Errorf("%s: gets %d, scanned %d, range guards %d, befores %d; want %d gets only",
				c.name, got.Gets, got.Scanned, got.RangeGuards, got.Befores, c.gets)
		}
	}
}

// What a section's CheckTx reads is tracked: the record it read, written
// through the store's own write between Check and the commit, makes the
// write retry, and another record does not.
func TestCheckTxConflict(t *testing.T) {
	prof.Watch(t, prof.Options{})

	tb := settings.Table{limitSection()}

	for _, c := range []struct {
		group string
		want  int
	}{
		{limitGroup, 2},
		{"unrelated", 1},
	} {
		t.Run(c.group, func(t *testing.T) {
			s := open(t)
			s.netgroup(t, limitGroup)

			got := s.attempts(t, tb, setting(installation, "test.limit.max", "1"), func() {
				s.netgroup(t, c.group)
			})
			if got != c.want {
				t.Errorf("attempts = %d, want %d", got, c.want)
			}
		})
	}
}
