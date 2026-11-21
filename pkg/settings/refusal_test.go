package settings_test

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// rfNamed is one refusal Refusals named: the scope instance its value
// belongs to and the key it prints.
type rfNamed struct {
	scope entity.SettingScope
	key   string
}

// rfAll is every refusal Refusals names in err, in the order it names them.
func rfAll(err error) []rfNamed {
	var got []rfNamed
	for scope, key := range settings.Refusals(err) {
		got = append(got, rfNamed{scope, key})
	}

	return got
}

// Refusals names each refused value's own scope and key and nothing else:
// one bad row of a per-instance section names its own share alone, beside a
// fine share; two bad rows are named in scope order; a value its section's
// Validate refused is named once, at its scope and the section's prefix.
func TestRefusalsNamesEachRefusedScopeAndKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdServerDefault()),
		settings.Of("sha", settings.At(entity.ScopeShare), dfShare{}),
	}

	s.put(t, rdShareScope(1), "sha.f", "1")
	s.raw(t, rdShareScope(2), "sha.f", []byte{0xff})
	s.raw(t, rdShareScope(3), "sha.f", []byte{0xff})
	s.put(t, rdInstallation, "srv.port", "60001")

	_, refused := rdRead(t, s.nodes, table, rdNodeA)
	if len(refused) != 2 {
		t.Fatalf("Read refused %d sections, want 2, sha and srv", len(refused))
	}

	got := rfAll(refused["sha"])

	want := []rfNamed{
		{rdShareScope(2), "sha.f"},
		{rdShareScope(3), "sha.f"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Refusals of sha = %+v, want %+v, the two bad rows by scope order", got, want)
	}

	got = rfAll(refused["srv"])

	want = []rfNamed{{rdInstallation, "srv"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Refusals of srv = %+v, want %+v, the whole refused value at its scope", got, want)
	}
}

// A section Read refused for nothing yields nothing, and an error that is
// not a refusal of this package yields nothing either.
func TestRefusalsNamesNothingOutsideARefusal(t *testing.T) {
	if got := rfAll(nil); len(got) != 0 {
		t.Fatalf("Refusals of nil = %+v, want none", got)
	}

	outside := entity.ErrBadRow

	if err := outside; err != nil {
		if got := rfAll(err); len(got) != 0 {
			t.Fatalf("Refusals of an outside error = %+v, want none", got)
		}
	}
}

// PrefixOf answers the section a key belongs to: the longest prefix, so a
// nested key and a look-alike around another section each name their own,
// and an undeclared key names none.
func TestPrefixOfAnswersTheLongestPrefix(t *testing.T) {
	table := settings.Table{
		settings.Of("identity", settings.At(entity.ScopeInstallation), rdPeer{}),
		settings.Of("identity.directory", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	for _, tc := range []struct {
		key  string
		want string
	}{
		{"identity.address", "identity"},
		{"identity.directory.port", "identity.directory"},
		{"identity.nope", "identity"},
		{"elsewhere.key", ""},
	} {
		if got := table.PrefixOf(tc.key); got != tc.want {
			t.Errorf("PrefixOf(%s) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
