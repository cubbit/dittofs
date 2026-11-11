// Package readretry bounds how long the journal keeps starting a read that
// keeps failing. A failing read returned no bytes, so it is never evidence
// of zeros, of a torn write or of corruption: the error may pass, and the
// bytes are read again — but only while the bound's three gates still allow
// starting an attempt. The recovery at open reaches every file's bytes
// through it, and the unread set reuses it, so the open and a running
// journal cannot drift apart on when a read is tried again.
package readretry

import (
	"context"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Attempts is the attempt bound for a record no read has failed yet — three.
// A record already unread gets one attempt per access.
const Attempts = 3

// window is how long a read's retries stay within on the injected clock
// since the first attempt's start. It has no timer: a read blocked in the
// kernel cannot be cancelled, and the kernel has already retried before
// returning the error, so the window bounds only starting an attempt. With
// attempts of similar length the total stays under about twice it.
const window = 15 * time.Second

// Clock is the journal's one clock, core.Clock, under the name this
// package's callers write it with. A second clock interface would give the
// journal two sources of time to drift between; the test double satisfies
// this one as it does core's.
type Clock = core.Clock

// Do runs read until it returns nil, attempts have been made, the window has
// passed on clk since the first started, or ctx is done, and returns the
// number tried and the error of the last thing that stopped it. read is
// started again only while fewer than attempts have been made, less than the
// window has passed since the first, and ctx allows. When the last permitted
// attempt fails and ctx is done by the time it ends, the read's error is
// returned, not the context's: the bound, not the context, is what spent the
// attempt, and a caller counting its own attempts answers for the device
// failure it saw. An attempts below 1 is read as 1.
func Do(ctx context.Context, clk Clock, attempts int, read func() error) (tried int, err error) {
	if attempts < 1 {
		attempts = 1
	}
	var start time.Time
	for tried < attempts {
		// decision: the context's error, not the read's, is returned once ctx
		// ends the retries: the caller answering at its deadline needs its
		// own error reported, not the last device failure it supersedes. A
		// read that succeeds anyway is still a success — it returned bytes.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tried, ctxErr
		}
		if tried == 0 {
			start = clk.Now()
		} else if !clk.Now().Before(start.Add(window)) {
			// The window passed, if at all, during the attempt just made;
			// the last read's failure is what the caller reports.
			return tried, err
		}
		if err = read(); err == nil {
			return tried + 1, nil
		}
		tried++
	}
	return tried, err
}
