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
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// conditionJournalLost is RFC 25 §3.9's condition for a journal found
// missing or foreign: unhealthy, until an operator acknowledges the loss.
const conditionJournalLost = "journal_lost"

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

// Condition is one health condition the engine raises, in the fields of
// RFC 25 §8.2's, so the composition root adapts it into the health evaluator.
type Condition struct {
	Name    string
	Subject Subject
	Cause   error  // the error or reason that raised it
	Detail  string // a constant message and the figures that matter
}

// Subject is what a condition is about (RFC 25 §8.2).
type Subject struct {
	Scope Scope
	ID    string
	Name  string
}

// Scope is the kind of thing a condition is about (RFC 25 §3.3).
type Scope uint8

const (
	ScopeJournal Scope = iota + 1 // a journal device: ID its identity, Name its directory
	ScopeNode                     // this node: ID its identity
	ScopeShare                    // a share: ID its identity
)

// JournalStats is one device's journal, as Stats reports it: the journal's
// figures, copied from its own Stats, and its conditions.
type JournalStats struct {
	MaxBytes         int64
	UsedBytes        int64
	DirtyBytes       int64
	UnreclaimedBytes int64
	NextVersion      journal.Version
	VersionLimit     journal.Version
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
