package readretry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/readretry"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// errRead is the failure every test read below returns, so a case can demand
// it back by identity.
var errRead = errors.New("the read failed")

// failing builds a read that fails its first `failures` calls and then
// returns nil forever. A failing call advances the driven clock by hold, so a
// case places each next attempt where the code under test reads the clock:
// a hold past the window makes the next check of it stop the retries.
func failing(clk kit.DrivenClock, failures int, hold time.Duration) (read func() error, calls *[]time.Time) {
	times := make([]time.Time, 0, failures)
	return func() error {
		times = append(times, clk.Now())
		if len(times) <= failures {
			clk.Advance(hold)
			return errRead
		}
		return nil
	}, &times
}

// TestDo holds every bound of the retry in one table. Each read is a real
// closure over the driven clock and its own failure count; nothing answers in
// the code's place. The window case advances the clock past 15 s inside the
// second failing read, so the third is never started despite the attempt
// bound allowing it.
func TestDo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		attempts int
		failures int
		// hold advances the driven clock inside each failing read; a case
		// sets it past half the window to run the window out.
		hold      time.Duration
		wantTried int
		wantCalls int
		wantErr   error
	}{
		{
			name:      "the third attempt reads",
			attempts:  readretry.Attempts,
			failures:  2,
			hold:      time.Second,
			wantTried: 3,
			wantCalls: 3,
			wantErr:   nil,
		},
		{
			name:      "every attempt fails",
			attempts:  readretry.Attempts,
			failures:  3,
			hold:      time.Second,
			wantTried: 3,
			wantCalls: 3,
			wantErr:   errRead,
		},
		{
			name:      "a read already unread makes one attempt",
			attempts:  1,
			failures:  3,
			hold:      time.Second,
			wantTried: 1,
			wantCalls: 1,
			wantErr:   errRead,
		},
		{
			// The first read ends at instant 10 s on the driven clock, the
			// second at 20 s: past the window, so the third the attempt
			// bound would allow is never started.
			name:      "the 15 s window ends the retries",
			attempts:  readretry.Attempts,
			failures:  3,
			hold:      10 * time.Second,
			wantTried: 2,
			wantCalls: 2,
			wantErr:   errRead,
		},
		{
			// The second read ends at exactly 15 s on the driven clock: 15 s
			// have then passed since the first started, so the third the
			// attempt bound would allow is never started.
			name:      "the window ends the retries at its exact 15 s",
			attempts:  readretry.Attempts,
			failures:  3,
			hold:      7500 * time.Millisecond,
			wantTried: 2,
			wantCalls: 2,
			wantErr:   errRead,
		},
		{
			// A hair short of the window's end: the third attempt starts
			// inside it, so the window never ends the retries at all and
			// the attempt bound does, on a failing read.
			name:      "a hold short of the window's end keeps every attempt",
			attempts:  readretry.Attempts,
			failures:  3,
			hold:      7400 * time.Millisecond,
			wantTried: 3,
			wantCalls: 3,
			wantErr:   errRead,
		},
		{
			name:      "an attempt bound below one is read as one",
			attempts:  0,
			failures:  3,
			hold:      time.Second,
			wantTried: 1,
			wantCalls: 1,
			wantErr:   errRead,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clk := kit.NewStopwatchClock()
			read, calls := failing(clk, tc.failures, tc.hold)
			tried, err := readretry.Do(context.Background(), clk, tc.attempts, read)

			if tried != tc.wantTried {
				t.Errorf("Do claimed %d reads tried, want %d", tried, tc.wantTried)
			}
			if gotCalls := len(*calls); gotCalls != tc.wantCalls {
				t.Errorf("the read ran %d times, want %d", gotCalls, tc.wantCalls)
			}
			if err != tc.wantErr {
				t.Errorf("Do returned err %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestDoDeadline checks the context's part alone: a context cancelled inside
// the first failing read is what ends the retries, and its error is the one
// returned — not the read's. The read would otherwise keep failing well
// inside both other bounds, so nothing else could stop it.
func TestDoDeadline(t *testing.T) {
	t.Parallel()

	clk := kit.NewStopwatchClock()
	ctx, cancel := context.WithCancel(context.Background())
	read, calls := failing(clk, 1<<20, time.Second)
	wrapped := func() error {
		cancel()
		return read()
	}

	tried, err := readretry.Do(ctx, clk, readretry.Attempts, wrapped)

	if tried != 1 {
		t.Errorf("Do claimed %d reads tried after the context was cancelled, want 1", tried)
	}
	if gotCalls := len(*calls); gotCalls != 1 {
		t.Errorf("the read ran %d times after the context was cancelled, want 1", gotCalls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do returned err %v, want %v", err, context.Canceled)
	}
}

// TestDoCancelledBeforeStart checks the context's part at the boundary: a
// context already done before the first attempt makes Do return its error
// having read nothing, so a caller whose deadline passed before the read
// got under way never starts it.
func TestDoCancelledBeforeStart(t *testing.T) {
	t.Parallel()

	clk := kit.NewStopwatchClock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	read, calls := failing(clk, 1<<20, time.Second)

	tried, err := readretry.Do(ctx, clk, readretry.Attempts, read)

	if tried != 0 {
		t.Errorf("Do claimed %d reads tried before any read, want 0", tried)
	}
	if gotCalls := len(*calls); gotCalls != 0 {
		t.Errorf("the read ran %d times with a done context, want 0", gotCalls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do returned err %v, want %v", err, context.Canceled)
	}
}

// TestDoSuccessPastDeadline checks the other side of the decision marker: a
// read whose deadline passes while it runs and that returns bytes anyway is
// still a success — the attempt's result stands, whatever the context did
// during it.
func TestDoSuccessPastDeadline(t *testing.T) {
	t.Parallel()

	clk := kit.NewStopwatchClock()
	ctx, cancel := context.WithCancel(context.Background())
	read, calls := failing(clk, 0, time.Second)
	wrapped := func() error {
		err := read()
		cancel()
		return err
	}

	tried, err := readretry.Do(ctx, clk, readretry.Attempts, wrapped)

	if tried != 1 {
		t.Errorf("Do claimed %d reads tried, want 1", tried)
	}
	if gotCalls := len(*calls); gotCalls != 1 {
		t.Errorf("the read ran %d times, want 1", gotCalls)
	}
	if err != nil {
		t.Errorf("Do returned err %v, want nil", err)
	}
}
