package engine_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// A lost tail is one of the refusals the journal's own Open answers at
// recovery, wrapped around ErrOpenRefused as RFC 1 §3's refused open is.
func lostTailRefusal() error {
	return fmt.Errorf(
		"journal: the mark below segment 3 names a lost synced tail: %w",
		journal.ErrOpenRefused,
	)
}

// A read the device never completed is the unreadable-range refusal, whose
// value carries the ranges the device never read.
func unreadableRefusal() error {
	return &journal.UnreadableRangesError{
		Ranges: []journal.SegmentRange{{Segment: 4, Off: 4096, Len: 16}},
	}
}

// An Open the journal refuses on its own recovery does not fail Start: its
// shares stay unattached, named with the device and the refusal, a condition
// names why, and the shares answer retry-later (RFC 8 §2.5, RFC 28 §4.4).
func TestStartLeavesARefusedReopensSharesUnattached(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refusal   func() error
		cause     error
		condition string
	}{
		{
			name:      "a lost synced tail",
			refusal:   lostTailRefusal,
			cause:     journal.ErrOpenRefused,
			condition: "journal_open_refused",
		},
		{
			name:      "an unreadable range",
			refusal:   unreadableRefusal,
			cause:     journal.ErrUnreadable,
			condition: "journal_range_unreadable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStarter(t)
			s.openErr = tc.refusal()
			s.closeAtEnd(t)

			if err := s.start(t); err != nil {
				t.Fatalf("Start = %v, want nil: a refused reopen does not stop the node", err)
			}

			st := s.e.Stats()

			for _, id := range []engine.Share{s.one, s.two} {
				got := st.Shares[id.ID]
				if got.Device != startDevice || !errors.Is(got.Refused, tc.cause) {
					t.Errorf(
						"share %v: device %q, refused %v; want %q and the refusal",
						id.ID, got.Device, got.Refused, startDevice,
					)
				}

				if err := s.write(t, id.ID); !isDelay(err, vfserr.DelayFrozen) {
					t.Errorf("a write on share %v = %v, want ErrDelay(DelayFrozen)", id.ID, err)
				}
			}

			c, ok := conditionOf(st, tc.condition)
			if !ok || !errors.Is(c.Cause, tc.cause) {
				t.Errorf("%s = %+v (raised %t), want one naming the refusal", tc.condition, c, ok)
			}

			if got := len(st.Journals[startDevice].Conditions); got != 1 {
				t.Errorf("%d conditions on the device, want exactly %s", got, tc.condition)
			}
		})
	}
}

// The unreadable-range condition names the ranges the value carries, so an
// operator reads them without the error's text.
func TestUnreadableRangeConditionNamesTheRanges(t *testing.T) {
	s := newStarter(t)
	s.openErr = unreadableRefusal()
	s.closeAtEnd(t)

	if err := s.start(t); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}

	c, ok := conditionOf(s.e.Stats(), "journal_range_unreadable")
	if !ok {
		t.Fatal("journal_range_unreadable was not raised")
	}

	want := "segment 4 at 4096 for 16 bytes; the device's shares are not attached; the open is retried every 30s"
	if c.Detail != want {
		t.Errorf("the condition's detail = %q, want %q", c.Detail, want)
	}
}

// The open of a refused reopen is retried: 29 s on the clock leave the shares
// unattached, the 30th second meets the same refusal cleanly — the timer is
// re-armed, the shares keep their cause and the condition is the one refusal —
// and the shares attach once an open stops refusing.
func TestARefusedReopenIsRetriedToTheSameRefusal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refusal   func() error
		cause     error
		condition string
	}{
		{
			name:      "a lost synced tail",
			refusal:   lostTailRefusal,
			cause:     journal.ErrOpenRefused,
			condition: "journal_open_refused",
		},
		{
			name:      "an unreadable range",
			refusal:   unreadableRefusal,
			cause:     journal.ErrUnreadable,
			condition: "journal_range_unreadable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStarter(t, withOpens(tc.refusal(), tc.refusal()))
			s.closeAtEnd(t)

			if err := s.start(t); err != nil {
				t.Fatalf("Start = %v, want nil", err)
			}

			s.retry(t, retryEvery-time.Second)

			for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
				if s.refused(id) == nil {
					t.Fatalf("share %v is attached 29 s after Start, want unattached until the 30th second", id)
				}
			}

			s.retry(t, time.Second)
			eventually(t, "the second refusal re-arms the retry", func() bool {
				return s.clock.Pending() == 1
			})

			for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
				if got := s.refused(id); !errors.Is(got, tc.cause) {
					t.Errorf("share %v refused with %v after a second refusal, want %v", id, got, tc.cause)
				}
			}

			st := s.e.Stats()

			c, ok := conditionOf(st, tc.condition)
			if !ok || len(st.Journals[startDevice].Conditions) != 1 ||
				!errors.Is(c.Cause, tc.cause) {
				t.Errorf("conditions after a second refusal = %+v, want one %s", st.Journals[startDevice].Conditions, tc.condition)
			}

			s.retry(t, retryEvery)
			s.attached(t)

			for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
				if err := s.write(t, id); err != nil {
					t.Errorf("a write on share %v after the attach = %v, want nil", id, err)
				}
			}
		})
	}
}

// The other details the refusal conditions name from the error's value: the
// past-reserve refusal wrapped as Open wraps it names the setting that sizes
// the reserve, and an unreadable format file, which carries no ranges, names
// the file rather than an empty list.
func TestRefusalConditionsNameTheirRemedy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refusal   error
		condition string
		want      string
	}{
		{
			name: "corrupt losses past the open reserve",
			refusal: fmt.Errorf("%w: %s: %w", journal.ErrOpenRefused, startDevice,
				&journal.LostPastReserveError{N: 2, Seg: 944}),
			condition: "journal_open_refused",
			want: "the open would defer 2 corrupt losses past its reserve, the first against segment 944; " +
				"raise journal.headroom_records to hold them; " +
				"the device's shares are not attached; the open is retried every 30s",
		},
		{
			name:      "an unreadable format file",
			refusal:   &journal.UnreadableRangesError{Format: true},
			condition: "journal_range_unreadable",
			want:      "the format file is unreadable; the device's shares are not attached; the open is retried every 30s",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStarter(t)
			s.openErr = tc.refusal
			s.closeAtEnd(t)

			if err := s.start(t); err != nil {
				t.Fatalf("Start = %v, want nil", err)
			}

			c, ok := conditionOf(s.e.Stats(), tc.condition)
			if !ok {
				t.Fatalf("%s was not raised", tc.condition)
			}

			if c.Detail != tc.want {
				t.Errorf("the %s detail = %q, want %q", tc.condition, c.Detail, tc.want)
			}
		})
	}
}

// An unbuilt operation's ErrNotImplemented is no refusal of the open: it is
// never retried, so it fails Start as any other error at open does.
func TestABareNotImplementedAtOpenFailsStart(t *testing.T) {
	s := newStarter(t)
	s.openErr = fmt.Errorf("journal: an unbuilt step: %w", journal.ErrNotImplemented)

	err := s.start(t)
	if !errors.Is(err, journal.ErrNotImplemented) {
		t.Fatalf("Start = %v, want it failed with the open's ErrNotImplemented", err)
	}
}
