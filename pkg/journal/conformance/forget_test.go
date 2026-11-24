//go:build journal_conformance

package conformance

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/raw"
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
		f.WaitSpares()
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

	t.Run("a forget whose record cannot sync fails with the tag whole", func(t *testing.T) {
		f := journaltest.New(t)
		f.WaitSpares()
		tt := journaltest.Tag(3)
		ut := journaltest.Tag(8)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		uh, err := f.J.Share(ut, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach U: %v", err)
		}
		fid := journaltest.File(0)
		p := distinct(0, 64<<10)
		write(t, th, fid, 0, p)
		syncFile(t, th, fid)
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		// The record is admitted and appended with the journal writable; the
		// sync its durability wait starts is parked and then faulted EROFS, so
		// the window fails with the record on disk and the journal turns
		// read-only: the wait answers the refusal with nothing dropped — the
		// tag is whole — and the mark went with the call.
		hold := f.Seam.Hold(t, kit.OpSync, "*.seg")
		forgot := make(chan error, 1)
		go func() { forgot <- f.J.Forget(t.Context(), tt) }()
		<-hold.Reached()
		schedule(t, f, kit.Failing(kit.OpSync, "*", 0, syscall.EROFS), kit.Failing(kit.OpSyncDir, "*", 0, syscall.EROFS))
		hold.Release()
		err = <-forgot
		stub(t, "Forget", err)
		if err == nil {
			t.Fatalf("the Forget whose record's sync turned EROFS returned nil, want the failure its window settled with")
		}
		eventually(t, "Stats to name the read-only state", func() bool { return f.J.Stats().ReadOnly.ReadOnly })

		// Nothing was dropped: the file is still T's — a write through U is
		// still refused with the owner's tag, not free — and the refusal the
		// mark answers a retry with is the read-only one, never ErrHandleOpen,
		// so the forgetting mark was released with the failed call.
		if _, err := uh.WriteAt(t.Context(), fid, 0, distinct(2, 16), 0); !errors.Is(err, journal.ErrOtherShare) {
			t.Fatalf("the write through U after the failed forget returned %v, want the file still T's whole", err)
		}
		if err := f.J.Forget(t.Context(), tt); !errors.Is(err, journal.ErrJournalReadOnly) {
			t.Fatalf("the retry after the failed forget returned %v, want the read-only refusal naming nothing busy", err)
		}
		th, err = f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("re-attach T after the failed forget: %v", err)
		}
		wantBytes(t, th, fid, 0, p, "the content of the tag whose forget record could not make its sync")
		if err := th.Close(); err != nil {
			t.Fatalf("close the re-attached handle: %v", err)
		}

		// The crash drops the unsynced record — nothing was dropped for it —
		// so the tag reopens whole, and a forget on the journal opened writable
		// again then succeeds, leaving nothing of it after another crash.
		f.Seam.SetFault(nil)
		f.Crash()
		if err := f.J.Forget(t.Context(), tt); err != nil {
			t.Fatalf("the forget after the read-only journal reopened writable: %v", err)
		}
		f.Crash()
		if _, ok := f.J.Stats().Shares[tt]; ok {
			t.Fatalf("the tag is still listed after the forget that followed the refused sync")
		}
	})

	t.Run("a copy under the mark is abandoned", func(t *testing.T) {
		f := journaltest.NewWithSegmentCap(t, 64<<10, nil)
		f.WaitSpares()
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
		f.WaitSpares()
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

	// The files the tag claims go with it whole: a file whose every extent
	// was released keeps its owner while its records ride their segments, so
	// the running journal refuses another share's write where a reopen would
	// rebuild the file unclaimed — the forget must unclaim it too.
	t.Run("a settled file is unclaimed with the tag", func(t *testing.T) {
		f := journaltest.New(t)
		tt, ut := journaltest.Tag(3), journaltest.Tag(8)
		th, err := f.J.Share(tt, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach T: %v", err)
		}
		uh, err := f.J.Share(ut, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach U: %v", err)
		}
		fid := journaltest.File(0)
		const size = 64 << 10
		p := distinct(0, size)
		write(t, th, fid, 0, p)
		// Delete and settle at its own version: the file holds nothing and no
		// marker answers a Since, but its owner tag stands while its records
		// ride their segments — the shape a full Release of every extent will
		// give too, once epic 9b lands it.
		dv, err := th.Delete(t.Context(), fid)
		stub(t, "Delete", err)
		if err != nil {
			t.Fatalf("delete the file of T: %v", err)
		}
		th.Settle(fid, dv)
		if _, err := uh.WriteAt(t.Context(), fid, 0, distinct(1, 16), 0); !errors.Is(err, journal.ErrOtherShare) {
			t.Fatalf("the write through U before the forget returned %v, want the refused file of T", err)
		}
		if err := th.Close(); err != nil {
			t.Fatalf("close T's handle: %v", err)
		}

		if err := f.J.Forget(t.Context(), tt); err != nil {
			t.Fatalf("the forget of the settled tag: %v", err)
		}
		if _, err := uh.WriteAt(t.Context(), fid, 0, distinct(2, 16), 0); err != nil {
			t.Fatalf("the write through U after the forget: %v, want the file unclaimed", err)
		}
		syncFile(t, uh, fid)

		// The reopen agrees: recovery rebuilds nothing of T, so the file
		// stays U's and the write reads back through it.
		f.Crash()
		uh, err = f.J.Share(ut, 0, f.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("re-attach U after the crash: %v", err)
		}
		wantBytes(t, uh, fid, 0, distinct(2, 16), "the write through U after the forget and the crash")
		if err := uh.Close(); err != nil {
			t.Fatalf("close the re-attached handle: %v", err)
		}
	})

	t.Run("the rival of a conflict is cleared by the forget of it", func(t *testing.T) {
		const size = 64 << 10
		g := journaltest.NewUnwatched(t)
		b := journaltest.Tag(1)
		gh, gid := g.Share(0), journaltest.File(0)
		p := distinct(0, size)
		w := write(t, gh, gid, 0, p)
		syncFile(t, gh, gid)

		// The planted record sits at its own extent, so both sides of the
		// conflict hold live pieces: the forget of the rival must take the
		// rival's alone, the owner's staying with the file the owner keeps.
		g.Shutdown()
		id := newestID(t, g) + 1
		record := raw.Record{
			Kind: 1, FileID: gid.Bytes(), Offset: size, Length: size,
			Seq: 1 << 40, Version: raw.Version{Epoch: w.Epoch, Counter: w.Counter + 1}, Tag: uint32(b),
			Write: &raw.WriteBody{Payload: distinct(1, size)},
		}
		entry := raw.Entry{
			FileID: gid.Bytes(), Offset: size, RecordOffset: raw.SegmentHeaderSize, Length: size,
			Seq: 1 << 40, Version: raw.Version{Epoch: w.Epoch, Counter: w.Counter + 1}, Kind: 1, Tag: uint32(b),
		}
		plant(t, g, segmentName(id), sealedSegmentBytes(journalIdentity(t, g), id, []raw.Record{record}, []raw.Entry{entry}))
		g.Reopen()

		cs := g.J.Stats().TagConflicts
		if len(cs) != 1 {
			t.Fatalf("the reopen rebuilt the file under two tags, but TagConflicts holds %+v, want one", cs)
		}
		owner, rival := cs[0].Tags[0], cs[0].Tags[1]
		if rival != b {
			t.Fatalf("the conflict holds owner %d and rival %d, want the planted tag %d as the rival", owner, rival, b)
		}

		oh, err := g.J.Share(owner, 0, g.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("attach the owner over the conflict: %v", err)
		}
		if _, err := oh.WriteAt(t.Context(), gid, 0, distinct(2, 16), 0); !errors.Is(err, journal.ErrOtherShare) {
			t.Fatalf("the write through the owner before the forget returned %v, want the conflicted file refused", err)
		}

		if err := g.J.Forget(t.Context(), rival); err != nil {
			t.Fatalf("the forget of the rival tag: %v", err)
		}
		if got := g.J.Stats().TagConflicts; len(got) != 0 {
			t.Fatalf("the conflict survived the forget of its rival: %+v", got)
		}
		// The rival's extent is gone with its tag and the owner's still
		// serves: the drop took the rival's records alone.
		wantBytes(t, oh, gid, 0, p, "the owner's content after the rival's forget")
		gotMissing := read(t, oh, gid, size, size)
		wantMissing(t, gotMissing, size, []journal.Extent{ext(size, size)}, "the rival's extent after the rival's forget")
		if _, err := oh.WriteAt(t.Context(), gid, 0, distinct(2, size), 0); err != nil {
			t.Fatalf("the write through the owner after the forget: %v, want the file its own again", err)
		}

		// A reopen agrees: recovery withholds the rival's records below the
		// forget alone, so the owner's content backs whatever it was and the
		// conflict stays gone.
		g.Crash()
		rh, err := g.J.Share(owner, 0, g.Floor())
		stub(t, "Share", err)
		if err != nil {
			t.Fatalf("re-attach the owner after the crash: %v", err)
		}
		if got := g.J.Stats().TagConflicts; len(got) != 0 {
			t.Fatalf("the conflict resurfaced after the crash: %+v", got)
		}
		wantBytes(t, rh, gid, 0, p, "the owner's content after the rival's forget and the crash")
		if err := rh.Close(); err != nil {
			t.Fatalf("close the re-attached handle: %v", err)
		}
	})
}
