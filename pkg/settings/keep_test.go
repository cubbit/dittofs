package settings_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/settings"
)

// kpTable is two sections of one value each, oth for the installation and
// srv for each node, so a snapshot that loses its node fails Get of srv.
func kpTable() settings.Table {
	return settings.Table{
		settings.Of("oth", settings.At(entity.ScopeInstallation), rdServerDefault()),
		settings.Of("srv", settings.At(entity.ScopeNode), rdServerDefault()),
	}
}

// kpWantNoValue fails unless Get of section in snap is an error and the
// zero value, never the section's default.
func kpWantNoValue(t *testing.T, snap *settings.Snapshot, section string) {
	t.Helper()

	got, err := settings.Get[rdServer](snap, section, rdNodeA)
	if err == nil || got != (rdServer{}) {
		t.Fatalf("Get %s = %+v, %v; want the zero value and an error", section, got, err)
	}
}

// kpWantF fails unless the instance id of section in snap holds f.
func kpWantF(t *testing.T, snap *settings.Snapshot, section string, id byte, f uint32) {
	t.Helper()

	if snap == nil {
		t.Fatalf("instance %x of %s: no snapshot", id, section)
	}

	got, err := settings.InstanceValue(snap, section, [16]byte{id})
	if err != nil {
		t.Fatalf("instance %x of %s: %v", id, section, err)
	}

	if v := got.(dfShare); v.F != f {
		t.Fatalf("instance %x of %s: f %d, want %d", id, section, v.F, f)
	}
}

func TestKeepRefusedSection(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("one value", func(t *testing.T) {
		s := newRdStore(t)
		table := kpTable()

		oth := dfPut(t, s, rdInstallation, "oth.port", "90", 0)
		dfPut(t, s, rdNodeScope(rdNodeA), "srv.port", "81", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		dfPut(t, s, rdInstallation, "oth.port", "91", oth)
		s.raw(t, rdNodeScope(rdNodeA), "srv.port", []byte("garbage"))

		cur, refused := rdRead(t, s.nodes, table, rdNodeA)
		if refused["srv"] == nil {
			t.Fatalf("refused = %v, want srv refused", refused)
		}

		kept := cur.Keep("srv", prev)
		rdWantPort(t, kept, "srv", rdNodeA, 81)
		rdWantPort(t, kept, "oth", rdNodeA, 91)

		kpWantNoValue(t, cur, "srv")
		rdWantPort(t, cur, "oth", rdNodeA, 91)
		rdWantPort(t, prev, "srv", rdNodeA, 81)
		rdWantPort(t, prev, "oth", rdNodeA, 90)

		same := cur.Keep("undeclared", prev)
		kpWantNoValue(t, same, "srv")
		rdWantPort(t, same, "oth", rdNodeA, 91)

		var none *settings.Snapshot

		only := none.Keep("srv", prev)
		rdWantPort(t, only, "srv", rdNodeA, 81)
		kpWantNoValue(t, only, "oth")
	})

	t.Run("one value per share", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of("pol", settings.At(entity.ScopeShare), dfShare{}),
		}

		dfPut(t, s, rdShareScope(0xA), "pol.f", "1", 0)
		genB := dfPut(t, s, rdShareScope(0xB), "pol.f", "1", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		s.raw(t, rdShareScope(0xA), "pol.f", []byte("garbage"))
		dfPut(t, s, rdShareScope(0xB), "pol.f", "2", genB)

		cur, refused := rdRead(t, s.nodes, table, rdNodeA)
		if refused["pol"] == nil {
			t.Fatalf("refused = %v, want pol refused", refused)
		}

		kept := cur.Keep("pol", prev)
		kpWantF(t, kept, "pol", 0xA, 1)
		kpWantF(t, kept, "pol", 0xB, 1)
		kpWantF(t, cur, "pol", 0xB, 2)
	})
}

func TestKeepWithoutPrev(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := kpTable()

	dfPut(t, s, rdInstallation, "oth.port", "90", 0)
	dfPut(t, s, rdNodeScope(rdNodeA), "srv.port", "81", 0)

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	noSrv, _ := rdRead(t, s.nodes, table[:1], rdNodeA)

	for _, prev := range []*settings.Snapshot{nil, noSrv} {
		kept := cur.Keep("srv", prev)
		kpWantNoValue(t, kept, "srv")
		rdWantPort(t, kept, "oth", rdNodeA, 90)
	}

	rdWantPort(t, cur, "srv", rdNodeA, 81)
}

func TestKeepThenDiff(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	table := kpTable()

	oth := dfPut(t, s, rdInstallation, "oth.port", "90", 0)
	dfPut(t, s, rdNodeScope(rdNodeA), "srv.port", "81", 0)

	prev, _ := rdRead(t, s.nodes, table, rdNodeA)
	noSrv, _ := rdRead(t, s.nodes, table[:1], rdNodeA)

	dfPut(t, s, rdInstallation, "oth.port", "91", oth)
	s.raw(t, rdNodeScope(rdNodeA), "srv.port", []byte("garbage"))

	cur, _ := rdRead(t, s.nodes, table, rdNodeA)
	want := []settings.Change{
		dfChange("oth", "oth.port", settings.Live, rdInstallation),
	}

	dfWant(t, "kept from prev", settings.Diff(prev, cur.Keep("srv", prev)), want)

	kept := cur.Keep("srv", noSrv)
	dfWant(t, "nothing to keep", settings.Diff(noSrv, kept), want)

	// A section the kept snapshot holds no value of comes up whole once
	// its row is readable again.
	s.raw(t, rdNodeScope(rdNodeA), "srv.port", rdRow("82", 1))

	fine, _ := rdRead(t, s.nodes, table, rdNodeA)
	dfWant(t, "section back after nothing kept", settings.Diff(kept, fine), []settings.Change{
		dfChange("srv", "srv.port", settings.Live, rdNodeScope(rdNodeA)),
		dfChange("srv", "srv.host", settings.Restart, rdNodeScope(rdNodeA)),
	})
}

// An instance or a thing the previous snapshot does not hold keeps what the
// current read made of it, never the section's defaults: a refused one stays
// refused and a fine one keeps its value.
func TestKeepNewInstances(t *testing.T) {
	prof.Watch(t, prof.Options{})

	t.Run("share", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of("pol", settings.At(entity.ScopeShare), dfShare{
				F: 1,
			}),
		}

		dfPut(t, s, rdShareScope(0xB), "pol.f", "2", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		s.raw(t, rdShareScope(0xA), "pol.f", []byte("garbage"))
		dfPut(t, s, rdShareScope(0xC), "pol.f", "9", 0)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		kept := cur.Keep("pol", prev)

		if got, err := settings.InstanceValue(kept, "pol", [16]byte{0xA}); err == nil {
			t.Fatalf("share a after Keep = %+v, <nil>; want its refusal", got)
		}

		kpWantF(t, kept, "pol", 0xC, 9)
		kpWantF(t, kept, "pol", 0xB, 2)
		dfWant(t, "new share kept", settings.Diff(prev, kept), []settings.Change{
			dfChange("pol", "pol.f", settings.Live, rdShareScope(0xC)),
		})
	})

	t.Run("thing", func(t *testing.T) {
		s := newRdStore(t)
		table := settings.Table{
			settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), dfSpare{
				SpareTarget: 1,
			}),
		}
		node := rdNodeScope(rdNodeA)
		key1 := "journal." + dfJournal1 + ".spare_target"
		key2 := "journal." + dfJournal2 + ".spare_target"
		journal2 := dfJournal2

		dfPut(t, s, node, key1, "2", 0)

		prev, _ := rdRead(t, s.nodes, table, rdNodeA)

		dfPut(t, s, node, key2, "9", 0)

		cur, _ := rdRead(t, s.nodes, table, rdNodeA)
		kept := cur.Keep("journal", prev)

		got, err := settings.ThingValue(kept, "journal", rdNodeA, journal2)
		if err != nil || got.(dfSpare).SpareTarget != 9 {
			t.Fatalf("journal 2 after Keep = %+v, %v; want spare_target 9", got, err)
		}

		dfWant(t, "new journal kept", settings.Diff(prev, kept), []settings.Change{
			dfChange("journal", key2, settings.Live, node),
		})
	})
}
