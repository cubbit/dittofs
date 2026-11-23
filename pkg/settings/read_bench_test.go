package settings_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/settings"
)

// benchShares is the share count the scan's 100 ms ceiling is read at.
const benchShares = 10_000

// benchPrefixes are the fixture's four share sections, in the order a
// share's index chooses among them.
var benchPrefixes = []string{"bench.alpha", "bench.beta", "bench.gamma", "bench.delta"}

// The fixture's other sections.
const (
	benchInstall = "bench.install"
	benchNode    = "bench.node"
	benchJournal = "bench.journal"
)

// benchJournals are the two journals the fixture seeds, in hex as their keys
// hold them.
var benchJournals = []string{
	"3f2a00000000000000000000000000a1",
	"5c2000000000000000000000000000b2",
}

// benchStore is the remote store the fixture's namespaces name: it needs an
// ID, and nothing checks the store exists at this layer.
var benchStore = [16]byte{0: 'b', 15: 1}

// benchFive is the one struct the fixture's seven sections declare, five
// fields each: integers, strings and booleans, no size or duration.
type benchFive struct {
	Replicas int    `json:"replicas" class:"live"`
	Quorum   int    `json:"quorum"   class:"live"`
	Ratio    uint32 `json:"ratio"    class:"live"`
	Tag      string `json:"tag"      class:"live"`
	Strict   bool   `json:"strict"   class:"live"`
}

// benchFields are benchFive's json names, the keys a seeded row holds.
var benchFields = []string{"replicas", "quorum", "ratio", "tag", "strict"}

// Validate refuses a value out of range, one check per field that has a
// range; a boolean's two values are both inside any range.
func (f benchFive) Validate() error {
	switch {
	case f.Replicas > 64:
		return fmt.Errorf("replicas must be at most 64, got %d", f.Replicas)
	case f.Quorum > 9:
		return fmt.Errorf("quorum must be at most 9, got %d", f.Quorum)
	case f.Ratio > 999:
		return fmt.Errorf("ratio must be at most 999, got %d", f.Ratio)
	case len(f.Tag) > 32:
		return fmt.Errorf("tag must hold at most 32 bytes, got %d", len(f.Tag))
	}

	return nil
}

// benchTable is the reconcile's own table: the four share sections, the
// installation's, the node's and the per-journal one the fixture seeds.
func benchTable() settings.Table {
	table := settings.Table{
		settings.Of(benchInstall, settings.At(entity.ScopeInstallation), benchFive{}),
		settings.Of(benchNode, settings.At(entity.ScopeNode), benchFive{}),
		settings.Of(benchJournal, settings.PerThing(entity.ScopeNode, "journal"), benchFive{}),
	}

	for _, prefix := range benchPrefixes {
		table = append(table, settings.Of(prefix, settings.At(entity.ScopeShare), benchFive{}))
	}

	return table
}

// benchSeeded are the share sections the share with index i seeds: all four
// dense, the first i%5 sparse, so a share holds 10 rows on average and 20 at
// most.
func benchSeeded(i int, dense bool) []string {
	if dense {
		return benchPrefixes
	}

	return benchPrefixes[:i%5]
}

// benchValue is the value the field named holds for the share with index i,
// as its row stores it; every one of them is inside Validate's ranges.
func benchValue(name string, i int) string {
	switch name {
	case "replicas":
		return strconv.Itoa(1 + i%4)
	case "quorum":
		return strconv.Itoa(i % 5)
	case "ratio":
		return strconv.Itoa(i % 1000)
	case "strict":
		return strconv.FormatBool(i%2 == 0)
	case "tag":
		return `"tag-` + strconv.Itoa(i%16) + `"`
	}

	return "null"
}

// benchFixture is a seeded store, the table it was seeded for and the answers
// its timed work guards.
type benchFixture struct {
	db    kv.KV
	cp    kvstore.ControlPlane
	nodes kvstore.Nodes
	table settings.Table

	node   entity.NodeID
	shares []entity.Share
	one    []settings.Change
	all    []settings.Change
	prev   *settings.Snapshot
	cur    *settings.Snapshot
}

// seedBench opens a Badger under tb's TempDir, seeds it as the node's own
// writes would: the shares and their namespaces in batches of a thousand,
// then the settings one write a share, then the installation's, the node's
// and the journals' rows, every field set. It then makes the one change
// every timed Diff answers, and reads the two snapshots and the answers the
// guards check against, refusing to run on an unhealthy store.
func seedBench(tb testing.TB, shares int, dense bool) benchFixture {
	tb.Helper()

	node := entity.NodeID{0x1}

	db, err := badgerkv.Open(badgerkv.Options{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Error(err)
		}
	})

	ctx := context.Background()

	if err := kvstore.Init(ctx, db, node); err != nil {
		tb.Fatal(err)
	}

	st, err := kvstore.Open(ctx, db)
	if err != nil {
		tb.Fatal(err)
	}

	f := benchFixture{
		db:    db,
		cp:    st.ControlPlane(),
		nodes: st.Nodes(),
		table: benchTable(),
		node:  node,
	}

	f.shares = make([]entity.Share, shares)

	for from := 0; from < shares; from += 1000 {
		to := min(from+1000, shares)

		if err := f.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			for i := from; i < to; i++ {
				ns := entity.Namespace{
					ID:    entity.NewNamespaceID(),
					Store: benchStore,
				}
				if _, err := tx.PutNamespace(ns, 0); err != nil {
					return err
				}

				f.shares[i] = entity.Share{
					ID:   entity.NewShareID(),
					Name: fmt.Sprintf("share-%d", i),
					Path: fmt.Sprintf("/share-%d", i),
					Root: entity.NewFileID(),
					Namespaces: []entity.NamespaceGen{{
						Generation: 1,
						Namespace:  ns.ID,
					}},
				}

				if _, err := tx.PutShare(f.shares[i], 0); err != nil {
					return err
				}
			}

			return nil
		}); err != nil {
			tb.Fatal(err)
		}
	}

	f.all = make([]settings.Change, 0, 5*len(benchFields)*shares)

	for i, share := range f.shares {
		if err := f.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			for _, prefix := range benchSeeded(i, dense) {
				scope := entity.SettingScope{
					Kind: entity.ScopeShare,
					ID:   share.ID,
				}

				for _, name := range benchFields {
					row := entity.Setting{
						Scope:  scope,
						Key:    prefix + "." + name,
						Value:  []byte(benchValue(name, i)),
						Schema: 1,
					}

					if _, err := tx.PutSetting(row, 0); err != nil {
						return err
					}

					f.all = append(f.all, settings.Change{
						Section: prefix,
						Key:     row.Key,
						Scope:   scope,
					})
				}
			}

			return nil
		}); err != nil {
			tb.Fatal(err)
		}
	}

	nodeScope := entity.SettingScope{
		Kind: entity.ScopeNode,
		ID:   node,
	}

	if err := f.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		scopes := []struct {
			scope   entity.SettingScope
			section string
		}{
			{entity.SettingScope{Kind: entity.ScopeInstallation}, benchInstall},
			{nodeScope, benchNode},
		}

		for _, at := range scopes {
			for _, name := range benchFields {
				row := entity.Setting{
					Scope:  at.scope,
					Key:    at.section + "." + name,
					Value:  []byte(benchValue(name, 0)),
					Schema: 1,
				}

				if _, err := tx.PutSetting(row, 0); err != nil {
					return err
				}

				f.all = append(f.all, settings.Change{
					Section: at.section,
					Key:     row.Key,
					Scope:   at.scope,
				})
			}
		}

		for _, journal := range benchJournals {
			for _, name := range benchFields {
				row := entity.Setting{
					Scope:  nodeScope,
					Key:    benchJournal + "." + journal + "." + name,
					Value:  []byte(benchValue(name, 0)),
					Schema: 1,
				}

				if _, err := tx.PutSetting(row, 0); err != nil {
					return err
				}

				f.all = append(f.all, settings.Change{
					Section: benchJournal,
					Key:     row.Key,
					Scope:   nodeScope,
				})
			}
		}

		return nil
	}); err != nil {
		tb.Fatal(err)
	}

	var refused map[string]error

	f.prev, refused, err = settings.Read(ctx, f.nodes, f.table, node)
	if err != nil {
		tb.Fatal(err)
	}

	if err := benchGuardRead(f.prev, refused); err != nil {
		tb.Fatal(err)
	}

	// The one change every timed Diff answers: share 1's replicas, a section
	// both the sparse and the dense case seed for it.
	flip := entity.Setting{
		Scope: entity.SettingScope{
			Kind: entity.ScopeShare,
			ID:   f.shares[1].ID,
		},
		Key:    benchPrefixes[0] + ".replicas",
		Value:  []byte("5"),
		Schema: 1,
	}

	if err := f.cp.Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		row, found, err := tx.Setting(flip.Scope, flip.Key)
		if err != nil {
			return err
		}

		if !found {
			return errors.New("the fixture's flip row was not seeded")
		}

		_, err = tx.PutSetting(flip, row.Generation)

		return err
	}); err != nil {
		tb.Fatal(err)
	}

	f.cur, refused, err = settings.Read(ctx, f.nodes, f.table, node)
	if err != nil {
		tb.Fatal(err)
	}

	if err := benchGuardRead(f.cur, refused); err != nil {
		tb.Fatal(err)
	}

	f.one = settings.Diff(f.prev, f.cur)
	if err := benchGuardDiff(f.one, []settings.Change{{
		Section: benchPrefixes[0],
		Key:     benchPrefixes[0] + ".replicas",
		Scope:   flip.Scope,
	}}); err != nil {
		tb.Fatal(err)
	}

	if err := benchGuardDiff(settings.Diff(nil, f.cur), f.all); err != nil {
		tb.Fatal(err)
	}

	return f
}

// raw writes value straight under the row key of scope and key, for rows the
// store's own write refuses to make.
func (f benchFixture) raw(tb testing.TB, scope entity.SettingScope, key string, value []byte) {
	tb.Helper()

	k := []byte{keys.KindCFG, byte(scope.Kind)}
	if scope.Kind != entity.ScopeInstallation {
		k = keys.ID(k, scope.ID[:])
	}

	if err := f.db.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set(keys.String(k, []byte(key)), value)
	}); err != nil {
		tb.Fatal(err)
	}
}

// benchTriple is one change's section, key and scope as one comparable
// string; the class is left out, the reconcile applies none.
func benchTriple(c settings.Change) string {
	return c.Section + "|" + c.Key + "|" + c.Scope.Kind.String() + "|" +
		fmt.Sprintf("%x", c.Scope.ID)
}

// benchName is one change as a guard's failure names it.
func benchName(c settings.Change) string {
	return fmt.Sprintf("%s %s at %s %x", c.Section, c.Key, c.Scope.Kind, c.Scope.ID)
}

// benchCounts is how many times each (section, key, scope) triple is listed.
func benchCounts(changes []settings.Change) map[string]int {
	counts := make(map[string]int, len(changes))

	for _, c := range changes {
		counts[benchTriple(c)]++
	}

	return counts
}

// benchGuardRead is the benchmark's own check of Read's answer: a snapshot
// and no refused section. The benchmark runs it outside its timed region, on
// the last iteration's answer, so it leaves no figure out.
func benchGuardRead(snap *settings.Snapshot, refused map[string]error) error {
	if snap == nil {
		return errors.New("Read returned no snapshot")
	}

	if len(refused) == 0 {
		return nil
	}

	prefixes := slices.Sorted(maps.Keys(refused))

	return fmt.Errorf("Read refused %d sections: %s", len(refused), strings.Join(prefixes, ", "))
}

// benchGuardDiff is the benchmark's own check of Diff's answer: exactly the
// wanted (section, key, scope) triples, so an extra key, a missing one, a
// right count with a wrong key, or a right key of the wrong share fails it.
func benchGuardDiff(got, want []settings.Change) error {
	if len(got) != len(want) {
		return fmt.Errorf("Diff listed %d changes, want %d", len(got), len(want))
	}

	gotCounts := benchCounts(got)
	wantCounts := benchCounts(want)

	for triple, n := range wantCounts {
		if got := gotCounts[triple]; got != n {
			change := want[slices.IndexFunc(want, func(c settings.Change) bool {
				return benchTriple(c) == triple
			})]

			return fmt.Errorf("Diff lists %s %d times, want %d", benchName(change), got, n)
		}
	}

	// Equal lengths and every wanted change counted leave no room for an
	// extra one.

	return nil
}

// BenchmarkReconcileScan times the reconcile's own walk and diff at
// benchShares shares' settings on a Badger under b.TempDir(): `sparse`, the
// fixture's own arithmetic, about 10^5 rows, and `dense`, every share's
// every field, 2x10^5 rows. Each case times only Read through Nodes and
// Diff: `read` one scan, `diff_one_key` the diff of the one change, and
// `diff_first` the first reconcile's diff over no previous snapshot, and
// `scan` the scan and the diff together, the reconcile's own shape, the
// figure the one-scan marker's 100 ms trigger is read against. The fixture
// is seeded and both snapshots read before the case runs; each
// sub-benchmark fails itself when Read refuses a section or Diff lists
// another key set than the one it answers, so a bad run fails rather than
// timing a wrong reconcile. Run it as its suite does, with -race off: it
// changes the figure.
func BenchmarkReconcileScan(b *testing.B) {
	for _, c := range []struct {
		name  string
		dense bool
	}{
		{"sparse", false},
		{"dense", true},
	} {
		b.Run(c.name, func(b *testing.B) {
			f := seedBench(b, benchShares, c.dense)
			b.ResetTimer()

			b.Run("read", func(b *testing.B) {
				b.ReportAllocs()

				var snap *settings.Snapshot

				var refused map[string]error

				var readErr error

				for b.Loop() {
					snap, refused, readErr = settings.Read(b.Context(), f.nodes, f.table, f.node)
				}

				if readErr != nil {
					b.Fatalf("Read: %v", readErr)
				}

				if err := benchGuardRead(snap, refused); err != nil {
					b.Fatal(err)
				}
			})

			b.Run("diff_one_key", func(b *testing.B) {
				b.ReportAllocs()

				var changes []settings.Change

				for b.Loop() {
					changes = settings.Diff(f.prev, f.cur)
				}

				if err := benchGuardDiff(changes, f.one); err != nil {
					b.Fatal(err)
				}
			})

			b.Run("diff_first", func(b *testing.B) {
				b.ReportAllocs()

				var changes []settings.Change

				for b.Loop() {
					changes = settings.Diff(nil, f.cur)
				}

				if err := benchGuardDiff(changes, f.all); err != nil {
					b.Fatal(err)
				}
			})

			b.Run("scan", func(b *testing.B) {
				b.ReportAllocs()

				var snap *settings.Snapshot

				var refused map[string]error

				var changes []settings.Change

				var readErr error

				for b.Loop() {
					snap, refused, readErr = settings.Read(b.Context(), f.nodes, f.table, f.node)
					changes = settings.Diff(f.prev, snap)
				}

				if readErr != nil {
					b.Fatalf("Read: %v", readErr)
				}

				if err := benchGuardRead(snap, refused); err != nil {
					b.Fatal(err)
				}

				if err := benchGuardDiff(changes, f.one); err != nil {
					b.Fatal(err)
				}
			})
		})
	}
}

// The fixture holds the rows its own arithmetic says: a share with k
// sections holds 5k rows, no share more than 20, the installation's, the
// node's and the journals' rows present, every field set so the keys equal
// the rows and Read refuses nothing, checked through Get where Get reaches.
// A value out of range is refused, so the fixture's Validate really runs.
func TestReconcileFixtureShape(t *testing.T) {
	prof.Watch(t, prof.Options{})

	for _, c := range []struct {
		name  string
		dense bool
	}{
		{"sparse", false},
		{"dense", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := seedBench(t, 100, c.dense)

			rows := 0
			perShare := map[[16]byte]int{}
			byPrefix := map[string]int{}

			for s, err := range f.nodes.AllSettings(context.Background(), entity.SettingKey{}) {
				if err != nil {
					t.Fatalf("walk the store: %v", err)
				}

				rows++

				for _, prefix := range []string{benchInstall, benchNode, benchJournal} {
					if strings.HasPrefix(s.Key, prefix+".") {
						byPrefix[prefix]++
					}
				}

				if s.Scope.Kind == entity.ScopeShare {
					perShare[s.Scope.ID]++
				}
			}

			for prefix, want := range map[string]int{
				benchInstall: 5,
				benchNode:    5,
				benchJournal: 10,
			} {
				if got := byPrefix[prefix]; got != want {
					t.Errorf("the %s rows: %d, want %d", prefix, got, want)
				}
			}

			// The arithmetic the figure is read against, spelled here:
			// the fixture's own picker is what it checks, not its own twin.
			rowsPerShare := func(i int) int {
				if c.dense {
					return 20
				}

				return (i % 5) * 5
			}

			wantTotal := 20 // the installation's, the node's and the journals'

			for i, share := range f.shares {
				per := rowsPerShare(i)

				if got := perShare[share.ID]; got != per {
					t.Errorf("share %d holds %d rows, want %d", i, got, per)
				}

				if per > 20 {
					t.Errorf("share %d holds %d rows, over 20", i, per)
				}

				wantTotal += per
			}

			if rows != wantTotal {
				t.Errorf("the fixture holds %d rows, want %d", rows, wantTotal)
			}

			if len(f.all) != wantTotal {
				t.Errorf("the fixture names %d changes, want %d, one per row",
					len(f.all), wantTotal)
			}

			if err := benchGuardDiff(settings.Diff(nil, f.cur), f.all); err != nil {
				t.Errorf("Diff over no previous snapshot: %v", err)
			}

			want := benchFive{
				Replicas: 1,
				Tag:      "tag-0",
				Strict:   true,
			}

			got, err := settings.Get[benchFive](f.cur, benchInstall, f.node)
			if err != nil || got != want {
				t.Errorf("the installation's value: %v (%v), want %v", got, err, want)
			}

			got, err = settings.Get[benchFive](f.cur, benchNode, f.node)
			if err != nil || got != want {
				t.Errorf("the node's value: %v (%v), want %v", got, err, want)
			}

			// A fixture value out of range is refused by Read, so
			// Validate really runs over the fixture's rows.
			f.raw(t, entity.SettingScope{
				Kind: entity.ScopeShare,
				ID:   f.shares[1].ID,
			}, benchPrefixes[0]+".replicas", rdRow("1000", 1))

			snap, refused, err := settings.Read(context.Background(), f.nodes, f.table, f.node)
			if err != nil {
				t.Fatalf("Read over the out-of-range row: %v", err)
			}

			if _, ok := refused[benchPrefixes[0]]; !ok {
				t.Errorf("the out-of-range row refused %d sections, want %s among them",
					len(refused), benchPrefixes[0])
			}

			if err := benchGuardRead(snap, refused); err == nil {
				t.Errorf("the guard passed a read with a refused section, want a failure")
			}
		})
	}
}

// The benchmark's guards fail: a refused read, and a Diff that lists an
// extra change, a missing one, a wrong key at the right count, or the right
// key of the wrong share; the right answers pass.
func TestReconcileGuardsFail(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := seedBench(t, 100, false)

	snap, refused, err := settings.Read(context.Background(), f.nodes, f.table, f.node)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if err := benchGuardRead(snap, refused); err != nil {
		t.Errorf("the read guard refused a healthy read: %v", err)
	}

	if err := benchGuardDiff(settings.Diff(f.prev, f.cur), f.one); err != nil {
		t.Errorf("the diff guard refused the one change: %v", err)
	}

	f.raw(t, entity.SettingScope{
		Kind: entity.ScopeShare,
		ID:   f.shares[1].ID,
	}, benchPrefixes[0]+".replicas", []byte{0xFF})

	snap, refused, err = settings.Read(context.Background(), f.nodes, f.table, f.node)
	if err != nil {
		t.Fatalf("Read over the garbage row: %v", err)
	}

	guard := benchGuardRead(snap, refused)
	if guard == nil {
		t.Errorf("the read guard passed a read with a refused section, want a failure")
	} else if !strings.Contains(guard.Error(), benchPrefixes[0]) {
		t.Errorf("the read guard's error %q names no refused section", guard.Error())
	}

	benchShare1 := entity.SettingScope{
		Kind: entity.ScopeShare,
		ID:   f.shares[1].ID,
	}

	benchShare2 := entity.SettingScope{
		Kind: entity.ScopeShare,
		ID:   f.shares[2].ID,
	}

	one := []settings.Change{{
		Section: benchPrefixes[0],
		Key:     benchPrefixes[0] + ".replicas",
		Scope:   benchShare1,
	}}

	wrongKey := []settings.Change{{
		Section: benchPrefixes[0],
		Key:     benchPrefixes[0] + ".quorum",
		Scope:   one[0].Scope,
	}}

	wrongShare := []settings.Change{{
		Section: benchPrefixes[0],
		Key:     one[0].Key,
		Scope:   benchShare2,
	}}

	extra := slices.Clone(one)
	extra = append(extra, settings.Change{
		Section: benchPrefixes[3],
		Key:     benchPrefixes[3] + ".replicas",
		Scope:   one[0].Scope,
	})

	for _, c := range []struct {
		name string
		got  []settings.Change
		want []settings.Change
	}{
		{"an extra change beside the wanted one", extra, one},
		{"no change where one is wanted", nil, one},
		{"a wrong key at the right count", wrongKey, one},
		{"the right key of the wrong share", wrongShare, one},
	} {
		if err := benchGuardDiff(c.got, c.want); err == nil {
			t.Errorf("%s: the diff guard passed, want a failure", c.name)
		}
	}
}
