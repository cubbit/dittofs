package engine

import (
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Every limit the journal names has a row, with its exact answer: the version
// limit waits for the raise and then is retry-later for a commit, every other
// limit is retry-later for capacity. A limit with no name, and the zero limit,
// have no row.
func TestRefusalTableHasARowPerLimit(t *testing.T) {
	want := map[journal.Limit]row{
		journal.LimitShare:    {delay: vfserr.DelayCapacity},
		journal.LimitJournal:  {delay: vfserr.DelayCapacity},
		journal.LimitIndex:    {delay: vfserr.DelayCapacity},
		journal.LimitHeadroom: {delay: vfserr.DelayCapacity},
		journal.LimitVersion:  {wait: true, delay: vfserr.DelayCommit},
		journal.LimitSpare:    {delay: vfserr.DelayCapacity},
	}

	walked := 0
	l := journal.LimitShare

	for ; !strings.HasPrefix(l.String(), "Limit("); l++ {
		walked++

		got, ok := rowOf(refusal{limit: l})
		if !ok {
			t.Errorf("limit %d (%s) has no row", l, l)

			continue
		}

		if got != want[l] {
			t.Errorf("row of limit %s = %+v, want %+v", l, got, want[l])
		}
	}

	if walked != 6 {
		t.Errorf("walked %d limits, want 6", walked)
	}

	for _, unnamed := range []journal.Limit{0, l} {
		if got, ok := rowOf(refusal{limit: unnamed}); ok {
			t.Errorf("limit %d has the row %+v, want none", unnamed, got)
		}
	}
}

// A full device is no limit: it is told from the spare pool, which is a limit,
// answers ErrNoSpace at once without a wait, and a read-only journal or a
// failing device is no capacity refusal at all, even when the read-only
// error wraps ENOSPC.
func TestFullDeviceIsNotALimit(t *testing.T) {
	enospc := &fs.PathError{Op: "write", Path: "segment", Err: syscall.ENOSPC}

	got, ok := capacityRefusal(enospc)
	if !ok || got != (refusal{device: true}) {
		t.Fatalf("ENOSPC = %+v, %v, want the device", got, ok)
	}

	r, ok := rowOf(got)
	if !ok || r != (row{noSpace: true}) {
		t.Errorf("row of the device = %+v, %v, want ErrNoSpace at once with no wait", r, ok)
	}

	spare := &journal.NoSpaceError{
		Limit:   journal.LimitSpare,
		Missing: 1,
		Unit:    journal.UnitSegments,
	}

	got, ok = capacityRefusal(spare)
	if !ok || got != (refusal{limit: journal.LimitSpare}) {
		t.Errorf("the spare pool = %+v, %v, want limit spare and not the device", got, ok)
	}

	readOnly := fmt.Errorf("%w: %w", journal.ErrJournalReadOnly, syscall.EROFS)
	readOnlyFull := fmt.Errorf("%w: %w", journal.ErrJournalReadOnly, enospc)
	eio := &fs.PathError{Op: "write", Path: "segment", Err: syscall.EIO}

	for name, err := range map[string]error{
		"EIO":                       eio,
		"read-only, EROFS":          readOnly,
		"read-only wrapping ENOSPC": readOnlyFull,
		"nil":                       nil,
	} {
		if got, ok := capacityRefusal(err); ok {
			t.Errorf("%s = %+v, want no capacity refusal", name, got)
		}
	}
}
