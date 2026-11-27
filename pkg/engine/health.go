package engine

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/workqueue"
)

// conditionJournalLost is RFC 25 §3.9's condition for a journal found
// missing or foreign: unhealthy, until an operator acknowledges the loss.
const conditionJournalLost = "journal_lost"

// conditionJournalUnsuitable is RFC 25 §3.9's condition for a journal whose
// open the platform profile refuses, or whose retried open failed: its shares
// stay unattached until an open succeeds (RFC 8 §2.5).
const conditionJournalUnsuitable = "journal_unsuitable"

// conditionJournalOpenRefused is the condition RFC 25 §3.9 registers as
// journal_open_refused: unhealthy, scoped to the journal, paged at 15 min. It
// is raised for a journal whose open the journal itself refuses at recovery
// for a cause a retry can clear, a lost synced tail or a range that names no
// record. Its shares stay unattached and the open is retried every 30 s
// (RFC 8 §2.5); a record kind the journal cannot build fails Start instead
// and raises no condition.
const conditionJournalOpenRefused = "journal_open_refused"

// conditionJournalRangeUnreadable is RFC 25 §3.9's condition for a journal
// some of whose bytes no bounded read completed: its shares stay unattached
// and the open is retried (RFC 8 §2.5).
const conditionJournalRangeUnreadable = "journal_range_unreadable"

// conditionJournalReadOnly is RFC 8 §11.1's condition for a journal turned
// read-only: it refuses writes and serves reads.
const conditionJournalReadOnly = "journal_read_only"

// readOnlyCondition is journal_read_only for the journal id of device dir in
// state ro, and whether ro is read-only. It names the check the journal's
// probe found failing, which keeps the state, or else the error that entered
// it.
func readOnlyCondition(ro journal.ReadOnlyState, dir string, id journal.JournalID) (Condition, bool) {
	if !ro.ReadOnly {
		return Condition{}, false
	}
	cause := ro.Err
	if ro.Check != nil {
		cause = ro.Check
	}
	return Condition{
		Name:    conditionJournalReadOnly,
		Subject: Subject{Scope: ScopeJournal, ID: hex.EncodeToString(id[:]), Name: dir},
		Cause:   cause,
		Detail:  "the journal refuses writes until it is reopened; reads are served",
	}, true
}

// The engine raises the health package's conditions, so the node's evaluator
// reads them as they are.
type (
	Condition = health.Condition
	Subject   = health.Subject
	Scope     = health.Scope
)

const (
	ScopeJournal = health.ScopeJournal
	ScopeNode    = health.ScopeNode
	ScopeShare   = health.ScopeShare
)

// JournalStats is one device's journal, as Stats reports it.
type JournalStats struct {
	// ID is the journal's identity, as the store records it: the hex of
	// the ID is the journal_ series' journal label and the subject of its
	// conditions, one identity for both. Zero on a refusal that names no
	// journal it opened.
	ID journal.JournalID
	// Stats is the journal's own snapshot, one copy per Engine.Stats call.
	// nil when no journal is open behind this entry — a device refused at
	// Start or after Close — so no zero snapshot can read as healthy.
	Stats *journal.Stats
	// TagShares names the tag of every share attached to this journal, and
	// no other: only the engine knows which share holds a tag, and the
	// snapshot's Shares map, read through this map, is the one source of
	// every per-share figure.
	TagShares  map[journal.ShareTag]entity.ShareID
	Conditions []Condition
	// Work is the journal's work scheduler's figures, keyed by each source's
	// name as the engine registers it (offload, and removal, which runs
	// removal batches and prunes), and then by shard: engine_queue_entries
	// (Pending, Backoff and Running, by state),
	// engine_queue_oldest_pending_seconds,
	// engine_queue_last_done_timestamp_seconds, engine_work_runs_inflight
	// (Running), engine_work_runs_overdue_total (Overdue),
	// engine_work_items_dropped_total (Dropped) and
	// engine_work_items_expired_total (Expired). The three counters keep the
	// scheduler's own types. Overdue is an int beside its gauges, but RFC 8
	// §14 makes engine_work_runs_overdue_total a counter, so Overdue counts
	// every run that has passed twice its run timeout and never falls;
	// Dropped and Expired are uint64. The node adapter converts each to its
	// counter. WorkLimits is the limits each source registered: their
	// Total is engine_queue_entries_limit and their Concurrency
	// engine_work_runs_inflight_limit.
	Work       map[string]map[entity.ShardID]workqueue.Stats
	WorkLimits map[string]workqueue.Limits
	// QueueEvents is engine_queue_events_total by its event label.
	QueueEvents map[string]int64
	// ReseedFilesRemaining is engine_reseed_files_remaining.
	ReseedFilesRemaining int64
}

// NodeStats is this node, as Stats reports it.
type NodeStats struct {
	Conditions []Condition
}

// ShareStats is one share, as Stats reports it. Device and Refused are set
// for a share refused at start: its device, and why.
type ShareStats struct {
	Device     string
	Refused    error
	Conditions []Condition
	// The share's offload figures, zero until offload records them:
	// DirtyBytes is engine_dirty_bytes, DrainBytesPerSecond
	// engine_drain_bytes_per_second, DrainTime engine_drain_seconds,
	// OldestUnoffloaded engine_unoffloaded_oldest_seconds, RemovalsPending
	// engine_removals_pending and OldestRemoval
	// engine_removals_oldest_seconds. DrainTime at a zero rate is as Stats's.
	DirtyBytes          int64
	DrainBytesPerSecond float64
	DrainTime           time.Duration
	OldestUnoffloaded   time.Duration
	RemovalsPending     int64
	OldestRemoval       time.Duration
}

// WaitOffloaded returns once every byte written to the node before the call
// is offloaded; at ctx's end, an error naming the oldest file left and why
// (RFC 8 §11.4). Offload is not built, so it answers unbuilt.
func (e *Engine) WaitOffloaded(ctx context.Context) error {
	return errUnbuilt
}

// newStats is an empty Stats, its maps made.
func newStats() Stats {
	return Stats{
		Journals: map[string]JournalStats{},
		Shares:   map[entity.ShareID]ShareStats{},
	}
}

// clone is a copy of s that shares no map or slice with it, so a caller
// keeps what Stats returns. Each journal's maps are copied, the nested Work
// maps included, so a caller's edit never reaches the engine's record.
func (s Stats) clone() Stats {
	out := newStats()

	for dev, j := range s.Journals {
		j.Conditions = slices.Clone(j.Conditions)
		j.TagShares = maps.Clone(j.TagShares)
		j.WorkLimits = maps.Clone(j.WorkLimits)
		j.QueueEvents = maps.Clone(j.QueueEvents)
		if j.Work != nil {
			work := make(map[string]map[entity.ShardID]workqueue.Stats, len(j.Work))
			for src, shards := range j.Work {
				work[src] = maps.Clone(shards)
			}
			j.Work = work
		}
		out.Journals[dev] = j
	}

	for id, sh := range s.Shares {
		sh.Conditions = slices.Clone(sh.Conditions)
		out.Shares[id] = sh
	}

	return out
}

// The conditions the engine raises as it runs, beside those of start.
const (
	conditionExistenceCommitFailing = "existence_commit_failing"
	conditionRemovalStalled         = "removal_stalled"
	conditionJournalSyncFailed      = "journal_sync_failed"
)

// syncFailedFor is how long journal_sync_failed stands after the latest
// failed window: the window leaves nothing to repair, only writes its
// clients resend.
const syncFailedFor = time.Hour

// errRemovalStalled is removal_stalled's cause.
var errRemovalStalled = fmt.Errorf("the removal failed %d times in a row", stallAfter)

// errSyncWindowFailed is journal_sync_failed's cause.
var errSyncWindowFailed = errors.New("a sync window failed and its unsynced writes were lost")

// condKey keys one raised condition: its name and what it is raised for.
type condKey struct {
	name string
	of   any
}

// raisedCondition is a condition in the set, and when it lapses: zero, never.
type raisedCondition struct {
	c     Condition
	until time.Time
}

// conditionSet is the conditions the engine raises and clears as it runs.
// Its lock is taken last and held for no call, so a raise under a group
// commit's or a removal loop's lock waits on nothing else, and Stats, which
// takes only this one, waits on no content call.
type conditionSet struct {
	mu  sync.Mutex
	set map[condKey]raisedCondition
}

// raise raises c under k, replacing what k held; it lapses at until, unless
// that is zero.
func (s *conditionSet) raise(k condKey, c Condition, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set == nil {
		s.set = map[condKey]raisedCondition{}
	}
	s.set[k] = raisedCondition{c: c, until: until}
}

// clear clears what k holds.
func (s *conditionSet) clear(k condKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.set, k)
}

// current is the conditions standing at now, those lapsed dropped.
func (s *conditionSet) current(now time.Time) []Condition {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Condition
	for k, r := range s.set {
		if !r.until.IsZero() && !now.Before(r.until) {
			delete(s.set, k)
			continue
		}
		out = append(out, r.c)
	}
	slices.SortFunc(out, func(a, b Condition) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Subject.ID, b.Subject.ID), cmp.Compare(a.Detail, b.Detail))
	})
	return out
}
