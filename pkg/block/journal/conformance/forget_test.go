//go:build journal_conformance

package conformance

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestRFC1C_ForgetRules checks the rules a forget answers by at the journal's
// own boundary: a done context and a repeated forget append nothing, a record
// whose sync failed leaves the tag whole, a share of the forgotten tag is
// refused while the forget runs, and a repack copy racing the forget either is
// abandoned or lands below the forget record — never past it.
func TestRFC1C_ForgetRules(t *testing.T) {
	t.Run("a done context appends nothing", func(t *testing.T) {
		f := journaltest.New(t)
		tt := journaltest.Tag(3)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		qid := journaltest.File(0)
		p := distinct(0, 64<<10)
		write(t, th, qid, 0, p)
		syncFile(t, th, qid)
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		f.WaitSpares()
		writes := f.Seam.Calls(kit.OpWrite)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.J.Forget(ctx, tt); !errors.Is(err, context.Canceled) {
			t.Fatalf("Forget with a done context returned %v, want the context's error", err)
		}
		if got := f.Seam.Calls(kit.OpWrite); got != writes {
			t.Fatalf("the refused Forget made %d segment writes, want the %d there were", got, writes)
		}
		// The tag stays usable and whole: a later forget of it succeeds.
		if err := f.J.Forget(t.Context(), tt); err != nil {
			t.Fatalf("the Forget after the refused one: %v", err)
		}
	})

	t.Run("a second forget and an empty tag append nothing", func(t *testing.T) {
		f := journaltest.New(t)
		tt := journaltest.Tag(3)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		fid := journaltest.File(0)
		p := distinct(0, 64<<10)
		write(t, th, fid, 0, p)
		syncFile(t, th, fid)
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		if err := f.J.Forget(t.Context(), tt); err != nil {
			t.Fatalf("the first forget: %v", err)
		}
		// The forget is durable and idempotent: run again, on the tag that
		// holds nothing any more and on one that never held a record, it
		// succeeds and appends nothing. The retirements the first forget
		// prompted only unlink — the spare pool is settled first, so no
		// background spare-making lands in the count either side of the pair.
		f.WaitSpares()
		after := f.Seam.Calls(kit.OpWrite)
		if err := f.J.Forget(t.Context(), tt); err != nil {
			t.Fatalf("the second forget: %v", err)
		}
		ut := journaltest.Tag(8)
		if err := f.J.Forget(t.Context(), ut); err != nil {
			t.Fatalf("the forget of a tag that never held a record: %v", err)
		}
		if got := f.Seam.Calls(kit.OpWrite); got != after {
			t.Fatalf("the second and the empty forget made %d segment writes, want the %d there were", got, after)
		}
		// What the first forget dropped stays dropped across a crash.
		f.Crash()
		if err := f.J.Forget(t.Context(), ut); err != nil {
			t.Fatalf("the forget after the crash: %v", err)
		}
		if _, ok := f.J.Stats().Shares[tt]; ok {
			t.Fatalf("the forgotten tag is listed in Stats().Shares after the crash")
		}
	})

	t.Run("a record that cannot land leaves the tag whole", func(t *testing.T) {
		f := journaltest.New(t)
		tt := journaltest.Tag(3)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		fid := journaltest.File(0)
		p := distinct(0, 64<<10)
		write(t, th, fid, 0, p)
		syncFile(t, th, fid)
		// One unsynced write, so the Sync below has a window to fail: the
		// read-only state turns on the first EROFS the drain observes, and a
		// Sync with nothing to flush never touches storage.
		write(t, th, fid, int64(len(p)), distinct(1, 64<<10))

		// The journal turns read-only before the forget runs, so the refusal
		// the record's admission names reaches the caller with nothing of the
		// tag dropped — the rule the read-only half of the forget's own
		// durability answer owes. A Sync trips the state first, as the
		// read-only rows' own check does.
		schedule(t, f, readOnlyRules(syscall.EROFS)...)
		_, err = th.Sync(t.Context(), fid)
		stub(t, "Sync", err)
		if !errors.Is(err, journal.ErrJournalReadOnly) {
			t.Fatalf("the Sync that turned the journal read-only returned %v, want ErrJournalReadOnly", err)
		}
		eventually(t, "Stats to name the read-only state", func() bool { return f.J.Stats().ReadOnly.ReadOnly })
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		err = f.J.Forget(t.Context(), tt)
		stub(t, "Forget", err)
		if !errors.Is(err, journal.ErrJournalReadOnly) {
			t.Fatalf("the Forget on a read-only journal returned %v, want ErrJournalReadOnly", err)
		}
		// The tag is whole: nothing was dropped for a record that could not
		// land, so the content still reads back, the tag still reports, and
		// the refused call left no mark — the handle attaches again.
		th, err = f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("re-attach T after the refused forget: %v", err)
		}
		wantBytes(t, th, fid, 0, p, "the content of the tag whose forget record could not land")
		if _, ok := f.J.Stats().Shares[tt]; !ok {
			t.Fatalf("the refused forget dropped the tag from Stats().Shares")
		}
		if err := th.Close(); err != nil {
			t.Fatalf("close the re-attached handle: %v", err)
		}

		// The forget succeeds once a reopen is writable again, and the crash
		// proves it: nothing of the tag survives it.
		f.Seam.SetFault(nil)
		f.Crash()
		if err := f.J.Forget(t.Context(), tt); err != nil {
			t.Fatalf("the forget after the journal opened writable again: %v", err)
		}
		f.Crash()
		if _, ok := f.J.Stats().Shares[tt]; ok {
			t.Fatalf("the tag is still listed after the forget that followed the read-only refusal")
		}
	})

	t.Run("a share of the tag is refused while the forget runs", func(t *testing.T) {
		f := journaltest.New(t)
		tt := journaltest.Tag(3)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		fid := journaltest.File(0)
		write(t, th, fid, 0, distinct(0, 64<<10))
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		hold := f.Seam.Hold(t, kit.OpWrite, "*.seg")
		forgot := make(chan error, 1)
		go func() { forgot <- f.J.Forget(t.Context(), tt) }()
		<-hold.Reached()
		_, err = f.J.Share(tt, 0, f.Floor())
		stub(t, "Share during the forget", err)
		if !errors.Is(err, journal.ErrHandleOpen) {
			t.Fatalf("the Share of the forgetting tag returned %v, want ErrHandleOpen naming the forget", err)
		}
		if !strings.Contains(err.Error(), "forget") || !strings.Contains(err.Error(), "in progress") {
			t.Fatalf("the refusal %q does not say a forget is in progress", err)
		}
		hold.Release()
		if err := <-forgot; err != nil {
			t.Fatalf("the Forget the choreography ran: %v", err)
		}
		// The refusal lasts only the forget: the tag attaches again.
		th, err = f.J.Share(tt, 0, f.Floor())
		stub(t, "Share after the forget", err)
		if err != nil {
			t.Fatalf("the attach after the forget: %v", err)
		}
		if err := th.Close(); err != nil {
			t.Fatalf("close the re-attached handle: %v", err)
		}
	})

	t.Run("a copy under the mark is abandoned", func(t *testing.T) {
		f := journaltest.NewWithSegmentCap(t, 64<<10, nil)
		tt := journaltest.Tag(3)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		fid := journaltest.File(0)
		const piece = 60 << 10
		write(t, th, fid, 0, distinct(0, piece))
		write(t, th, fid, piece, distinct(1, piece))
		syncFile(t, th, fid)
		seg := segmentOf(t, f, fid, 0)
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		// The forget is held at its own record's append — WriteAt's hold is
		// every segment write — so the pass below runs entirely under the
		// forgetting mark: every copy of the tag it would make is abandoned,
		// and the pass that copies nothing frees nothing, its source kept.
		hold := f.Seam.Hold(t, kit.OpWrite, "*.seg")
		forgot := make(chan error, 1)
		go func() { forgot <- f.J.Forget(t.Context(), tt) }()
		<-hold.Reached()

		freed, err := f.J.Repack(t.Context(), int64(piece*4), journal.RepackScope{Segments: []journal.SegmentID{seg}})
		if err != nil {
			t.Fatalf("the pass under the forget's mark: %v", err)
		}
		if freed != 0 {
			t.Fatalf("the pass under the forget's mark freed %d, want nothing of the tag copied or retired", freed)
		}
		hold.Release()
		if err := <-forgot; err != nil {
			t.Fatalf("the Forget the choreography ran: %v", err)
		}

		// The crash rebuilds nothing of the tag, and nothing of F is served
		// again through a re-attach of it.
		f.Crash()
		th, err = f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("re-attach T after the abandoned pass: %v", err)
		}
		got := read(t, th, fid, 0, piece)
		wantMissing(t, got, 0, []journal.Extent{ext(0, piece)}, "F after the forget over the abandoned pass")
	})

	t.Run("a copy racing the forget is abandoned or lands below it", func(t *testing.T) {
		f := journaltest.NewWithSegmentCap(t, 64<<10, nil)
		tt := journaltest.Tag(3)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		fid := journaltest.File(0)
		const piece = 60 << 10
		p0, p1 := distinct(0, piece), distinct(1, piece)
		write(t, th, fid, 0, p0)
		write(t, th, fid, piece, p1)
		syncFile(t, th, fid)
		seg := segmentOf(t, f, fid, 0)
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		// A pass copies the first piece while the forget runs. Whichever of
		// the two passes the file's checks first, the copy either is
		// abandoned — the forgetting mark answered — or it is appended before
		// the forget's barrier releases the file's append domain, so the
		// forget record's sequence number is above it and the forget's drop
		// takes the pieces wherever the pass repointed them. On no ordering
		// does a copy of the tag survive the forget.
		hold := f.Seam.Hold(t, kit.OpWrite, "*.seg")
		copied := make(chan error, 1)
		go func() {
			_, err := f.J.Repack(t.Context(), int64(piece*4), journal.RepackScope{Segments: []journal.SegmentID{seg}})
			copied <- err
		}()
		<-hold.Reached()
		forgot := make(chan error, 1)
		go func() { forgot <- f.J.Forget(t.Context(), tt) }()

		hold.Release()
		if err := <-copied; err != nil {
			t.Fatalf("the pass racing the forget: %v", err)
		}
		if err := <-forgot; err != nil {
			t.Fatalf("the forget racing the pass: %v", err)
		}

		// A crash and a reopen rebuild nothing of the tag: whichever records
		// the race left on disk sit below the durable forget record, so
		// recovery withholds them, and nothing of F is served again.
		f.Crash()
		th, err = f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("re-attach T after the raced forget: %v", err)
		}
		got := read(t, th, fid, 0, piece)
		wantMissing(t, got, 0, []journal.Extent{ext(0, piece)}, "the first piece of F after the forget raced the pass")
		if n := len(since(t, th, fid, journal.Version{})); n != 0 {
			t.Fatalf("Since over F after the raced forget yielded %d changes, want none", n)
		}
		if err := th.Close(); err != nil {
			t.Fatalf("close the re-attached handle: %v", err)
		}
	})
}
