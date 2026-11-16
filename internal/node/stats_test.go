package node

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The engine's series, each of its kind and with its labels, are read from
// its Stats at each scrape, not copied once: a figure that moved between two
// scrapes shows at the second.
func TestEngineSeriesReadAtScrape(t *testing.T) {
	s := engine.Stats{Reads: map[string]int64{}, RemovalRuns: map[string]int64{}}
	c := engineSeries{func() engine.Stats { return s }}
	scrape := func(n int) string {
		return strings.ReplaceAll(`# HELP dittofs_engine_group_commit_splits_total Group commit batches split.
# TYPE dittofs_engine_group_commit_splits_total counter
dittofs_engine_group_commit_splits_total N
# HELP dittofs_engine_lock_wait_seconds Contended takes of a lock.
# TYPE dittofs_engine_lock_wait_seconds histogram
dittofs_engine_lock_wait_seconds_bucket{area="file_state",le="+Inf"} N
dittofs_engine_lock_wait_seconds_sum{area="file_state"} N
dittofs_engine_lock_wait_seconds_count{area="file_state"} N
# HELP dittofs_engine_pending_existence_ops Operations the existence commits committed.
# TYPE dittofs_engine_pending_existence_ops counter
dittofs_engine_pending_existence_ops N
# HELP dittofs_engine_read_seconds Time reads took.
# TYPE dittofs_engine_read_seconds histogram
dittofs_engine_read_seconds_bucket{le="+Inf"} N
dittofs_engine_read_seconds_sum N
dittofs_engine_read_seconds_count N
# HELP dittofs_engine_reads_total Reads, by their outcome.
# TYPE dittofs_engine_reads_total counter
dittofs_engine_reads_total{class="hole"} N
# HELP dittofs_engine_removal_runs_total Removal runs, by their result.
# TYPE dittofs_engine_removal_runs_total counter
dittofs_engine_removal_runs_total{result="done"} N
# HELP dittofs_engine_self_fences_total Times the node fenced for a store that committed no write transaction.
# TYPE dittofs_engine_self_fences_total counter
dittofs_engine_self_fences_total{reason="store"} N
# HELP dittofs_engine_stability_points_total Stability points the engine reached.
# TYPE dittofs_engine_stability_points_total counter
dittofs_engine_stability_points_total N
# HELP dittofs_engine_write_refusals_total Writes the engine refused.
# TYPE dittofs_engine_write_refusals_total counter
dittofs_engine_write_refusals_total N
`, "N", strconv.Itoa(n))
	}
	set := func(n int64) {
		s.SelfFences, s.StabilityPoints, s.WriteRefusals, s.PendingExistenceOps, s.GroupCommitSplits = n, n, n, n, n
		s.Reads["hole"], s.RemovalRuns["done"] = n, n
		s.ReadSeconds = engine.Series{Count: n, Total: time.Duration(n) * time.Second}
		s.FileStateLockWaits = s.ReadSeconds
	}
	for _, n := range []int64{1, 2} {
		set(n)
		if err := testutil.CollectAndCompare(c, strings.NewReader(scrape(int(n)))); err != nil {
			t.Errorf("scrape with every figure at %d: %v", n, err)
		}
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
	src := &engineConditions{func() engine.Stats { return s }}
	e.Add(src)
	want := []health.Condition{
		{Name: "node_fenced", Subject: health.Subject{Scope: health.ScopeNode, ID: "n1"}, Cause: cause, Detail: "fenced"},
		{Name: "journal_read_only", Subject: health.Subject{Scope: health.ScopeJournal, ID: "j1", Name: "/dev/a"}},
		{Name: "journal_unreconciled", Subject: health.Subject{Scope: health.ScopeShare, ID: "s1"}},
	}
	if got := e.Conditions(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("conditions: got %+v, want %+v", got, want)
	}
	e.Remove(src)
	if got := e.Conditions(nil); len(got) != 0 {
		t.Errorf("conditions after removing the engine's source: got %+v, want none", got)
	}
}

// Only a pointer to the engine's adapter is a health.Source: Remove finds a
// source by identity, and a value holding a func can't be compared.
func TestEngineConditionsIsAPointerSource(t *testing.T) {
	src := reflect.TypeFor[health.Source]()
	if reflect.TypeFor[engineConditions]().Implements(src) {
		t.Error("engineConditions as a value implements health.Source, want only *engineConditions")
	}
	if !reflect.TypeFor[*engineConditions]().Implements(src) {
		t.Error("*engineConditions does not implement health.Source")
	}
}
