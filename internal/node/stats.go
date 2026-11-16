package node

import (
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/prometheus/client_golang/prometheus"
)

// engineSeries is the engine's Stats as the registry's series, read from a
// fresh Stats at each scrape.
type engineSeries struct{ stats func() engine.Stats }

func engineDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dittofs_engine_"+name, help, labels, nil)
}

var (
	selfFences      = engineDesc("self_fences_total", "Times the node fenced for a store that committed no write transaction.", "reason")
	stabilityPoints = engineDesc("stability_points_total", "Stability points the engine reached.")
	writeRefusals   = engineDesc("write_refusals_total", "Writes the engine refused.")
	pendingOps      = engineDesc("pending_existence_ops", "Operations the existence commits committed.")
	commitSplits    = engineDesc("group_commit_splits_total", "Group commit batches split.")
	reads           = engineDesc("reads_total", "Reads, by their outcome.", "class")
	readSeconds     = engineDesc("read_seconds", "Time reads took.")
	lockWaits       = engineDesc("lock_wait_seconds", "Contended takes of a lock.", "area")
	removalRuns     = engineDesc("removal_runs_total", "Removal runs, by their result.", "result")
)

func (engineSeries) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{selfFences, stabilityPoints, writeRefusals, pendingOps, commitSplits, reads, readSeconds, lockWaits, removalRuns} {
		ch <- d
	}
}

func (e engineSeries) Collect(ch chan<- prometheus.Metric) {
	s := e.stats()
	counter := func(d *prometheus.Desc, v int64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	summary := func(d *prometheus.Desc, v engine.Series, labels ...string) {
		ch <- prometheus.MustNewConstSummary(d, uint64(v.Count), v.Total.Seconds(), nil, labels...)
	}
	counter(selfFences, s.SelfFences, "store")
	counter(stabilityPoints, s.StabilityPoints)
	counter(writeRefusals, s.WriteRefusals)
	ch <- prometheus.MustNewConstMetric(pendingOps, prometheus.GaugeValue, float64(s.PendingExistenceOps))
	counter(commitSplits, s.GroupCommitSplits)
	for class, n := range s.Reads {
		counter(reads, n, class)
	}
	summary(readSeconds, s.ReadSeconds)
	summary(lockWaits, s.FileStateLockWaits, "file_state")
	for result, n := range s.RemovalRuns {
		counter(removalRuns, n, result)
	}
}

// engineConditions is the engine's conditions as the health evaluator reads
// them: its node's, each journal's and each share's.
type engineConditions struct{ stats func() engine.Stats }

func (e engineConditions) Conditions(dst []health.Condition) []health.Condition {
	s := e.stats()
	dst = appendConditions(dst, s.Node.Conditions)
	for _, j := range s.Journals {
		dst = appendConditions(dst, j.Conditions)
	}
	for _, sh := range s.Shares {
		dst = appendConditions(dst, sh.Conditions)
	}
	return dst
}

func appendConditions(dst []health.Condition, cs []engine.Condition) []health.Condition {
	for _, c := range cs {
		dst = append(dst, health.Condition{
			Name:    health.Name(c.Name),
			Subject: health.Subject{Scope: scopes[c.Subject.Scope], ID: c.Subject.ID, Name: c.Subject.Name},
			Cause:   c.Cause,
			Detail:  c.Detail,
		})
	}
	return dst
}

// scopes maps the engine's scopes onto the evaluator's.
var scopes = map[engine.Scope]health.Scope{
	engine.ScopeJournal: health.ScopeJournal,
	engine.ScopeNode:    health.ScopeNode,
	engine.ScopeShare:   health.ScopeShare,
}
