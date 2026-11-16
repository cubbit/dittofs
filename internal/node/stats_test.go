package node

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The engine's series are read from its Stats at each scrape, not copied
// once: a figure that moved between two scrapes shows at the second.
func TestEngineSeriesReadAtScrape(t *testing.T) {
	s := engine.Stats{SelfFences: 1, Reads: map[string]int64{"hit": 3}}
	c := engineSeries{func() engine.Stats { return s }}
	names := []string{"dittofs_engine_self_fences_total", "dittofs_engine_reads_total", "dittofs_engine_read_seconds"}
	scrape := func(fences, hits, count, sum int) string {
		return fmt.Sprintf(`
# HELP dittofs_engine_read_seconds Time reads took.
# TYPE dittofs_engine_read_seconds summary
dittofs_engine_read_seconds_sum %d
dittofs_engine_read_seconds_count %d
# HELP dittofs_engine_reads_total Reads, by their outcome.
# TYPE dittofs_engine_reads_total counter
dittofs_engine_reads_total{class="hit"} %d
# HELP dittofs_engine_self_fences_total Times the node fenced for a store that committed no write transaction.
# TYPE dittofs_engine_self_fences_total counter
dittofs_engine_self_fences_total{reason="store"} %d
`, sum, count, hits, fences)
	}
	if err := testutil.CollectAndCompare(c, strings.NewReader(scrape(1, 3, 0, 0)), names...); err != nil {
		t.Errorf("first scrape: %v", err)
	}
	s.SelfFences, s.Reads["hit"], s.ReadSeconds = 2, 5, engine.Series{Count: 4, Total: 2 * time.Second}
	if err := testutil.CollectAndCompare(c, strings.NewReader(scrape(2, 5, 4, 2)), names...); err != nil {
		t.Errorf("second scrape after the figures moved: %v", err)
	}
}

// The engine's conditions reach the evaluator with their subject's scope,
// identity and name, their cause and their detail: the node's, each journal's
// and each share's.
func TestEngineConditions(t *testing.T) {
	cause := errors.New("store committed nothing for 30 s")
	s := engine.Stats{
		Node: engine.NodeStats{Conditions: []engine.Condition{{Name: "node_fenced", Subject: engine.Subject{Scope: engine.ScopeNode, ID: "n1"}, Cause: cause, Detail: "fenced"}}},
		Journals: map[string]engine.JournalStats{"/dev/a": {Conditions: []engine.Condition{
			{Name: "journal_read_only", Subject: engine.Subject{Scope: engine.ScopeJournal, ID: "j1", Name: "/dev/a"}},
		}}},
		Shares: map[entity.ShareID]engine.ShareStats{{1}: {Conditions: []engine.Condition{
			{Name: "journal_unreconciled", Subject: engine.Subject{Scope: engine.ScopeShare, ID: "s1"}},
		}}},
	}
	var e health.Evaluator
	e.Add(engineConditions{func() engine.Stats { return s }})
	want := []health.Condition{
		{Name: "node_fenced", Subject: health.Subject{Scope: health.ScopeNode, ID: "n1"}, Cause: cause, Detail: "fenced"},
		{Name: "journal_read_only", Subject: health.Subject{Scope: health.ScopeJournal, ID: "j1", Name: "/dev/a"}},
		{Name: "journal_unreconciled", Subject: health.Subject{Scope: health.ScopeShare, ID: "s1"}},
	}
	if got := e.Conditions(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("conditions: got %+v, want %+v", got, want)
	}
}
