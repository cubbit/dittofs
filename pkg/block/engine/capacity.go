package engine

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"time"

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

// refusalLogEvery is how often a share's refused writes are logged at Warn: a
// line that names something noisy is rate-limited to once a minute per
// subject, with the count suppressed (RFC 25 §5.2).
const refusalLogEvery = time.Minute

// refusalLimiter decides, for one share, when a refused write is logged: the
// first, then one a minute later with the count of those in between. It holds
// no lock and starts no goroutine; it reads the engine's clock where it is
// asked.
type refusalLimiter struct {
	last       atomic.Int64 // the Elapsed of the last line, plus one; zero before any
	suppressed atomic.Int64
}

// take reports whether a refusal at engine time now is logged, and how many
// were suppressed since the last line.
func (l *refusalLimiter) take(now time.Duration) (bool, int64) {
	for {
		last := l.last.Load()

		if last != 0 && now-time.Duration(last-1) < refusalLogEvery {
			l.suppressed.Add(1)

			return false, 0
		}

		if l.last.CompareAndSwap(last, int64(now)+1) {
			return true, l.suppressed.Swap(0)
		}
	}
}

// unknownRow is the answer to a limit the table has no row for: retry-later
// for capacity, never an I/O error and never a false disk full.
var unknownRow = row{delay: vfserr.DelayCapacity}

// answer is the error a client gets for a refusal with this row.
func (r row) answer() error {
	if r.noSpace {
		return fmt.Errorf("engine: the journal's device is full: %w", vfserr.ErrNoSpace)
	}

	return &vfserr.Delay{Cause: r.delay}
}

// tableAnswer is err as the table answers it when err is a capacity refusal,
// without counting or logging it: for a call that answers no client.
func tableAnswer(err error) (error, bool) {
	r, ok := capacityRefusal(err)
	if !ok {
		return nil, false
	}

	rw, known := rowOf(r)
	if !known {
		rw = unknownRow
	}

	return rw.answer(), true
}

// answered is err as the table answers it when err is a capacity refusal of
// b's journal, which it counts and logs, and false for any other error.
func (e *Engine) answered(b *binding, err error) (error, bool) {
	r, ok := capacityRefusal(err)
	if !ok {
		return nil, false
	}

	rw, known := rowOf(r)
	if !known {
		rw = unknownRow

		if _, seen := e.unknownLimits.LoadOrStore(r.limit, struct{}{}); !seen {
			e.d.Logger.Error("journal refused by an unknown limit", "limit", r.limit.String())
		}
	}

	e.refusals.Add(1)

	answer := rw.answer()

	if logged, suppressed := b.refusals.take(e.d.Clock.Elapsed()); logged {
		e.d.Logger.Warn("write refused",
			"share", b.share.ID.String(),
			"limit", r.String(),
			"answer", answer.Error(),
			"suppressed", suppressed,
		)
	}

	return answer, true
}

// String names the refusal for a log line: its limit, or the device.
func (r refusal) String() string {
	if r.device {
		return "device"
	}

	return r.limit.String()
}

// answeredRefusal reports whether err is an answer the table gave.
func answeredRefusal(err error) bool {
	return errors.Is(err, vfserr.ErrDelay) || errors.Is(err, vfserr.ErrNoSpace)
}

// refusing runs call, a journal call of binding b's, and answers its error: a
// capacity refusal as the table rules, anything else as journalErr maps it, so
// the caller returns the error as it is.
func refusing[T any](e *Engine, b *binding, call func() (T, error)) (T, error) {
	got, err := call()
	if err == nil {
		return got, nil
	}

	if answer, ok := e.answered(b, err); ok {
		return got, answer
	}

	return got, e.journalErr(err)
}
