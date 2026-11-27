package node_test

import (
	"errors"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/settings"
)

// formatGauges reads one format gauge family, without the dittofs_ prefix:
// each sample's format label, which must be the family's only label, with
// its value.
func formatGauges(t *testing.T, reg *metrics.Registry, name string) map[string]float64 {
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
			labels := m.GetLabel()
			if len(labels) != 1 || labels[0].GetName() != "format" {
				t.Errorf("%s's sample labels: got %v, want exactly one, named format", name, labels)
				continue
			}

			got[labels[0].GetValue()] = m.GetGauge().GetValue()
		}
	}

	return got
}

// A start over an installation whose active journal format lies past the
// binary's range is refused at the register step, naming the format and both
// versions, with no component started and the record as it was; the node
// never turns ready, and Stop closes what the refusal left open.
func TestStartRefusesAnActiveVersionPastTheBinarysRange(t *testing.T) {
	var h *nodetest.Harness

	s := newSteps(t, &h)
	h = nodetest.New(
		t,
		nodetest.Watch(),
		nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
			kvstoretest.WriteActive(t, cfg.KV, map[entity.Format]uint32{entity.FormatJournal: 2})
			return node.NewStepping(s.stepped)(cfg)
		}),
	)

	err := h.Node().Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "start step register:") {
		t.Fatalf(
			"Start over journal format 2: got %v, want the refusal at start step register",
			err,
		)
	}

	if w := "journal format 2 is active, this node reads 1 to 1"; !strings.Contains(
		err.Error(),
		w,
	) {
		t.Errorf(
			"the refusal must name the format and both versions: %q missing from %q",
			w,
			err.Error(),
		)
	}

	rec, found, err := h.Store().Nodes().Node(t.Context(), h.NodeID())
	if err != nil {
		t.Fatalf("read the node's record: %v", err)
	}

	if found || rec.Epoch != 0 {
		t.Errorf(
			"the node's record after the refusal: found %v at epoch %d, want none, as it was",
			found,
			rec.Epoch,
		)
	}

	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/ready"); got != notReady {
		t.Errorf("readiness after the refused Start: got %+v, want %+v", got, notReady)
	}

	conds := h.Node().StartFailed()
	if len(conds) != 1 || conds[0].Name != "start_failed" ||
		!strings.Contains(conds[0].Cause.Error(), "start step register") {
		t.Errorf(
			"conditions after the refused Start: got %+v, want start_failed naming register",
			conds,
		)
	}

	if got := s.reported(); !reflect.DeepEqual(got, []string{"refuse", "listeners"}) {
		t.Errorf(
			"steps the refused Start ran: got %v, want the refusal before any component started",
			got,
		)
	}

	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop after the refused Start: %v", err)
	}

	if got := get(t, api, "/health/ready"); got != closed {
		t.Errorf("readiness after Stop: got %+v, want the listener closed", got)
	}
}

// A start over an installation whose active versions this binary reads
// records this binary's ranges in the node's record, and the node's
// registry holds each format's active version and range, one sample per
// format labelled by format only.
func TestStartRegistersItsRangesAndTheFormatSeries(t *testing.T) {
	t.Parallel()

	h := nodetest.New(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		kvstoretest.WriteActive(t, cfg.KV, map[entity.Format]uint32{
			entity.FormatJournal:  1,
			entity.FormatBlock:    1,
			entity.FormatSettings: 1,
		})

		return node.New(cfg)
	}))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start over this binary's versions: %v, want accepted", err)
	}
	defer func() {
		if err := h.Node().Stop(t.Context()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	rec, found, err := h.Store().Nodes().Node(t.Context(), h.NodeID())
	if err != nil || !found {
		t.Fatalf("read the node's record: found %v, err %v", found, err)
	}

	if got := rec.Formats; !reflect.DeepEqual(got, settings.Versions()) {
		t.Errorf("the node record's ranges: got %v, want %v", got, settings.Versions())
	}

	ones := map[string]float64{"store": 1, "journal": 1, "block": 1, "settings": 1}
	for _, name := range []string{
		"format_active_version",
		"format_supported_min",
		"format_supported_max",
	} {
		if got := formatGauges(t, h.Metrics(), name); !maps.Equal(got, ones) {
			t.Errorf("%s after Start: %v, want one sample per format at 1: %v", name, got, ones)
		}
	}
}

// A fresh installation, which holds no active version, starts: an unset
// active version never refuses a start, and the record carries its ranges.
func TestStartOverAFreshInstallationPassesTheGate(t *testing.T) {
	t.Parallel()

	h := nodetest.New(t)
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("Start over a fresh installation: %v, want accepted", err)
	}
	defer func() {
		if err := h.Node().Stop(t.Context()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	api, _ := h.Node().Addrs()
	if got := get(t, api, "/health/ready"); got != (answer{http.StatusOK, "ready"}) {
		t.Errorf("readiness after Start: got %+v, want 200 ready", got)
	}

	rec, found, err := h.Store().Nodes().Node(t.Context(), h.NodeID())
	if err != nil || !found {
		t.Fatalf("read the node's record: found %v, err %v", found, err)
	}

	if got := rec.Formats; !reflect.DeepEqual(got, settings.Versions()) {
		t.Errorf("the node record's ranges: got %v, want %v", got, settings.Versions())
	}
}

// The store the node opens writes a settings row up to the newest schema the
// settings table reads, and refuses one above it, so no node writes a row
// its own readers refuse.
func TestNodeStoreWritesUpToTheSettingsSchema(t *testing.T) {
	t.Parallel()

	h := nodetest.New(t)
	cp := h.Node().Meta().ControlPlane()
	top := settings.Versions()[entity.FormatSettings].Max
	row := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "k", Value: []byte("k"), Schema: top}

	gen, err := cp.PutSetting(t.Context(), row, 0)
	if err != nil {
		t.Fatalf("put at schema %d, the newest the table reads: %v, want written", top, err)
	}

	row.Schema = top + 1
	if _, err := cp.PutSetting(t.Context(), row, gen); !errors.Is(err, entity.ErrNewerSchema) {
		t.Errorf("put at schema %d: %v, want %v", row.Schema, err, entity.ErrNewerSchema)
	}
}
