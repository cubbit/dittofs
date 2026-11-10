package engine_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
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
	err  error
	lies bool
}

func (r *repackFault) Repack(
	ctx context.Context,
	budget int64,
	scope journal.RepackScope,
) (int64, error) {
	if r.err != nil {
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
	return func(d *engine.Deps) {
		open := d.Open

		d.Open = func(
			ctx context.Context,
			dir string,
			floor journal.Version,
			id journal.JournalID,
		) (engine.Journal, error) {
			j, err2 := open(ctx, dir, floor, id)

			return &repackFault{Journal: j, err: err, lies: lies}, err2
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

// When the scoped repack suffices, the last step does not run: the share's own
// unsynced overwrite stays unsynced.
func TestAShareRefusalStopsAtTheRepackWhenItSuffices(t *testing.T) {
	s := newSegUnit(t, 150, 0)
	fa := entity.NewFileID()

	s.j.AddEnded(s.a.Tag, 50)
	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.a, fa)

	if err := s.put(t.Context(), s.a, fa, 100, 40, 'c'); err != nil {
		t.Fatalf("A's write after the scoped repack: %v, want served", err)
	}

	if got := s.j.Stats().Shares[s.a.Tag].UsedBytes; got != 120 {
		t.Errorf("A holds %d bytes on disk, want 120: the last step synced A's file", got)
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

// syncHold is a handle whose first Sync waits until released or its context
// ends, after telling entered it arrived: a delay over the fake's Sync.
type syncHold struct {
	engine.Handle
	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (h *syncHold) Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error) {
	if h.once.CompareAndSwap(false, true) {
		close(h.entered)

		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}

	return h.Handle.Sync(ctx, ids...)
}

// Another refusal held in its scope sync is no pass running: an index refusal
// with nothing left to free is no space at once, not retry-later for as long
// as the other refusal stays in relief.
func TestAnIndexRefusalBesideARefusalInItsSyncIsNoSpace(t *testing.T) {
	hold := &syncHold{entered: make(chan struct{}), release: make(chan struct{})}
	wrap := func(h engine.Handle) engine.Handle {
		hold.Handle = h

		return hold
	}
	u := newUnitWith(t, unitConfig{wrap: wrap})
	u.j.SegmentCap(segBytes, 0)

	f := u.create(t, "f")
	g := u.create(t, "g")
	u.write(t, f, 0, []byte("x"))

	t.Cleanup(func() { close(hold.release) })

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

	close(hold.release)
	hold.release = make(chan struct{})

	if err := within(t, "the first refusal's answer", first); err == nil {
		t.Error("the first refusal was served, want its answer by what is left")
	}
}

// Over a journal whose Repack is not built, a refusal keeps the interim
// retry-later answer and does not end no space by a figure of 0.
func TestARefusalOverAJournalThatCannotRepackIsRetryLater(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitShare)); err != nil {
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
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a refusal with Repack not built: %v, want ErrDelay(DelayCapacity)", err)
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
// first's pass is held with it selected, is retry-later and never no space; the
// first is served when it ends.
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

	err := s.put(bounded(t), s.a, entity.NewFileID(), 0, 40, 'y')
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
