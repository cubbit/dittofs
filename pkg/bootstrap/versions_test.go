package bootstrap

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/codec"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/settings"
)

// installationKey is the raw key of the installation record.
var installationKey = string([]byte{keys.KindIN})

// gateNode is a node every other rule admits: an ID and both roles.
var gateNode = entity.Node{
	ID:      entity.NodeID{15: 7},
	Address: "10.0.0.1:12049",
	Roles:   entity.NodeRoleProtocol | entity.NodeRoleStorage,
}

// wide is a table whose ranges differ per format, so a swapped bound or
// label shows.
var wide = map[entity.Format]entity.VersionRange{
	entity.FormatStore:    {Min: 1, Max: 1},
	entity.FormatJournal:  {Min: 2, Max: 4},
	entity.FormatBlock:    {Min: 3, Max: 7},
	entity.FormatSettings: {Min: 5, Max: 9},
}

// gateStore is a store on Badger under a test directory, initialised and
// opened as the node opens it, and the KV beneath it.
func gateStore(t *testing.T) (*kvstore.Store, kv.KV) {
	t.Helper()

	ctx := context.Background()

	db, err := badgerkv.Open(badgerkv.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}

	s, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	return s, db
}

// rawKeys is every key and value of db.
func rawKeys(t *testing.T, db kv.KV) map[string]string {
	t.Helper()

	all := map[string]string{}

	err := db.View(context.Background(), func(r kv.Reader) error {
		for kv, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}

			all[string(kv.Key)] = string(kv.Value)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return all
}

// changed is the keys whose value differs between before and after, or that
// only one of them holds.
func changed(before, after map[string]string) []string {
	var keys []string

	for k, v := range after {
		if w, ok := before[k]; !ok || w != v {
			keys = append(keys, k)
		}
	}

	for k := range before {
		if _, ok := after[k]; !ok {
			keys = append(keys, k)
		}
	}

	slices.Sort(keys)

	return keys
}

// nodeRecord reads the node's record, the zero record with only its ID when
// there is none.
func nodeRecord(t *testing.T, s *kvstore.Store) entity.Node {
	t.Helper()

	rec, _, err := s.Nodes().Node(context.Background(), gateNode.ID)
	if err != nil {
		t.Fatal(err)
	}

	return rec
}

// gauges reads each format's value of the gauge named name, without the
// dittofs_ prefix.
func gauges(t *testing.T, reg *metrics.Registry, name string) map[string]float64 {
	t.Helper()

	families, err := reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]float64{}

	for _, f := range families {
		if f.GetName() != metrics.Namespace+"_"+name {
			continue
		}

		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "format" {
					got[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}

	return got
}

// perFormat is a gauge's value for the store, journal, block and settings
// formats.
func perFormat(store, journal, block, settings float64) map[string]float64 {
	return map[string]float64{
		"store":    store,
		"journal":  journal,
		"block":    block,
		"settings": settings,
	}
}

// A refusal of each format, above its range and below it, names the format,
// the active version and the range, and sets no gauge.
func TestRegisterRefusesOutOfRange(t *testing.T) {
	for _, c := range []struct {
		name                     string
		journal, block, settings uint32
		want                     string
	}{
		{
			"journal above", 5, 3, 5,
			"version gate: journal format 5 is active, this node reads 2 to 4",
		},
		{
			"block above", 2, 8, 5,
			"version gate: block format 8 is active, this node reads 3 to 7",
		},
		{
			"settings above", 2, 3, 10,
			"version gate: settings format 10 is active, this node reads 5 to 9",
		},
		{
			"journal below", 1, 3, 5,
			"version gate: journal format 1 is active, this node reads 2 to 4",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, db := gateStore(t)
			kvstoretest.WriteActive(t, db, map[entity.Format]uint32{
				entity.FormatJournal:  c.journal,
				entity.FormatBlock:    c.block,
				entity.FormatSettings: c.settings,
			})

			reg := metrics.New("v", "c")
			err := register(context.Background(), s, gateNode, reg, wide)

			if err == nil || err.Error() != c.want {
				t.Errorf("register: error %v, want %q", err, c.want)
			}

			if rec := nodeRecord(t, s); !reflect.DeepEqual(rec, entity.Node{ID: gateNode.ID}) {
				t.Errorf("node record after the refusal: %+v, want none", rec)
			}

			for _, name := range []string{
				"format_active_version",
				"format_supported_min",
				"format_supported_max",
			} {
				if got := gauges(t, reg, name); len(got) != 0 {
					t.Errorf("%s after the refusal: %v, want no sample", name, got)
				}
			}
		})
	}
}

// A refusal leaves the record of the start before it, ranges included, and
// a store with no record holds none after it.
func TestRegisterRefusalKeepsRecord(t *testing.T) {
	ctx := context.Background()

	s, db := gateStore(t)
	reg := metrics.New("v", "c")

	if err := Register(ctx, s, gateNode, reg); err != nil {
		t.Fatal(err)
	}

	first := nodeRecord(t, s)

	kvstoretest.WriteActive(t, db, map[entity.Format]uint32{entity.FormatJournal: 2})

	again := gateNode
	again.Address = "10.0.0.2:12049"

	if err := Register(ctx, s, again, reg); err == nil {
		t.Fatal("register under journal format 2: accepted, want refused")
	}

	if rec := nodeRecord(t, s); !reflect.DeepEqual(rec, first) {
		t.Errorf("node record after the refusal: %+v, want the first start's %+v", rec, first)
	}

	fresh, freshDB := gateStore(t)
	kvstoretest.WriteActive(t, freshDB, map[entity.Format]uint32{entity.FormatJournal: 2})

	if err := Register(ctx, fresh, gateNode, metrics.New("v", "c")); err == nil {
		t.Fatal("register on a new store under journal format 2: accepted, want refused")
	}

	if rec := nodeRecord(t, fresh); !reflect.DeepEqual(rec, entity.Node{ID: gateNode.ID}) {
		t.Errorf("node record after the refusal on a new store: %+v, want none", rec)
	}
}

// A start under an active version below its range's top keeps it: the
// installation record is the same bytes after, and only the node record
// changed.
func TestRegisterKeepsActive(t *testing.T) {
	s, db := gateStore(t)
	kvstoretest.WriteActive(t, db, map[entity.Format]uint32{entity.FormatJournal: 1})

	before := rawKeys(t, db)
	table := map[entity.Format]entity.VersionRange{
		entity.FormatStore:   {Min: 1, Max: 1},
		entity.FormatJournal: {Min: 1, Max: 2},
	}

	err := register(context.Background(), s, gateNode, metrics.New("v", "c"), table)
	if err != nil {
		t.Fatal(err)
	}

	after := rawKeys(t, db)
	if after[installationKey] != before[installationKey] {
		t.Errorf(
			"installation record changed by a start: %x, want %x",
			after[installationKey],
			before[installationKey],
		)
	}

	got, err := s.Installation(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if want := map[entity.Format]uint32{entity.FormatJournal: 1}; !reflect.DeepEqual(
		got.Active,
		want,
	) {
		t.Errorf("active versions after a start: %v, want %v", got.Active, want)
	}

	if keys := changed(before, after); len(keys) != 1 {
		t.Errorf("keys a start wrote: %x, want only its node record", keys)
	}
}

// A start records the binary's table, the store format's range being
// kvstore's own bounds.
func TestRegisterRecordsRanges(t *testing.T) {
	s, _ := gateStore(t)

	if err := Register(context.Background(), s, gateNode, metrics.New("v", "c")); err != nil {
		t.Fatal(err)
	}

	got := nodeRecord(t, s).Formats
	want := map[entity.Format]entity.VersionRange{
		entity.FormatStore:    {Min: 1, Max: 1},
		entity.FormatJournal:  {Min: 1, Max: 1},
		entity.FormatBlock:    {Min: 1, Max: 1},
		entity.FormatSettings: {Min: 1, Max: 1},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("node record's ranges: %v, want %v", got, want)
	}

	if table := settings.Versions(); !reflect.DeepEqual(got, table) {
		t.Errorf("node record's ranges: %v, want settings.Versions() %v", got, table)
	}

	store := entity.VersionRange{Min: kvstore.MinFormat, Max: kvstore.MaxFormat}
	if got[entity.FormatStore] != store {
		t.Errorf(
			"store format's range: %v, want kvstore's bounds %v",
			got[entity.FormatStore],
			store,
		)
	}
}

// An active version of a format this binary registers no range for, kept
// by the installation's codec, refuses the start, naming the format.
func TestRegisterRefusesUnknownFormat(t *testing.T) {
	ctx := context.Background()

	s, db := gateStore(t)
	kvstoretest.WriteActive(t, db, map[entity.Format]uint32{200: 1})

	in, err := s.Installation(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if in.Active[200] != 1 {
		t.Errorf("active versions read back: %v, want format 200 at 1", in.Active)
	}

	err = Register(ctx, s, gateNode, metrics.New("v", "c"))

	want := "version gate: format 200 is active at version 1, this node registers no range for it"
	if err == nil || err.Error() != want {
		t.Errorf("register: error %v, want %q", err, want)
	}

	if rec := nodeRecord(t, s); !reflect.DeepEqual(rec, entity.Node{ID: gateNode.ID}) {
		t.Errorf("node record after the refusal: %+v, want none", rec)
	}
}

// A new store records no active version: a start passes, writes nothing
// but its node record, and reports the store format's active version only.
func TestRegisterNoActiveEntry(t *testing.T) {
	s, db := gateStore(t)
	reg := metrics.New("v", "c")

	before := rawKeys(t, db)

	if err := Register(context.Background(), s, gateNode, reg); err != nil {
		t.Fatalf("register on a new store: %v, want accepted", err)
	}

	after := rawKeys(t, db)
	if after[installationKey] != before[installationKey] {
		t.Errorf(
			"installation record changed by a start: %x, want %x",
			after[installationKey],
			before[installationKey],
		)
	}

	if keys := changed(before, after); len(keys) != 1 {
		t.Errorf("keys a start wrote: %x, want only its node record", keys)
	}

	if got := nodeRecord(t, s).Formats; !reflect.DeepEqual(got, settings.Versions()) {
		t.Errorf("node record's ranges: %v, want %v", got, settings.Versions())
	}

	got, want := gauges(t, reg, "format_active_version"), map[string]float64{"store": 1}
	if !maps.Equal(got, want) {
		t.Errorf("format_active_version: %v, want %v", got, want)
	}
}

// Active versions that do not decode refuse the start with the decode
// error, the record as it was.
func TestRegisterUndecodableActive(t *testing.T) {
	s, db := gateStore(t)

	// A count of three, then one format and its version.
	kvstoretest.WriteActiveRaw(t, db, []byte{3, byte(entity.FormatJournal), 0, 0, 0, 1})

	err := Register(context.Background(), s, gateNode, metrics.New("v", "c"))
	if !errors.Is(err, codec.ErrTruncated) {
		t.Errorf(
			"register over active versions cut short: error %v, want %v",
			err,
			codec.ErrTruncated,
		)
	}

	want := "version gate: installation record: codec: truncated value"
	if err == nil || err.Error() != want {
		t.Errorf("register over active versions cut short: error %v, want %q", err, want)
	}

	if rec := nodeRecord(t, s); !reflect.DeepEqual(rec, entity.Node{ID: gateNode.ID}) {
		t.Errorf("node record after the refusal: %+v, want none", rec)
	}
}

// After a start each format's gauges hold its range and its active version.
func TestRegisterSeries(t *testing.T) {
	s, db := gateStore(t)
	kvstoretest.WriteActive(t, db, map[entity.Format]uint32{
		entity.FormatJournal:  3,
		entity.FormatBlock:    6,
		entity.FormatSettings: 8,
	})

	reg := metrics.New("v", "c")

	if err := register(context.Background(), s, gateNode, reg, wide); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		want map[string]float64
	}{
		{"format_supported_min", perFormat(1, 2, 3, 5)},
		{"format_supported_max", perFormat(1, 4, 7, 9)},
		{"format_active_version", perFormat(1, 3, 6, 8)},
	} {
		if got := gauges(t, reg, c.name); !maps.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// An active version at either bound of its range passes: through register
// with the wide table at journal's minimum and block's maximum, and through
// Register with this binary's table at every format's one version, the
// store then unchanged but the node record.
func TestRegisterAcceptsBounds(t *testing.T) {
	ctx := context.Background()

	s, db := gateStore(t)
	kvstoretest.WriteActive(t, db, map[entity.Format]uint32{
		entity.FormatJournal:  2,
		entity.FormatBlock:    7,
		entity.FormatSettings: 5,
	})

	if err := register(ctx, s, gateNode, metrics.New("v", "c"), wide); err != nil {
		t.Errorf("register at journal's minimum and block's maximum: %v, want accepted", err)
	}

	actual, actualDB := gateStore(t)
	kvstoretest.WriteActive(t, actualDB, map[entity.Format]uint32{
		entity.FormatJournal:  1,
		entity.FormatBlock:    1,
		entity.FormatSettings: 1,
	})

	before := rawKeys(t, actualDB)

	if err := Register(ctx, actual, gateNode, metrics.New("v", "c")); err != nil {
		t.Errorf("register at every format's one version: %v, want accepted", err)
	}

	if keys := changed(before, rawKeys(t, actualDB)); len(keys) != 1 {
		t.Errorf("keys a start wrote: %x, want only its node record", keys)
	}
}
