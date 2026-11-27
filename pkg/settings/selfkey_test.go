package settings

import (
	"fmt"
	"maps"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// skLimit is the section a node declares for its connection limit: one
// field whose key is the section's prefix, RFC 13's Appendix B bare
// connection_limit.
type skLimit struct {
	Limit uint32 `json:"limit" class:"live"`
}

// Validate refuses a limit past the node's bound, printing the value alone,
// so a refusal never names the key it already sits under.
func (s skLimit) Validate() error {
	if s.Limit > 4096 {
		return fmt.Errorf("limit must be at most 4096, got %d", s.Limit)
	}

	return nil
}

// skTwo holds two setting fields, which a SelfKey section cannot: its one
// key would sit under both.
type skTwo struct {
	valid
	Limit uint32 `json:"limit" class:"live"`
	Spare uint32 `json:"spare" class:"live"`
}

// skNone holds no setting field, so a SelfKey section names nothing at all.
type skNone struct {
	valid
	Note string `json:"-"`
}

func TestOfSelfKey(t *testing.T) {
	prof.Watch(t, prof.Options{})

	node := At(entity.ScopeNode)

	sec := declared(t, func() Section {
		return Of("connection_limit", node, skLimit{}, SelfKey())
	})

	want := map[string]Class{"connection_limit": Live}
	if got := classesOf(sec); !maps.Equal(got, want) {
		t.Errorf("keys and classes = %v, want %v", got, want)
	}

	if len(sec.fields) != 1 || sec.fields[0].name != "limit" {
		t.Errorf("fields = %+v, want one field of json name limit", sec.fields)
	}
}

func TestOfSelfKeyPanics(t *testing.T) {
	prof.Watch(t, prof.Options{})

	node := At(entity.ScopeNode)
	perJournal := PerThing(entity.ScopeNode, "journal")

	cases := []struct {
		name string
		of   func()
		want string
	}{
		{
			"two setting fields share the one key",
			func() {
				Of("connection_limit", node, skTwo{}, SelfKey())
			},
			"settings: section connection_limit: fields Limit and Spare share the key " +
				"connection_limit",
		},
		{
			"no setting field",
			func() {
				Of("connection_limit", node, skNone{}, SelfKey())
			},
			"settings: section connection_limit: SelfKey names one field, 0 declared",
		},
		{
			"per-thing scope",
			func() {
				Of("connection_limit", perJournal, skLimit{}, SelfKey())
			},
			"settings: section connection_limit: SelfKey with a per-thing scope",
		},
		{
			"given twice",
			func() {
				Of("connection_limit", node, skLimit{}, SelfKey(), SelfKey())
			},
			"settings: section connection_limit: SelfKey given twice",
		},
		{
			"empty prefix with the option",
			func() {
				Of("", node, skLimit{}, SelfKey())
			},
			`settings: section "": empty prefix`,
		},
		{
			"empty prefix without the option",
			func() {
				Of("", node, skLimit{})
			},
			`settings: section "": empty prefix`,
		},
		{
			"key role at the section's write role",
			func() {
				Of("connection_limit", node, skLimit{}, SelfKey(),
					KeyRole("Limit", entity.RoleOperator))
			},
			"settings: section connection_limit: KeyRole Limit names 2, " +
				"at or below the section's write role 2",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := panicText(t, c.of); got != c.want {
				t.Errorf("panic = %q, want %q", got, c.want)
			}
		})
	}
}

// A selfkey section and a section whose prefix sits under it live in one
// table: the selfkey section owns its bare key and, as any prefix does, the
// names below it, which the longer prefix then takes back.
func TestSelfKeyTableIndex(t *testing.T) {
	prof.Watch(t, prof.Options{})

	var idx index

	if text := panicText(t, func() {
		idx = Table{
			Of("x", installation, skLimit{}, SelfKey()),
			Of("x.y", installation, identityMode{}),
		}.index()
	}); text != "" {
		t.Fatalf("index of a selfkey section with a section under it panicked: %s", text)
	}

	cases := []struct {
		key  string
		want string
	}{
		{"x", "x"},
		{"x.z", "x"},
		{"x.y", "x.y"},
		{"x.y.mode", "x.y"},
	}

	for _, c := range cases {
		got := ""
		if sec, ok := idx.lookup(c.key); ok {
			got = sec.prefix
		}

		if got != c.want {
			t.Errorf("lookup(%q) = section %q, want %q", c.key, got, c.want)
		}
	}
}

// The table's consumed-key panic still fires beside a selfkey section: the
// selfkey section neither hides another section's conflict nor makes one.
func TestSelfKeyLeavesConsumedKeyPanic(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got := panicText(t, func() {
		Table{
			Of("connection_limit", installation, skLimit{}, SelfKey()),
			Of("identity", installation, identityWithOIDC{}),
			Of("identity.oidc", installation, oidcIssuer{}),
		}.index()
	})

	const want = "settings: section identity (settings.identityWithOIDC) declares key " +
		"identity.oidc, which section identity.oidc (settings.oidcIssuer) takes"
	if got != want {
		t.Errorf("panic = %q, want %q", got, want)
	}
}
