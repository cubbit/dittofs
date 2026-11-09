//go:build journal_conformance

package conformance

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// readOnlyProbe is the interval at which a read-only journal tries a spare.
const readOnlyProbe = 30 * time.Second

// writingOps are the seam operations that change the directory or a file: a
// read-only mount refuses each with EROFS and serves reads.
var writingOps = []string{
	kit.OpOpen, kit.OpWrite, kit.OpSync, kit.OpCreate,
	kit.OpAllocate, kit.OpRename, kit.OpRemove,
	kit.OpTruncate, kit.OpSyncDir,
}

// readOnlyRules fails every writing operation on every file with err.
func readOnlyRules(err error) []kit.Rule {
	rules := make([]kit.Rule, 0, len(writingOps))
	for _, op := range writingOps {
		rules = append(rules, kit.Failing(op, "*", 0, err))
	}
	return rules
}

// TestRFC1B_ReadOnlyStopsTheDrain checks that a journal whose storage turns
// read-only refuses further writes at once, keeps serving what it holds, names
// the state in Stats, tries a spare every probe interval, and writes again
// after a reopen once the storage is writable.
//
// decision: the check fails every writing operation from the next sync on
// rather than one named call, since a journal that answers each EROFS with a
// new segment drains its spares, which no single failing call shows.
// Overturned if the seam gains a read-only mode of its own.
func TestRFC1B_ReadOnlyStopsTheDrain(t *testing.T) {
	f := journaltest.New(t)
	h, id := f.Share(0), journaltest.File(0)

	pa := pattern("a", 4096)
	write(t, h, id, 0, pa)
	syncNow(t, h, id)
	write(t, h, id, 4096, pattern("b", 4096))

	schedule(t, f, readOnlyRules(syscall.EROFS)...)
	err := h.Sync(t.Context(), id)
	stub(t, "Sync", err)
	if !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("a Sync whose sync failed with EROFS returned %v, want ErrJournalReadOnly", err)
	}

	eventually(t, "Stats to name the read-only state", func() bool { return f.J.Stats().ReadOnly.ReadOnly })
	if ro := f.J.Stats().ReadOnly; !errors.Is(ro.Err, syscall.EROFS) || ro.SpareMade {
		t.Fatalf("Stats().ReadOnly = %+v, want read-only with EROFS and no spare", ro)
	}
	if _, err := tryWrite(t, h, id, 8192, pattern("c", 4096)); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("a WriteAt on a read-only journal returned %v, want ErrJournalReadOnly", err)
	}
	if err := h.Release(t.Context(), id, []journal.Extent{{Off: 0, Len: 4096}}); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("a Release on a read-only journal returned %v, want ErrJournalReadOnly", err)
	}
	if err := h.Sync(t.Context(), id); !errors.Is(err, journal.ErrJournalReadOnly) {
		t.Fatalf("a Sync of the failed write returned %v, want ErrJournalReadOnly at once", err)
	}
	wantBytes(t, h, id, 0, pa, "A after the journal turned read-only")

	// A spare is tried every probe interval and made once the faults clear.
	f.Seam.SetFault(nil)
	f.Advance(readOnlyProbe)
	eventually(t, "Stats to report the spare", func() bool { return f.J.Stats().ReadOnly.SpareMade })

	f.Crash()
	wantBytes(t, f.Share(0), id, 0, pa, "A after the reopen")
	if _, err := tryWrite(t, f.Share(0), id, 8192, pattern("d", 4096)); err != nil {
		t.Fatalf("a WriteAt after the reopen returned %v, want the journal writing again", err)
	}
}

// TestRFC1B_ReadOnlyProbe checks the probe of a read-only journal: with the
// device writable but its checks failing, as on a mount without cache
// flushes, the journal stays read-only, makes no spare and names the check.
//
// decision: a mount without cache flushes is modelled as a seam that drops
// every sync, the one fault the probe's check can see. Overturned once the
// fixture can fail the probe's check on its own.
func TestRFC1B_ReadOnlyProbe(t *testing.T) {
	f := journaltest.New(t)
	h, id := f.Share(0), journaltest.File(0)
	write(t, h, id, 0, pattern("a", 4096))
	syncNow(t, h, id)
	write(t, h, id, 4096, pattern("b", 4096))

	schedule(t, f, readOnlyRules(syscall.EROFS)...)
	err := h.Sync(t.Context(), id)
	stub(t, "Sync", err)
	eventually(t, "the journal to turn read-only", func() bool { return f.J.Stats().ReadOnly.ReadOnly })

	files := len(segmentFiles(t, f))
	f.Drop(kit.OpSync, "*")
	f.Advance(readOnlyProbe)
	eventually(t, "Stats to name the failed check", func() bool { return f.J.Stats().ReadOnly.Check != nil })
	if ro := f.J.Stats().ReadOnly; !ro.ReadOnly || ro.SpareMade {
		t.Fatalf("Stats().ReadOnly = %+v, want read-only with no spare under a failing check", ro)
	}
	if got := len(segmentFiles(t, f)); got != files {
		t.Fatalf("%d segment files after the failed check, want the %d before it: no spare file", got, files)
	}
}

// TestRFC1B_ReadOnlyOpenRefusedStaysClosed checks the other half of the probe:
// a check that passes where the open then fails leaves the journal closed,
// and the open succeeds once the failure ends.
//
// decision: the check covers the refusal of the open and its later success;
// that content only the closed journal held answers retry-later is not read,
// as the closed journal has no surface to read it through. Overturned once a
// closed journal exposes one.
func TestRFC1B_ReadOnlyOpenRefusedStaysClosed(t *testing.T) {
	f := journaltest.New(t)
	h, id := f.Share(0), journaltest.File(0)
	pa := pattern("a", 4096)
	write(t, h, id, 0, pa)
	syncNow(t, h, id)

	schedule(t, f, kit.Failing(kit.OpOpen, "*", 0, syscall.EIO))
	if err := f.TryReopen(); err == nil {
		t.Fatal("the open returned nil while every open failed, want the journal to stay closed")
	}
	f.Seam.SetFault(nil)
	f.Advance(readOnlyProbe)
	eventually(t, "the open to succeed once its failure ended", func() bool { return f.TryReopen() == nil })
	wantBytes(t, f.Share(0), id, 0, pa, "A after the open that was retried")
}
