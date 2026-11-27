package settings_test

import (
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// dfClasses has one field of each binding class.
type dfClasses struct {
	A uint32 `json:"a" class:"live"`
	B uint32 `json:"b" class:"restart"`
	C uint32 `json:"c" class:"next write"`
	D uint32 `json:"d" class:"bound"`
}

func (dfClasses) Validate() error {
	return nil
}

// dfSpare is a per-journal section, named as RFC 13 writes it.
type dfSpare struct {
	SpareTarget uint32 `json:"spare_target" class:"live"`
}

func (dfSpare) Validate() error {
	return nil
}

// dfPending is a per-adapter section, named as RFC 13 writes it.
type dfPending struct {
	PendingTimeout uint32 `json:"pending_timeout" class:"live"`
}

func (dfPending) Validate() error {
	return nil
}

// dfSMB is a section whose port only a node may override.
type dfSMB struct {
	Port uint16 `json:"port" class:"restart"`
	Host string `json:"host" class:"live"`
}

func (dfSMB) Validate() error {
	return nil
}

// dfShare is a section with two fields, for one value per share.
type dfShare struct {
	F uint32 `json:"f" class:"live"`
	G uint32 `json:"g" class:"restart"`
}

func (dfShare) Validate() error {
	return nil
}

// DfInner is embedded in dfEmbedded, so its field is read as dfEmbedded's.
type DfInner struct {
	Deep uint32 `json:"deep" class:"restart"`
}

// dfEmbedded has an embedded struct, a field json does not read and one json
// leaves out at its zero value.
type dfEmbedded struct {
	DfInner

	Hidden uint32 `json:"-"`
	Top    uint32 `json:"top"           class:"live"`
	Opt    uint32 `json:"opt,omitempty" class:"live"`
}

func (dfEmbedded) Validate() error {
	return nil
}

// The two journals the per-thing tests write, in hex as a key holds them.
const (
	dfJournal1 = "11000000000000000000000000000000"
	dfJournal2 = "22000000000000000000000000000000"
)

// dfPut writes value at scope and key over the row at generation expect, 0
// for no row, and returns the new generation.
func dfPut(
	t *testing.T,
	s rdStore,
	scope entity.SettingScope,
	key, value string,
	expect uint64,
) uint64 {
	t.Helper()

	gen, err := s.cp.PutSetting(context.Background(), entity.Setting{
		Scope:  scope,
		Key:    key,
		Value:  []byte(value),
		Schema: 1,
	}, expect)
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}

	return gen
}

// dfDelete deletes the row at scope and key, at generation gen.
func dfDelete(t *testing.T, s rdStore, scope entity.SettingScope, key string, gen uint64) {
	t.Helper()

	err := s.cp.DeleteSetting(context.Background(), scope, key, gen)
	if err != nil {
		t.Fatalf("delete %s: %v", key, err)
	}
}

// dfWant fails unless got is exactly want, in order.
func dfWant(t *testing.T, what string, got, want []settings.Change) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Fatalf("%s: Diff = %+v, want %+v", what, got, want)
	}
}

func dfChange(
	section, key string,
	class settings.Class,
	scope entity.SettingScope,
) settings.Change {
	return settings.Change{
		Section: section,
		Key:     key,
		Class:   class,
		Scope:   scope,
	}
}

func TestDiffOneChangePerClass(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("cls", settings.At(entity.ScopeNode), dfClasses{}),
	}
	node := rdNodeScope(rdNodeA)

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)
	gens := map[string]uint64{}

	fields := []struct {
		key   string
		class settings.Class
	}{
		{"cls.a", settings.Live},
		{"cls.b", settings.Restart},
		{"cls.c", settings.NextWrite},
		{"cls.d", settings.Bound},
	}

	for _, f := range fields {
		gens[f.key] = dfPut(t, s, node, f.key, "1", 0)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, f.key, settings.Diff(prev, cur), []settings.Change{
			dfChange("cls", f.key, f.class, node),
		})

		prev = cur
	}

	want := make([]settings.Change, 0, len(fields))
	for _, f := range fields {
		gens[f.key] = dfPut(t, s, node, f.key, "2", gens[f.key])
		want = append(want, dfChange("cls", f.key, f.class, node))
	}

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "all four", settings.Diff(prev, cur), want)
}

func TestDiffUnchangedNotListed(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	gen := dfPut(t, s, rdInstallation, "srv.port", "81", 0)

	a, _ := rdRead(t, s.nodes, table, rdNodeA)
	b, _ := rdRead(t, s.nodes, table, rdNodeA)

	before, err := settings.Get[rdServer](a, "srv", rdNodeA)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	dfWant(t, "two reads", settings.Diff(a, b), nil)
	dfWant(t, "one snapshot", settings.Diff(a, a), nil)

	dfPut(t, s, rdInstallation, "srv.port", "81", gen)

	c, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "same value rewritten", settings.Diff(b, c), nil)

	dfPut(t, s, rdInstallation, "srv.host", `"default"`, 0)

	d, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "default written", settings.Diff(c, d), nil)

	after, err := settings.Get[rdServer](a, "srv", rdNodeA)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}

	if after != before {
		t.Fatalf("Get after Diff = %+v, want %+v", after, before)
	}
}

func TestDiffPerThing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), dfSpare{}),
	}
	node := rdNodeScope(rdNodeA)
	key1 := "journal." + dfJournal1 + ".spare_target"
	key2 := "journal." + dfJournal2 + ".spare_target"

	gen1 := dfPut(t, s, node, key1, "1", 0)
	gen2 := dfPut(t, s, node, key2, "2", 0)

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)

	dfPut(t, s, node, key1, "3", gen1)

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "first journal", settings.Diff(prev, cur), []settings.Change{
		dfChange("journal", key1, settings.Live, node),
	})

	dfPut(t, s, node, key2, "4", gen2)

	next, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "second journal", settings.Diff(cur, next), []settings.Change{
		dfChange("journal", key2, settings.Live, node),
	})
}

func TestDiffAdapterKeyAsWritten(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of(
			"adapters",
			settings.PerThing(entity.ScopeInstallation, "adapter"),
			dfPending{},
		),
	}

	gen := dfPut(t, s, rdInstallation, "adapters.smb.pending_timeout", "5", 0)

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)

	dfPut(t, s, rdInstallation, "adapters.smb.pending_timeout", "6", gen)

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "adapter", settings.Diff(prev, cur), []settings.Change{
		dfChange("adapters", "adapters.smb.pending_timeout", settings.Live, rdInstallation),
	})
}

func TestDiffNilPrev(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), dfSpare{}),
		settings.Of("pol", settings.At(entity.ScopeShare), dfShare{}),
	}
	node := rdNodeScope(rdNodeA)
	key1 := "journal." + dfJournal1 + ".spare_target"
	key2 := "journal." + dfJournal2 + ".spare_target"

	dfPut(t, s, node, key2, "2", 0)
	dfPut(t, s, node, key1, "1", 0)
	dfPut(t, s, rdInstallation, "srv.port", "81", 0)
	dfPut(t, s, rdShareScope(0xB), "pol.f", "2", 0)
	dfPut(t, s, rdShareScope(0xA), "pol.f", "1", 0)

	snap, _ := rdRead(t, s.nodes, table, rdNodeA)
	want := []settings.Change{
		dfChange("journal", key1, settings.Live, node),
		dfChange("journal", key2, settings.Live, node),
		dfChange("pol", "pol.f", settings.Live, rdShareScope(0xA)),
		dfChange("pol", "pol.g", settings.Restart, rdShareScope(0xA)),
		dfChange("pol", "pol.f", settings.Live, rdShareScope(0xB)),
		dfChange("pol", "pol.g", settings.Restart, rdShareScope(0xB)),
		dfChange("srv", "srv.port", settings.Live, rdInstallation),
		dfChange("srv", "srv.host", settings.Restart, rdInstallation),
	}

	dfWant(t, "first call", settings.Diff(nil, snap), want)
	dfWant(t, "second call", settings.Diff(nil, snap), want)
}

func TestDiffOverride(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("node", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of(
				"adapters.smb",
				settings.Override(entity.ScopeInstallation, entity.ScopeNode),
				dfSMB{
					Port: 445,
					Host: "default",
				},
				settings.OverrideOnly("Port"),
			),
		}
		node := rdNodeScope(rdNodeA)

		inst := dfPut(t, s, rdInstallation, "adapters.smb.port", "80", 0)
		own := dfPut(t, s, node, "adapters.smb.port", "81", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		dfPut(t, s, rdInstallation, "adapters.smb.port", "90", inst)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "installation under a node row", settings.Diff(prev, cur), nil)

		dfDelete(t, s, node, "adapters.smb.port", own)

		next, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "node row deleted", settings.Diff(cur, next), []settings.Change{
			dfChange("adapters.smb", "adapters.smb.port", settings.Restart, node),
		})
	})

	t.Run("share", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of(
				"shr",
				settings.Override(entity.ScopeInstallation, entity.ScopeShare),
				dfShare{},
			),
		}
		shareA := rdShareScope(0xA)
		shareB := rdShareScope(0xB)

		instF := dfPut(t, s, rdInstallation, "shr.f", "1", 0)
		instG := dfPut(t, s, rdInstallation, "shr.g", "1", 0)
		aF := dfPut(t, s, shareA, "shr.f", "5", 0)
		dfPut(t, s, shareB, "shr.g", "7", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		dfPut(t, s, rdInstallation, "shr.f", "2", instF)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "installation f", settings.Diff(prev, cur), []settings.Change{
			dfChange("shr", "shr.f", settings.Live, rdInstallation),
		})

		aF = dfPut(t, s, shareA, "shr.f", "6", aF)

		prev = cur
		cur, _ = rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "share A f changed", settings.Diff(prev, cur), []settings.Change{
			dfChange("shr", "shr.f", settings.Live, shareA),
		})

		dfDelete(t, s, shareA, "shr.f", aF)

		prev = cur
		cur, _ = rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "share A f deleted", settings.Diff(prev, cur), []settings.Change{
			dfChange("shr", "shr.f", settings.Live, shareA),
		})

		dfPut(t, s, shareA, "shr.f", "9", 0)

		prev = cur
		cur, _ = rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "share A f set", settings.Diff(prev, cur), []settings.Change{
			dfChange("shr", "shr.f", settings.Live, shareA),
		})

		dfPut(t, s, rdInstallation, "shr.g", "3", instG)

		prev = cur
		cur, _ = rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "installation g", settings.Diff(prev, cur), []settings.Change{
			dfChange("shr", "shr.g", settings.Restart, rdInstallation),
		})
	})
}

func TestDiffInstances(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("pol", settings.At(entity.ScopeShare), dfShare{}),
	}
	shareA := rdShareScope(0xA)
	shareB := rdShareScope(0xB)

	dfPut(t, s, shareA, "pol.f", "1", 0)
	genB := dfPut(t, s, shareB, "pol.f", "1", 0)

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)

	dfPut(t, s, shareB, "pol.f", "2", genB)

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "share B", settings.Diff(prev, cur), []settings.Change{
		dfChange("pol", "pol.f", settings.Live, shareB),
	})
}

func TestDiffSectionWithoutValue(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("refused section", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of("oth", settings.At(entity.ScopeInstallation), rdServerDefault()),
			settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
		}

		dfPut(t, s, rdInstallation, "oth.port", "90", 0)
		s.raw(t, rdInstallation, "srv.port", []byte("garbage"))

		refusedSnap, refused := rdRead(t, s.nodes, table, rdNodeA)
		if refused["srv"] == nil {
			t.Fatalf("refused = %v, want srv refused", refused)
		}

		s.raw(t, rdInstallation, "srv.port", rdRow("81", 1))

		fine, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "section comes up", settings.Diff(refusedSnap, fine), []settings.Change{
			dfChange("srv", "srv.port", settings.Live, rdInstallation),
			dfChange("srv", "srv.host", settings.Restart, rdInstallation),
		})

		dfWant(t, "section refused, not kept", settings.Diff(fine, refusedSnap), nil)
	})

	t.Run("instance", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of("pol", settings.At(entity.ScopeShare), dfShare{
				F: 1,
				G: 1,
			}),
		}
		shareA := rdShareScope(0xA)
		shareB := rdShareScope(0xB)

		dfPut(t, s, shareA, "pol.f", "5", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		genF := dfPut(t, s, shareB, "pol.f", "6", 0)
		genG := dfPut(t, s, shareB, "pol.g", "1", 0)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "share B in cur only", settings.Diff(prev, cur), []settings.Change{
			dfChange("pol", "pol.f", settings.Live, shareB),
		})

		dfDelete(t, s, shareB, "pol.f", genF)
		dfDelete(t, s, shareB, "pol.g", genG)

		next, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "share B's last row deleted", settings.Diff(cur, next), []settings.Change{
			dfChange("pol", "pol.f", settings.Live, shareB),
		})
	})

	t.Run("thing", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), dfSpare{}),
		}
		node := rdNodeScope(rdNodeA)
		key1 := "journal." + dfJournal1 + ".spare_target"
		key2 := "journal." + dfJournal2 + ".spare_target"

		gen1 := dfPut(t, s, node, key1, "3", 0)
		dfPut(t, s, node, key2, "4", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		dfDelete(t, s, node, key1, gen1)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		dfWant(t, "journal's last row deleted", settings.Diff(prev, cur), []settings.Change{
			dfChange("journal", key1, settings.Live, node),
		})
	})
}

func TestDiffWalksDeclaredFields(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("emb", settings.At(entity.ScopeInstallation), dfEmbedded{}),
	}

	every := []settings.Change{
		dfChange("emb", "emb.deep", settings.Restart, rdInstallation),
		dfChange("emb", "emb.top", settings.Live, rdInstallation),
		dfChange("emb", "emb.opt", settings.Live, rdInstallation),
	}

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "nil prev", settings.Diff(nil, prev), every)

	dfPut(t, s, rdInstallation, "emb.deep", "3", 0)

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "embedded field changed", settings.Diff(prev, cur), []settings.Change{
		dfChange("emb", "emb.deep", settings.Restart, rdInstallation),
	})
	// A field json leaves out at its zero value is still listed when the
	// previous value is missing or refused.
	s.raw(t, rdInstallation, "emb.top", []byte("garbage"))

	refused, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "refused prev", settings.Diff(refused, cur), every)
}

func TestDiffNilCur(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := settings.Table{
		settings.Of("srv", settings.At(entity.ScopeInstallation), rdServerDefault()),
	}

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)

	dfWant(t, "nil cur", settings.Diff(prev, nil), nil)
	dfWant(t, "both nil", settings.Diff(nil, nil), nil)
}
