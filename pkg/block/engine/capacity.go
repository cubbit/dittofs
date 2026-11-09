package engine

import (
	"errors"
	"syscall"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// refusal is a capacity refusal as the journal named it: the limit that
// refused, or a full device, which is no limit.
type refusal struct {
	limit  journal.Limit
	device bool
}

// row is what the refusal table answers for one refusal.
type row struct {
	wait    bool              // wait for the version limit's raise before answering
	delay   vfserr.DelayCause // the cause of the retry-later answer
	noSpace bool              // answer ErrNoSpace at once
}

// capacityRefusal reports whether err is a capacity refusal and which. A
// read-only journal is not one, whatever it wraps: RFC 8 §11.2 answers it by
// the error that entered the state, apart from the table below.
func capacityRefusal(err error) (refusal, bool) {
	var noSpace *journal.NoSpaceError

	switch {
	case errors.Is(err, journal.ErrJournalReadOnly):
		return refusal{}, false
	case errors.As(err, &noSpace):
		return refusal{limit: noSpace.Limit}, true
	case errors.Is(err, syscall.ENOSPC):
		return refusal{device: true}, true
	}

	return refusal{}, false
}

// refusalTable is the one table that decides every capacity refusal, by what
// refused it (RFC 8 §10.2).
//
// decision: the first two rows of RFC 8 §10.2 split on one criterion, whether
// the system clears the cause by itself. A limit the journal drains by itself
// answers retry-later, so the client waits; a full device is cleared only by
// an operator, so waiting on it would hold every writer's call open for as
// long as no one acts, and it answers no space at once. Until the repack and
// the last step exist, a limit that offload and repack can drain answers
// retry-later without trying either. Overturned if a store gains a signal that
// an operator is acting.
var refusalTable = map[journal.Limit]row{
	journal.LimitShare:    {delay: vfserr.DelayCapacity},
	journal.LimitJournal:  {delay: vfserr.DelayCapacity},
	journal.LimitIndex:    {delay: vfserr.DelayCapacity},
	journal.LimitHeadroom: {delay: vfserr.DelayCapacity},
	journal.LimitVersion:  {wait: true, delay: vfserr.DelayCommit},
	journal.LimitSpare:    {delay: vfserr.DelayCapacity},
}

// deviceRow is the row of a full device.
var deviceRow = row{noSpace: true}

// rowOf is the table's row for r, and false when it has none.
func rowOf(r refusal) (row, bool) {
	if r.device {
		return deviceRow, true
	}

	got, ok := refusalTable[r.limit]

	return got, ok
}
