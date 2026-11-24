package settings_test

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// rfNamed is one refusal Refusals named: the scope instance its value
// belongs to and the key the row wrote.
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
// fine share; two bad rows are named in scope order; an unknown key comes
// bare, as the row wrote it, so PrefixOf answers it; a value its section's
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
	s.put(t, rdShareScope(4), "sha.nope", "1")
	s.put(t, rdInstallation, "srv.port", "60001")

	_, refused := rdRead(t, s.nodes, table, rdNodeA)
	if len(refused) != 2 {
		t.Fatalf("Read refused %d sections, want 2, sha and srv", len(refused))
	}

	got := rfAll(refused["sha"])

	want := []rfNamed{
		{rdShareScope(2), "sha.f"},
		{rdShareScope(3), "sha.f"},
		{rdShareScope(4), "sha.nope"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Refusals of sha = %+v, want %+v, the bad rows by scope order "+
			"and the unknown key bare", got, want)
	}

	// An early break stops the sequence: no more key is yielded once the
	// loop body returns false, where a sequence that went on would panic.
	for range settings.Refusals(refused["sha"]) {
		break
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

	if got := rfAll(entity.ErrBadRow); len(got) != 0 {
		t.Fatalf("Refusals of an outside error = %+v, want none", got)
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

// Reads answers as Read does, row by row: a row Reads keeps is one Read
// refuses when it does not decode, and a row Reads skips is one Read passes
// over, so a refusal never names a row Read skips and never drops one it
// keeps. The rows cover each skip: a scope kind the section lacks, a field an
// OverrideOnly option does not name, a per-thing row with no thing, and a key
// no section claims.
func TestReadsAgreesWithReadRowByRow(t *testing.T) {
	prof.Watch(t, prof.Options{})

	table := settings.Table{
		settings.Of("srv", settings.Override(entity.ScopeInstallation, entity.ScopeNode),
			rdServerDefault(), settings.OverrideOnly("Port")),
		settings.Of("pt", settings.PerThing(entity.ScopeNode, "thing"), rdServerDefault()),
	}

	node := rdNodeScope(rdNodeA)

	for _, tc := range []struct {
		name  string
		scope entity.SettingScope
		key   string
		want  bool
	}{
		{"the wide scope", rdInstallation, "srv.port", true},
		{"the narrow scope, a field OverrideOnly names", node, "srv.port", true},
		{"the narrow scope, a field it does not name", node, "srv.host", false},
		{"a scope kind the section lacks", rdShareScope(1), "srv.port", false},
		{"a per-thing row with its thing", node, "pt.t1.port", true},
		{"a per-thing row with no thing", node, "pt.port", false},
		{"a key no section claims", rdInstallation, "other.key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRdStore(t)
			s.raw(t, tc.scope, tc.key, []byte{0xff})

			_, refused := rdRead(t, s.nodes, table, rdNodeA)

			var named int

			for _, err := range refused {
				named += len(rfAll(err))
			}

			if kept := named > 0; kept != tc.want {
				t.Fatalf("Read names %d refusals, so it keeps the row: %v, want %v", named, kept, tc.want)
			}

			row := entity.Setting{Scope: tc.scope, Key: tc.key}

			prefix, got := table.Reads(row)
			if got != tc.want {
				t.Errorf("Reads = %v, want %v, as Read does", got, tc.want)
			}

			if want := table.PrefixOf(tc.key); got && prefix != want {
				t.Errorf("Reads names section %q, want %q, the one PrefixOf names", prefix, want)
			}
		})
	}
}

// A row with no key, as the walk yields one whose key does not parse, is read
// by no section, so the walk needs no check of its own for it.
func TestReadsSkipsARowWithNoKey(t *testing.T) {
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	if prefix, ok := table.Reads(entity.Setting{}); ok || prefix != "" {
		t.Fatalf("Reads of a row with no key = %q, %v; want no section", prefix, ok)
	}
}
