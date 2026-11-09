package engine

import (
	"encoding/hex"
	"maps"
	"slices"

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
)

// JournalStats is one device's journal, as Stats reports it.
type JournalStats struct {
	Conditions []Condition
}

// NodeStats is this node, as Stats reports it.
type NodeStats struct {
	Conditions []Condition
}

// ShareStats is one share, as Stats reports it. Device and Refused are set
// for a share refused at start: its device, and why.
type ShareStats struct {
	Device  string
	Refused error
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
		out.Journals[dev] = JournalStats{
			Conditions: slices.Clone(j.Conditions),
		}
	}

	maps.Copy(out.Shares, s.Shares)

	return out
}
