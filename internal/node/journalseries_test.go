package node

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/prometheus/client_golang/prometheus"
)

const corruptExtentsSeries = "dittofs_journal_corrupt_extents_total"

const journalHex = "0a0b0c0d0e0f0102030405060708090a"

var startish = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// histogramOf reads the one sample of series name's histogram family into
// its cumulative buckets, keyed by upper bound, and its count.
func histogramOf(t *testing.T, c prometheus.Collector, name string) (map[float64]uint64, uint64) {
	t.Helper()
	r := prometheus.NewRegistry()
	if err := r.Register(c); err != nil {
		t.Fatalf("register the collector for %s: %v", name, err)
	}
	fams, err := r.Gather()
	if err != nil {
		t.Fatalf("gather %s: %v", name, err)
	}
	for _, f := range fams {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			h := f.GetMetric()[0].GetHistogram()
			out := map[float64]uint64{}
			for _, b := range h.GetBucket() {
				out[b.GetUpperBound()] = b.GetCumulativeCount()
			}
			return out, h.GetSampleCount()
		}
	}
	t.Fatalf("no one-sample histogram family %s in the scrape", name)
	return nil, 0
}

// scrapeOf gathers every metric c emits into: the series names, and per
// series the sample values keyed by the sample's labels as k=v pairs.
func scrapeOf(t *testing.T, c prometheus.Collector) map[string]map[string]float64 {
	t.Helper()
	r := prometheus.NewRegistry()
	if err := r.Register(c); err != nil {
		t.Fatalf("register the collector: %v", err)
	}
	fams, err := r.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]map[string]float64{}
	for _, f := range fams {
		vals := map[string]float64{}
		for _, m := range f.GetMetric() {
			labels := ""
			for _, l := range m.GetLabel() {
				labels += l.GetName() + "=" + l.GetValue() + ","
			}
			var v float64
			switch {
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				v = float64(m.GetHistogram().GetSampleCount())
			}
			vals[labels] = v
		}
		out[f.GetName()] = vals
	}
	return out
}

// answeredBy builds the field-to-series table the collector declares: every
// field of journal.Stats and its nested shapes that a series answers, keyed
// "<Field>", "Counters.<Field>", "ShareStats.<Field>", "EngineShares.<Field>",
// "SyncMark.<Field>", "Recovery.<Field>" — and fails t on any duplicate:
// two entries under one key would serve whichever the map kept, masking a
// figure with two sources.
func answeredBy(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	add := func(key, series string) {
		if _, dup := out[key]; dup {
			t.Errorf("the table answers journal.Stats %s twice: a figure with two sources serves whichever map wins", key)
		}
		out[key] = series
	}
	for _, c := range journalCounters {
		add("Counters."+c.field, "dittofs_journal_"+c.name)
	}
	// The corrupt pair is two fields under one series, split by the
	// outcome label: distinct keys, so the duplicate check holds for them
	// as for any other figure.
	add("Counters.CorruptExtentsDataLoss", corruptExtentsSeries)
	add("Counters.CorruptExtentsRepairable", corruptExtentsSeries)
	for _, g := range journalGauges {
		add(g.field, "dittofs_journal_"+g.name)
	}
	// The containers themselves are answered by the series their nested
	// shapes each cover, which the walk reads field by field; the histogram
	// is one series of its own.
	add("SyncDuration", "dittofs_journal_sync_duration_seconds")
	for _, g := range journalShareGauges {
		add("ShareStats."+g.field, "dittofs_journal_"+g.name)
	}
	for _, m := range journalSyncMarks {
		add("SyncMark."+m.field, "dittofs_journal_"+m.name)
	}
	add("ReadOnlyState.ReadOnly", "dittofs_journal_read_only")
	return out
}

// absentField says whether a table key names a field that exists today, over
// the shapes the reflection read walks.
func absentField(key string) bool {
	shapes := []struct {
		ty     reflect.Type
		prefix string
	}{
		{reflect.TypeFor[journal.Stats](), ""},
		{reflect.TypeFor[journal.Counters](), "Counters."},
		{reflect.TypeFor[journal.ShareStats](), "ShareStats."},
		{reflect.TypeFor[journal.SyncMark](), "SyncMark."},
		{reflect.TypeFor[journal.RecoveryStats](), "Recovery."},
		{reflect.TypeFor[journal.ReadOnlyState](), "ReadOnlyState."},
		{reflect.TypeFor[journal.SyncFailure](), "SyncFailure."},
		{reflect.TypeFor[journal.TagConflict](), "TagConflict."},
		{reflect.TypeFor[journal.LossEvent](), "LossEvent."},
	}
	for _, shape := range shapes {
		name := strings.TrimPrefix(key, shape.prefix)
		if _, ok := shape.ty.FieldByName(name); ok && (shape.prefix != "" || name == key) {
			return false
		}
	}
	return true
}

// The table the collector declares and journal.Stats agree: every field of
// Stats and of the nested Counters, ShareStats, SyncMark, RecoveryStats and
// the engine's own share figures is answered by exactly one series or
// carries a written reason. The day a field is added with no series, or a
// reason is held for a field that has landed, this fails on its assertion.
func TestJournalStatsEveryFieldHasASeries(t *testing.T) {
	t.Parallel()
	answered := answeredBy(t)
	shapes := []struct {
		ty     reflect.Type
		prefix string
	}{
		{reflect.TypeFor[journal.Stats](), ""},
		{reflect.TypeFor[journal.Counters](), "Counters."},
		{reflect.TypeFor[journal.ShareStats](), "ShareStats."},
		{reflect.TypeFor[journal.SyncMark](), "SyncMark."},
		{reflect.TypeFor[journal.RecoveryStats](), "Recovery."},
		{reflect.TypeFor[journal.ReadOnlyState](), "ReadOnlyState."},
		{reflect.TypeFor[journal.SyncFailure](), "SyncFailure."},
		{reflect.TypeFor[journal.TagConflict](), "TagConflict."},
		{reflect.TypeFor[journal.LossEvent](), "LossEvent."},
	}
	// The container fields Stats packs its nested shapes in are walked
	// below by those shapes themselves; every other field needs a series
	// or a reason.
	walkedSeparately := map[string]bool{"Counters": true, "SyncMarks": true, "Shares": true, "Recovery": true}
	for _, shape := range shapes {
		for _, f := range reflect.VisibleFields(shape.ty) {
			key := shape.prefix + f.Name
			if shape.prefix == "" && walkedSeparately[key] {
				continue
			}
			reason, excluded := journalExcluded[key]
			if series, ok := answered[key]; ok {
				if series == "" {
					t.Errorf("journal.Stats %s is answered by an empty series name", key)
				}
				if excluded {
					t.Errorf("journal.Stats %s is answered by %s and excluded for %q: a figure with two entries serves whichever the reader finds first", key, series, reason.reason)
				}
				continue
			}
			if !excluded {
				t.Errorf("journal.Stats %s has neither a series nor a written reason in the table", key)
				continue
			}
			if reason.absent {
				t.Errorf("journal.Stats %s is carried, but the table holds it for a field not yet added", key)
			}
		}
	}

	// Every excluded entry names a field that exists, or is held for a field
	// nothing maintains a source for yet.
	for key, reason := range journalExcluded {
		if reason.absent != absentField(key) {
			t.Errorf("journal.Stats %s: the table names it absent=%v, but the field is absent=%v", key, reason.absent, absentField(key))
		}
	}

	// Every per-share figure of ShareStats has a series, exactly once: the
	// carried snapshot's Shares map is the one source, so a second source
	// for any figure is both impossible by construction and caught by the
	// duplicate check in answeredBy above.
	journalFields := map[string]bool{}
	for _, g := range journalShareGauges {
		journalFields["ShareStats."+g.field] = true
	}
	wantJournal := map[string]bool{}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[journal.ShareStats]()) {
		wantJournal["ShareStats."+f.Name] = true
	}
	if !reflect.DeepEqual(journalFields, wantJournal) {
		t.Errorf("the share series are %v, want exactly every ShareStats field: %v", journalFields, wantJournal)
	}
}

// statsSnapshot builds a journal Stats with a distinct sentinel in every
// field, so a series that reads the wrong one fails the value checks.
func statsSnapshot() *journal.Stats {
	return &journal.Stats{
		MaxBytes: 101, UsedBytes: 102, ReservedBytes: 103, HeldBytes: 104, DirtyBytes: 105,
		OldestDirty: startish.Add(-37 * time.Second),
		Files:       106, ExtentCount: 107, DropRanges: 108,
		Segments: 109, Sealed: 110, Spares: 111, HeadroomSparesShort: 301,
		UnreclaimedBytes: 112, UnreclaimedEndedBytes: 113,
		HeadroomBytes: 114, RepackReserveBytes: 115,
		PinnedSegments: 116, OpenDescriptors: 117, DescriptorLimit: 118,
		DamagedSegments: 119,
		SyncMarks:       map[uint32]journal.SyncMark{7: {Stream: 7, Through: 120, Offset: 121, Sequence: 122}},
		LastSyncError:   &journal.SyncFailure{Err: errors.New("the sync failed"), At: startish},
		SyncDuration: journal.Histogram{
			Bounds: []time.Duration{time.Second, 2 * time.Second},
			Counts: []uint64{1, 2, 3}, Sum: 4 * time.Second,
		},
		RemovalMarkers: 123, VersionsLeft: 124, ExtentLimit: 125,
		Counters: journal.Counters{
			WrittenBytes: 201, FilledBytes: 202, FillRefused: 203, OfferedBytes: 204,
			MarkedBytes: 205, ReleasedBytes: 206, FreedBytes: 207, RepackedBytes: 208,
			ReservationRefused: 209, Syncs: 210, SyncFailures: 211, SyncReappendedBytes: 212,
			HeadroomDraws: 213, CorruptExtentsDataLoss: 214, CorruptExtentsRepairable: 215,
			StaleExtents: 216, SupersededExtents: 217, TornTails: 218, ScanResyncs: 219,
			IndexRefusals: 220, SegmentReopens: 221, SpareRefusals: 222, DoubleRetires: 223,
		},
		RejectedSegments: 126, ReadOnly: journal.ReadOnlyState{ReadOnly: true},
		Recovery:       journal.RecoveryStats{CatalogSegments: 127, ScannedSegments: 128, Duration: 129 * time.Second},
		Losses:         []journal.LossEvent{{}, {}},
		LossGeneration: 130,
		TagConflicts:   []journal.TagConflict{{}, {}, {}},
		Shares: map[journal.ShareTag]journal.ShareStats{1: {
			Limit: 131, UsedBytes: 132, ReservedBytes: 133, HeldBytes: 134,
			DirtyBytes: 135, UnreclaimedBytes: 136, UnreclaimedEndedBytes: 137,
		}},
	}
}

var shareIDs = newShareIDs()

// newShareIDs hands every test distinct share identities.
func newShareIDs() []entity.ShareID {
	ids := make([]entity.ShareID, 32)
	for i := range ids {
		ids[i] = entity.ShareID{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24), 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	}
	return ids
}

// collectorFor builds the journal collector over a snapshot answering st,
// on a clock that reads startish and never advances.
func collectorFor(st engine.Stats) *journalSeries {
	return &journalSeries{snap: newSnapshot(func() engine.Stats { return st }, sim.NewTestClock(startish), newPartialCounter())}
}

// journalID is the identity journalHex names, the label the series carry
// when a snapshot's journals hold it.
var journalID = [16]byte{0xa, 0xb, 0xc, 0xd, 0xe, 0xf, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0xa}

// oneJournal wraps the journal snapshot of one attached share in an engine
// Stats: the snapshot's own Shares map is the per-share figures' one source,
// and TagShares names the share that holds its tag.
func oneJournal(s *journal.Stats) engine.Stats {
	js := engine.JournalStats{ID: journalID, Stats: s, TagShares: map[journal.ShareTag]entity.ShareID{1: shareIDs[0]}}
	return engine.Stats{Journals: map[string]engine.JournalStats{"/dev/test": js}}
}

// expect fails unless series holds exactly one sample over one journal,
// equal to want.
func expect(t *testing.T, scr map[string]map[string]float64, series string, want float64) {
	t.Helper()
	vals, ok := scr[series]
	if !ok {
		t.Errorf("the scrape has no series %s", series)
		return
	}
	if len(vals) != 1 {
		t.Errorf("series %s holds %d samples over one journal, want 1", series, len(vals))
		return
	}
	for _, got := range vals {
		if got != want {
			t.Errorf("series %s = %v, want %v", series, got, want)
		}
	}
}

// Every counter, gauge and operator series of one journal reads its field
// from the snapshot, labelled with the journal's hex identity; the text the
// error and read-only figures carry stays out of the labels.
func TestJournalSeriesReadTheirFields(t *testing.T) {
	t.Parallel()
	s := statsSnapshot()
	c := collectorFor(oneJournal(s))
	scr := scrapeOf(t, c)
	answered := answeredBy(t)

	for key, series := range answered {
		if key == "Counters.CorruptExtentsDataLoss" || key == "Counters.CorruptExtentsRepairable" {
			continue
		}
		vals, ok := scr[series]
		if !ok {
			t.Errorf("the scrape has no series %s answering %s", series, key)
			continue
		}
		if !strings.HasPrefix(series, "dittofs_journal_share_") && len(vals) != 1 {
			t.Errorf("series %s holds %d samples over one journal, want 1", series, len(vals))
			continue
		}
		for labels := range vals {
			if !strings.Contains(labels, "journal="+journalHex) {
				t.Errorf("series %s is labelled %q, want the journal's hex identity %s", series, labels, journalHex)
			}
		}
	}

	// The whole table, checked: every scalar figure's sample equals what its
	// own spec reads off the same snapshot, so a series wired to the wrong
	// getter or the wrong descriptor fails on its neighbour's sentinel —
	// the sentinels in statsSnapshot are distinct per field.
	for _, spec := range journalCounters {
		expect(t, scr, "dittofs_journal_"+spec.name, spec.get(s.Counters))
	}
	for _, spec := range journalGauges {
		expect(t, scr, "dittofs_journal_"+spec.name, spec.get(s, startish))
	}

	// One named value per figure shape too, for the reader: the flags, the
	// counts, and the operator's five.
	want := map[string]float64{
		"dittofs_journal_written_bytes_total":       201,
		"dittofs_journal_sync_failures_total":       211,
		"dittofs_journal_max_bytes":                 101,
		"dittofs_journal_unreclaimed_ended_bytes":   113,
		"dittofs_journal_descriptor_limit":          118,
		"dittofs_journal_rejected_segments":         126,
		"dittofs_journal_loss_generation":           130,
		"dittofs_journal_tag_conflicts":             3,
		"dittofs_journal_oldest_dirty_seconds":      37,
		"dittofs_journal_last_sync_error":           1,
		"dittofs_journal_read_only":                 1,
		"dittofs_journal_recovery_duration_seconds": 129,
	}
	for series, v := range want {
		vals, ok := scr[series]
		if !ok {
			t.Errorf("the scrape has no series %s", series)
			continue
		}
		for labels, got := range vals {
			if got != v {
				t.Errorf("series %s%s = %v, want %v", series, labels, got, v)
			}
		}
	}

	// The corrupt counter is one series labelled by the outcome; no other
	// label distinguishes its two sources.
	corrupt := scr[corruptExtentsSeries]
	if len(corrupt) != 2 {
		t.Fatalf("corrupt_extents_total holds %d samples, want one per outcome: %v", len(corrupt), corrupt)
	}
	for labels, want := range map[string]float64{
		"journal=" + journalHex + ",outcome=data_loss,":  214,
		"journal=" + journalHex + ",outcome=repairable,": 215,
	} {
		if got, ok := corrupt[labels]; !ok || got != want {
			t.Errorf("corrupt_extents_total %q = %v (sample present %v), want %v", labels, got, ok, want)
		}
	}

	// The sync histogram reads the journal's bounds, counts and sum. The
	// journal keeps one count per interval — at or below its bound and
	// above the bound before it — while the exposition's each `le` bucket
	// is cumulative: 1 below 1 s, 3 at or below 2 s, and 6 samples in all.
	buckets, count := histogramOf(t, c, "dittofs_journal_sync_duration_seconds")
	for le, want := range map[float64]uint64{1: 1, 2: 3} {
		if got := buckets[le]; got != want {
			t.Errorf("sync_seconds_bucket{le=%v} = %d, want the cumulative %d", le, got, want)
		}
	}
	if count != 6 {
		t.Errorf("sync_seconds_count = %d, want the counts' sum 6", count)
	}

	// The sync marks, per stream.
	marks := map[string]float64{
		"dittofs_journal_sync_mark_through": 120, "dittofs_journal_sync_mark_offset": 121, "dittofs_journal_sync_mark_sequence": 122,
	}
	for series, v := range marks {
		vals, ok := scr[series]
		if !ok {
			t.Errorf("the scrape has no series %s", series)
			continue
		}
		for labels, got := range vals {
			if !strings.Contains(labels, "stream=7") || got != v {
				t.Errorf("series %s %q = %v, want stream=7 and %v", series, labels, got, v)
			}
		}
	}
}

// shareSample is one row of a per-share series: its share label, empty for
// the unlabelled sum, and its value.
type shareSample struct {
	share string
	value float64
}

// shareRows reads series' samples as share rows, keyed by the journal label
// and, for the sum, an empty share.
func shareRows(t *testing.T, scr map[string]map[string]float64, series string) []shareSample {
	t.Helper()
	vals, ok := scr[series]
	if !ok {
		t.Fatalf("the scrape has no series %s", series)
	}
	var out []shareSample
	for labels, v := range vals {
		if !strings.Contains(labels, "journal="+journalHex) {
			t.Fatalf("series %s row %q carries no journal identity", series, labels)
		}
		var share string
		if rest, ok := cutLabels(labels, "share="); ok {
			share = rest
		}
		out = append(out, shareSample{share: share, value: v})
	}
	return out
}

func cutLabels(labels, n string) (string, bool) {
	i := strings.Index(labels, n)
	if i < 0 {
		return "", false
	}
	rest := labels[i+len(n):]
	if j := strings.IndexByte(rest, ','); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}

// The per-share gauges carry at most twenty share labels, the worst shares
// by that gauge — largest, as every one of them is a backlog or occupancy
// figure — recomputed at every export so a change of worst is followed;
// beside them one sum row covers every share, attached or not. The four
// figures the engine carries its own copy of read that copy for an attached
// share, never the journal's map; the journal's map feeds every other
// figure, and for an unattached tag the sum alone.
func TestJournalSeriesSharesWorstNAndSum(t *testing.T) {
	t.Parallel()
	s := statsSnapshot()
	js := engine.JournalStats{ID: journalID, Stats: s}
	// Attached shares numbered with the index, each holding i+1 dirty KiB
	// and i+1 used KiB, every figure written once, in the snapshot's map.
	var dirtySum, usedSum float64
	js.TagShares = map[journal.ShareTag]entity.ShareID{}
	for i := range 25 {
		tag := journal.ShareTag(i + 1)
		s.Shares[tag] = journal.ShareStats{
			UsedBytes:  int64(i+1) << 10,
			DirtyBytes: int64(i+1) << 10, UnreclaimedBytes: 500, ReservedBytes: 50,
			Limit: 1, HeldBytes: 1, UnreclaimedEndedBytes: 1,
		}
		js.TagShares[tag] = shareIDs[i]
		dirtySum += float64(int64(i+1) << 10)
		usedSum += float64(int64(i+1) << 10)
	}
	// Tag 99 no attached share holds: a detached share's records, counted in
	// the sum and nothing else.
	const unattached = journal.ShareTag(99)
	s.Shares[unattached] = journal.ShareStats{UsedBytes: 777, DirtyBytes: 300}
	st := engine.Stats{Journals: map[string]engine.JournalStats{"d": js}}

	scr := scrapeOf(t, collectorFor(st))

	// Dirty bytes, read from the journal's map: the twenty worst attached
	// shares, and a sum over everyone's.
	dirty := shareRows(t, scr, "dittofs_journal_share_dirty_bytes")
	if sum, ok := splitSum(dirty); !ok || sum != dirtySum+300 {
		t.Errorf("dirty_bytes sum row = %v (present %v), want %v: every share's and the unattached tag's", sum, ok, dirtySum+300)
	}
	seen := shareSet(t, dirty, "dittofs_journal_share_dirty_bytes")
	if len(seen) != 20 {
		t.Errorf("dirty_bytes names %d shares, want the top 20 of 25", len(seen))
	}
	for i := range 5 {
		if seen[shareIDs[i].String()] {
			t.Errorf("dirty_bytes names share %s with %d KiB, one of the five least dirty", shareIDs[i], i+1)
		}
	}
	for i := 5; i < 25; i++ {
		if !seen[shareIDs[i].String()] {
			t.Errorf("dirty_bytes misses share %s with %d KiB, among the twenty most dirty", shareIDs[i], i+1)
		}
	}
	// The sum row is the total, not the sum of the labelled rows: with more
	// shares than the worst-n bound, and a tag no attached share holds, the
	// labelled rows truncate and the unattached one is nowhere labelled, so
	// summing over share!="" undercounts. The pin: total strictly greater.
	var labeled float64
	for _, row := range dirty {
		if row.share != "" {
			labeled += row.value
		}
	}
	if sum, _ := splitSum(dirty); !(sum > labeled) {
		t.Errorf("dirty_bytes sum row %v is not strictly greater than the labelled rows' sum %v: the total covers the shares the worst-n cut and the unattached tag", sum, labeled)
	}

	// Used bytes, the same single source: the sum covers every attached
	// share's and the unattached tag's, and the labelled rows are the worst
	// twenty of the same map.
	used := shareRows(t, scr, "dittofs_journal_share_used_bytes")
	if sum, ok := splitSum(used); !ok || sum != usedSum+777 {
		t.Errorf("used_bytes sum row = %v (present %v), want %v: every attached share's and the unattached tag's 777", sum, ok, usedSum+777)
	}

	// A change of worst between two scrapes is followed: share 0 becomes the
	// dirtiest, and only it joins, evicting yesterday's worst set's least.
	sh0 := s.Shares[journal.ShareTag(1)]
	sh0.DirtyBytes = 26 << 10
	s.Shares[journal.ShareTag(1)] = sh0
	scr = scrapeOf(t, collectorFor(st))
	seen = shareSet(t, shareRows(t, scr, "dittofs_journal_share_dirty_bytes"), "dittofs_journal_share_dirty_bytes")
	if !seen[shareIDs[0].String()] {
		t.Error("dirty_bytes misses share 0 after it became the dirtiest: the worst set was picked once and kept")
	}
	if seen[shareIDs[5].String()] {
		t.Error("dirty_bytes keeps share 5 after it left the twenty worst: a stale entry was not dropped")
	}
}

func splitSum(rows []shareSample) (sum float64, ok bool) {
	for _, row := range rows {
		if row.share == "" {
			return row.value, true
		}
	}
	return 0, false
}

func shareSet(t *testing.T, rows []shareSample, series string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, row := range rows {
		if row.share == "" {
			continue
		}
		out[row.share] = true
	}
	return out
}

// A journal with no open journal behind its entry — a device refused at
// Start or after Close — exports no series: a zero snapshot would read as
// healthy, so the nil entry is skipped whole.
func TestJournalSeriesClosedJournalExportsNothing(t *testing.T) {
	t.Parallel()
	st := engine.Stats{Journals: map[string]engine.JournalStats{
		"refused": {ID: journalID, Stats: nil, TagShares: map[journal.ShareTag]entity.ShareID{}},
	}}
	scr := scrapeOf(t, collectorFor(st))
	for series := range scr {
		if strings.HasPrefix(series, "dittofs_journal_") {
			t.Errorf("a closed journal exported the series %s, want none", series)
		}
	}
	if len(scr) != 0 {
		t.Errorf("the scrape holds %d series families for a journal that is absent, want none: %v", len(scr), scr)
	}
}
