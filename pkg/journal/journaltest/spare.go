package journaltest

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// SpareAttempts bounds the calls RetrySpare makes before it gives up on a
// journal that never makes a spare: a count of yields, never a time.
const SpareAttempts = 1 << 22

// RetrySpare calls op and, while it is refused for want of a spare, yields
// the processor and calls it again, up to SpareAttempts times, and returns
// op's last error, or an error saying the journal never made a spare once
// the attempts run out. It waits on no clock.
//
// decision: spares are made off the write path, and a roll that finds none
// ready is refused at once, so with the fixture's small segments a correct
// journal can refuse a write now and accept it a moment later. Retrying is
// what its caller does. The wait yields rather than holding a seam call,
// because no seam call marks the moment a spare is in the pool: the journal
// records it after the directory sync that follows the spare's rename
// returns into its own goroutine, and a refusal in that gap would wait on a
// sync that never comes. Upgrade to a hold on a point the journal reports
// through the seam once it records a spare, if the seam ever carries such
// points.
func RetrySpare(op func() error) error {
	var err error
	for range SpareAttempts {
		err = op()
		var ns *journal.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != journal.LimitSpare {
			return err
		}

		runtime.Gosched()
	}

	return fmt.Errorf("journaltest: calls refused for want of a spare; the journal never made one: %w", err)
}

// spareQuietYields is how many yields in a row RetrySpare's fixture form
// sees the seam quiet before it takes a refusal for want of a spare as the
// journal's last word.
const spareQuietYields = 1 << 16

// RetrySpare is the package RetrySpare for a call through this fixture's
// journal, with two more ways to give up, each a word that no spare is
// coming: the pool settles short twice in a row, each time after a refusal
// that queued it a pass; or spareQuietYields yields in a row find the seam
// quiet, no call started or ended through it and every call in flight
// parked at a hold.
//
// decision: the pool's settle is its own word, awaited on a goroutine the
// retries never block on, so a pass parked at a hold, which would hold the
// wait for ever, costs nothing but the wait; the retry before giving up
// answers the pass that settled short having made the one spare the call
// needed. A spare is made, or recycled, only through calls on the seam, and
// recorded by the journal in the moment after its directory sync returns,
// so a seam quiet for that many yields, each handing the processor to
// whatever the journal has runnable, makes no spare until the test, the
// caller here, releases a hold. The quiet bound stands in for that moment
// and for a journal goroutine about to start a call, both work on the
// processor alone; a journal that computes for longer than that many yields
// between a spare's calls is taken as refusing. A call stalled by a fault's
// delay keeps the seam acting, so the package bound applies to it unchanged.
// Raise the bound if a correct journal is ever seen refused here.
func (f *Fixture) RetrySpare(op func() error) error {
	ctx, cancel := context.WithCancel(f.tb.Context())
	settled := make(chan struct{ err error }, 1)
	waiting := false
	defer func() {
		cancel()
		if waiting {
			<-settled
		}
	}()

	var err error
	last, _ := f.Seam.Activity()
	quiet, short := 0, 0
	for range SpareAttempts {
		err = op()
		if !spareRefusal(err) {
			return err
		}

		if !waiting && f.spares != nil {
			waiting = true
			go func(w core.SparesWait) { settled <- struct{ err error }{w.WaitSpares(ctx)} }(f.spares)
		}
		select {
		case s := <-settled:
			waiting = false
			if !spareRefusal(s.err) {
				short = 0
				break
			}
			if short++; short == 2 {
				return fmt.Errorf("journaltest: calls refused for want of a spare; the journal never made one: %w", err)
			}
		default:
		}

		moves, acting := f.Seam.Activity()
		switch {
		case acting || moves != last:
			quiet, last = 0, moves
		case quiet == spareQuietYields:
			return fmt.Errorf("journaltest: calls refused for want of a spare; the journal never made one: %w", err)
		default:
			quiet++
		}

		runtime.Gosched()
	}

	return fmt.Errorf("journaltest: calls refused for want of a spare; the journal never made one: %w", err)
}

// spareRefusal reports whether err is a refusal for want of a spare.
func spareRefusal(err error) bool {
	var ns *journal.NoSpaceError
	return errors.As(err, &ns) && ns.Limit == journal.LimitSpare
}
