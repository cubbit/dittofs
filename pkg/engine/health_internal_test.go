package engine

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/workqueue"
)

// A clone shares no journal map with the record it copies: an edit to every
// map a caller gets, the nested Work maps included, leaves the record as it
// was.
func TestStatsCloneCopiesJournalMaps(t *testing.T) {
	var shard entity.ShardID
	rec := newStats()
	rec.Journals["d"] = JournalStats{
		TagShares:   map[journal.ShareTag]entity.ShareID{1: {}},
		Work:        map[string]map[entity.ShardID]workqueue.Stats{"offload": {shard: {Pending: 1}}},
		WorkLimits:  map[string]workqueue.Limits{"offload": {Total: 1}},
		QueueEvents: map[string]int64{"dirty": 1},
	}

	c := rec.clone().Journals["d"]
	c.TagShares[2] = entity.ShareID{}
	c.Work["offload"][shard] = workqueue.Stats{Pending: 2}
	c.Work["removal"] = nil
	c.WorkLimits["offload"] = workqueue.Limits{Total: 2}
	c.QueueEvents["dirty"]++

	j := rec.Journals["d"]
	if len(j.TagShares) != 1 {
		t.Errorf("record TagShares = %v after editing the clone, want its one entry", j.TagShares)
	}
	if got := j.Work["offload"][shard].Pending; got != 1 {
		t.Errorf("record Work[offload] Pending = %d after editing the clone, want 1", got)
	}
	if len(j.Work) != 1 {
		t.Errorf("record Work = %v after editing the clone, want only offload", j.Work)
	}
	if got := j.WorkLimits["offload"].Total; got != 1 {
		t.Errorf("record WorkLimits[offload] Total = %d after editing the clone, want 1", got)
	}
	if got := j.QueueEvents["dirty"]; got != 1 {
		t.Errorf("record QueueEvents[dirty] = %d after editing the clone, want 1", got)
	}
}
