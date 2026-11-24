package engine_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// segBytes is a segment of the fake journal in these tests.
const segBytes = 100

// segUnit is a unit over the fake journal's segment model, with a share A of
// its own limit beside the unit's share B.
type segUnit struct {
	*unit
	a engine.Share
}

// newSegUnit is a unit with segments of segBytes, the journal's maximum max
// (zero: none) and a share A limited to aLimit.
func newSegUnit(t *testing.T, aLimit, max int64, deps ...func(*engine.Deps)) *segUnit {
	t.Helper()

	u := newUnitWith(t, unitConfig{deps: deps})
	u.j.SegmentCap(segBytes, max)

	a := enginetest.AddShare(t, u.fs.Store, "a", aLimit)
	if err := u.e.Bind(t.Context(), a); err != nil {
		t.Fatal(err)
	}

	return &segUnit{unit: u, a: a}
}

// newSegUnitWith is newSegUnit over a handle wrap, so a unit can park the
// scope's own sync.
func newSegUnitWith(t *testing.T, aLimit, max int64, wrap func(engine.Handle) engine.Handle) *segUnit {
	t.Helper()

	u := newUnitWith(t, unitConfig{wrap: wrap})
	u.j.SegmentCap(segBytes, max)

	a := enginetest.AddShare(t, u.fs.Store, "a", aLimit)
	if err := u.e.Bind(t.Context(), a); err != nil {
		t.Fatal(err)
	}

	return &segUnit{unit: u, a: a}
}

// put writes n bytes of c at off of file f of share sh under ctx.
func (s *segUnit) put(
	ctx context.Context,
	sh engine.Share,
	f entity.FileID,
	off int64,
	n int,
	c byte,
) error {
	_, err := s.e.Write(
		ctx,
		sh.ID,
		f,
		off,
		bytes.Repeat([]byte{c}, n),
		engine.WriteOpts{Done: func() {}},
	)

	return err
}

// must is put that must be served.
func (s *segUnit) must(t *testing.T, sh engine.Share, f entity.FileID, off int64, n int, c byte) {
	t.Helper()

	if err := s.put(t.Context(), sh, f, off, n, c); err != nil {
		t.Fatalf("write of %d bytes at %d: %v", n, off, err)
	}
}

// overwritten writes 40 bytes at 0 of f twice: 40 bytes are superseded.
func (s *segUnit) overwritten(t *testing.T, sh engine.Share, f entity.FileID) {
	t.Helper()

	s.must(t, sh, f, 0, 40, 'a')
	s.must(t, sh, f, 0, 40, 'b')
}

// stream puts the records of f on stream n.
func (s *segUnit) stream(f entity.FileID, n uint32) {
	s.j.StreamOf(journal.NewFileID(f), n)
}

// refuse arms every write to answer a refusal by limit l.
func (s *segUnit) refuse(t *testing.T, l journal.Limit) {
	t.Helper()

	if err := s.j.Refuse(enginetest.CallWriteAt, refusalErr(l)); err != nil {
		t.Fatal(err)
	}
}

// fullConds are the journal_full conditions the engine reports.
func fullConds(e *engine.Engine) []engine.Condition {
	var out []engine.Condition

	for _, j := range e.Stats().Journals {
		for _, c := range j.Conditions {
			if c.Name == "journal_full" {
				out = append(out, c)
			}
		}
	}

	return out
}

// bound is a generous real-time backstop for a wait that must end: a wrong
// engine fails here on the assertion instead of hanging the package.
const bound = 10 * time.Second

// within waits for ch, failing t naming what never happened.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(bound):
		t.Fatalf("%s: not within %v", what, bound)
	}

	panic("unreachable")
}

// bounded is a context that ends after bound, for a call run in a goroutine.
func bounded(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), bound)
	t.Cleanup(cancel)

	return ctx
}

// holdOnce arms the fake's pause point to block the first repack pass that
// reaches it until release, and to tell entered when it does; later passes are
// not held.
func holdOnce(j *enginetest.FakeJournal) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})

	var first atomic.Bool

	j.AtRepackSelected(func(ctx context.Context) {
		if !first.CompareAndSwap(false, true) {
			return
		}

		close(in)

		select {
		case <-out:
		case <-ctx.Done():
		}
	})

	var once atomic.Bool

	return in, func() {
		if once.CompareAndSwap(false, true) {
			close(out)
		}
	}
}

// repackFault is a journal whose Repack fails with err, or answers a figure
// that is not what it freed, and passes everything else to the journal.
type repackFault struct {
	engine.Journal
	err    error
	lies   bool
	healed atomic.Bool  // Repack no longer fails with err
	calls  atomic.Int64 // Repack calls made
}

func (r *repackFault) Repack(
	ctx context.Context,
	budget int64,
	scope journal.RepackScope,
) (int64, error) {
	r.calls.Add(1)

	if r.err != nil && !r.healed.Load() {
		return 0, r.err
	}

	freed, err := r.Journal.Repack(ctx, budget, scope)
	if r.lies {
		freed = 1<<40 - freed
	}

	return freed, err
}

// faultyRepack makes the unit's journal a repackFault.
func faultyRepack(err error, lies bool) func(*engine.Deps) {
	return trackedRepack(err, lies, new(*repackFault))
}

// trackedRepack is faultyRepack that also hands the journal it made to *out.
func trackedRepack(err error, lies bool, out **repackFault) func(*engine.Deps) {
	return func(d *engine.Deps) {
		open := d.Open

		d.Open = func(
			ctx context.Context,
			dir string,
			floor journal.Version,
			id journal.JournalID,
			marks []entity.SyncMark,
		) (engine.Journal, error) {
			j, err2 := open(ctx, dir, floor, id, marks)

			*out = &repackFault{Journal: j, err: err, lies: lies}

			return *out, err2
		}
	}
}

// syncFault is a handle whose Sync fails with EIO.
type syncFault struct{ engine.Handle }

func (syncFault) Sync(context.Context, ...journal.FileID) ([]entity.SyncMark, error) {
	return nil, errors.New("sync: input/output error")
}

// A share at its limit is repacked by its own scope, so its write is served and
// another share's unreclaimed bytes, in a segment of its own, stay.
func TestAShareRefusalRepacksTheShareScope(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)

	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)

	if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
		t.Fatalf("A's write after the scoped repack: %v, want served", err)
	}

	if got := s.j.Stats().Shares[s.share.Tag].UnreclaimedBytes; got != 40 {
		t.Errorf(
			"B's unreclaimed bytes = %d, want 40: a pass over A's scope leaves B's segments",
			got,
		)
	}
}

// A share's refusal syncs the share's own files and no other share's, and the
// overwrite its sync supersedes is freed: the write is served after the last
// step.
func TestAShareRefusalSyncsOnlyItsOwnFiles(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.j.HoldSyncTimer(true)

	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)

	if got := s.j.Stats().Shares[s.a.Tag].UnreclaimedBytes; got != 0 {
		t.Fatalf("A's unreclaimed bytes = %d before the scope sync, want 0", got)
	}

	if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
		t.Fatalf("A's write after the last step: %v, want served", err)
	}

	st := s.j.Stats().Shares
	if st[s.a.Tag].UsedBytes != 80 {
		t.Errorf("A's bytes on disk = %d, want 80: 40 live copied, 40 new", st[s.a.Tag].UsedBytes)
	}

	if st[s.share.Tag].UsedBytes != 80 || st[s.share.Tag].UnreclaimedBytes != 0 {
		t.Errorf("B holds %d bytes, %d unreclaimed, want 80 and 0: A's refusal synced B's file",
			st[s.share.Tag].UsedBytes, st[s.share.Tag].UnreclaimedBytes)
	}
}

// The journal's maximum is answered by a journal-wide repack before the
// answer, and the last step does not run when that suffices.
func TestAJournalRefusalRepacksTheJournalFirst(t *testing.T) {
	s := newSegUnit(t, 1<<20, 300)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.j.AddEnded(s.a.Tag, 50)
	s.j.HoldSyncTimer(true)

	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)

	if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
		t.Fatalf("a write after the journal-wide repack: %v, want served", err)
	}

	// B's overwrite is still unsynced: its 40 superseded bytes are on disk.
	if got := s.j.Stats().Shares[s.share.Tag].UsedBytes; got != 80 {
		t.Errorf("B holds %d bytes on disk, want 80: the last step synced B's file", got)
	}

	for _, sh := range []engine.Share{s.a, s.share} {
		if got := s.j.Stats().Shares[sh.Tag].UnreclaimedBytes; got != 0 {
			t.Errorf(
				"share %v unreclaimed = %d, want 0: no sync ran, the first step sufficed",
				sh.Tag,
				got,
			)
		}
	}

	if c := fullConds(s.e); len(c) != 0 {
		t.Errorf("journal_full raised for a write that was served: %v", c)
	}
}

// The index bound with no other pass: every bound share's files are synced and
// the journal repacked, then no space at once.
func TestTheIndexBoundWithNoOtherPassIsNoSpace(t *testing.T) {
	s := newSegUnit(t, 1<<20, 0)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)
	s.refuse(t, journal.LimitIndex)

	err := s.put(t.Context(), s.a, fa, 100, 1, 'c')
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("an index refusal with no other pass: %v, want ErrNoSpace", err)
	}

	for _, sh := range []engine.Share{s.a, s.share} {
		if got := s.j.Stats().Shares[sh.Tag].UsedBytes; got != 40 {
			t.Errorf(
				"share %v holds %d bytes, want 40: every share's files were synced and repacked",
				sh.Tag,
				got,
			)
		}
	}
}

// The same with another pass running on the journal: retry-later.
func TestTheIndexBoundWithAnotherPassRunningIsDelay(t *testing.T) {
	s := newSegUnit(t, 1<<20, 0)
	s.j.AddEnded(s.a.Tag, 10)
	s.refuse(t, journal.LimitIndex)

	entered, release := holdOnce(s.j)
	t.Cleanup(release)

	first := make(chan error, 1)

	go func() {
		first <- s.put(bounded(t), s.a, entity.NewFileID(), 0, 1, 'x')
	}()

	within(t, "the first pass reaching its pause point", entered)

	err := s.put(bounded(t), s.a, entity.NewFileID(), 0, 1, 'y')
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("an index refusal beside a running pass: %v, want ErrDelay(DelayCapacity)", err)
	}

	release()

	if err := within(t, "the first write's answer", first); !errors.Is(err, vfserr.ErrNoSpace) {
		t.Errorf("the held pass's own answer: %v, want ErrNoSpace once alone", err)
	}
}

// A headroom refusal runs the repack path: a repairable ended segment is
// retired and every share's files are synced; nothing left is no space, and an
// unresolved failed segment is retry-later.
func TestAHeadroomRefusalRepacksAndSyncsEveryShare(t *testing.T) {
	s := newSegUnit(t, 1<<20, 0)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.j.AddEnded(s.a.Tag, 30)
	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)
	s.refuse(t, journal.LimitHeadroom)

	err := s.put(t.Context(), s.a, fa, 100, 1, 'c')
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a headroom refusal with nothing left: %v, want ErrNoSpace", err)
	}

	st := s.j.Stats()
	if st.UnreclaimedEndedBytes != 0 || st.Shares[s.share.Tag].UsedBytes != 40 {
		t.Errorf("ended %d, B holds %d, want 0 and 40: the ended segment retired, B's file synced",
			st.UnreclaimedEndedBytes, st.Shares[s.share.Tag].UsedBytes)
	}
}

func TestAHeadroomRefusalWithAnUnresolvedFailedSegmentIsDelay(t *testing.T) {
	s := newSegUnit(t, 1<<20, 0)
	s.j.AddFailed(s.a.Tag, 30, true)
	s.refuse(t, journal.LimitHeadroom)

	err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'c')
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a headroom refusal over an unresolved failed segment: %v, want ErrDelay", err)
	}
}

// repackSyncHold is a handle whose first Sync waits until released, or fails with its
// context's error when that ends, after telling entered it arrived: a delay
// over the fake's Sync.
type repackSyncHold struct {
	engine.Handle
	entered  chan struct{}
	release  chan struct{}
	returned chan struct{} // closed, when set, once the held Sync returns
	once     atomic.Bool
}

func (h *repackSyncHold) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	if h.once.CompareAndSwap(false, true) {
		if h.returned != nil {
			defer close(h.returned)
		}

		close(h.entered)

		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return h.Handle.Sync(ctx, ids...)
}

// Another refusal held in its scope sync is no pass running: an index refusal
// with nothing left to free is no space at once, not retry-later for as long
// as the other refusal stays in relief.
func TestAnIndexRefusalBesideARefusalInItsSyncIsNoSpace(t *testing.T) {
	hold := &repackSyncHold{entered: make(chan struct{}), release: make(chan struct{})}
	wrap := func(h engine.Handle) engine.Handle {
		hold.Handle = h

		return hold
	}
	u := newUnitWith(t, unitConfig{wrap: wrap})
	u.j.SegmentCap(segBytes, 0)

	f := u.create(t, "f")
	g := u.create(t, "g")
	u.write(t, f, 0, []byte("x"))

	release := sync.OnceFunc(func() { close(hold.release) })
	t.Cleanup(release)

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitShare)); err != nil {
		t.Fatal(err)
	}

	if err := u.j.Refuse(enginetest.CallDeallocate, refusalErr(journal.LimitIndex)); err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)

	go func() {
		_, err := u.e.Write(
			bounded(t),
			u.share.ID,
			f,
			0,
			[]byte("y"),
			engine.WriteOpts{Done: func() {}},
		)
		first <- err
	}()

	within(t, "the first refusal reaching its scope sync", hold.entered)

	err := u.e.Deallocate(bounded(t), u.share.ID, g, 0, 1)
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Errorf("an index refusal beside a refusal in its sync: %v, want ErrNoSpace", err)
	}

	if !strings.Contains(err.Error(), "index limit") {
		t.Errorf("the answer %q does not name the index limit", err)
	}

	release()

	if err := within(t, "the first refusal's answer", first); err == nil {
		t.Error("the first refusal was served, want its answer by what is left")
	}
}

// Over a journal whose Repack is not built, a refusal by the journal's own
// limit keeps the interim retry-later answer and does not end no space by a
// figure of 0. A refusal by the share's own limit runs the share's pass,
// which repacks only when the share has unreclaimed bytes after its sync, so
// it reaches no Repack, and with nothing of the share's to free it is no space.
func TestARefusalOverAJournalThatCannotRepackIsRetryLater(t *testing.T) {
	for _, c := range []struct {
		limit journal.Limit
		want  func(error) bool
		what  string
	}{
		{journal.LimitJournal, func(err error) bool { return isDelay(err, vfserr.DelayCapacity) }, "ErrDelay(DelayCapacity)"},
		{journal.LimitShare, func(err error) bool {
			return errors.Is(err, vfserr.ErrNoSpace) && strings.Contains(err.Error(), "the share limit has nothing left to free")
		}, "ErrNoSpace, the share limit has nothing left to free"},
	} {
		t.Run(c.limit.String(), func(t *testing.T) {
			u := newUnit(t)
			f := u.create(t, "f")

			if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(c.limit)); err != nil {
				t.Fatal(err)
			}

			_, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{Done: func() {}})
			if !c.want(err) {
				t.Errorf("a %v refusal with Repack not built: %v, want %s", c.limit, err, c.what)
			}
		})
	}
}

// No space names the limit that had nothing left to free, and only a full
// device says the device is full.
func TestNoSpaceNamesItsOwnCause(t *testing.T) {
	for _, c := range []struct {
		limit journal.Limit
		want  string
	}{
		{journal.LimitShare, "share limit"},
		{journal.LimitJournal, "journal limit"},
		{journal.LimitHeadroom, "headroom limit"},
		{journal.LimitIndex, "index limit"},
	} {
		t.Run(c.want, func(t *testing.T) {
			s := newSegUnit(t, 100, 0)
			s.refuse(t, c.limit)

			err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
			if !errors.Is(err, vfserr.ErrNoSpace) || !strings.Contains(err.Error(), c.want) ||
				strings.Contains(err.Error(), "device is full") {
				t.Errorf("answer %v, want no space naming the %s and not the device", err, c.want)
			}
		})
	}
}

// A SyncError from the scope sync goes to the file's report and the refused
// write is still answered by the table.
func TestAScopeSyncsFailedWindowIsReportedToItsFile(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	fa := entity.NewFileID()

	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.a, fa)
	s.j.FailSync(journal.NewFileID(fa), journal.Extent{Off: 1000, Len: 4})

	if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
		t.Fatalf("the refused write: %v, want served after the last step", err)
	}

	if got := s.e.LossSeq(s.a.ID, fa); got != 1 {
		t.Errorf("the file's loss sequence = %d after the scope sync took its window, want 1", got)
	}
}

// A repack that fails, or a sync that fails, is never the client's answer.
func TestAFailingStepIsNeverTheAnswer(t *testing.T) {
	var logged lockedBuffer

	logger := func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	}

	t.Run("repack, bytes left", func(t *testing.T) {
		s := newSegUnit(
			t,
			100,
			0,
			faultyRepack(errors.New("repack: input/output error"), false),
			logger,
		)
		s.j.AddFailed(s.a.Tag, 5, true)
		s.refuse(t, journal.LimitShare)

		err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
		if !isDelay(err, vfserr.DelayCapacity) {
			t.Errorf("a failing repack with bytes left: %v, want ErrDelay(DelayCapacity)", err)
		}
	})

	t.Run("repack, nothing left", func(t *testing.T) {
		s := newSegUnit(
			t,
			100,
			0,
			faultyRepack(errors.New("repack: input/output error"), false),
			logger,
		)

		// Unreclaimed bytes in an active segment are not left to free: the
		// pass that would seal them failed.
		s.overwritten(t, s.a, entity.NewFileID())
		s.refuse(t, journal.LimitHeadroom)

		err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
		if !errors.Is(err, vfserr.ErrNoSpace) {
			t.Errorf("a failing repack with nothing left: %v, want ErrNoSpace", err)
		}
	})

	t.Run("a closed journal is unavailable", func(t *testing.T) {
		s := newSegUnit(t, 100, 0, faultyRepack(journal.ErrClosed, false))
		s.j.AddFailed(s.a.Tag, 5, true)
		s.refuse(t, journal.LimitShare)

		err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
		if !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("a repack over a closed journal: %v, want ErrUnavailable", err)
		}
	})

	t.Run("sync", func(t *testing.T) {
		wrap := func(h engine.Handle) engine.Handle {
			return syncFault{h}
		}
		u := newUnitWith(t, unitConfig{wrap: wrap, deps: []func(*engine.Deps){logger}})
		u.j.SegmentCap(segBytes, 0)
		u.j.AddFailed(u.share.Tag, 5, true)

		// A held file the scope sync has to name, accepted before the refusal.
		f := u.create(t, "f")
		u.j.HoldSyncTimer(true)
		u.write(t, f, 0, []byte("x"))

		if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitShare)); err != nil {
			t.Fatal(err)
		}

		_, err := u.e.Write(
			t.Context(),
			u.share.ID,
			f,
			0,
			[]byte("y"),
			engine.WriteOpts{Done: func() {}},
		)
		if !isDelay(err, vfserr.DelayCapacity) {
			t.Errorf("a failing scope sync with bytes left: %v, want ErrDelay(DelayCapacity)", err)
		}

		if !strings.Contains(logged.String(), "sync: input/output error") {
			t.Errorf("the failing Sync was never reached: log %q", logged.String())
		}
	})
}

// A failing step is logged once at Error, as a summary once a minute, and one
// line when it ends, never one per refused write.
func TestAFailingStepIsLoggedOncePerMinute(t *testing.T) {
	var logged lockedBuffer

	s := newSegUnit(t, 100, 0, func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	}, faultyRepack(errors.New("repack: input/output error"), false))
	s.j.AddFailed(s.a.Tag, 5, true)
	s.refuse(t, journal.LimitShare)

	count := func(level string) int {
		n := 0

		for _, l := range strings.Split(logged.String(), "\n") {
			if strings.Contains(l, "capacity step") && strings.Contains(l, "level="+level) {
				n++
			}
		}

		return n
	}

	for range 100 {
		_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
	}

	if got := count("ERROR"); got != 1 {
		t.Errorf("%d Error lines for 100 refusals within a second, want 1", got)
	}

	if got := count("WARN"); got != 0 {
		t.Errorf("%d summary lines within the minute, want 0", got)
	}

	s.clock.Advance(time.Minute)
	_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')

	if got := count("WARN"); got != 1 {
		t.Errorf("%d summary lines a minute later, want 1", got)
	}
}

// A step that succeeds ends the failing stretch with one Info line, so a later
// failure logs a fresh Error instead of staying quiet as a summary.
func TestAStepThatRecoversAndFailsAgainLogsAFreshError(t *testing.T) {
	var logged lockedBuffer

	var fault *repackFault

	s := newSegUnit(t, 100, 0, func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	}, trackedRepack(errors.New("repack: input/output error"), false, &fault))
	s.j.AddFailed(s.a.Tag, 5, true)
	s.refuse(t, journal.LimitShare)

	count := func(level, msg string) int {
		n := 0

		for _, l := range strings.Split(logged.String(), "\n") {
			if strings.Contains(l, msg) && strings.Contains(l, "level="+level) {
				n++
			}
		}

		return n
	}

	_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')

	if got := count("ERROR", "capacity step failed"); got != 1 {
		t.Fatalf("%d Error lines for the first failure, want 1", got)
	}

	fault.healed.Store(true)

	_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')

	if got := count("INFO", "capacity step recovered"); got != 1 {
		t.Errorf("%d recovered lines after a step that succeeded, want 1", got)
	}

	fault.healed.Store(false)

	_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')

	if got := count("ERROR", "capacity step failed"); got != 2 {
		t.Errorf("%d Error lines after a second failure, want 2: the reset is lost", got)
	}

	if got := count("INFO", "capacity step recovered"); got != 1 {
		t.Errorf("%d recovered lines in all, want 1", got)
	}
}

// A deadline already passed on the engine's clock answers retry-later at once
// with nothing repacked.
func TestAPassedDeadlineRepacksNothing(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	s.j.AddEnded(s.a.Tag, 30)
	s.refuse(t, journal.LimitShare)

	ctx := clockCtx{Context: t.Context(), now: s.clock.Now()}

	err := s.put(ctx, s.a, entity.NewFileID(), 0, 1, 'x')
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a refusal past its deadline: %v, want ErrDelay(DelayCapacity)", err)
	}

	if got := s.j.Stats().UnreclaimedEndedBytes; got != 30 {
		t.Errorf("ended bytes = %d, want 30: nothing is repacked past the deadline", got)
	}
}

// A refusal whose context has ended, with no deadline passed, answers
// retry-later and repacks and seals nothing: the journal's figures stay.
func TestAnEndedContextRepacksAndSealsNothing(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	fa := entity.NewFileID()

	s.overwritten(t, s.a, fa)
	s.j.AddEnded(s.a.Tag, 30)
	s.refuse(t, journal.LimitShare)

	before := s.j.Stats()
	ctx, cancel := context.WithCancel(bounded(t))
	cancel()

	err := s.put(ctx, s.a, fa, 100, 40, 'x')
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a refusal under an ended context: %v, want ErrDelay(DelayCapacity)", err)
	}

	after := s.j.Stats()
	tag := s.a.Tag

	if after.UnreclaimedBytes != before.UnreclaimedBytes ||
		after.UnreclaimedEndedBytes != before.UnreclaimedEndedBytes ||
		after.Shares[tag].UnreclaimedBytes != before.Shares[tag].UnreclaimedBytes ||
		after.Shares[tag].UnreclaimedEndedBytes != before.Shares[tag].UnreclaimedEndedBytes ||
		after.Counters.FreedBytes != before.Counters.FreedBytes {
		t.Errorf("figures moved under an ended context: unreclaimed %d to %d, ended %d to %d, "+
			"freed %d to %d, want none: nothing is repacked or sealed",
			before.UnreclaimedBytes, after.UnreclaimedBytes,
			before.UnreclaimedEndedBytes, after.UnreclaimedEndedBytes,
			before.Counters.FreedBytes, after.Counters.FreedBytes)
	}
}

// A repack held past the caller's deadline is retry-later, never no space, and
// the write is not staged: the call ends with the caller's context.
func TestARepackHeldPastTheDeadlineIsDelay(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	f := entity.NewFileID()

	s.j.AddEnded(s.a.Tag, 30)
	s.refuse(t, journal.LimitShare)

	entered, release := holdOnce(s.j)
	t.Cleanup(release)

	ctx := &endsAtCtx{Context: t.Context(), now: s.clock.Now(), done: make(chan struct{})}
	got := make(chan error, 1)

	go func() {
		got <- s.put(ctx, s.a, f, 0, 1, 'x')
	}()

	within(t, "the first pass reaching its pause point", entered)
	close(ctx.done)

	err := within(t, "the held write's answer", got)
	if !isDelay(err, vfserr.DelayCapacity) || errors.Is(err, vfserr.ErrNoSpace) {
		t.Errorf("a repack held past the deadline: %v, want ErrDelay(DelayCapacity)", err)
	}

	if p := s.e.Pending(f); len(p) != 0 {
		t.Errorf("the refused write staged %v, want nothing", p)
	}
}

// Two refusals racing for the only unreclaimed segment: the second, while the
// first's pass is held in its repack with it selected, is retry-later at its
// bound and never no space; the first is served when its pass ends.
func TestRacingRefusalsAnswerOnWhatIsLeft(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	s.j.AddEnded(s.a.Tag, 100)

	entered, release := holdOnce(s.j)
	t.Cleanup(release)

	first := make(chan error, 1)

	go func() {
		first <- s.put(bounded(t), s.a, entity.NewFileID(), 0, 40, 'x')
	}()

	within(t, "the first pass reaching its pause point", entered)

	// The second comes after the pass's sync began, so it waits for the next
	// pass, and answers at a tenth of its deadline by the engine's clock.
	armed := s.clock.Armed(time.Hour / 10)
	second := make(chan error, 1)

	go func() {
		ctx := &endsAtCtx{Context: t.Context(), now: s.clock.Now(), done: make(chan struct{})}
		second <- s.put(ctx, s.a, entity.NewFileID(), 0, 40, 'y')
	}()

	within(t, "the second refusal's wait for the pass", armed)
	s.clock.Advance(time.Hour / 10)

	err := within(t, "the second refusal's answer", second)
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf(
			"the second refusal while the first's pass is held: %v, want ErrDelay(DelayCapacity)",
			err,
		)
	}

	release()

	if err := within(t, "the first write's answer", first); err != nil {
		t.Errorf("the first write once its pass ended: %v, want served", err)
	}

	if err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 40, 'z'); err != nil {
		t.Errorf("the second write retried after the hold: %v, want served", err)
	}
}

// D4 per tag: a share at its limit with nothing of its own left to free is no
// space, though another share has ended bytes; a journal refusal is retry-later
// for ended bytes in any share.
func TestLeftToFreeIsPerTagForAShareAndPerJournalOtherwise(t *testing.T) {
	t.Run("share", func(t *testing.T) {
		s := newSegUnit(t, 100, 0)
		s.j.AddEnded(s.share.Tag, 50)
		s.refuse(t, journal.LimitShare)

		err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
		if !errors.Is(err, vfserr.ErrNoSpace) {
			t.Errorf("A at its limit, B holds ended bytes: %v, want ErrNoSpace", err)
		}
	})

	t.Run("journal", func(t *testing.T) {
		s := newSegUnit(t, 100, 0)
		s.j.AddFailed(s.share.Tag, 50, true)
		s.refuse(t, journal.LimitJournal)

		err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
		if !isDelay(err, vfserr.DelayCapacity) {
			t.Errorf(
				"a journal refusal, B holds ended bytes: %v, want ErrDelay(DelayCapacity)",
				err,
			)
		}
	})
}

// With nothing left to free the answer is no space and what was written reads
// back.
func TestNothingLeftToFreeIsNoSpaceAndTheBytesRead(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	f := entity.NewFileID()
	s.must(t, s.a, f, 0, 100, 'k')

	err := s.put(t.Context(), s.a, f, 100, 1, 'x')
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a share at its limit with nothing to free: %v, want ErrNoSpace", err)
	}

	got := make([]byte, 100)
	if _, err := s.e.Read(t.Context(), s.a.ID, f, 0, got); err != nil ||
		!bytes.Equal(got, bytes.Repeat([]byte{'k'}, 100)) {
		t.Errorf("the first write reads back %q, %v, want 100 'k'", got[:4], err)
	}
}

// The journal at its maximum with its unreclaimed bytes only in active
// segments: the last step's sync and sealing pass serve the write.
func TestAJournalAtItsMaximumIsServedAfterTheLastStep(t *testing.T) {
	s := newSegUnit(t, 1<<20, 200)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)

	if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
		t.Errorf("a write at the journal's maximum: %v, want served after the last step", err)
	}
}

// Repack draws on the repack reserve: a third share's write is served though
// the one share is at its limit with dirty bytes and the other's segments hold
// superseded bytes.
func TestARepackReserveServesAThirdShare(t *testing.T) {
	s := newSegUnit(t, 100, 300)
	third := enginetest.AddShare(t, s.fs.Store, "third", 1<<20)

	if err := s.e.Bind(t.Context(), third); err != nil {
		t.Fatal(err)
	}

	fa, fb, fc := entity.NewFileID(), entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.stream(fc, 2)
	s.must(t, s.a, fa, 0, 100, 'd')
	s.overwritten(t, s.share, fb)
	s.must(t, third, fc, 0, 40, 'c')

	if err := s.put(t.Context(), third, fc, 100, 60, 'e'); err != nil {
		t.Errorf(
			"the third share's write: %v, want served: repack's copies are outside any limit",
			err,
		)
	}
}

// No answer reads what Repack returned as freed.
func TestNoAnswerReadsWhatRepackFreed(t *testing.T) {
	t.Run("freed space, nonsense figure", func(t *testing.T) {
		s := newSegUnit(t, 100, 0, faultyRepack(nil, true))
		fa := entity.NewFileID()
		s.overwritten(t, s.a, fa)

		if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
			t.Errorf(
				"a write after a repack that freed space but reported nonsense: %v, want served",
				err,
			)
		}
	})

	t.Run("freed nothing, huge figure", func(t *testing.T) {
		s := newSegUnit(t, 100, 0, faultyRepack(nil, true))
		s.refuse(t, journal.LimitShare)

		err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')
		if !errors.Is(err, vfserr.ErrNoSpace) {
			t.Errorf(
				"a repack that freed nothing but reported a huge figure: %v, want ErrNoSpace",
				err,
			)
		}
	})
}

// settled answers by what is left, and the index bound by another pass.
func TestSettledAnswersByWhatIsLeft(t *testing.T) {
	for _, c := range []struct {
		name   string
		limit  journal.Limit
		left   int64
		others bool
		delay  bool
	}{
		{"share, left", journal.LimitShare, 1, false, true},
		{"share, none", journal.LimitShare, 0, true, false},
		{"journal, left", journal.LimitJournal, 1, false, true},
		{"headroom, none", journal.LimitHeadroom, 0, false, false},
		{"index, other pass", journal.LimitIndex, 0, true, true},
		{"index, alone, bytes left", journal.LimitIndex, 9, false, false},
	} {
		got := engine.SettledForTest(c.limit, c.left, c.others)
		if got != c.delay {
			t.Errorf("%s: delay = %v, want %v", c.name, got, c.delay)
		}
	}
}

// journal_full: raised by a roll refused at the maximum, and kept while writes
// into the other stream's allocated segment are accepted.
func TestJournalFullAtTheMaximumStaysWhileOtherWritesAreAccepted(t *testing.T) {
	s := newSegUnit(t, 1<<20, 200)
	f1, f2 := entity.NewFileID(), entity.NewFileID()
	s.stream(f2, 1)
	s.must(t, s.a, f1, 0, 60, 'a')
	s.must(t, s.a, f2, 0, 60, 'b')

	err := s.put(t.Context(), s.a, f1, 100, 60, 'c')
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a roll at the maximum: %v, want ErrNoSpace", err)
	}

	got := fullConds(s.e)
	if len(got) != 1 {
		t.Fatalf("%d journal_full conditions after the refusal, want 1", len(got))
	}

	if !strings.Contains(got[0].Detail, "lacking 100 bytes") ||
		!strings.Contains(got[0].Detail, "used 200 of 200 bytes") {
		t.Errorf("Detail %q lacks what the refusal lacked and the journal's figures", got[0].Detail)
	}

	s.must(t, s.a, f2, 100, 30, 'd')

	if n := len(fullConds(s.e)); n != 1 {
		t.Errorf(
			"%d journal_full conditions after an accepted write on the second stream, want 1",
			n,
		)
	}
}

// After a headroom refusal a clone's reserved copy is accepted and
// journal_full stays; the next unreserved content write clears it.
func TestJournalFullAfterAHeadroomRefusalClearsAtAnUnreservedWrite(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	dst := u.committedFile(t, "dst", fill(2*mib, 'd'))
	f := u.create(t, "plain")

	// The repack path needs a journal that repacks: the segment model, with
	// nothing left to free, ends the refusal in no space.
	u.j.SegmentCap(4*mib, 0)

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitHeadroom)); err != nil {
		t.Fatal(err)
	}

	_, err := u.e.Write(
		t.Context(),
		u.share.ID,
		f,
		0,
		[]byte("x"),
		engine.WriteOpts{Done: func() {}},
	)
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a write after a headroom refusal: %v, want ErrNoSpace", err)
	}

	if n := len(fullConds(u.e)); n != 1 {
		t.Fatalf("%d journal_full conditions, want 1", n)
	}

	u.clone(t, src, dst, vfserr.Extent{Len: mib})

	if n := len(fullConds(u.e)); n != 1 {
		t.Errorf("%d journal_full conditions after a reserved copy, want 1", n)
	}

	if err := u.j.Refuse(enginetest.CallWriteAt, nil); err != nil {
		t.Fatal(err)
	}

	opts := engine.WriteOpts{Done: func() {}}

	if _, err = u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), opts); err != nil {
		t.Fatal(err)
	}

	if n := len(fullConds(u.e)); n != 0 {
		t.Errorf("%d journal_full conditions after an unreserved write, want 0", n)
	}
}

// A share at its own limit raises none, and the other share stays healthy.
func TestAShareLimitRaisesNoJournalFull(t *testing.T) {
	s := newSegUnit(t, 100, 0)
	s.j.AddFailed(s.a.Tag, 5, true)
	s.refuse(t, journal.LimitShare)

	_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')

	if c := fullConds(s.e); len(c) != 0 {
		t.Errorf("a share's own limit raised %v, want nothing", c)
	}

	if err := s.j.Refuse(enginetest.CallWriteAt, nil); err != nil {
		t.Fatal(err)
	}

	s.must(t, s.share, entity.NewFileID(), 0, 10, 'o')
}

// A full device raises it and keeps it while writes into allocated segments are
// accepted.
func TestJournalFullOnAFullDevice(t *testing.T) {
	s := newSegUnit(t, 1<<20, 0)
	f := entity.NewFileID()
	s.must(t, s.a, f, 0, 10, 'a')
	s.j.DeviceFull(true)

	err := s.put(t.Context(), s.a, f, 10, 95, 'b')
	if !errors.Is(err, vfserr.ErrNoSpace) || !strings.Contains(err.Error(), "device is full") {
		t.Fatalf("a roll on a full device: %v, want ErrNoSpace naming the device", err)
	}

	if n := len(fullConds(s.e)); n != 1 {
		t.Fatalf("%d journal_full conditions on a full device, want 1", n)
	}

	s.must(t, s.a, f, 10, 20, 'c')

	if n := len(fullConds(s.e)); n != 1 {
		t.Errorf("%d journal_full conditions after an allocated write, want 1", n)
	}
}

// The index and headroom raise it; a spare refusal while the device has room
// does not.
func TestJournalFullCauses(t *testing.T) {
	for _, c := range []struct {
		limit journal.Limit
		want  int
	}{
		{journal.LimitIndex, 1},
		{journal.LimitHeadroom, 1},
		{journal.LimitSpare, 0},
	} {
		t.Run(c.limit.String(), func(t *testing.T) {
			s := newSegUnit(t, 1<<20, 0)
			s.refuse(t, c.limit)

			_ = s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x')

			if n := len(fullConds(s.e)); n != c.want {
				t.Errorf(
					"%d journal_full conditions after a %v refusal, want %d",
					n,
					c.limit,
					c.want,
				)
			}
		})
	}
}

// A deadline that passes inside the scope sync answers retry-later, never the
// figures' no space: the caller's deadline ends the wait, not the cause, and
// a delay that outlasts the deadline stays a delay (RFC 0 section 10.3). The
// scope is held at its sync while the engine's clock passes the deadline, and
// the release lets the sync finish: the answer must be the delay either way.
func TestADeadlinePassingInTheScopeSyncIsADelay(t *testing.T) {
	hold := &repackSyncHold{entered: make(chan struct{}), release: make(chan struct{})}
	wrap := func(h engine.Handle) engine.Handle {
		hold.Handle = h

		return hold
	}

	s := newSegUnitWith(t, 100, 0, wrap)
	f := s.create(t, "f")
	s.write(t, f, 0, []byte("x"))
	s.refuse(t, journal.LimitShare)

	ctx := &endsAtCtx{Context: t.Context(), now: s.clock.Now(), done: make(chan struct{})}
	got := make(chan error, 1)

	go func() {
		got <- s.put(ctx, s.a, f, 0, 1, 'x')
	}()

	within(t, "the refusal reaching its scope sync", hold.entered)
	s.clock.Advance(2 * time.Hour)
	close(hold.release)

	err := within(t, "the held write's answer", got)
	if !isDelay(err, vfserr.DelayCapacity) || errors.Is(err, vfserr.ErrNoSpace) {
		t.Errorf("a deadline passing inside the scope sync: %v, want ErrDelay(DelayCapacity)", err)
	}
}

// The settled answer of a share holding an open reservation is a delay, at
// whatever step the relief ends: the reservation's room may reach the caller
// by landing or rolling back without a free, so the no-space of an empty
// scope would answer ahead of a path the caller is still on. The row whose
// answer flips when the reservation compose leaves the settled step.
func TestAReservationInFlightSettlesADelay(t *testing.T) {
	for _, c := range []struct {
		name      string
		heldBytes int64
	}{
		{"nothing else held", 0},
		{"content beside it", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSegUnit(t, 100, 0)

			f := s.create(t, "f")
			if c.heldBytes > 0 {
				s.must(t, s.a, f, 0, 1, 'a')
			}

			h, err := s.j.Share(s.a.Tag, 100, journal.Version{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = h.Close() }()

			if _, err := h.Reserve(40, 1); err != nil {
				t.Fatal(err)
			}

			s.refuse(t, journal.LimitShare)

			err = s.put(bounded(t), s.a, f, 40, 1, 'x')
			if !isDelay(err, vfserr.DelayCapacity) || errors.Is(err, vfserr.ErrNoSpace) {
				t.Errorf("a refusal with a reservation in flight: %v, want ErrDelay(DelayCapacity)", err)
			}
		})
	}
}

// A reservation whose writes have all landed holds nothing further — headers
// included: like the real journal's, the fake's reserved figure falls as the
// reservation fills, by the whole record charge each run takes, so a
// fully-written-but-unclosed reservation reads no reserved bytes and the
// settled answer is the no-space of the empty scope, not the delay an
// unwritten part would hold for. The reservation is charged ahead of the
// write it covers and the write does not split it evenly, so the header bytes
// are part of what the fill must hand back for the figure to reach zero.
func TestAFullyWrittenReservationSettlesNoSpace(t *testing.T) {
	s := newSegUnit(t, 100, 0)

	f := s.create(t, "f")
	s.must(t, s.a, f, 0, 40, 'a')

	h, err := s.j.Share(s.a.Tag, 100, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	// Reserved ahead at the record charge of the write it will carry: the
	// 30-byte run below charges 96 + 8 + 30 rounded to its grid, past the
	// payload alone, so the fill hands back the whole charge.
	const want int64 = 40
	res, err := h.Reserve(want, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()

	// One run, 30 bytes: it does not split the reservation evenly.
	if _, err := res.WriteAt(context.Background(), journal.NewFileID(f), 40, bytes.Repeat([]byte{'r'}, 30), 0); err != nil {
		t.Fatal(err)
	}

	if got := s.j.Stats().Shares[s.a.Tag].ReservedBytes; got != 0 {
		t.Fatalf("the fully-written reservation holds %d reserved bytes, want 0: the fill hands back the record's whole charge", got)
	}

	s.refuse(t, journal.LimitShare)

	err = s.put(bounded(t), s.a, f, 80, 1, 'x')
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Errorf("a refusal over a share whose reservation fully landed: %v, want ErrNoSpace", err)
	}
}

// fullJudges arms u's engine to signal each judge of journal_full's room by
// the group commit's loop. The signal is sent under the judge's lock, so a
// test that hears it reads the state that judge left.
func fullJudges(u *unit) <-chan struct{} {
	ch := make(chan struct{}, 64)
	u.e.OnFullCheck(func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	})

	return ch
}

// awaitJudged waits, judge by judge, until the loop has judged the latest
// refusal of share sh; nothing judges it again until the clock moves.
func awaitJudged(t *testing.T, u *unit, sh engine.Share, judged <-chan struct{}) {
	t.Helper()

	for !u.e.FullJudgedForTest(sh.ID) {
		within(t, "the loop's judge of the refusal's room", judged)
	}
}

// drain drops the signals of the judges already run.
func drain(judged <-chan struct{}) {
	for {
		select {
		case <-judged:
		default:
			return
		}
	}
}

// rejudge advances u's clock to the loop's next judge of journal_full's
// room and waits for that judge, which reads the figures as they are now.
// The latest refusal must have been judged (awaitJudged).
func rejudge(t *testing.T, u *unit, judged <-chan struct{}) {
	t.Helper()

	drain(judged)
	u.clock.Advance(time.Second)
	within(t, "the loop's judge of journal_full's room a second later", judged)
}

// atTheMaximum is a segUnit whose roll was refused at a maximum of 300 bytes,
// lacking a segment of 100, with a failed segment whose unresolved window
// keeps repack off it until the test resolves it, after the refusal; the
// loop has judged the refusal once, and judged signals each judge.
func atTheMaximum(t *testing.T) (s *segUnit, judged <-chan struct{}) {
	t.Helper()

	s = newSegUnit(t, 1<<20, 300)
	s.j.AddFailed(s.a.Tag, 50, true)
	judged = fullJudges(s.unit)

	f1, f2 := entity.NewFileID(), entity.NewFileID()
	s.stream(f2, 1)
	s.must(t, s.a, f1, 0, 60, 'a')
	s.must(t, s.a, f2, 0, 60, 'b')

	if err := s.put(t.Context(), s.a, f1, 100, 60, 'c'); err == nil {
		t.Fatal("a roll at the maximum was served")
	}

	got := fullConds(s.e)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "lacking 100 bytes") {
		t.Fatalf("journal_full after the refusal = %v, want one lacking 100 bytes", got)
	}

	awaitJudged(t, s.unit, s.a, judged)

	return s, judged
}

// journal_full raised at the maximum clears once storage retired since the
// refusal and any change of the maximum reach what the refusal lacked, with
// nothing written; less leaves it up, and so does a lowered maximum that
// takes back what was freed.
func TestJournalFullAtTheMaximumClearsOnRoom(t *testing.T) {
	repack := func(t *testing.T, s *segUnit) {
		s.j.ResolveWindows()

		freed, err := s.j.Repack(t.Context(), 1<<20, journal.RepackScope{})
		if err != nil || freed != segBytes {
			t.Fatalf("Repack freed %d, %v; want %d", freed, err, segBytes)
		}
	}

	for _, c := range []struct {
		name string
		room func(t *testing.T, s *segUnit)
		want int
	}{
		{"the maximum raised by less than it lacked", func(_ *testing.T, s *segUnit) {
			s.j.SetMaxBytes(350)
		}, 1},
		{"the maximum raised by what it lacked", func(_ *testing.T, s *segUnit) {
			s.j.SetMaxBytes(400)
		}, 0},
		{"the maximum removed", func(_ *testing.T, s *segUnit) {
			s.j.SetMaxBytes(0)
		}, 0},
		{"a segment freed by repack", repack, 0},
		{"a segment freed and the maximum lowered by one", func(t *testing.T, s *segUnit) {
			repack(t, s)
			s.j.SetMaxBytes(200)
		}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, judged := atTheMaximum(t)
			c.room(t, s)
			rejudge(t, s.unit, judged)

			if n := len(fullConds(s.e)); n != c.want {
				t.Errorf("%d journal_full conditions after %s, want %d", n, c.name, c.want)
			}
		})
	}
}

// A headroom refusal is cleared by an unreserved write, never by room under
// the maximum: the group commit's loop has no room of it to judge.
func TestJournalFullAfterAHeadroomRefusalIgnoresTheMaximum(t *testing.T) {
	s := newSegUnit(t, 1<<20, 300)
	judged := fullJudges(s.unit)
	s.refuse(t, journal.LimitHeadroom)

	if err := s.put(t.Context(), s.a, entity.NewFileID(), 0, 1, 'x'); !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a write after a headroom refusal: %v, want ErrNoSpace", err)
	}

	if s.e.RoomPendingForTest(s.a.ID) {
		t.Fatal("a headroom refusal waits for the loop to judge its room, want an unreserved write")
	}

	s.j.SetMaxBytes(1 << 30)
	s.clock.Advance(time.Minute)

	// No clock can show a judge that never runs: the bounded wait is the
	// assertion.
	select {
	case <-judged:
		t.Error("the loop judged the room of a headroom refusal")
	case <-time.After(100 * time.Millisecond):
	}

	if n := len(fullConds(s.e)); n != 1 {
		t.Errorf("%d journal_full conditions after a headroom refusal and a raised maximum, want 1", n)
	}
}

// fullDeviceUnit is a unit over segments of a device's room, with a write
// refused on a full device and that refusal judged once by the loop; free
// is the free space the engine reads, and judged signals each judge.
func fullDeviceUnit(t *testing.T) (u *unit, free *atomic.Int64, judged <-chan struct{}) {
	t.Helper()

	free = &atomic.Int64{}
	u = newUnit(t, func(d *engine.Deps) {
		d.FreeSpace = func(string) (int64, error) { return free.Load(), nil }
	})
	u.j.SegmentCap(engine.DeviceRoomBytes, 0)
	judged = fullJudges(u)

	if err := u.j.Refuse(enginetest.CallWriteAt, fullDevice()); err != nil {
		t.Fatal(err)
	}

	refuseOnFullDevice(t, u)
	awaitJudged(t, u, u.share, judged)

	return u, free, judged
}

// refuseOnFullDevice writes once on u's full device, which must be refused
// and raise journal_full.
func refuseOnFullDevice(t *testing.T, u *unit) {
	t.Helper()

	_, err := u.e.Write(t.Context(), u.share.ID, entity.NewFileID(), 0, []byte("x"),
		engine.WriteOpts{Done: func() {}})
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Fatalf("a write on a full device: %v, want ErrNoSpace", err)
	}

	if n := len(fullConds(u.e)); n != 1 {
		t.Fatalf("%d journal_full conditions on a full device with no room, want 1", n)
	}
}

// journal_full raised for a full device clears once the journal directory's
// filesystem has a segment's cap free, or a retirement returned one, with
// nothing written; less free space leaves it up.
func TestJournalFullOnAFullDeviceClearsOnRoom(t *testing.T) {
	for _, c := range []struct {
		name string
		room func(t *testing.T, u *unit, free *atomic.Int64)
		want int
	}{
		{"free space short of a segment", func(_ *testing.T, _ *unit, free *atomic.Int64) {
			free.Store(engine.DeviceRoomBytes - 1)
		}, 1},
		{"a segment's cap free", func(_ *testing.T, _ *unit, free *atomic.Int64) {
			free.Store(engine.DeviceRoomBytes)
		}, 0},
		{"a segment retired", func(t *testing.T, u *unit, _ *atomic.Int64) {
			u.j.AddFailed(u.share.Tag, 1, false)

			freed, err := u.j.Repack(t.Context(), 1<<20, journal.RepackScope{})
			if err != nil || freed != engine.DeviceRoomBytes {
				t.Fatalf("Repack freed %d, %v; want %d", freed, err, engine.DeviceRoomBytes)
			}
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, free, judged := fullDeviceUnit(t)
			c.room(t, u, free)
			rejudge(t, u, judged)

			if n := len(fullConds(u.e)); n != c.want {
				t.Errorf("%d journal_full conditions after %s, want %d", n, c.name, c.want)
			}
		})
	}
}

// With a file due on every pass of the group commit's loop, so each pass
// commits a batch, journal_full still clears once the device has room.
func TestJournalFullClearsUnderSteadyLoad(t *testing.T) {
	u, free, judged := fullDeviceUnit(t)

	if err := u.j.Refuse(enginetest.CallWriteAt, nil); err != nil {
		t.Fatal(err)
	}

	// Each write falls due on the pass after it, as the loop advances the
	// clock past the existence age.
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Nanosecond, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}

	f := u.create(t, "busy")
	batches := make(chan struct{}, 1)
	var stopped atomic.Bool
	t.Cleanup(func() { stopped.Store(true) })
	u.e.OnTake(func() {
		select {
		case batches <- struct{}{}:
		default:
		}
	})
	u.e.OnLoop(func() {
		if stopped.Load() {
			return
		}
		_, _ = u.e.Write(t.Context(), u.share.ID, f, 0, []byte("y"), engine.WriteOpts{Done: func() {}})
		u.clock.Advance(time.Nanosecond)
	})
	u.write(t, f, 0, []byte("y")) // wakes the loop, which runs the load from then on
	within(t, "a batch of the steady load", batches)

	free.Store(engine.DeviceRoomBytes)
	drain(judged)
	u.clock.Advance(time.Second)

	for len(fullConds(u.e)) != 0 {
		within(t, "a judge of journal_full's room under steady load", judged)
	}
}

// A judge that read room for an older refusal never clears the condition a
// newer one raised meanwhile: the verdict is dropped.
func TestJournalFullAnOlderVerdictKeepsANewerRefusal(t *testing.T) {
	u, free, judged := fullDeviceUnit(t)

	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	u.e.OnFullJudging(func() {
		once.Do(func() {
			close(paused)
			<-release
		})
	})

	free.Store(engine.DeviceRoomBytes)
	drain(judged)
	u.clock.Advance(time.Second)
	within(t, "the judge that read room, paused before its verdict", paused)

	free.Store(0) // the device filled again
	refuseOnFullDevice(t, u)
	close(release)
	within(t, "the paused judge's verdict", judged)

	if n := len(fullConds(u.e)); n != 1 {
		t.Errorf("%d journal_full conditions after an older verdict of room, want the newer refusal's 1", n)
	}
}
