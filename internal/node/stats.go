package node

import (
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/prometheus/client_golang/prometheus"
)

// statsDeadline bounds how long one fetch of the engine's Stats may take:
// past it the scrape omits every series the fetch would serve and counts
// the fetch in the health export's partial counter (RFC 25 §2.8).
const statsDeadline = 5 * time.Second

// topShares is how many shares each per-share gauge names: RFC 25 §2.5's
// worst-n rule's n.
//
// ponytail: the constant 20 is RFC 25's own default, for the metrics registry
// carries no metrics.top_shares setting yet; move it to the registry when
// the settings name one.
const topShares = 20

// snapshot serves one deadlined, single-flight fetch of the engine's Stats
// to every collector and the health evaluator: the engine series, the
// journal series and the engine's conditions all read it, where each once
// called Engine.Stats on its own. The deadline waits on the injected clock
// alone, never wall time, so a test controls it; single flight keeps a
// wedged journal from leaking one in-flight fetch per scrape.

// statsFunc is the seam production and a fault-injecting test both write:
// production hands the engine's own Stats, a test one it controls.
type statsFunc func() engine.Stats

type snapshot struct {
	// stats is a pointer so a test can swap the seam while a scrape's fetch
	// is in flight: the swap must be atomic against the reading goroutine,
	// which the fault it injects makes concurrent by design.
	stats   atomic.Pointer[statsFunc]
	clock   engine.Clock
	partial *prometheus.CounterVec // health_export_partial_total, by component

	mu      sync.Mutex
	current *fetch
}

// newSnapshot hands s the seams it serves one deadlined flight at.
func newSnapshot(stats statsFunc, clock engine.Clock, partial *prometheus.CounterVec) *snapshot {
	s := &snapshot{clock: clock, partial: partial}
	s.stats.Store(&stats)
	return s
}

// fetch is one in-flight Stats call, and its deadline. The deadline's timer
// is armed by the leader synchronously, at the clock it read the flight
// under, never by the watcher when it happens to run: a goroutine spawned
// can be scheduled past any later clock step, which would leave its
// watchers' timer armed after the deadline it names has gone by.
type fetch struct {
	done    chan struct{} // closed with the result, ending the flight
	expired chan struct{} // closed once the deadline passed with no result
	after   <-chan struct{}
	st      engine.Stats
}

// get answers the engine's Stats, or false once the fetch's deadline
// passed: a scrape that takes false omits every series it would have
// served, the engine's included, and the health evaluator reads a pass
// without the engine's conditions, rather than wait on the wedge.
//
// Prometheus Collect carries no context, so the deadline runs in a
// goroutine of its own per flight; the leader's timer counts one deadlined
// fetch, never a joining scrape's own.
func (s *snapshot) get() (engine.Stats, bool) {
	s.mu.Lock()
	f := s.current
	lead := f == nil
	if lead {
		f = &fetch{done: make(chan struct{}), expired: make(chan struct{}), after: s.clock.After(statsDeadline)}
		s.current = f
		go s.run(f)
		go s.watch(f)
	}
	s.mu.Unlock()
	// done is served first, at both checks: a fetch ending exactly at the
	// deadline is a served snapshot, never a counted omission.
	select {
	case <-f.done:
		return f.st, true
	default:
	}
	select {
	case <-f.done:
		return f.st, true
	case <-f.expired:
		select {
		case <-f.done:
			return f.st, true
		default:
		}
		return engine.Stats{}, false
	}
}

// now is the node's one reading of time.
func (s *snapshot) now() time.Time { return s.clock.Now() }

// run fetches the Stats and ends the flight, wedged or not: a later get
// then starts a fresh one, with a fresh deadline. Single flight joins only
// callers concurrent with a fetch, and no longer: a collector whose Collect
// begins once the current fetch has ended leads a fresh fetch of its own, so
// two collectors of one scrape — gathered in parallel — can read two
// different snapshots when their starts straddle a fetch's end.
func (s *snapshot) run(f *fetch) {
	var st engine.Stats
	if fn := s.stats.Load(); fn != nil {
		st = (*fn)()
	}
	f.st = st
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
	close(f.done)
}

// watch waits the flight out and, past the deadline, releases its waiters
// with none and counts the omission; it leaves when the flight ends in
// time, so a fetched snapshot raises nothing.
func (s *snapshot) watch(f *fetch) {
	select {
	case <-f.done:
		return
	default:
	}
	select {
	case <-f.done:
	case <-f.after:
		// A second look at done keeps a fetch that landed while the fired
		// timer woke this watcher from counting as an omission.
		select {
		case <-f.done:
		default:
			s.partial.WithLabelValues("engine").Inc()
			close(f.expired)
		}
	}
}

// newPartialCounter is the export's omission counter: once per component
// whose fetch ran past the deadline.
func newPartialCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "dittofs",
			Subsystem: "health",
			Name:      "export_partial_total",
			Help:      "Exports that omitted a component: its snapshot was not read within 5 s, and its series were left out rather than the scrape blocked.",
		},
		[]string{"component"},
	)
}

// engineSeries is the engine's Stats as the registry's series, read from
// one deadlined snapshot shared with the journal's series and the health
// evaluator.
type engineSeries struct{ snap *snapshot }

func engineDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dittofs_engine_"+name, help, labels, nil)
}

var (
	selfFences      = engineDesc("self_fences_total", "Times the node fenced for a store that committed no write transaction.", "reason")
	stabilityPoints = engineDesc("stability_points_total", "Stability points the engine reached.")
	writeRefusals   = engineDesc("write_refusals_total", "Writes the engine refused.")
	// ponytail: a counter of the operations the existence commits committed,
	// where RFC 8 asks for a histogram of the operations each commit
	// committed, because Stats counts the operations and not the commits.
	// Moving to the histogram changes the series' type, so do it, counting
	// the group commit's and a removal's commits in the engine, before a
	// dashboard reads this one.
	pendingOps   = engineDesc("pending_existence_ops", "Operations the existence commits committed.")
	commitSplits = engineDesc("group_commit_splits_total", "Group commit batches split.")
	reads        = engineDesc("reads_total", "Reads, by their outcome.", "class")
	readSeconds  = engineDesc("read_seconds", "Time reads took.")
	lockWaits    = engineDesc("lock_wait_seconds", "Contended takes of a lock.", "area")
	removalRuns  = engineDesc("removal_runs_total", "Removal runs, by their result.", "result")
)

func (engineSeries) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{selfFences, stabilityPoints, writeRefusals, pendingOps, commitSplits, reads, readSeconds, lockWaits, removalRuns} {
		ch <- d
	}
}

func (e engineSeries) Collect(ch chan<- prometheus.Metric) {
	s, ok := e.snap.get()
	if !ok {
		// decision: a fetch past its deadline omits the engine's own series
		// with the journal's, not their zero values: the scrape answers, and
		// the partial counter names the omission. Withdraw when Collect
		// carries a context a caller can cancel instead.
		return
	}
	counter := func(d *prometheus.Desc, v int64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	// ponytail: a histogram with only the +Inf bucket, since Stats carries a
	// series' count and sum and no bucket; buckets are added without changing
	// the series' type once Stats carries them.
	histogram := func(d *prometheus.Desc, v engine.Series, labels ...string) {
		ch <- prometheus.MustNewConstHistogram(d, uint64(v.Count), v.Total.Seconds(), nil, labels...)
	}
	counter(selfFences, s.SelfFences, "store")
	counter(stabilityPoints, s.StabilityPoints)
	counter(writeRefusals, s.WriteRefusals)
	counter(pendingOps, s.PendingExistenceOps)
	counter(commitSplits, s.GroupCommitSplits)
	for class, n := range s.Reads {
		counter(reads, n, class)
	}
	histogram(readSeconds, s.ReadSeconds)
	histogram(lockWaits, s.FileStateLockWaits, "file_state")
	for result, n := range s.RemovalRuns {
		counter(removalRuns, n, result)
	}
}

// engineConditions is the engine's conditions as the health evaluator reads
// them: its node's, each journal's and each share's, from the same deadlined
// snapshot as the series.
type engineConditions struct{ snap *snapshot }

func (e *engineConditions) Conditions(dst []health.Condition) []health.Condition {
	s, ok := e.snap.get()
	if !ok {
		// decision: a fetch past its deadline leaves this pass's conditions
		// out with its series: the evaluator reads a pass without the
		// engine's, and the partial counter names the omission, rather than
		// wait on the wedge that failed the export.
		return dst
	}
	dst = append(dst, s.Node.Conditions...)
	for _, j := range s.Journals {
		dst = append(dst, j.Conditions...)
	}
	for _, sh := range s.Shares {
		dst = append(dst, sh.Conditions...)
	}
	return dst
}

// journalPrefix builds the journal series' full name.
func journalDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dittofs_journal_"+name, help, labels, nil)
}

// counterSpec is one of the journal's cumulative counters (RFC 1 §3.9), read
// off the snapshot the engine carries whole (T13.1).
type counterSpec struct {
	field string // the Counters field it answers, by its Go name
	name  string // the series' name, without the dittofs_journal_ prefix
	help  string
	get   func(c journal.Counters) float64
}

// journalCounters is every counter journal.Counters maintains. The corrupt
// pair is exported alone, labelled by its outcome, below.
var journalCounters = []counterSpec{
	{"WrittenBytes", "written_bytes_total", "Client bytes accepted by WriteAt.", func(c journal.Counters) float64 { return float64(c.WrittenBytes) }},
	{"FilledBytes", "filled_bytes_total", "Remote bytes placed by Fill.", func(c journal.Counters) float64 { return float64(c.FilledBytes) }},
	{"FillRefused", "fill_refused_total", "Fills refused as older than the file.", func(c journal.Counters) float64 { return float64(c.FillRefused) }},
	{"OfferedBytes", "offered_bytes_total", "Bytes offered at offload.", func(c journal.Counters) float64 { return float64(c.OfferedBytes) }},
	{"MarkedBytes", "marked_bytes_total", "Bytes marked offloaded.", func(c journal.Counters) float64 { return float64(c.MarkedBytes) }},
	{"ReleasedBytes", "released_bytes_total", "Bytes released.", func(c journal.Counters) float64 { return float64(c.ReleasedBytes) }},
	{"FreedBytes", "freed_bytes_total", "Storage returned by retiring segments: to the filesystem by an unlink, or to the spare pool by a recycle.", func(c journal.Counters) float64 { return float64(c.FreedBytes) }},
	{"RepackedBytes", "repacked_bytes_total", "Bytes moved by repack.", func(c journal.Counters) float64 { return float64(c.RepackedBytes) }},
	{"ReservationRefused", "reservation_refused_total", "Writes refused at capacity.", func(c journal.Counters) float64 { return float64(c.ReservationRefused) }},
	{"Syncs", "syncs_total", "Syncs issued.", func(c journal.Counters) float64 { return float64(c.Syncs) }},
	{"SyncFailures", "sync_failures_total", "Syncs failed.", func(c journal.Counters) float64 { return float64(c.SyncFailures) }},
	{"SyncReappendedBytes", "sync_reappended_bytes_total", "Bytes re-appended to a new segment after a failed sync.", func(c journal.Counters) float64 { return float64(c.SyncReappendedBytes) }},
	{"HeadroomDraws", "headroom_draws_total", "Records without bytes appended from the reserved headroom while the journal was at its limit.", func(c journal.Counters) float64 { return float64(c.HeadroomDraws) }},
	{"StaleExtents", "stale_extents_total", "Extents dropped as older than offloaded content.", func(c journal.Counters) float64 { return float64(c.StaleExtents) }},
	{"SupersededExtents", "superseded_extents_total", "Extents dropped as older than metadata.", func(c journal.Counters) float64 { return float64(c.SupersededExtents) }},
	{"TornTails", "torn_tails_total", "Torn tails truncated at open.", func(c journal.Counters) float64 { return float64(c.TornTails) }},
	{"ScanResyncs", "scan_resyncs_total", "Scans that found a record by resynchronising past a bad one.", func(c journal.Counters) float64 { return float64(c.ScanResyncs) }},
	{"IndexRefusals", "index_refusals_total", "Calls refused naming the index.", func(c journal.Counters) float64 { return float64(c.IndexRefusals) }},
	{"SpareRefusals", "spare_refusals_total", "Rolls refused at once because no spare was ready.", func(c journal.Counters) float64 { return float64(c.SpareRefusals) }},
	{"SegmentReopens", "segment_reopens_total", "Reads that had to reopen a closed segment.", func(c journal.Counters) float64 { return float64(c.SegmentReopens) }},
	{"DoubleRetires", "double_retires_total", "Retires that subtracted nothing from the accounted footprint.", func(c journal.Counters) float64 { return float64(c.DoubleRetires) }},
}

// gaugeSpec is one scalar figure of the journal's Stats (RFC 1 §3.7), or the
// five an operator needs most read from them.
type gaugeSpec struct {
	field string // the Stats field it answers, by its Go name, or "Recovery.<Field>"
	name  string
	help  string
	get   func(st *journal.Stats, now time.Time) float64
}

// journalGauges is every scalar figure Stats carries that a series answers.
// The five an operator needs most are computed here: the backlog's age in
// seconds, the failed-sync and read-only flags — their text stays out of the
// labels — and recovery's three.
var journalGauges = []gaugeSpec{
	{"MaxBytes", "max_bytes", "The journal's configured capacity.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.MaxBytes) }},
	{"UsedBytes", "used_bytes", "Local storage allocated: the segments, each at its accounted size.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.UsedBytes) }},
	{"ReservedBytes", "reserved_bytes", "Record bytes reserved by in-flight writes inside allocated segments, not yet written.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.ReservedBytes) }},
	{"HeldBytes", "held_bytes", "Sum of held extent lengths.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.HeldBytes) }},
	{"DirtyBytes", "dirty_bytes", "Held bytes whose offloaded bit is unset.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.DirtyBytes) }},
	{"OldestDirty", "oldest_dirty_seconds", "Age of the oldest held extent not yet offloaded; zero when nothing is dirty.", func(s *journal.Stats, now time.Time) float64 {
		if s.OldestDirty.IsZero() {
			return 0
		}
		return now.Sub(s.OldestDirty).Seconds()
	}},
	{"Files", "files", "Files with at least one held extent.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.Files) }},
	{"ExtentCount", "extent_count", "Placement index entries: held extents, retained extents and removal markers.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.ExtentCount) }},
	{"DropRanges", "drop_ranges", "Ranges the removal-drop test keeps per file and segment.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.DropRanges) }},
	{"Segments", "segments", "Segments in total.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.Segments) }},
	{"Sealed", "sealed", "Sealed segments.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.Sealed) }},
	{"Spares", "spares", "Content spare segments the pool holds, ready for a stream to take.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.Spares) }},
	{"UnreclaimedBytes", "unreclaimed_bytes", "Storage in segments not yet unlinked that backs no held or retained extent and no live record: what repack can recover.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.UnreclaimedBytes) }},
	{"UnreclaimedEndedBytes", "unreclaimed_ended_bytes", "The unreclaimed bytes in segments that take no further append.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.UnreclaimedEndedBytes) }},
	{"HeadroomBytes", "headroom_bytes", "Reserved headroom for records without bytes.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.HeadroomBytes) }},
	{"RepackReserveBytes", "repack_reserve_bytes", "The repack reserve, as allocated now.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.RepackReserveBytes) }},
	{"PinnedSegments", "pinned_segments", "Segments held by running offers.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.PinnedSegments) }},
	{"OpenDescriptors", "open_descriptors", "Open segment descriptors.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.OpenDescriptors) }},
	{"DescriptorLimit", "descriptor_limit", "The bound open descriptors are held under.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.DescriptorLimit) }},
	{"DamagedSegments", "damaged_segments", "Segments holding records that do not verify.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.DamagedSegments) }},
	{"RemovalMarkers", "removal_markers", "Removal markers not yet settled: a caller that never settles.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.RemovalMarkers) }},
	{"VersionsLeft", "versions_left", "Versions remaining below the limit.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.VersionsLeft) }},
	{"ExtentLimit", "extent_limit", "The configured index entry bound.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.ExtentLimit) }},
	{"RejectedSegments", "rejected_segments", "Segments not attached at open.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.RejectedSegments) }},
	{"LossGeneration", "loss_generation", "Raised on every loss of content not yet offloaded and every failed sync window; never lower than any value this process exposed.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.LossGeneration) }},
	{"TagConflicts", "tag_conflicts", "Files recovery rebuilt under two tags; every call on one is refused until one tag is forgotten.", func(s *journal.Stats, _ time.Time) float64 { return float64(len(s.TagConflicts)) }},
	{"LastSyncError", "last_sync_error", "One when a sync has failed since open, zero when none has; never the error text.", func(s *journal.Stats, _ time.Time) float64 {
		if s.LastSyncError == nil {
			return 0
		}
		return 1
	}},
	{"ReadOnly", "read_only", "One when the journal refuses writes, zero when it takes them; never the state's cause.", func(s *journal.Stats, _ time.Time) float64 {
		if s.ReadOnly.ReadOnly {
			return 1
		}
		return 0
	}},
	{"Recovery.Duration", "recovery_duration_seconds", "How long the last open's rebuild took.", func(s *journal.Stats, _ time.Time) float64 { return s.Recovery.Duration.Seconds() }},
	{"Recovery.CatalogSegments", "recovery_catalog_segments", "Segments the last open read by their catalog.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.Recovery.CatalogSegments) }},
	{"Recovery.ScannedSegments", "recovery_scanned_segments", "Segments the last open read by scan.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.Recovery.ScannedSegments) }},
}

// shareGaugeSpec is one per-share figure, worst-n under RFC 25 §2.5. The
// four the engine carries its own copies of read those, never the journal's
// Shares map, for an attached share: one source per figure. The journal's
// map feeds every figure for a share no engine copy holds.
type shareGaugeSpec struct {
	field, name, help string
	// getEngine reads the engine's own copy of the figure; nil when the
	// journal's map is the figure's only source.
	getEngine  func(s engine.JournalShareStats) float64
	getJournal func(s journal.ShareStats) float64
}

// journalShareGauges is every figure a share is measured by, each tie-named
// for its source.
var journalShareGauges = []shareGaugeSpec{
	{"Limit", "share_limit", "The share's local journal limit.", func(s engine.JournalShareStats) float64 { return float64(s.Limit) }, func(s journal.ShareStats) float64 { return float64(s.Limit) }},
	{"UsedBytes", "share_used_bytes", "Record bytes of the share's records still on disk.", func(s engine.JournalShareStats) float64 { return float64(s.UsedBytes) }, func(s journal.ShareStats) float64 { return float64(s.UsedBytes) }},
	{"HeldBytes", "share_held_bytes", "The share's held extent bytes.", func(s engine.JournalShareStats) float64 { return float64(s.HeldBytes) }, func(s journal.ShareStats) float64 { return float64(s.HeldBytes) }},
	{"UnreclaimedBytes", "share_unreclaimed_bytes", "The unreclaimed storage counted to the share.", func(s engine.JournalShareStats) float64 { return float64(s.UnreclaimedBytes) }, func(s journal.ShareStats) float64 { return float64(s.UnreclaimedBytes) }},
	{"ReservedBytes", "share_reserved_bytes", "The share's record bytes reserved by in-flight writes, not yet written.", nil, func(s journal.ShareStats) float64 { return float64(s.ReservedBytes) }},
	{"DirtyBytes", "share_dirty_bytes", "The share's held bytes whose offloaded bit is unset.", nil, func(s journal.ShareStats) float64 { return float64(s.DirtyBytes) }},
	{"UnreclaimedEndedBytes", "share_unreclaimed_ended_bytes", "The share's unreclaimed bytes in segments that take no further append.", nil, func(s journal.ShareStats) float64 { return float64(s.UnreclaimedEndedBytes) }},
}

// markSpec is one figure of a stream's sync mark, under a stream label.
type markSpec struct {
	field, name, help string
	get               func(m journal.SyncMark) float64
}

// journalSyncMarks is every sync-mark figure an operator can see; the stream
// itself is the label.
var journalSyncMarks = []markSpec{
	{"Through", "sync_mark_through", "The synced-through offset of the stream's last settled mark.", func(m journal.SyncMark) float64 { return float64(m.Through) }},
	{"Offset", "sync_mark_offset", "The end of the last verifying record of the stream's settled mark.", func(m journal.SyncMark) float64 { return float64(m.Offset) }},
	{"Sequence", "sync_mark_sequence", "The sequence number of the stream's settled mark.", func(m journal.SyncMark) float64 { return float64(m.Sequence) }},
}

// exclusion is a journal.Stats field no series answers, and why. absent
// names a field nothing maintains a source for yet: the day it is added the
// reflection test fails, until a series or an honest reason replaces the
// entry.
type exclusion struct {
	absent bool
	reason string
}

// journalExcluded is every field the tables above do not answer, with its
// written reason (RFC 25 §2.8 keeps a series scarce only by design, R2
// requires the reason). The reflection test walks it against journal.Stats.
var journalExcluded = map[string]exclusion{
	// decision: SuspectSegments is carried but never set — nothing
	// maintains a source for it — and a zero would claim nothing is
	// suspect. The series lands with its source.
	"SuspectSegments": {reason: "carried but never set: a zero would claim nothing is suspect; the series lands with its source"},
	// decision: VersionLimit and NextVersion are static per open, and
	// VersionsLeft carries the level an operator or an alert needs; a
	// series per static value grows the export without an action to take.
	// Revisit when a dashboard asks for either by name.
	"VersionLimit": {reason: "a static bound; versions_left is the exported gauge an operator needs"},
	"NextVersion":  {reason: "the next version the counter issues, a moment-in-time value; not charted"},
	// decision: Losses is a bounded list of events and a series per event
	// needs the file label, which RFC 25 §2.8 forbids; loss_generation
	// carries every loss past, and the loss events' detail is the tracer's
	// (RFC 1 §3.8).
	"Losses": {reason: "a series per loss event needs the file label, which the export forbids; loss_generation carries the count"},
	// decision: the mark's segment names the on-disk record behind it; the
	// through-offset and the sequence carry what an operator needs, and the
	// segment is a filesystem detail behind the journal's name.
	"SyncMark.Segment": {reason: "the mark's segment is the record's on-disk location; through and sequence carry what an operator needs"},
	// decision: the stream a mark belongs to is the label on each
	// sync_mark series, not a series of its own.
	"SyncMark.Stream": {reason: "the stream itself is the stream label on each sync_mark series"},
	// decision: HeadroomSparesShort and the descriptor counters are absent
	// from journal.Stats — nothing maintains a source for any of them — and
	// a zero would claim nothing is short or overrun. Each series lands
	// with its source (RFC 1 §3.9's table holds their names).
	"HeadroomSparesShort":             {absent: true, reason: "absent from journal.Stats; nothing maintains a source yet"},
	"Counters.DescriptorOverruns":     {absent: true, reason: "absent from journal.Counters; nothing maintains a source yet"},
	"Counters.DescriptorOpenFailures": {absent: true, reason: "absent from journal.Counters; nothing maintains a source yet"},
}

// corruptExtentsName is the one labelled counter's series: extents dropped
// as corrupt, by the outcome their offloaded bit names.
const corruptExtentsName = "dittofs_journal_corrupt_extents_total"

var corruptExtents = journalDesc("corrupt_extents_total", "Extents dropped as corrupt; any data_loss is an alert.", "journal", "outcome")

// syncSeconds reads the journal's sync duration histogram.
var syncSeconds = journalDesc("sync_seconds", "Time each sync took.", "journal")

// journalDeps declares every series the collector emits, once, for the
// registry's consistency check.
func journalDeps() map[string]*prometheus.Desc {
	out := map[string]*prometheus.Desc{}
	for _, c := range journalCounters {
		out["dittofs_journal_"+c.name] = journalDesc(c.name, c.help, "journal")
	}
	out[corruptExtentsName] = corruptExtents
	for _, g := range journalGauges {
		out["dittofs_journal_"+g.name] = journalDesc(g.name, g.help, "journal")
	}
	for _, g := range journalShareGauges {
		out["dittofs_journal_"+g.name] = journalDesc(g.name, g.help, "journal", "share")
	}
	for _, m := range journalSyncMarks {
		out["dittofs_journal_"+m.name] = journalDesc(m.name, m.help, "journal", "stream")
	}
	out["dittofs_journal_sync_seconds"] = syncSeconds
	return out
}

// journalDescs is built once, so a scrape holds only the metrics it emits.
var journalDescs = journalDeps()

// journalSeries is the journal's Stats as the node's journal_ series: one
// read-through collector over the same deadlined snapshot the engine's own
// series read, the journal labelled with its hex identity and never its
// device path, every per-share gauge worst-n under RFC 25 §2.5. The
// decision: markers inside record what it deliberately omits.
type journalSeries struct {
	snap *snapshot
	ids  func(dir string) (string, bool) // the device's journal identity, hex
}

func (c *journalSeries) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range journalDescs {
		ch <- d
	}
}

// shareRow is one attached share's figure, for the worst-n selection.
type shareRow struct {
	share string
	value float64
}

func (c *journalSeries) Collect(ch chan<- prometheus.Metric) {
	st, ok := c.snap.get()
	if !ok {
		// decision: a fetch past its deadline omits every journal series:
		// the snapshot that would fill them never came, and their zeros
		// would read an empty, healthy journal. The engine's own series
		// omit with them.
		return
	}
	now := c.snap.now()
	for dir, js := range st.Journals {
		s := js.Stats
		if s == nil {
			// decision: a journal no open is behind — refused at Start or
			// after Close — exports no series: its zero snapshot would
			// read as a healthy, empty journal. The engine's conditions
			// carry the refusal.
			continue
		}
		id, ok := c.ids(dir)
		if !ok {
			// decision: a journal whose identity the store cannot resolve
			// serves no series: the journal label is the identity, never a
			// path, and an unresolved device names no identity at all.
			// Never reached while the engine opens recorded journals only.
			continue
		}
		for _, spec := range journalCounters {
			ch <- prometheus.MustNewConstMetric(journalDescs["dittofs_journal_"+spec.name], prometheus.CounterValue, spec.get(s.Counters), id)
		}
		ch <- prometheus.MustNewConstMetric(corruptExtents, prometheus.CounterValue, float64(s.Counters.CorruptExtentsDataLoss), id, "data_loss")
		ch <- prometheus.MustNewConstMetric(corruptExtents, prometheus.CounterValue, float64(s.Counters.CorruptExtentsRepairable), id, "repairable")
		for _, spec := range journalGauges {
			ch <- prometheus.MustNewConstMetric(journalDescs["dittofs_journal_"+spec.name], prometheus.GaugeValue, spec.get(s, now), id)
		}
		for stream, m := range s.SyncMarks {
			for _, spec := range journalSyncMarks {
				ch <- prometheus.MustNewConstMetric(journalDescs["dittofs_journal_"+spec.name], prometheus.GaugeValue, spec.get(m), id, strconv.FormatUint(uint64(stream), 10))
			}
		}
		c.shareGauges(ch, js, s, id)
		c.syncHistogram(ch, s, id)
	}
}

// shareGauges emits each per-share figure: the worst topShares shares by
// that gauge — largest, as every one of these figures is a backlog or
// occupancy figure — recomputed at every export, and beside them the sum
// over every share.
func (c *journalSeries) shareGauges(ch chan<- prometheus.Metric, js engine.JournalStats, s *journal.Stats, id string) {
	// A share's tag, once: TagShares names every attached share's tag.
	byTag := make(map[journal.ShareTag]entity.ShareID, len(js.TagShares))
	for tag, sid := range js.TagShares {
		byTag[tag] = sid
	}
	for _, spec := range journalShareGauges {
		desc := journalDescs["dittofs_journal_"+spec.name]
		var sum float64
		var rows []shareRow
		for tag, ss := range s.Shares {
			// One source per figure: an attached share's engine copy for
			// the four the engine carries, the journal's map otherwise —
			// and, for a tag no attached share holds, the map alone.
			sid, attached := byTag[tag]
			value := spec.getJournal(ss)
			if attached && spec.getEngine != nil {
				value = spec.getEngine(js.Shares[sid])
			}
			sum += value
			if attached {
				rows = append(rows, shareRow{share: sid.String(), value: value})
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].value != rows[j].value {
				return rows[i].value > rows[j].value
			}
			return rows[i].share < rows[j].share
		})
		if len(rows) > topShares {
			rows = rows[:topShares]
		}
		for _, row := range rows {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, row.value, id, row.share)
		}
		// decision: the unlabelled sum rides the same series name with an
		// empty share value, because the client library refuses one name
		// under two label dimensions — a sum without the share label
		// beside the labelled rows is what the name cannot carry. The RFC
		// itself makes the shape deliberate: it asks for the same gauge
		// "without a share label, as the sum over every share". The
		// double-count hazard this shape carries is real, so a PromQL total
		// must read the labelled rows alone — sum(...) share!="" — and
		// never every row of the name, which counts each share's bytes
		// once as labelled and once inside the sum. Split the name only if
		// the RFC's wording or the library ever allows the two shapes.
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, sum, id, "")
	}
}

// syncHistogram emits the journal's sync duration distribution, buckets and
// all, read from the snapshot's bounds and counts. The journal keeps one
// count per interval — at or below each bound and above the bound before it
// — while the exposition serves every `le` bucket cumulatively, so the
// running total is what a bucket names: Counts[i] accrued over
// Counts[:i+1], the last count left as the overflow.
func (c *journalSeries) syncHistogram(ch chan<- prometheus.Metric, s *journal.Stats, id string) {
	h := s.SyncDuration
	if len(h.Counts) < len(h.Bounds) {
		// A histogram too short to fill its bounds serves nothing: naming a
		// bucket it cannot count would under-report every count after it.
		return
	}
	buckets := make(map[float64]uint64, len(h.Bounds))
	var past, count uint64
	for i, b := range h.Bounds {
		past += h.Counts[i]
		buckets[b.Seconds()] = past
	}
	for _, n := range h.Counts {
		count += n
	}
	ch <- prometheus.MustNewConstHistogram(syncSeconds, count, h.Sum.Seconds(), buckets, id)
}

// journalIdentity resolves a device's journal identity once per device, hex,
// as the journal label names it: never the device path.
func journalIdentity(lookup func(dir string) (id [16]byte, ok bool)) func(dir string) (string, bool) {
	resolved := map[string]string{}
	var mu sync.Mutex
	return func(dir string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if id, ok := resolved[dir]; ok {
			return id, true
		}
		// decision: one store read per device, at the first scrape that
		// names it, keeps the metric path off the store for every later
		// scrape; the identity is recorded at initialisation and never
		// changes while the node runs.
		id, ok := lookup(dir)
		if !ok {
			return "", false
		}
		hexed := hex.EncodeToString(id[:])
		resolved[dir] = hexed
		return hexed, true
	}
}
