package node

import (
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/journal"
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

// statsFunc is the seam production and a fault-injecting test both write:
// production hands the engine's own Stats, a test one it controls.
type statsFunc func() engine.Stats

// snapshot serves one deadlined, single-flight fetch of the engine's Stats
// to every collector and the health evaluator: the engine series, the
// journal series and the engine's conditions all read it, where each once
// called Engine.Stats on its own. The deadline waits on the injected clock
// alone, never wall time, so a test controls it; single flight keeps a
// wedged journal from leaking one in-flight fetch per scrape.
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
	st := (*s.stats.Load())()
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
	case <-f.after:
		// A second look at done keeps a fetch that landed while the fired
		// timer woke this watcher from being answered as expired.
		select {
		case <-f.done:
		default:
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
		// with the journal's, not their zero values, and this scrape counts
		// in the partial counter: RFC 25's row is exports that omitted a
		// component, so the count is per omitting scrape, not per deadlined
		// flight — the journal collector never counts, and a wedged flight
		// re-omitted by every later scrape keeps the counter's rate honest.
		// Withdraw when Collect carries a context a caller can cancel
		// instead.
		e.snap.partial.WithLabelValues("engine").Inc()
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

// errStatsUnavailable is the cause of the condition a deadlined stats
// fetch raises: the node picked it, so nothing below the engine names it.
var errStatsUnavailable = errors.New("the engine's stats did not answer within 5 s")

// engineConditions is the engine's conditions as the health evaluator reads
// them: its node's, each journal's and each share's, from the same deadlined
// snapshot as the series. subject is the node as conditions name it, the
// one the condition a deadlined fetch raises carries, keyed as every other
// node-scope condition's is.
type engineConditions struct {
	snap    *snapshot
	subject health.Subject
}

func (e *engineConditions) Conditions(dst []health.Condition) []health.Condition {
	s, ok := e.snap.get()
	if !ok {
		// decision: a fetch past its deadline raises one node-scope
		// condition, because leaving the pass empty would read as health
		// while the status writer answers fresh-looking statuses over a
		// wedge: the export names the omission, the condition names the
		// wedge's effect on the node's own health report. The condition is
		// generic — it names no journal to avoid an unwedged peer looking
		// implicated — and its ceiling is single-flight weighting: while
		// any pass can omit the engine's conditions, this is the one honest
		// signal it reads. Withdraw when the engine raises its own condition
		// for a stats deadline, which this then defers to.
		dst = append(dst, health.Condition{
			Name:    "engine_stats_unavailable",
			Subject: e.subject,
			Cause:   errStatsUnavailable,
			Detail:  "the engine's statistics did not answer within their deadline; the export omits its series and this condition names the omission",
		})
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

// journalDesc builds one journal series' descriptor: the dittofs_journal_
// prefix, its help and its labels.
func journalDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dittofs_journal_"+name, help, labels, nil)
}

// counterSpec is one of the journal's cumulative counters, read off the
// Stats the engine carries whole.
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
	{"HeadroomSparesShort", "headroom_spares_short", "Headroom spares an open borrowed for its re-appends and the pool has not made again.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.HeadroomSparesShort) }},
	{"UnreclaimedBytes", "unreclaimed_bytes", "Storage in segments not yet unlinked that backs no held or retained extent and no live record: what repack can recover.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.UnreclaimedBytes) }},
	// decision: UnreclaimedEndedBytes reads zero until Repack seals — the
	// journal's own ponytail names the unmaintained counter — and a zero
	// here is honest: "nothing left to free" is what the journal documents
	// for a journal of clean held content. The series lands with the
	// counter's source, unchanged here.
	{"UnreclaimedEndedBytes", "unreclaimed_ended_bytes", "The unreclaimed bytes in segments that take no further append.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.UnreclaimedEndedBytes) }},
	{"HeadroomBytes", "headroom_bytes", "The headroom's unused reservation for records without bytes, at one record's charge each: not allocated storage.", func(s *journal.Stats, _ time.Time) float64 { return float64(s.HeadroomBytes) }},
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
	get               func(s journal.ShareStats) float64
}

// journalShareGauges is every figure a share is measured by, each read from
// the carried snapshot's Shares map — the one source, the tag the entry's
// TagShares names turning a tag into a share.
var journalShareGauges = []shareGaugeSpec{
	// decision: share_limit is ranked largest-first as though it were a
	// backlog and summed as though it were additive, because the worst-n
	// table serves every per-share figure the same way; a limit is neither,
	// and the sum of limits answers nothing an operator asks. The labelled
	// rows are the useful half. Revisit if a dashboard ever needs a total
	// limit, which would want its own name and kind.
	{"Limit", "share_limit", "The share's local journal limit.", func(s journal.ShareStats) float64 { return float64(s.Limit) }},
	{"UsedBytes", "share_used_bytes", "Record bytes of the share's records still on disk.", func(s journal.ShareStats) float64 { return float64(s.UsedBytes) }},
	{"HeldBytes", "share_held_bytes", "The share's held extent bytes.", func(s journal.ShareStats) float64 { return float64(s.HeldBytes) }},
	{"UnreclaimedBytes", "share_unreclaimed_bytes", "The unreclaimed storage counted to the share.", func(s journal.ShareStats) float64 { return float64(s.UnreclaimedBytes) }},
	{"ReservedBytes", "share_reserved_bytes", "The share's record bytes reserved by in-flight writes, not yet written.", func(s journal.ShareStats) float64 { return float64(s.ReservedBytes) }},
	{"DirtyBytes", "share_dirty_bytes", "The share's held bytes whose offloaded bit is unset.", func(s journal.ShareStats) float64 { return float64(s.DirtyBytes) }},
	{"UnreclaimedEndedBytes", "share_unreclaimed_ended_bytes", "The share's unreclaimed bytes in segments that take no further append.", func(s journal.ShareStats) float64 { return float64(s.UnreclaimedEndedBytes) }},
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
// written reason, which RFC 1 §3.9's export contract demands of every field
// it does not name a series for. The reflection test walks it against
// journal.Stats.
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
	// decision: the descriptor counters are absent from journal.Counters —
	// nothing maintains a source for either — and a zero would claim nothing
	// is overrun. Each series lands with its source (RFC 1 §3.9's table holds
	// their names).
	"Counters.DescriptorOverruns":     {absent: true, reason: "absent from journal.Counters; nothing maintains a source yet"},
	"Counters.DescriptorOpenFailures": {absent: true, reason: "absent from journal.Counters; nothing maintains a source yet"},

	// decision: the read-only state's own fields carry the diagnosis behind
	// the flag, and an export keeps no error text and no check identity: the
	// probe's findings are read by the journal_read_only condition, which
	// names the failing check as its cause.
	"ReadOnlyState.Err":       {reason: "the error that made the journal read-only is excluded text; the read_only flag carries the state"},
	"ReadOnlyState.SpareMade": {reason: "whether a spare was made after the journal went read-only is the probe's figure for the read-only condition, not a series"},
	"ReadOnlyState.Check":     {reason: "the check a probe found failing names the read-only condition's cause; no check identity is exported"},

	// decision: the failed sync's error text and time are the tracer's and
	// the downtime tooling's; the export keeps only the flag, never the
	// error text (RFC 25 §2.8).
	"SyncFailure.Err": {reason: "the sync error's text is excluded; the last_sync_error flag carries whether one failed"},
	"SyncFailure.At":  {reason: "when the sync failed is the tracer's and alerting window's; the flag carries whether one failed"},

	// decision: every per-event and per-file field below needs the file
	// label, which RFC 25 §2.8 forbids an export to carry; the bounded list
	// these fields belong to is served per event to the tracer, and the
	// counts (tag_conflicts, loss_generation) are the exported figures.
	"TagConflict.File":    {reason: "a file label is forbidden in an export; tag_conflicts counts the conflicts"},
	"TagConflict.Tags":    {reason: "the two tags are journal-internal identities; tag_conflicts carries the count alone"},
	"LossEvent.SeqNum":    {reason: "the event's sequence number needs the event's file label, which the export forbids"},
	"LossEvent.Share":     {reason: "the lost extent's share is a label the export forbids per event; the count alone is exported"},
	"LossEvent.File":      {reason: "a file label is forbidden in an export; loss_generation carries every loss past"},
	"LossEvent.Extent":    {reason: "the lost extent's range needs a file label, which the export forbids"},
	"LossEvent.Version":   {reason: "the lost write's version needs the file label, which the export forbids"},
	"LossEvent.Reason":    {reason: "the drop's reason per event needs the file label, which the export forbids"},
	"LossEvent.Offloaded": {reason: "the extent's bit per event needs the file label, which the export forbids"},
	"LossEvent.Synced":    {reason: "per-event sync state needs the file label, which the export forbids"},
	"LossEvent.AtOpen":    {reason: "per-event open state needs the file label, which the export forbids"},
	"LossEvent.Deferred":  {reason: "per-event deferral needs the file label, which the export forbids"},

	// decision: the recovery remainder the RFC's row names — the
	// reconciliation a recovered journal owes, and the byte ranges no
	// bounded read completed — live outside journal.Stats: the first in the
	// engine's open path, the second on the open's refusal, where the
	// journal_range_unreadable condition reads them. The journal side owns
	// carrying either if a Stat ever asks for it.
	"Recovery.ReconcileOwed":    {absent: true, reason: "the reconciliation a recovery owes runs in the engine's open path, not a journal Stat; the journal side owns the figure"},
	"Recovery.UnreadableRanges": {absent: true, reason: "ranges no bounded read completed ride the open's refusal, where the journal_range_unreadable condition reads them; the journal side owns carrying them in a Stat"},
}

// corruptExtentsName is the one labelled counter's series: extents dropped
// as corrupt, by the outcome their offloaded bit names.
const corruptExtentsName = "dittofs_journal_corrupt_extents_total"

var corruptExtents = journalDesc("corrupt_extents_total", "Extents dropped as corrupt; any data_loss is an alert.", "journal", "outcome")

// syncSeconds reads the journal's sync duration histogram, named after the
// field it answers (Stats.SyncDuration), seconds its unit.
var syncSeconds = journalDesc("sync_duration_seconds", "Time each sync took.", "journal")

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
	out["dittofs_journal_sync_duration_seconds"] = syncSeconds
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
		// would read an empty, healthy journal. The omission counts under
		// the journal's own component of the partial counter — one per
		// omitting scrape, as the engine's collector counts its own — so
		// the per-component rate stays honest under a lasting wedge.
		c.snap.partial.WithLabelValues("journal").Inc()
		return
	}
	now := c.snap.now()
	for _, js := range st.Journals {
		s := js.Stats
		if s == nil {
			// decision: a journal no open is behind — refused at Start or
			// after Close — exports no series: its zero snapshot would
			// read as a healthy, empty journal. The engine's conditions
			// carry the refusal.
			continue
		}
		id := hex.EncodeToString(js.ID[:])
		if js.ID == (journal.JournalID{}) {
			// decision: a journal with no identity serves no series: the
			// journal label is the identity, never a device path, and the
			// engine fills it where the snapshot is filled, so an open
			// journal's label is the same identity its conditions name. A
			// zero here names a journal that never opened, whose entry
			// carries no snapshot anyway.
			continue
		}
		// decision: the journal label carries the journal's internal
		// identity, though RFC 25 §2.4 asks an entity label take the
		// object's name, because no name exists to carry: a journal owns
		// only its identity, its device path may not be exported, and the
		// identity is the key RFC 25 §3.9's conditions already subject an
		// operator's alert to. Yields when the journal ever gains a name a
		// caller can read.
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
	for _, spec := range journalShareGauges {
		desc := journalDescs["dittofs_journal_"+spec.name]
		var sum float64
		var rows []shareRow
		for tag, ss := range s.Shares {
			value := spec.get(ss)
			sum += value
			// A share's tag, once: TagShares names every attached share's
			// tag, and a tag it omits belongs to no attached share, so it
			// holds a labelled row nowhere and rides the sum alone.
			if sid, attached := js.TagShares[tag]; attached {
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
		// decision: the row with share="" is the unlabelled total over every
		// share that RFC 25 §2.5 asks for — Prometheus drops a label with an
		// empty value on ingest, so share="" is the same gauge without the
		// share label, no second name needed. Read a total here, directly;
		// the labelled rows beside it carry the internal share ID —
		// RFC 25 §2.4 asks an entity label take the object's name, but no
		// name exists to carry: the engine's share bindings hold none —
		// and are only the current worst topShares, for ranking and
		// inspecting individual shares. At more shares than that they are a
		// truncated view, and an unattached tag holds no labelled row at
		// all, so summing over share!="" undercounts. Never sum every row of
		// the name: the total is its own row, and each labelled row is
		// already inside it.
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, sum, id, "")
	}
}

// syncHistogram emits the journal's sync duration distribution, buckets and
// all, read from the snapshot's bounds and counts. The journal keeps one
// count per interval — at or below each bound and above the bound before it
// — while the exposition serves every `le` bucket cumulatively, so the
// running total is what a bucket names: Counts[i] accrued over
// Counts[:i+1], the last count left as the overflow.
//
// decision: the buckets are the journal's own bounds, not one of the
// export's fixed bucket sets, so they drift from every other duration
// histogram a node exports. Move to the shared set — which costs redefining
// the journal's bounds on the journal side, the one place that keeps them —
// when the journal's bounds are redefined, by that change.
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
