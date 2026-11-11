package datapath

import (
	"bytes"
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
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

	// Records without bytes drawn from the headroom at the limit.
	{
		c, _, _, _ := atTheLimit(t)
		mustAdmit(t, c, capacity.Release)
		out = append(out, statsScenario{"headroom", c.Stats()})
	}
	return out
}

// TestStatsMetricsMoveOnTheirCondition checks each counter the condition that
// names it raises, read through Core.Stats, the layer that serves them.
func TestStatsMetricsMoveOnTheirCondition(t *testing.T) {
	by := map[string]Stats{}
	for _, sc := range driveStats(t) {
		by[sc.name] = sc.s
	}
	for _, tc := range []struct {
		scenario, field string
		got             uint64
	}{
		{"writes", "WrittenBytes", by["writes"].Counters.WrittenBytes},
		{"writes", "FilledBytes", by["writes"].Counters.FilledBytes},
		{"writes", "Syncs", by["writes"].Counters.Syncs},
		{"stale fill", "FillRefused", by["stale fill"].Counters.FillRefused},
		{"index bound", "IndexRefusals", by["index bound"].Counters.IndexRefusals},
		{"share limit", "ReservationRefused", by["share limit"].Counters.ReservationRefused},
		{"headroom", "HeadroomDraws", by["headroom"].Counters.HeadroomDraws},
	} {
		if tc.got == 0 {
			t.Errorf("%s: Counters.%s = 0, want it raised", tc.scenario, tc.field)
		}
	}
	// The other scenarios leave a counter they do not name at zero.
	if got := by["stale fill"].Counters.IndexRefusals + by["writes"].Counters.FillRefused + by["writes"].Counters.ReservationRefused; got != 0 {
		t.Errorf("a counter moved without its condition: %d", got)
	}
}

// TestStatsFreshCountersAreZero checks every counter reads zero on a core
// that has done nothing.
func TestStatsFreshCountersAreZero(t *testing.T) {
	c, _ := newTestCore(t)
	if got := c.Stats().Counters; got != (Counters{}) {
		t.Fatalf("Counters on a fresh core = %+v, want zero", got)
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

// TestStatsResultIsCallerOwned checks a caller's changes to a returned map or
// slice reach no later Stats.
func TestStatsResultIsCallerOwned(t *testing.T) {
	c, _ := newTestCore(t)
	s := mustShare(t, c, 1)
	mustWrite(t, c, s, testFile(1), 0, make([]byte, 4096))
	first := c.Stats()
	want := maps.Clone(first.Shares)
	clear(first.Shares)
	first.SyncDuration.Counts[0] = 99
	first.SyncDuration.Bounds[0] = 99
	again := c.Stats()
	if !maps.Equal(again.Shares, want) {
		t.Fatalf("Shares after a caller cleared one = %+v, want %+v", again.Shares, want)
	}
	if again.SyncDuration.Counts[0] == 99 || again.SyncDuration.Bounds[0] == 99 {
		t.Fatal("a caller's change to the histogram reached the next Stats")
	}
}

// statsUnserved names the fields of Stats, ShareStats and Counters no
// scenario here raises, each with why.
var statsUnserved = map[string]string{
	"UnreclaimedBytes":                  "the index has no per-record backing hook, so it is kept at zero",
	"Counters.OfferedBytes":             "offload is not built yet",
	"Counters.MarkedBytes":              "offload is not built yet",
	"Counters.ReleasedBytes":            "release is not built yet",
	"Counters.RepackedBytes":            "repack is not built yet",
	"PinnedSegments":                    "offload is not built yet",
	"Recovery":                          "recovery reporting is not in the core's figures",
	"RejectedSegments":                  "recovery reporting is not in the core's figures",
	"Counters.TornTails":                "recovery reporting is not in the core's figures",
	"Counters.ScanResyncs":              "recovery reporting is not in the core's figures",
	"Losses":                            "needs a loss; the durability layer's tests drive it",
	"TagConflicts":                      "only a rebuild of rival records raises it; the index's own tests drive that",
	"LastSyncError":                     "needs an injected sync failure; the durability layer's tests drive it",
	"Counters.SyncFailures":             "needs an injected sync failure; the durability layer's tests drive it",
	"Counters.SyncReappendedBytes":      "needs an injected sync failure; the durability layer's tests drive it",
	"LossGeneration":                    "needs a loss; the durability layer's tests drive it",
	"ReadOnly":                          "needs an injected write failure; the read-only tests drive it",
	"DamagedSegments":                   "needs a damaged record; the read-corrupt tests drive it",
	"Counters.CorruptExtentsDataLoss":   "needs a corrupt record; the read-corrupt tests drive it",
	"Counters.CorruptExtentsRepairable": "needs a corrupt record; the read-corrupt tests drive it",
	"Counters.StaleExtents":             "needs a stale loss; the durability layer's tests drive it",
	"Counters.SupersededExtents":        "needs a superseded loss; the durability layer's tests drive it",
	"Counters.SpareRefusals":            "needs a roll with no spare ready; the pool's tests drive it",
	"Counters.DoubleRetires":            "needs two reclamation paths on one segment; the retire tests drive it",
	"Counters.FreedBytes":               "needs a retired segment; the retire tests drive it",
	"Counters.SegmentReopens":           "needs a closed descriptor reopened; the descriptor tests drive it",
	"RepackReserveBytes":                "the test core's pool targets no repack reserve",
	"ReservedBytes":                     "a reservation is open only inside a write, and no seam stands inside it; the capacity tests drive it",
}

// TestStatsEveryFieldIsServedOrNamed checks a field of Stats, ShareStats or
// Counters is raised by a scenario above or named in statsUnserved, so a field
// cannot go unserved unseen; and that a named field really is zero.
func TestStatsEveryFieldIsServedOrNamed(t *testing.T) {
	nonzero := map[string]bool{}
	note := func(prefix string, v reflect.Value) {
		for i := range v.NumField() {
			if f := v.Field(i); !f.IsZero() && !((f.Kind() == reflect.Slice || f.Kind() == reflect.Map) && f.Len() == 0) {
				nonzero[prefix+v.Type().Field(i).Name] = true
			}
		}
	}
	for _, sc := range driveStats(t) {
		note("", reflect.ValueOf(sc.s))
		note("Counters.", reflect.ValueOf(sc.s.Counters))
		for _, ss := range sc.s.Shares {
			note("", reflect.ValueOf(ss))
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
	collect("", reflect.TypeOf(ShareStats{}))
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

// TestStatsReportsNoOffloadedBit checks no field is named for, or typed as, an
// extent's offloaded bit.
func TestStatsReportsNoOffloadedBit(t *testing.T) {
	for _, ty := range []reflect.Type{reflect.TypeOf(Stats{}), reflect.TypeOf(Counters{}), reflect.TypeOf(ShareStats{})} {
		for i := range ty.NumField() {
			f := ty.Field(i)
			if strings.Contains(strings.ToLower(f.Name), "offloaded") || strings.Contains(strings.ToLower(f.Type.String()), "offloaded") {
				t.Errorf("%s.%s reports an offloaded bit", ty.Name(), f.Name)
			}
		}
	}
}
