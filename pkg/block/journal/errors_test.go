package journal

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestTypedErrorsMatchTheirSentinel catches the one bug an Is method of a
// typed error can have: matching another error's sentinel.
func TestTypedErrorsMatchTheirSentinel(t *testing.T) {
	for err, want := range map[error]error{
		&NoSpaceError{}:      ErrNoSpace,
		&OtherShareError{}:   ErrOtherShare,
		&IdentityError{}:     ErrIdentityMismatch,
		&SyncError{}:         ErrSyncFailed,
		&NotOffloadedError{}: ErrNotOffloaded,
	} {
		if !errors.Is(err, want) {
			t.Errorf("%T does not match %v", err, want)
		}
	}
}

// TestReadOnlySurface pins the read-only error as the one internal/core
// declares, and the interval between probes of a read-only journal.
func TestReadOnlySurface(t *testing.T) {
	if !errors.Is(ErrJournalReadOnly, core.ErrJournalReadOnly) {
		t.Error("ErrJournalReadOnly is not core.ErrJournalReadOnly")
	}
	if core.ProbeInterval != 30*time.Second {
		t.Errorf("ProbeInterval = %v, want 30s", core.ProbeInterval)
	}
}

// TestErrUnsuitableStorageIsExported pins the public name of the refusal a
// platform rule returns: it is the sentinel internal/core declares, so Init
// and Open can be refused with it while the internal packages stay the only
// ones that declare it.
func TestErrUnsuitableStorageIsExported(t *testing.T) {
	if ErrUnsuitableStorage != core.ErrUnsuitableStorage {
		t.Errorf("ErrUnsuitableStorage %v is not core.ErrUnsuitableStorage %v",
			ErrUnsuitableStorage, core.ErrUnsuitableStorage)
	}
}

// TestUnreadableRangesError_UnwrapsErrUnreadable pins the open's unread-range
// refusal as one the engine reads the ranges from: the value matches
// ErrUnreadable under errors.Is, and errors.As recovers it with its Ranges,
// so the engine and the journal_range_unreadable condition never parse the
// error's text.
func TestUnreadableRangesError_UnwrapsErrUnreadable(t *testing.T) {
	ranges := []SegmentRange{{Segment: 7, Off: 96, Len: 40}}
	err := &UnreadableRangesError{Ranges: ranges, Identity: JournalID{0x01}}

	if !errors.Is(err, ErrUnreadable) {
		t.Errorf("errors.Is(%v, ErrUnreadable) = false, want true", err)
	}
	var u *UnreadableRangesError
	if !errors.As(err, &u) {
		t.Fatal("errors.As did not recover the *UnreadableRangesError")
	}
	if !reflect.DeepEqual(u.Ranges, ranges) {
		t.Errorf("errors.As recovered Ranges %v, want %v", u.Ranges, ranges)
	}
}
