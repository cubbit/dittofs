package node_test

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/settings"
)

// labelled is one sample of a series: its labels, a name and a value each in
// the scrape's order, and its value.
type labelled struct {
	labels []string
	value  float64
}

// labelledEq is whether two samples hold the same labels and value.
func labelledEq(a, b labelled) bool {
	return slices.Equal(a.labels, b.labels) && a.value == b.value
}

// sameSamples is whether want is exactly the samples got holds: the same
// labels and values each, whatever order the scrape listed them in.
func sameSamples(got, want []labelled) bool {
	ordered := func(ss []labelled) []labelled {
		out := slices.Clone(ss)
		slices.SortStableFunc(out, func(a, b labelled) int {
			return strings.Compare(strings.Join(a.labels, "\x00"), strings.Join(b.labels, "\x00"))
		})

		return out
	}

	return slices.EqualFunc(ordered(got), ordered(want), labelledEq)
}

// configSeries is each sample the registry answers for name, without the
// deployment's prefix, so a series the node never registered answers none.
func configSeries(t *testing.T, reg *metrics.Registry, name string) []labelled {
	t.Helper()

	families, err := reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	out := []labelled{}

	for _, f := range families {
		if f.GetName() != metrics.Namespace+"_"+name {
			continue
		}

		for _, m := range f.GetMetric() {
			s := labelled{value: m.GetGauge().GetValue()}
			if m.GetCounter() != nil {
				s.value = m.GetCounter().GetValue()
			}

			for _, l := range m.GetLabel() {
				s.labels = append(s.labels, l.GetName(), l.GetValue())
			}

			out = append(out, s)
		}
	}

	return out
}

// After a start the node's registry answers every configuration series of
// the RFC's own list the node runs, none labelled by a record: the
// reconciled position and the lag once per kind, kinds being the closed set
// settings, shares and principals, the secrets the node holds behind and
// the refused bound changes at zero, and the three format series one sample
// per format the binary registers, the formats being their own closed set.
// A component whose series is wired into a table the node itself never
// registers is caught here, and so is a sample named by something a record
// reached. The refusal counter's label values, the components and the
// reasons, have their closed set checked by the settings package's own
// marker test over its whole table; here they are only what one refused
// write counted, named by the section and the reason.
func TestEveryConfigSeriesIsOnTheNodeRegistry(t *testing.T) {
	t.Parallel()

	h := nodetest.New(t)
	started(t, h)
	reg := h.Metrics()

	pos := h.Node().Positions()

	marked := []labelled{}
	for _, kind := range []struct {
		name   string
		marked uint64
	}{
		{"settings", pos.Settings},
		{"shares", pos.ShareList},
		{"principals", pos.Principals},
	} {
		marked = append(marked, labelled{
			labels: []string{"kind", kind.name},
			value:  float64(kind.marked),
		})
	}

	if got := configSeries(t, reg, "node_reconciled_position"); !sameSamples(got, marked) {
		t.Errorf(
			"node_reconciled_position after Start: got %+v, want one sample per kind at "+
				"the position the node reconciled through",
			got,
		)
	}

	lag := make([]labelled, len(marked))
	for i, m := range marked {
		lag[i] = labelled{labels: m.labels}
	}

	if got := configSeries(t, reg, "config_generation_lag_seconds"); !sameSamples(got, lag) {
		t.Errorf(
			"config_generation_lag_seconds after Start: got %+v, want one sample per kind at 0",
			got,
		)
	}

	for _, name := range []string{"config_secrets_behind", "config_bound_refusals_total"} {
		if got := configSeries(t, reg, name); !slices.EqualFunc(
			got,
			[]labelled{{value: 0}},
			labelledEq,
		) {
			t.Errorf("%s after Start: got %+v, want one sample with no label at 0", name, got)
		}
	}

	mins := map[string]float64{}
	maxs := map[string]float64{}

	for f, r := range settings.Versions() {
		mins[f.String()] = float64(r.Min)
		maxs[f.String()] = float64(r.Max)
	}

	for name, want := range map[string]map[string]float64{
		"format_supported_min": mins,
		"format_supported_max": maxs,
	} {
		if got := formatGauges(t, reg, name); !maps.Equal(got, want) {
			t.Errorf(
				"%s after Start: got %v, want one sample per format of the registered table: %v",
				name,
				got,
				want,
			)
		}
	}

	if got := formatGauges(t, reg, "format_active_version"); !maps.Equal(
		got,
		map[string]float64{"store": float64(h.Store().Format())},
	) {
		t.Errorf(
			"format_active_version after Start: got %v, want the store's format alone, at %d",
			got,
			h.Store().Format(),
		)
	}
}

// refusedSamples is the refusal counter's samples keyed by component and
// reason, refusing a sample whose labels are not exactly the two names the
// family declares, in order, so no wrong label name and no missing label
// passes the check.
func refusedSamples(t *testing.T, got []labelled) map[string]float64 {
	t.Helper()

	out := map[string]float64{}

	for _, s := range got {
		if len(s.labels) != 4 ||
			s.labels[0] != "component" ||
			s.labels[2] != "reason" {
			t.Fatalf(
				"a config_refused_total sample's labels: got %v, want component and reason named",
				s.labels,
			)
		}

		out[s.labels[1]+"\x00"+s.labels[3]] = s.value
	}

	return out
}

// A settings write the node refuses counts on the node's registry exactly
// once, under the refusing section's name and the refusal's, never under a
// key or a record, and every sample of the counter names exactly component
// and reason: a component's table the node itself never registers counts
// nothing, and a write judged twice counts twice, both caught by the exact
// one. Serial, like every watched test: the harness's watcher holds a test
// to the goroutines of the whole process, which it must not share with a
// parallel neighbour.
func TestARefusedSettingWriteCountsOnTheNodeRegistry(t *testing.T) {
	h, addr := judgedNode(t, entity.RoleAdmin)

	if got := h.Node().Refused(); len(got) != 0 {
		t.Fatalf("the refused records before the refused write: got %+v, want none", got)
	}

	// before is every sample the counter holds ahead of the refused write: a
	// component whose required rows no pass read whole counts here once a
	// pass, as the walks refuse it, none of it the write's work.
	before := refusedSamples(t, configSeries(t, h.Metrics(), "config_refused_total"))

	code, body := writeOf(t, addr, http.MethodPut, "/api/v1/settings/audit.retention",
		`{"spec":{"value":"24h"}}`, "If-None-Match", "*")
	if code != http.StatusBadRequest {
		t.Fatalf("an admin's PUT of audit.retention 24h: answered %d %s, want 400", code, body)
	}

	if p := problemOf(t, body); p.Code != "invalid" || p.Field != "audit.retention" {
		t.Errorf(
			"an admin's PUT of audit.retention 24h: %s, want invalid naming audit.retention",
			body,
		)
	}

	// the write's sample is the one kind it counts, exactly once: everything
	// the counter held before keeps its value, and the new sample alone is
	// the refusing section and the refusal's reason, never a key.
	after := refusedSamples(t, configSeries(t, h.Metrics(), "config_refused_total"))

	want := "audit\x00invalid"
	if got, ok := after[want]; !ok || got != 1 {
		t.Errorf(
			"the refused write's count on config_refused_total: got %v, want exactly once",
			got,
		)
	}

	got := maps.Clone(after)
	delete(got, want)

	if !maps.Equal(got, before) {
		t.Errorf(
			"config_refused_total after the write: %v, want the write's sample alone over %v",
			after,
			before,
		)
	}

	if got := configSeries(t, h.Metrics(), "config_bound_refusals_total"); !slices.EqualFunc(
		got,
		[]labelled{{value: 0}},
		labelledEq,
	) {
		t.Errorf(
			"config_bound_refusals_total after the write: got %+v, want one unlabeled sample at 0",
			got,
		)
	}
}
