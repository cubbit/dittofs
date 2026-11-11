package datapath

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// statsScenario is a driven core's Stats, labelled for a failure message.
type statsScenario struct {
	name string
	s    Stats
}

// driveStats runs the scenarios that move the counters named for a condition,
// each on a core of its own, and returns the Stats each ended with.
func driveStats(t *testing.T) []statsScenario {
	t.Helper()
	ctx := context.Background()
	var out []statsScenario

	// Writes, a sync, a fill and a deallocate, under a bounded journal.
	{
		c, _ := newTestCore(t, capacityCoreOption(8*testSegmentCap), func(o *coreOptions) { o.extentLimit = 1000 })
		s := mustLimitShare(t, c, 1, 1<<20)
		f, g := testFile(1), testFile(2)
		mustWrite(t, c, s, f, 0, bytes.Repeat([]byte{'a'}, 3*4096))
		mustWrite(t, c, s, g, 0, bytes.Repeat([]byte{'b'}, 4096))
		syncFiles(t, c, f, g)
		waitIn(t, 5*time.Second, "the sync to be counted", func() bool { return c.Durability().Stats().Syncs > 0 })
		if err := c.Fill(ctx, s, testFile(3), 0, ff(4096), ver(0), ver(1)); err != nil {
			t.Fatalf("Fill: %v", err)
		}
		if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 4096, Len: 4096}); err != nil {
			t.Fatalf("Deallocate: %v", err)
		}
		out = append(out, statsScenario{"writes", c.Stats()})
	}

	// A fill older than the file is refused.
	{
		c, _ := newTestCore(t)
		s := mustShare(t, c, 1)
		mustWrite(t, c, s, testFile(4), 0, bytes.Repeat([]byte{'a'}, 1024))
		if err := c.Fill(ctx, s, testFile(4), 0, ff(4096), ver(0), ver(1)); err == nil {
			t.Fatal("a stale fill was accepted")
		}
		out = append(out, statsScenario{"stale fill", c.Stats()})
	}

	// A removal that would split a range past the index bound is refused.
	{
		const limit = 80
		c, _ := newTestCore(t, func(o *coreOptions) { o.extentLimit = limit })
		s := mustShare(t, c, 1)
		f := testFile(1)
		mustWrite(t, c, s, f, 0, make([]byte, 3*4096))
		other := testFile(2)
		for k := int64(0); len(c.Index().FileEntries(other)) < limit+limit/8; k++ {
			at := index.RecordAt{Segment: core.SegmentID(1 + k/16), Offset: uint32(k%16) * 8}
			if _, err := c.Index().ApplyRemoval(s.Tag, other, core.ChangeDeallocate, core.Extent{Off: k * 8, Len: 8}, ver(uint64(k)+1), core.Seq(uint64(k)+1), at, nil); err != nil {
				t.Fatalf("padding the count: %v", err)
			}
		}
		if _, err := c.Deallocate(ctx, s, f, core.Extent{Off: 4096, Len: 4096}); err == nil {
			t.Fatal("a splitting removal at the ceiling was admitted")
		}
		out = append(out, statsScenario{"index bound", c.Stats()})
	}

	// A write past its share's limit is refused.
	{
		c, _ := newTestCore(t)
		s := mustLimitShare(t, c, 1, recordBytes(4096))
		if _, err := c.WriteAt(ctx, s, testFile(1), 0, make([]byte, 4096), 1); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if _, err := c.WriteAt(ctx, s, testFile(2), 0, make([]byte, 8192), 1); err == nil {
			t.Fatal("a write past the share limit was admitted")
		}
		out = append(out, statsScenario{"share limit", c.Stats()})
	}

	// A retire frees a segment, and a second retire of the same one is counted.
	{
		c, _ := newTestCore(t)
		s := mustShare(t, c, 1)
		f := testFile(1)
		mustWrite(t, c, s, f, 0, make([]byte, 4096))
		seg := recordOf(t, c, f, 0).Record.Segment
		if _, err := c.Streams().For(f).Seal(false); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		mustWrite(t, c, s, sameStreamFile(c, f), 0, make([]byte, 4096))
		syncFiles(t, c, f)
		if _, err := c.Retirer().Retire(seg, segment.NotHeld); err != nil {
			t.Fatalf("Retire: %v", err)
		}
		_, _ = c.Retirer().Retire(seg, segment.NotHeld)
		out = append(out, statsScenario{"retire", c.Stats()})
	}

	// A byte flipped in a synced record drops it as a corrupt loss.
	{
		c, seam := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
		s := mustShare(t, c, 3)
		f := testFile(1)
		if _, err := c.WriteAt(ctx, s, f, 0, pattern(1<<20, 5), 1); err != nil {
			t.Fatal(err)
		}
		syncFiles(t, c, f)
		waitIn(t, 5*time.Second, "the sync to be counted", func() bool { return c.Durability().Stats().Syncs > 0 })
		r := recordOf(t, c, f, 0).Record
		flip(t, seam, r.Segment, payloadStart(r)+1<<20-1)
		readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
		// The loss is recorded by the durability worker, so wait for it.
		lossOf(t, c, f)
		out = append(out, statsScenario{"corrupt", c.Stats()})
	}

	// The same with the extent offloaded: a fill's bytes are repairable.
	{
		c, seam := newTestCore(t, func(o *coreOptions) { o.segmentCap = 4 << 20 })
		s := mustShare(t, c, 3)
		f := testFile(1)
		if err := c.Fill(ctx, s, f, 0, pattern(1<<20, 5), ver(0), ver(1)); err != nil {
			t.Fatalf("Fill: %v", err)
		}
		syncFiles(t, c, f)
		r := recordOf(t, c, f, 0).Record
		flip(t, seam, r.Segment, payloadStart(r)-8+1<<20-1)
		readUntouched(t, c, s, f, 0, 4<<10, []core.Extent{{Off: 0, Len: 4 << 10}})
		lossOf(t, c, f)
		out = append(out, statsScenario{"corrupt offloaded", c.Stats()})
	}

	// A refused sync is counted and its failure kept.
	{
		c, seam := newTestCore(t)
		s := mustShare(t, c, 1)
		f := testFile(1)
		mustWrite(t, c, s, f, 0, make([]byte, 4096))
		seg := format.SegmentName(recordOf(t, c, f, 0).Record.Segment)
		if err := seam.Schedule(kit.Failing(kit.OpSync, seg, 1, errors.New("sync boom"))); err != nil {
			t.Fatal(err)
		}
		_ = c.Durability().Sync(ctx, f)
		waitIn(t, 5*time.Second, "the failure to be counted", func() bool { return c.Durability().Stats().SyncFailures > 0 })
		out = append(out, statsScenario{"sync failure", c.Stats()})
	}

	// A create that fails with EROFS turns the journal read-only, and the write
	// whose hold the pool could not serve is a spare refusal.
	{
		c, seam := newTestCore(t)
		s := mustShare(t, c, 1)
		if _, err := c.WriteAt(ctx, s, testFile(1), 0, []byte("w"), 1); err != nil {
			t.Fatal(err)
		}
		awaitSettledSpares(t, c, 2*testStreams)
		if err := seam.Schedule(kit.Failing(kit.OpCreate, "*", 0, syscall.EROFS)); err != nil {
			t.Fatal(err)
		}
		g := testFile(2)
		if streamOf(c, g) == streamOf(c, testFile(1)) {
			g = testFile(3)
		}
		_, err := c.WriteAt(ctx, s, g, 0, make([]byte, 4*testSegmentCap+roomAfter(0)), 1)
		_ = refused(t, err, core.LimitSpare)
		waitIn(t, 30*time.Second, "the journal to turn read-only", func() bool { return c.Durability().Admit() != nil })
		out = append(out, statsScenario{"read-only", c.Stats()})
	}

	// Records without bytes drawn from the headroom at the limit.
	{
		c, _, _, _ := atTheLimit(t)
		mustAdmit(t, c, capacity.Release)
		out = append(out, statsScenario{"headroom", c.Stats()})
	}
	return out
}

// TestStatsMetricsMoveOnTheirCondition checks each scenario raises the
// counters the condition names, read through Core.Stats, and no other.
func TestStatsMetricsMoveOnTheirCondition(t *testing.T) {
	want := map[string][]string{
		"writes":            {"WrittenBytes", "FilledBytes", "Syncs"},
		"stale fill":        {"WrittenBytes", "FillRefused"},
		"index bound":       {"WrittenBytes", "IndexRefusals"},
		"share limit":       {"WrittenBytes", "ReservationRefused"},
		"retire":            {"WrittenBytes", "FreedBytes", "DoubleRetires"},
		"corrupt":           {"WrittenBytes", "Syncs", "CorruptExtentsDataLoss"},
		"corrupt offloaded": {"FilledBytes", "Syncs", "CorruptExtentsRepairable"},
		"sync failure":      {"WrittenBytes", "Syncs", "SyncFailures", "SyncReappendedBytes"},
		"read-only":         {"WrittenBytes", "ReservationRefused"},
		"headroom":          {"WrittenBytes", "HeadroomDraws"},
	}
	for _, sc := range driveStats(t) {
		v := reflect.ValueOf(sc.s.Counters)
		var got []string
		for i := range v.NumField() {
			if !v.Field(i).IsZero() {
				got = append(got, v.Type().Field(i).Name)
			}
		}
		slices.Sort(got)
		w := slices.Clone(want[sc.name])
		slices.Sort(w)
		if !slices.Equal(got, w) {
			t.Errorf("%s: counters raised = %v, want %v", sc.name, got, w)
		}
	}
}

// TestStatsSharesAreAttachedOrHoldingRecords checks the per-share map lists a
// tag with an open handle and a tag that holds records with none, with the
// handle's limit only for the first, and never tag 0.
func TestStatsSharesAreAttachedOrHoldingRecords(t *testing.T) {
	c, _ := newTestCore(t)
	a := mustLimitShare(t, c, 1, 1<<20)
	b := mustLimitShare(t, c, 2, 1<<21)
	mustWrite(t, c, a, testFile(1), 0, make([]byte, 4096))
	mustWrite(t, c, b, testFile(2), 0, make([]byte, 4096))
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mustShare(t, c, 3)

	got := c.Stats().Shares
	if len(got) != 3 {
		t.Fatalf("Shares lists %d tags, want 3 (attached, detached with records, attached empty): %+v", len(got), got)
	}
	if _, ok := got[0]; ok {
		t.Fatal("Shares lists tag 0")
	}
	if got[1].Limit != 1<<20 || got[1].UsedBytes != recordBytes(4096) || got[1].HeldBytes != 4096 || got[1].DirtyBytes != 4096 {
		t.Errorf("share 1 = %+v, want limit 1<<20, used %d, held and dirty 4096", got[1], recordBytes(4096))
	}
	if got[2].Limit != 0 || got[2].UsedBytes != recordBytes(4096) || got[2].HeldBytes != 4096 {
		t.Errorf("detached share 2 = %+v, want limit 0 and its records still counted", got[2])
	}
	if got[3] != (ShareStats{Limit: 0}) {
		t.Errorf("empty attached share 3 = %+v, want zero", got[3])
	}
}

// statsUnserved names the fields of Stats, ShareStats and Counters no
// scenario here raises, each with why.
var statsUnserved = map[string]string{
	"Shares.UnreclaimedEndedBytes": "no Repack seals yet, so no segment is ended with unreclaimed bytes; zero until it does",
	"Shares.UnreclaimedBytes":      "the index has no per-record backing hook, so it is kept at zero",
	"Shares.ReservedBytes":         "a reservation is open only inside a write, and no seam stands inside it; the capacity tests drive it",
	"UnreclaimedBytes":             "the index has no per-record backing hook, so it is kept at zero",
	"Counters.OfferedBytes":        "offload is not built yet",
	"Counters.MarkedBytes":         "offload is not built yet",
	"Counters.ReleasedBytes":       "release is not built yet",
	"Counters.RepackedBytes":       "repack is not built yet",
	"PinnedSegments":               "offload is not built yet",
	"Recovery":                     "recovery reporting is not in the core's figures",
	"RejectedSegments":             "recovery reporting is not in the core's figures",
	"Counters.TornTails":           "recovery reporting is not in the core's figures",
	"Counters.ScanResyncs":         "recovery reporting is not in the core's figures",
	"TagConflicts":                 "only a rebuild of rival records raises it; the index's own tests drive that",
	"Counters.StaleExtents":        "needs a stale loss; the durability layer's tests drive it; no core fixture drives it here, so the Core.Stats mapping of it is unchecked",
	"Counters.SupersededExtents":   "needs a superseded loss; the durability layer's tests drive it; no core fixture drives it here, so the Core.Stats mapping of it is unchecked",
	"Counters.SpareRefusals":       "needs a roll with no spare ready; the pool's tests drive it; no core fixture drives it here, so the Core.Stats mapping of it is unchecked",
	"Counters.SegmentReopens":      "needs a closed descriptor reopened; the descriptor tests drive it; no core fixture drives it here, so the Core.Stats mapping of it is unchecked",
	"RepackReserveBytes":           "the test core's pool targets no repack reserve",
	"ReservedBytes":                "a reservation is open only inside a write, and no seam stands inside it; the capacity tests drive it",
}

// TestStatsEveryFieldIsServedOrNamed checks a field of Stats, ShareStats or
// Counters is raised by a scenario above or named in statsUnserved, so a field
// cannot go unserved unseen; and that a named field really is zero.
func TestStatsEveryFieldIsServedOrNamed(t *testing.T) {
	nonzero := map[string]bool{}
	note := func(prefix string, v reflect.Value) {
		for i := range v.NumField() {
			f := v.Field(i)
			empty := (f.Kind() == reflect.Slice || f.Kind() == reflect.Map) && f.Len() == 0
			if !f.IsZero() && !empty {
				nonzero[prefix+v.Type().Field(i).Name] = true
			}
		}
	}
	for _, sc := range driveStats(t) {
		note("", reflect.ValueOf(sc.s))
		note("Counters.", reflect.ValueOf(sc.s.Counters))
		for _, ss := range sc.s.Shares {
			note("Shares.", reflect.ValueOf(ss))
		}
	}
	var names []string
	collect := func(prefix string, ty reflect.Type) {
		for i := range ty.NumField() {
			names = append(names, prefix+ty.Field(i).Name)
		}
	}
	collect("", reflect.TypeOf(Stats{}))
	collect("Counters.", reflect.TypeOf(Counters{}))
	collect("Shares.", reflect.TypeOf(ShareStats{}))
	for _, n := range names {
		if n == "Counters" || n == "Shares" {
			continue
		}
		why, named := statsUnserved[n]
		switch {
		case nonzero[n] && named:
			t.Errorf("%s is raised by a scenario but listed unserved (%s)", n, why)
		case !nonzero[n] && !named:
			t.Errorf("%s is zero in every scenario and not listed unserved: it may be unwired", n)
		}
	}
}
