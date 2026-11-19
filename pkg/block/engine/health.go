package engine

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
	// no other: only the engine knows which share holds a tag.
	TagShares map[journal.ShareTag]entity.ShareID
	// Shares is, for each share bound to the journal, the journal's figures
	// for the share's tag.
	Shares     map[entity.ShareID]JournalShareStats
	Conditions []Condition
}

// JournalShareStats is the journal's figures for one share's tag.
type JournalShareStats struct {
	Limit            int64
	UsedBytes        int64
	HeldBytes        int64
	UnreclaimedBytes int64
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
}

// newStats is an empty Stats, its maps made.
func newStats() Stats {
	return Stats{
		Journals: map[string]JournalStats{},
		Shares:   map[entity.ShareID]ShareStats{},
	}
}

// clone is a copy of s that shares no map or slice with it, so a caller
// keeps what Stats returns.
func (s Stats) clone() Stats {
	out := newStats()

	for dev, j := range s.Journals {
		j.Conditions = slices.Clone(j.Conditions)
		j.Shares = maps.Clone(j.Shares)
		j.TagShares = maps.Clone(j.TagShares)
		// A nil snapshot stays nil: it is the refusal and close shape, and
		// cloning it would hand out a zero journal a healthy one reads as.
		if j.Stats != nil {
			st := *j.Stats
			if st.LastSyncError != nil {
				e := *st.LastSyncError
				st.LastSyncError = &e
			}
			st.SyncMarks = maps.Clone(st.SyncMarks)
			st.Shares = maps.Clone(st.Shares)
			st.Losses = slices.Clone(st.Losses)
			st.TagConflicts = slices.Clone(st.TagConflicts)
			st.SyncDuration.Bounds = slices.Clone(st.SyncDuration.Bounds)
			st.SyncDuration.Counts = slices.Clone(st.SyncDuration.Counts)
			j.Stats = &st
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
