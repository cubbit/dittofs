package settings_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// readRefused runs Read over s's store for the node of nodeA and returns the
// sections it refused.
func (s store) readRefused(t *testing.T, tb settings.Table) map[string]error {
	t.Helper()

	st, err := kvstore.Open(ctx, s.db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	_, refused, err := settings.Read(ctx, st.Nodes(), tb, entity.NodeID(nodeA.ID))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	return refused
}

// cappedJournal is a per-journal section whose Validate refuses a spare
// target past 10.
type cappedJournal struct {
	SpareTarget uint32 `json:"spare_target" class:"live"`
}

func (c cappedJournal) Validate() error {
	if c.SpareTarget > 10 {
		return fmt.Errorf("spare_target must be at most 10, got %d", c.SpareTarget)
	}

	return nil
}

// RFC 13 §9's "§6 one validator": for each invalid record, Check's refusal of
// its put and Read's refusal of the same row stored are the same line and
// the same sentinel, so the API and the component never disagree.
func TestCheckAgreesWithRead(t *testing.T) {
	prof.Watch(t, prof.Options{})

	newer := settings.Versions()[entity.FormatSettings].Max + 1
	oidc := settings.Table{
		settings.Of(
			"test.oidc",
			settings.At(entity.ScopeInstallation),
			oidcSettings{},
			settings.Required("Issuer"),
		),
	}

	journal := settings.Table{
		settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), cappedJournal{}),
	}

	cases := []struct {
		name    string
		tb      settings.Table
		section string
		row     entity.Setting
		want    error
	}{
		{
			"range Validate refuses", table(), "test.port",
			setting(installation, "test.port.port", "70000"), settings.ErrInvalid,
		},
		{
			"required key unset", oidc, "test.oidc",
			setting(installation, "test.oidc.audience", `"a"`), settings.ErrRequired,
		},
		{
			"wrong type", table(), "test.port",
			setting(installation, "test.port.port", `"x"`), settings.ErrInvalid,
		},
		{
			"per thing unknown field", journal, "journal",
			setting(nodeA, "journal."+journalID+".spare_targt", "1"), settings.ErrUnknownKey,
		},
		{
			"per thing Validate refuses", journal, "journal",
			setting(nodeA, "journal."+journalID+".spare_target", "11"), settings.ErrInvalid,
		},
		{"newer schema", table(), "test.port", entity.Setting{
			Scope:  installation,
			Key:    "test.port.port",
			Value:  []byte("81"),
			Schema: newer,
		}, settings.ErrNewerSchema},
	}

	for _, c := range cases {
		s := open(t)

		checkErr := s.check(c.tb, entity.RoleOperator, settings.Put(c.row))
		s.raw(t, c.row.Scope, c.row.Key, rowValue(string(c.row.Value), 1, c.row.Schema))
		readErr := s.readRefused(t, c.tb)[c.section]

		if !errors.Is(checkErr, c.want) || !errors.Is(readErr, c.want) {
			t.Errorf("%s: Check %v, Read %v, want both %v", c.name, checkErr, readErr, c.want)

			continue
		}

		if checkErr.Error() != readErr.Error() {
			t.Errorf("%s: Check %q, Read %q, want one line", c.name, checkErr, readErr)
		}
	}
}

// A key no section declares, stored under a declared prefix by a newer
// binary, refuses its section at Read; an admin's delete passes Check and,
// once the store removes the row, the next Read reads the section.
func TestCheckRepairDeleteRead(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	tb := table()
	key := "identity.newer"

	s.raw(t, installation, key, rowValue(`"x"`, 1, 1))

	if err := s.readRefused(t, tb)["identity"]; !errors.Is(err, settings.ErrUnknownKey) {
		t.Fatalf("Read before the delete: refused[identity] = %v, want ErrUnknownKey", err)
	}

	passed(t, "admin", s.check(tb, entity.RoleAdmin, settings.Delete(installation, key)))

	if err := s.cp.DeleteSetting(ctx, installation, key, 1); err != nil {
		t.Fatalf("delete %s: %v", key, err)
	}

	if err := s.readRefused(t, tb)["identity"]; err != nil {
		t.Fatalf("Read after the delete: refused[identity] = %v, want none", err)
	}
}
