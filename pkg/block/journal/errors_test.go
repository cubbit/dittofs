package journal

import (
	"errors"
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
