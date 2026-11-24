package settings_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"iter"
	"maps"
	"slices"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The settings structs the series tests declare sections over: a per-journal
// section, the SMB adapter's, one with a required key, one with a bound key,
// a remote store's endpoint and a reference field, one with the block format
// version, one per share.
type (
	mJournal struct {
		SpareTarget     uint32 `json:"spare_target"     class:"live"`
		HeadroomRecords uint32 `json:"headroom_records" class:"live"`
	}
	mSMB struct {
		PendingTimeout uint32 `json:"pending_timeout" class:"live"`
	}
	mCommit struct {
		MaxFiles uint32 `json:"max_files" class:"live"`
		MaxBatch uint32 `json:"max_batch" class:"live"`
	}
	mStore struct {
		Endpoint   string           `json:"endpoint"   class:"restart" location:"store"`
		Capacity   uint32           `json:"capacity"   class:"bound"`
		Credential entity.SecretRef `json:"credential" class:"restart"`
		Tag        string           `json:"tag"        class:"restart"`
	}
	mFormat struct {
		Version uint32 `json:"version" class:"next write"`
	}
	mSync struct {
		Quorum  uint32 `json:"quorum"  class:"live"`
		Retries uint32 `json:"retries" class:"live"`
	}
)

func (j mJournal) Validate() error {
	if j.SpareTarget > 65535 {
		return fmt.Errorf("spare target must be at most 65535, got %d", j.SpareTarget)
	}

	if j.HeadroomRecords > 100000 {
		return fmt.Errorf("headroom must be at most 100000, got %d", j.HeadroomRecords)
	}

	return nil
}

func (s mSMB) Validate() error {
	if s.PendingTimeout > 3600 {
		return fmt.Errorf("pending timeout must be at most 3600, got %d", s.PendingTimeout)
	}

	return nil
}

func (c mCommit) Validate() error {
	if c.MaxBatch > 64 {
		return fmt.Errorf("max batch must be at most 64, got %d", c.MaxBatch)
	}

	return nil
}

func (st mStore) Validate() error {
	switch {
	case st.Capacity > 1024:
		return fmt.Errorf("capacity must be at most 1024, got %d", st.Capacity)
	case len(st.Tag) > 32:
		return fmt.Errorf("tag must hold at most 32 bytes, got %q", st.Tag)
	}

	return nil
}

func (f mFormat) Validate() error {
	if f.Version > 999 {
		return fmt.Errorf("version must be at most 999, got %d", f.Version)
	}

	return nil
}

func (y mSync) Validate() error {
	if y.Quorum > 9 {
		return fmt.Errorf("quorum must be at most 9, got %d", y.Quorum)
	}

	if y.Retries > 9 {
		return fmt.Errorf("retries must be at most 9, got %d", y.Retries)
	}

	return nil
}

// seriesPrefixes are the section prefixes seriesTable declares, the values
// the component label may hold beside the unknown key's fixed one.
var seriesPrefixes = []string{
	"journal", "adapters.smb", "group_commit", "test.store", "test.format", "test.pershare",
}

// seriesTable is every section the series tests raise a refusal on.
func seriesTable() settings.Table {
	return settings.Table{
		settings.Of("journal", settings.PerThing(entity.ScopeNode, "journal"), mJournal{}),
		settings.Of("adapters.smb", settings.At(entity.ScopeInstallation), mSMB{}),
		settings.Of(
			"group_commit",
			settings.At(entity.ScopeInstallation),
			mCommit{},
			settings.WriteRole(entity.RoleAdmin),
			settings.Required("MaxFiles"),
		),
		settings.Of(
			"test.store",
			settings.At(entity.ScopeInstallation),
			mStore{},
			settings.SecretRef("Credential", entity.SecretKey),
		),
		settings.Of(
			"test.format",
			settings.At(entity.ScopeInstallation),
			mFormat{},
			settings.FormatVersion("Version"),
		),
		settings.Of("test.pershare", settings.At(entity.ScopeShare), mSync{}),
	}
}

// errScanRead is a scan error a store that stops mid-listing makes: an
// error wrapping none of the ten refusals.
var errScanRead = errors.New("kv: the store stopped mid-scan")

// mGather reads m's families.
func mGather(t *testing.T, m *metrics.Metrics) []*dto.MetricFamily {
	t.Helper()

	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	return mfs
}

// mFamily is the family named name, nil when the registry holds none.
func mFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}

	return nil
}

// mLabels is one sample's labels, as a map.
func mLabels(m *dto.Metric) map[string]string {
	labels := make(map[string]string, len(m.GetLabel()))

	for _, l := range m.GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}

	return labels
}

// mSeries is the counter value of the sample of family name whose labels
// equal want, and whether such a series exists.
func mSeries(mfs []*dto.MetricFamily, name string, want map[string]string) (float64, bool) {
	f := mFamily(mfs, name)
	if f == nil {
		return 0, false
	}

	for _, m := range f.GetMetric() {
		if got := mLabels(m); len(got) == len(want) && maps.Equal(got, want) {
			return m.GetCounter().GetValue(), true
		}
	}

	return 0, false
}

// mSamples is how many series family name holds.
func mSamples(mfs []*dto.MetricFamily, name string) int {
	f := mFamily(mfs, name)
	if f == nil {
		return 0
	}

	return len(f.GetMetric())
}

// mBound is the bound counter's value, which exists from the bind at zero.
func mBound(mfs []*dto.MetricFamily) (float64, bool) {
	f := mFamily(mfs, "dittofs_config_bound_refusals_total")
	if f == nil || len(f.GetMetric()) != 1 {
		return 0, false
	}

	return f.GetMetric()[0].GetCounter().GetValue(), true
}

// One refusal of each of the ten errors raises exactly one series of
// config_refused_total, under the error's own reason and its section's
// declared prefix, and the bound series only for the bound refusal. The
// cases share one store: only the newer-schema case stores a row of its
// own, so the required case runs before it.
func TestRefusedSeriesOneReasonPerError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)

	ref := "[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16]"

	cases := []struct {
		what      string
		before    func(*testing.T, store)
		scope     entity.SettingScope
		key       string
		value     string
		caller    entity.APIRole
		holds     func(entity.ControlPlaneTx) settings.HoldsContent
		extra     settings.Table
		remove    bool
		schema    uint32
		refusal   error
		reason    string
		component string
	}{
		{
			what:      "unknown key under a declared prefix",
			scope:     installation,
			key:       "adapters.smb.nope",
			value:     "1",
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrUnknownKey,
			reason:    "unknown_key",
			component: "adapters.smb",
		},
		{
			what:      "key at a scope the section lacks",
			scope:     nodeA,
			key:       "adapters.smb.pending_timeout",
			value:     "1",
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrWrongScope,
			reason:    "wrong_scope",
			component: "adapters.smb",
		},
		{
			what:      "role below the section's",
			scope:     installation,
			key:       "group_commit.max_batch",
			value:     "1",
			caller:    entity.RoleOperator,
			refusal:   settings.ErrForbidden,
			reason:    "forbidden",
			component: "group_commit",
		},
		{
			what:      "missing required key",
			scope:     installation,
			key:       "group_commit.max_batch",
			value:     "1",
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrRequired,
			reason:    "required",
			component: "group_commit",
		},
		{
			what: "stored row of a newer schema",
			before: func(t *testing.T, s store) {
				s.raw(t, installation, "group_commit.max_files", rowValue("1", 1, 9999))
			},
			scope:     installation,
			key:       "group_commit.max_batch",
			value:     "1",
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrNewerSchema,
			reason:    "newer_schema",
			component: "group_commit",
		},
		{
			what:      "value Validate refuses",
			scope:     nodeA,
			key:       "journal." + journalID + ".spare_target",
			value:     "70000",
			caller:    entity.RoleOperator,
			refusal:   settings.ErrInvalid,
			reason:    "invalid",
			component: "journal",
		},
		{
			what:   "bound key with content",
			scope:  installation,
			key:    "test.store.capacity",
			value:  "8",
			caller: entity.RoleAdmin,
			before: func(t *testing.T, s store) {
				s.netgroup(t, contentName(installation))
			},
			holds:     holdsContent,
			refusal:   settings.ErrBound,
			reason:    "bound",
			component: "test.store",
		},
		{
			what:      "reference field put",
			scope:     installation,
			key:       "test.store.credential",
			value:     ref,
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrSecretRef,
			reason:    "secret_ref",
			component: "test.store",
		},
		{
			what:      "store location change",
			scope:     installation,
			key:       "test.store.endpoint",
			value:     `"https://x.example"`,
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrUnproven,
			reason:    "unproven",
			component: "test.store",
		},
		{
			what:      "delete of an undeclared key by an operator",
			scope:     installation,
			key:       "adapters.smb.nope",
			remove:    true,
			caller:    entity.RoleOperator,
			refusal:   settings.ErrForbidden,
			reason:    "forbidden",
			component: "adapters.smb",
		},
		{
			what:      "put whose own schema is newer",
			scope:     installation,
			key:       "adapters.smb.pending_timeout",
			value:     "1",
			schema:    9999,
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrNewerSchema,
			reason:    "newer_schema",
			component: "adapters.smb",
		},
		{
			what:  "role below the key's own",
			scope: installation,
			key:   "test.keys.admu",
			value: "true",
			extra: settings.Table{
				settings.Of("test.keys", settings.At(entity.ScopeInstallation), keyedSettings{},
					settings.KeyRole("Admu", entity.RoleAdmin),
				),
			},
			caller:    entity.RoleOperator,
			refusal:   settings.ErrForbidden,
			reason:    "forbidden",
			component: "test.keys",
		},
		{
			what:  "refusal of the section's CheckTx",
			scope: installation,
			key:   "test.refuser.pending_timeout",
			value: "1",
			extra: settings.Table{
				settings.Of(
					"test.refuser",
					settings.At(entity.ScopeInstallation),
					mSMB{},
					settings.CheckTx(func(_ entity.ControlPlaneTx, w settings.Write, _ mSMB) error {
						return w.Refuse("the test's own rule")
					}),
				),
			},
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrInvalid,
			reason:    "invalid",
			component: "test.refuser",
		},
		{
			what:      "format version past the range",
			scope:     installation,
			key:       "test.format.version",
			value:     "5",
			caller:    entity.RoleAdmin,
			refusal:   settings.ErrFormatVersion,
			reason:    "format_version",
			component: "test.format",
		},
	}

	for _, c := range cases {
		m := metrics.New("t", "t")
		tb := append(seriesTable(), c.extra...).WithMetrics(m)

		if c.before != nil {
			c.before(t, s)
		}

		row := setting(c.scope, c.key, c.value)
		if c.schema != 0 {
			row.Schema = c.schema
		}

		w := settings.Put(row)
		if c.remove {
			w = settings.Delete(c.scope, c.key)
		}

		var err error
		if c.holds != nil {
			err = s.checkBuilt(tb, c.caller, w, c.holds)
		} else {
			err = s.check(tb, c.caller, w)
		}

		refused(t, c.what, err, c.refusal, "")

		mfs := mGather(t, m)
		want := map[string]string{"component": c.component, "reason": c.reason}

		got, ok := mSeries(mfs, "dittofs_config_refused_total", want)
		if !ok || got != 1 {
			t.Errorf("%s: config_refused_total%v = %v (series %v), want 1",
				c.what, want, got, ok)
		}

		if n := mSamples(mfs, "dittofs_config_refused_total"); n != 1 {
			t.Errorf("%s: config_refused_total holds %d series, want 1", c.what, n)
		}

		bound, ok := mBound(mfs)
		if !ok {
			t.Errorf("%s: config_bound_refusals_total is not in the registry", c.what)
		}

		wantBound := 0.0
		if errors.Is(c.refusal, settings.ErrBound) {
			wantBound = 1
		}

		if bound != wantBound {
			t.Errorf("%s: config_bound_refusals_total = %v, want %v", c.what, bound, wantBound)
		}
	}
}

// A bound refusal raises both series; a bound key rewritten with its own
// value and a bound put with no content raise neither, and a store error
// holds reads uncounted, as an error that wraps no refusal.
func TestBoundRefusalRaisesBothSeries(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	s.netgroup(t, contentName(installation))

	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)
	lbl := map[string]string{"component": "test.store", "reason": "bound"}

	refused(t, "bound change with content", s.checkBuilt(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "test.store.capacity", "8")), holdsContent),
		settings.ErrBound, "")

	if got, ok := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); !ok || got != 1 {
		t.Errorf("after the bound refusal: config_refused_total%v = %v (series %v), want 1",
			lbl, got, ok)
	}

	if got, ok := mBound(mGather(t, m)); !ok || got != 1 {
		t.Errorf("after the bound refusal: config_bound_refusals_total = %v (series %v), want 1",
			got, ok)
	}

	err := s.checkBuilt(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "test.store.capacity", "9")), failingHolds)
	if !errors.Is(err, errHolds) || errors.Is(err, settings.ErrBound) {
		t.Errorf("holds' store error: err = %v, want %v and not ErrBound", err, errHolds)
	}

	if got, ok := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); !ok || got != 1 {
		t.Errorf("after holds' error: config_refused_total%v = %v (series %v), want 1",
			lbl, got, ok)
	}

	if got, ok := mBound(mGather(t, m)); !ok || got != 1 {
		t.Errorf("after holds' error: config_bound_refusals_total = %v (series %v), want 1",
			got, ok)
	}

	empty := open(t)

	passed(t, "bound change with no content", empty.checkBuilt(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "test.store.capacity", "8")), holdsContent))

	passed(t, "bound key rewritten with its own value", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "test.store.capacity", "0"))))

	if got, _ := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); got != 1 {
		t.Errorf("after the passing writes: config_refused_total%v = %v, want 1", lbl, got)
	}

	if got, ok := mBound(mGather(t, m)); !ok || got != 1 {
		t.Errorf("after the passing writes: config_bound_refusals_total = %v, want 1", got)
	}
}

// An unknown key takes the one fixed component value whatever its spelling,
// so many spellings raise the one series instead of one each.
func TestRefusedUnknownKeyFixedComponent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	keys := []string{
		"nosuch.key",
		"MARKER-9d2e.key", // gitleaks:allow, a test key name, not a secret
		"bad\nkey",
		"\"" + strings.Repeat(".dot.run", 64) + ".key",
		strings.Repeat("a.", 2047) + "key",
	}

	for _, key := range keys {
		refused(t, key, s.check(tb, entity.RoleAdmin,
			settings.Put(setting(installation, key, "1"))), settings.ErrUnknownKey, "")
	}

	mfs := mGather(t, m)
	lbl := map[string]string{"component": "unknown", "reason": "unknown_key"}

	if got, ok := mSeries(mfs, "dittofs_config_refused_total", lbl); !ok || got != 5 {
		t.Errorf("after five spellings: config_refused_total%v = %v (series %v), want 5",
			lbl, got, ok)
	}

	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 1 {
		t.Errorf("after five spellings: config_refused_total holds %d series, want 1", n)
	}
}

// An unknown field under a declared prefix carries that prefix, so an
// operator sees which section they mistyped, not the fixed value.
func TestRefusedUnknownFieldUnderDeclaredPrefix(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	refused(t, "unknown field", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "journal.nope", "1"))), settings.ErrUnknownKey, "")

	mfs := mGather(t, m)
	lbl := map[string]string{"component": "journal", "reason": "unknown_key"}

	if got, ok := mSeries(mfs, "dittofs_config_refused_total", lbl); !ok || got != 1 {
		t.Errorf("unknown field: config_refused_total%v = %v (series %v), want 1",
			lbl, got, ok)
	}

	if _, ok := mSeries(mfs, "dittofs_config_refused_total",
		map[string]string{"component": "unknown", "reason": "unknown_key"}); ok {
		t.Errorf("unknown field under a declared prefix took the fixed component value")
	}
}

// A thing's ID never reaches a label: refusals of keys that differ only in
// their journal raise the one series, and a key that carries its adapter in
// its name raises the section's declared prefix, never a name cut from the
// key.
func TestThingIDNeverALabel(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	ids := []string{
		journalID,
		"5c2000000000000000000000000000b2",
		"MARKER-9d2e",
	}

	for _, id := range ids {
		refused(t, "journal "+id, s.check(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, "journal."+id+".spare_target", "70000"))),
			settings.ErrInvalid, "")
	}

	refused(t, "as-written adapter key", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(nodeA, "adapters.smb.pending_timeout", "1"))),
		settings.ErrWrongScope, "")

	mfs := mGather(t, m)

	lbl := map[string]string{"component": "journal", "reason": "invalid"}
	if got, ok := mSeries(mfs, "dittofs_config_refused_total", lbl); !ok || got != 3 {
		t.Errorf("three journals: config_refused_total%v = %v (series %v), want 3",
			lbl, got, ok)
	}

	adapter := map[string]string{"component": "adapters.smb", "reason": "wrong_scope"}
	if got, ok := mSeries(mfs, "dittofs_config_refused_total", adapter); !ok || got != 1 {
		t.Errorf("adapter key: config_refused_total%v = %v (series %v), want 1",
			adapter, got, ok)
	}

	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 2 {
		t.Errorf("config_refused_total holds %d series, want 2", n)
	}
}

// No label of either series holds the marker, in a key, in a thing's ID, in
// a refused value or in the stored value of a refused row: every label
// value stays in the closed set of declared prefixes, the fixed value and
// the ten reasons.
func TestNoLabelHoldsMarker(t *testing.T) {
	prof.Watch(t, prof.Options{})

	const marker = "MARKER-9d2e"

	s := open(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	refusals := []settings.Write{
		settings.Put(setting(installation, marker+".key", "1")),
		settings.Put(setting(nodeA, "journal."+marker+".spare_target", "70000")),
		settings.Put(setting(installation, "test.store.credential", `"`+marker+`"`)),
		settings.Put(setting(installation, "test.store.capacity", marker)),
		settings.Put(
			setting(installation, "test.store.tag", `"`+strings.Repeat(marker+" ", 4)+`"`),
		),
	}

	for _, w := range refusals {
		if err := s.check(tb, entity.RoleAdmin, w); err == nil {
			t.Errorf("the write of %s passed, want a refusal", w.Key)
		}
	}

	rd := newRdStore(t)
	rd.raw(t, rdShareScope(0xA), "test.pershare.quorum", []byte(marker))

	_, refusedRead, err := settings.Read(context.Background(), rd.nodes, tb, rdNodeA)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if _, ok := refusedRead["test.pershare"]; !ok {
		t.Errorf("Read refused %d sections, want test.pershare among them", len(refusedRead))
	}

	closed := map[string]bool{"unknown": true}
	for _, p := range seriesPrefixes {
		closed[p] = true
	}

	for _, name := range settings.ReasonNames() {
		closed[name] = true
	}

	for _, mf := range mGather(t, m) {
		if mf.GetName() != "dittofs_config_refused_total" &&
			mf.GetName() != "dittofs_config_bound_refusals_total" {
			continue
		}

		for _, mt := range mf.GetMetric() {
			for _, l := range mt.GetLabel() {
				if strings.Contains(l.GetName(), marker) || strings.Contains(l.GetValue(), marker) {
					t.Errorf("%s holds the marker in %q = %q",
						mf.GetName(), l.GetName(), l.GetValue())
				}

				if !closed[l.GetValue()] {
					t.Errorf("%s holds %q = %q outside the closed set",
						mf.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
}

// A thousand unknown keys and a thousand journal IDs leave the registry with
// exactly two series, one per fixed component and one per section: the
// series count does not grow with the input.
func TestRefusedSeriesCardinality(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	for i := range 1000 {
		if err := s.check(tb, entity.RoleAdmin,
			settings.Put(setting(installation, fmt.Sprintf("nosuch%d.key", i), "1"))); err == nil {
			t.Fatalf("the write of nosuch%d.key passed, want a refusal", i)
		}

		id := fmt.Sprintf("journal%04d0000000000000000000000ff", i)
		if err := s.check(tb, entity.RoleOperator,
			settings.Put(setting(nodeA, "journal."+id+".spare_target", "70000"))); err == nil {
			t.Fatalf("the write of journal %d passed, want a refusal", i)
		}
	}

	mfs := mGather(t, m)

	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 2 {
		t.Errorf("after 2000 refusals: config_refused_total holds %d series, want 2", n)
	}

	unknown := map[string]string{"component": "unknown", "reason": "unknown_key"}
	if got, ok := mSeries(mfs, "dittofs_config_refused_total", unknown); !ok || got != 1000 {
		t.Errorf("unknown keys: config_refused_total%v = %v (series %v), want 1000",
			unknown, got, ok)
	}

	journal := map[string]string{"component": "journal", "reason": "invalid"}
	if got, ok := mSeries(mfs, "dittofs_config_refused_total", journal); !ok || got != 1000 {
		t.Errorf("journals: config_refused_total%v = %v (series %v), want 1000",
			journal, got, ok)
	}
}

// Read counts a refused section once per pass: a section refused by a
// garbage row and read twice counts two, the read after the row is repaired
// adds nothing, and a healthy read creates no series.
func TestReadRefusalCountedPerPass(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	s.raw(t, rdInstallation, "adapters.smb.pending_timeout", []byte{0xFF})

	lbl := map[string]string{"component": "adapters.smb", "reason": "invalid"}

	for pass, want := range []float64{1, 2} {
		_, refusedRead, err := settings.Read(context.Background(), s.nodes, tb, rdNodeA)
		if err != nil {
			t.Fatalf("pass %d: Read: %v", pass+1, err)
		}

		if _, ok := refusedRead["adapters.smb"]; !ok {
			t.Fatalf("pass %d: Read refused %d sections, want adapters.smb among them",
				pass+1, len(refusedRead))
		}

		if got, ok := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); !ok ||
			got != want {
			t.Errorf("pass %d: config_refused_total%v = %v (series %v), want %v",
				pass+1, lbl, got, ok, want)
		}
	}

	s.raw(t, rdInstallation, "adapters.smb.pending_timeout", rdRow("81", 1))

	if _, refusedRead, err := settings.Read(context.Background(), s.nodes, tb, rdNodeA); err !=
		nil {
		t.Fatalf("Read after the repair: %v", err)
	} else if _, ok := refusedRead["adapters.smb"]; ok {
		t.Errorf("Read after the repair still refuses adapters.smb")
	}

	if got, _ := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); got != 2 {
		t.Errorf("read after the repair: config_refused_total%v = %v, want 2", lbl, got)
	}

	healthy := newRdStore(t)
	healthy.put(t, rdInstallation, "group_commit.max_files", "4096")

	clean := metrics.New("t", "t")

	if _, refusedRead, err := settings.Read(context.Background(), healthy.nodes,
		seriesTable().WithMetrics(clean), rdNodeA); err != nil {
		t.Fatalf("healthy Read: %v", err)
	} else if len(refusedRead) != 0 {
		t.Errorf("a healthy read refused %d sections, want 0", len(refusedRead))
	}

	if f := mFamily(mGather(t, clean), "dittofs_config_refused_total"); f != nil {
		t.Errorf("a healthy read created %d series of config_refused_total", len(f.GetMetric()))
	}
}

// A Read over several sections refused for different causes raises one
// reason per cause, each under its own prefix, and none for the sections
// that read fine.
func TestReadRefusalReasons(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	s.raw(t, rdInstallation, "adapters.smb.pending_timeout", []byte{0xFF})
	s.raw(t, rdInstallation, "test.format.version", rdRow("1", 2))

	_, refusedRead, err := settings.Read(context.Background(), s.nodes, tb, rdNodeA)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	for prefix := range refusedRead {
		switch prefix {
		case "adapters.smb", "group_commit", "test.format":
		default:
			t.Errorf("Read refused %q, which holds a healthy section", prefix)
		}
	}

	mfs := mGather(t, m)
	want := []struct {
		component string
		reason    string
	}{
		{"adapters.smb", "invalid"},
		{"group_commit", "required"},
		{"test.format", "newer_schema"},
	}

	for _, w := range want {
		lbl := map[string]string{"component": w.component, "reason": w.reason}

		if got, ok := mSeries(mfs, "dittofs_config_refused_total", lbl); !ok || got != 1 {
			t.Errorf("Read refusals: config_refused_total%v = %v (series %v), want 1",
				lbl, got, ok)
		}
	}

	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 3 {
		t.Errorf("config_refused_total holds %d series, want 3", n)
	}
}

// scanFails is a SettingsReader over the real listing that yields the
// listing's pairs until it has yielded one bad row, and then a scan error in
// place of the rest: a store that dies mid-listing once it has already seen
// a bad row. It is a fault wrapper around the real listing, not a fake of
// it.
type scanFails struct {
	inner entity.SettingsReader
	err   error
}

func (s scanFails) AllSettings(
	ctx context.Context,
	after entity.SettingKey,
) iter.Seq2[entity.Setting, error] {
	return func(yield func(entity.Setting, error) bool) {
		for got, err := range s.inner.AllSettings(ctx, after) {
			if errors.Is(err, entity.ErrBadRow) {
				if !yield(got, err) {
					return
				}

				yield(entity.Setting{}, s.err)

				return
			}

			if !yield(got, err) {
				return
			}
		}
	}
}

// A scan that ends in an error, after one bad row of a declared section,
// records no refusal: the read returns the scan's error, and neither series
// holds a sample.
func TestReadScanFailureCountsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	s.raw(t, rdInstallation, "adapters.smb.pending_timeout", []byte{0xFF})

	r := scanFails{
		inner: s.nodes,
		err:   errScanRead,
	}

	snap, refused, err := settings.Read(context.Background(), r, tb, rdNodeA)
	if !errors.Is(err, errScanRead) {
		t.Fatalf("Read over a failing scan: err = %v, want %v", err, errScanRead)
	}

	if snap != nil {
		t.Errorf("Read over a failing scan returned a snapshot, want nil")
	}

	if refused != nil {
		t.Errorf("Read over a failing scan returned %d refused sections, want none",
			len(refused))
	}

	mfs := mGather(t, m)
	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 0 {
		t.Errorf("a failed scan recorded %d series of config_refused_total, want none", n)
	}

	if got, ok := mBound(mfs); !ok || got != 0 {
		t.Errorf("a failed scan: config_bound_refusals_total = %v (series %v), want 0",
			got, ok)
	}
}

// A section refused in several instances, for several causes, counts once
// per pass, under the cause that comes first in the reasons table's order,
// whichever order the join holds them in.
func TestReadSectionRefusedForTwoCauses(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := newRdStore(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	s.raw(t, rdShareScope(0xA), "test.pershare.quorum", []byte{0xFF})
	s.raw(t, rdShareScope(0xB), "test.pershare.quorum", rdRow("1", 2))

	lbl := map[string]string{"component": "test.pershare", "reason": "newer_schema"}

	for pass, want := range []float64{1, 2} {
		_, refused, err := settings.Read(context.Background(), s.nodes, tb, rdNodeA)
		if err != nil {
			t.Fatalf("pass %d: Read: %v", pass+1, err)
		}

		if got, ok := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); !ok ||
			got != want {
			t.Errorf("pass %d: config_refused_total%v = %v (series %v), want %v",
				pass+1, lbl, got, ok, want)
		}

		if _, ok := refused["test.pershare"]; !ok {
			t.Errorf("pass %d: Read refused %d sections, want test.pershare among them",
				pass+1, len(refused))
		}
	}

	mfs := mGather(t, m)

	for _, reason := range []string{"invalid", "required", "unproven"} {
		bad := map[string]string{"component": "test.pershare", "reason": reason}

		if got, ok := mSeries(mfs, "dittofs_config_refused_total", bad); ok {
			t.Errorf("the per-share refusal also raised %v = %v, want nothing", bad, got)
		}
	}
}

// Ten refused writes raise ten in all, whatever the writes read to be
// judged, and a write that passes raises nothing.
func TestCheckRefusalCountedOncePerWrite(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	for range 10 {
		refused(t, "missing required key", s.check(tb, entity.RoleAdmin,
			settings.Put(setting(installation, "group_commit.max_batch", "1"))),
			settings.ErrRequired, "")
	}

	lbl := map[string]string{"component": "group_commit", "reason": "required"}
	if got, ok := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); !ok || got != 10 {
		t.Errorf("ten refused writes: config_refused_total%v = %v (series %v), want 10",
			lbl, got, ok)
	}

	if n := mSamples(mGather(t, m), "dittofs_config_refused_total"); n != 1 {
		t.Errorf("config_refused_total holds %d series, want 1", n)
	}

	passed(t, "a passing journal write", s.check(tb, entity.RoleOperator,
		settings.Put(setting(nodeA, "journal."+journalID+".spare_target", "1"))))

	if got, _ := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); got != 10 {
		t.Errorf("after a passing write: config_refused_total%v = %v, want 10", lbl, got)
	}
}

// A store error checked inside the write and a CheckTx refusal that wraps
// none of the ten come back as they are and raise no series: no invented
// reason breaks the closed list.
func TestCheckErrorOutsideTheListCountsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	errOwnPlain := errors.New("test: a check of the test's own, no refusal inside")

	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	txTable := append(seriesTable(), settings.Of(
		"test.tx",
		settings.At(entity.ScopeInstallation),
		mSMB{},
		settings.CheckTx(func(_ entity.ControlPlaneTx, _ settings.Write, _ mSMB) error {
			return errOwnPlain
		}),
	)).WithMetrics(m)

	s := open(t)

	err := s.check(txTable, entity.RoleAdmin,
		settings.Put(setting(installation, "test.tx.pending_timeout", "1")))
	if !errors.Is(err, errOwnPlain) {
		t.Fatalf("the CheckTx refusal: err = %v, want %v", err, errOwnPlain)
	}

	if n := mSamples(mGather(t, m), "dittofs_config_refused_total"); n != 0 {
		t.Errorf("the CheckTx refusal raised %d series, want none", n)
	}

	if got, ok := mBound(mGather(t, m)); !ok || got != 0 {
		t.Errorf("after the CheckTx refusal: config_bound_refusals_total = %v, want 0", got)
	}

	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	fault := kvtest.NewFaultKV(db)

	sched, err := sim.NewSchedule(1, nil, sim.Rule{
		Op:  kvtest.OpGet,
		Key: "CFG‖*",
		Action: sim.FaultAction{
			Err: errScanRead,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	fault.SetFault(sched)

	if err := kvstore.Init(context.Background(), fault, [16]byte{1}); err != nil {
		t.Fatal(err)
	}

	st, err := kvstore.Open(context.Background(), fault)
	if err != nil {
		t.Fatal(err)
	}

	s2 := store{
		cp: st.ControlPlane(),
		db: db,
	}

	err = s2.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "adapters.smb.pending_timeout", "1")))

	if !errors.Is(err, errScanRead) {
		t.Fatalf("the store error: err = %v, want %v", err, errScanRead)
	}

	for _, refusal := range []error{
		settings.ErrUnknownKey, settings.ErrWrongScope, settings.ErrNewerSchema,
		settings.ErrForbidden, settings.ErrRequired, settings.ErrInvalid, settings.ErrBound,
		settings.ErrSecretRef, settings.ErrFormatVersion, settings.ErrUnproven,
	} {
		if errors.Is(err, refusal) {
			t.Fatalf("the store error wraps %v, want no refusal", refusal)
		}
	}

	mfs := mGather(t, m)

	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 0 {
		t.Errorf("the store error raised %d series, want none", n)
	}

	if got, ok := mBound(mfs); !ok || got != 0 {
		t.Errorf("after the store error: config_bound_refusals_total = %v, want 0", got)
	}
}

// The reasons table covers every sentinel errors.go declares, exactly, each
// under a lower-case name of underscores only.
func TestReasonTableCoversEveryError(t *testing.T) {
	prof.Watch(t, prof.Options{})

	sentinels := map[string]error{
		"ErrUnknownKey":    settings.ErrUnknownKey,
		"ErrWrongScope":    settings.ErrWrongScope,
		"ErrNewerSchema":   settings.ErrNewerSchema,
		"ErrForbidden":     settings.ErrForbidden,
		"ErrRequired":      settings.ErrRequired,
		"ErrInvalid":       settings.ErrInvalid,
		"ErrBound":         settings.ErrBound,
		"ErrSecretRef":     settings.ErrSecretRef,
		"ErrFormatVersion": settings.ErrFormatVersion,
		"ErrUnproven":      settings.ErrUnproven,
	}

	f, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	declared := []string{}

	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}

		for _, spec := range g.Specs {
			for _, name := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(name.Name, "Err") {
					declared = append(declared, name.Name)
				}
			}
		}
	}

	slices.Sort(declared)

	if got, want := declared, slices.Sorted(maps.Keys(sentinels)); !slices.Equal(got, want) {
		t.Errorf("errors.go declares %v, want %v, one reason row each", got, want)
	}

	reasons := map[error]string{}

	for name, sentinel := range sentinels {
		reason, ok := settings.ReasonOf(sentinel)
		if !ok {
			t.Errorf("%s: no reason name, want one", name)

			continue
		}

		for _, r := range reason {
			if (r < 'a' || r > 'z') && r != '_' {
				t.Errorf("%s counts under %q, not lower case and underscores", name, reason)

				break
			}
		}

		reasons[sentinel] = reason
	}

	if len(reasons) != len(sentinels) {
		t.Errorf("the reasons table maps %d errors, want %d", len(reasons), len(sentinels))
	}

	names := settings.ReasonNames()
	if len(names) != len(sentinels) {
		t.Errorf("the reasons table holds %d names %v, want %d", len(names), names, len(sentinels))
	}

	slices.Sort(names)

	derived := slices.Sorted(maps.Values(reasons))
	if !slices.Equal(names, derived) {
		t.Errorf("the reasons table holds %v, want %v", names, derived)
	}
}

// Both series answer a refusal with the shape the dashboards read: a
// labelled counter whose help text says Read counts passes, and a plain
// counter bound at zero.
func TestSeriesShape(t *testing.T) {
	prof.Watch(t, prof.Options{})

	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	s := open(t)
	refused(t, "unknown field", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "adapters.smb.nope", "1"))),
		settings.ErrUnknownKey, "")

	mfs := mGather(t, m)

	f := mFamily(mfs, "dittofs_config_refused_total")
	if f == nil {
		t.Fatal("config_refused_total is not in the registry")
	}

	if f.GetType() != dto.MetricType_COUNTER {
		t.Errorf("config_refused_total is a %v, want a counter", f.GetType())
	}

	names := map[string]bool{}

	for _, mt := range f.GetMetric() {
		for _, l := range mt.GetLabel() {
			names[l.GetName()] = true
		}
	}

	if len(names) != 2 || !names["component"] || !names["reason"] {
		t.Errorf("config_refused_total's labels are %v, want component and reason", names)
	}

	help := f.GetHelp()
	if !strings.Contains(help, "Read") || !strings.Contains(help, "passes") {
		t.Errorf("config_refused_total's help is %q, want it to say Read counts passes", help)
	}

	b := mFamily(mfs, "dittofs_config_bound_refusals_total")
	if b == nil {
		t.Fatal("config_bound_refusals_total is not in the registry")
	}

	if b.GetType() != dto.MetricType_COUNTER {
		t.Errorf("config_bound_refusals_total is a %v, want a counter", b.GetType())
	}

	if len(b.GetMetric()) != 1 || len(b.GetMetric()[0].GetLabel()) != 0 {
		t.Errorf("config_bound_refusals_total is not one plain counter")
	}

	const wantHelp = "Writes refused because the setting is bound and content exists."

	if got := b.GetHelp(); got != wantHelp {
		t.Errorf("config_bound_refusals_total's help is %q, want %q", got, wantHelp)
	}
}

// A table no section declares, given a registry all the same, refuses an
// unknown key as usual and counts nothing: an empty table never reaches
// into itself for a series, and the bound counter it bound stays at zero.
func TestEmptyTableCountsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	s := open(t)
	m := metrics.New("t", "t")
	tb := settings.Table{}.WithMetrics(m)

	refused(t, "unknown key on an empty table", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "nosuch.key", "1"))),
		settings.ErrUnknownKey, "")

	mfs := mGather(t, m)
	if n := mSamples(mfs, "dittofs_config_refused_total"); n != 0 {
		t.Errorf("an empty table created %d series of config_refused_total, want none", n)
	}

	if got, ok := mBound(mfs); !ok || got != 0 {
		t.Errorf("an empty table's config_bound_refusals_total = %v (series %v), want 0",
			got, ok)
	}
}

// A table WithMetrics was never called on refuses as usual and counts
// nothing; a second call on one registry shares the first's series and
// leaves the receiver unchanged.
func TestUnboundCountsNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	m := metrics.New("t", "t")
	tb := seriesTable()

	s := open(t)

	refused(t, "unknown field, unbound Check", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "adapters.smb.nope", "1"))),
		settings.ErrUnknownKey, "")

	mfs := mGather(t, m)
	if f := mFamily(mfs, "dittofs_config_refused_total"); f != nil {
		t.Errorf("an unbound table created %d series of config_refused_total", len(f.GetMetric()))
	}

	if f := mFamily(mfs, "dittofs_config_bound_refusals_total"); f != nil {
		t.Errorf("an unbound table created config_bound_refusals_total")
	}

	rd := newRdStore(t)
	rd.raw(t, rdInstallation, "adapters.smb.pending_timeout", []byte{0xFF})

	if _, refusedRead, err := settings.Read(context.Background(), rd.nodes, tb, rdNodeA); err !=
		nil {
		t.Fatalf("unbound Read: %v", err)
	} else if _, ok := refusedRead["adapters.smb"]; !ok {
		t.Errorf("unbound Read refused %d sections, want adapters.smb among them",
			len(refusedRead))
	}

	if f := mFamily(mGather(t, m), "dittofs_config_refused_total"); f != nil {
		t.Errorf("an unbound Read created %d series of config_refused_total", len(f.GetMetric()))
	}

	first := tb.WithMetrics(m)
	second := tb.WithMetrics(m)

	refused(t, "unknown field, first copy", s.check(first, entity.RoleAdmin,
		settings.Put(setting(installation, "adapters.smb.nope", "1"))),
		settings.ErrUnknownKey, "")

	refused(t, "unknown field, second copy", s.check(second, entity.RoleAdmin,
		settings.Put(setting(installation, "adapters.smb.nope", "1"))),
		settings.ErrUnknownKey, "")

	lbl := map[string]string{"component": "adapters.smb", "reason": "unknown_key"}
	if got, ok := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); !ok || got != 2 {
		t.Errorf("two bound copies: config_refused_total%v = %v (series %v), want 2 shared",
			lbl, got, ok)
	}

	if n := mSamples(mGather(t, m), "dittofs_config_refused_total"); n != 1 {
		t.Errorf("two bound copies hold %d series, want 1 shared", n)
	}

	refused(t, "unknown field, the receiver", s.check(tb, entity.RoleAdmin,
		settings.Put(setting(installation, "adapters.smb.nope", "1"))),
		settings.ErrUnknownKey, "")

	if got, _ := mSeries(mGather(t, m), "dittofs_config_refused_total", lbl); got != 2 {
		t.Errorf("the receiver counted: config_refused_total%v = %v, want 2", lbl, got)
	}
}

// fuzzOpen opens one store for a fuzz target, which takes a testing.TB
// where open takes a *testing.T.
func fuzzOpen(tb testing.TB) store {
	tb.Helper()

	db, err := badgerkv.Open(badgerkv.Options{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Error(err)
		}
	})

	return openOver(tb, db)
}

// A refused write's labels hold the key nowhere. One store stays open for
// the whole target and the checks go through Check itself, the path that
// labels a refusal, so a label the code derives from the key, rather than
// from the section it resolved, fails the target. The key is an operator's
// typed input, which DittoFS did not write.
func FuzzComponentLabel(f *testing.F) {
	components := map[string]bool{"unknown": true}
	for _, p := range seriesPrefixes {
		components[p] = true
	}

	reasons := map[string]bool{}
	for _, name := range settings.ReasonNames() {
		reasons[name] = true
	}

	seeds := []string{
		"journal." + journalID + ".spare_target",
		"adapters.smb.pending_timeout",
		"adapters.smb.nope",
		"nosuch.key",
		"group_commit.max_files",
		"",
		".",
		"...",
		strings.Repeat(".", 4096),
		"journal",
		"journal.",
		"test.format.version",
		string([]byte{0xff, 0xfe, 0xfd}),
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	s := fuzzOpen(f)
	m := metrics.New("t", "t")
	tb := seriesTable().WithMetrics(m)

	// raised is the set of sample pairs the registry already holds, so one
	// call's check judges what that call raised, not what earlier keys did.
	raised := func(t *testing.T) map[[2]string]bool {
		t.Helper()

		out := map[[2]string]bool{}

		for _, mf := range mGather(t, m) {
			if mf.GetName() != "dittofs_config_refused_total" {
				continue
			}

			for _, mt := range mf.GetMetric() {
				l := mLabels(mt)

				if len(l) == 2 && l["component"] != "" && l["reason"] != "" {
					out[[2]string{l["component"], l["reason"]}] = true
				} else {
					t.Errorf("the refusal series carries %d labels, want component and reason",
						len(l))
				}
			}
		}

		return out
	}

	f.Fuzz(func(t *testing.T, key string) {
		before := raised(t)

		// A passing write is as welcome as a refused one: the target checks
		// what the refusal labelled either way.
		_ = s.check(tb, entity.RoleAdmin,
			settings.Put(setting(installation, key, "1")))

		for pair := range raised(t) {
			if before[pair] {
				continue
			}

			if !components[pair[0]] {
				t.Errorf("the component of %q raised %q, want a declared prefix or unknown",
					key, pair[0])
			}

			if !reasons[pair[1]] {
				t.Errorf("the reason of %q raised %q, outside the closed list", key, pair[1])
			}
		}
	})
}
