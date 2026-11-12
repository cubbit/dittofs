package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// clockCtx is a context whose deadline is the test clock's now and that never
// ends: the test clock starts long before the real time, so a real deadline at
// its reading would already have closed the context.
type clockCtx struct {
	context.Context
	now time.Time
}

func (c clockCtx) Deadline() (time.Time, bool) {
	return c.now, true
}

// failIfWaits makes a call that waits for the raise of the version limit fail
// t and ends its wait by cancelling the context it runs under: a call that
// must not wait then fails here instead of blocking, with no timer.
func failIfWaits(t *testing.T, e *engine.Engine, cancel context.CancelFunc) {
	t.Helper()

	e.OnRefused(func() {
		t.Error("the call waited for the raise")
		cancel()
	})
}

// answerOf is the test's own table of the answers a refusal must get: retry
// later for a commit when the version limit refused, for capacity when any
// other limit did.
func answerOf(l journal.Limit) vfserr.DelayCause {
	if l == journal.LimitVersion {
		return vfserr.DelayCommit
	}

	return vfserr.DelayCapacity
}

// refusalErr is the capacity refusal naming limit l.
func refusalErr(l journal.Limit) error {
	return &journal.NoSpaceError{Limit: l, Missing: 1, Unit: journal.UnitBytes}
}

// fullDevice is the error a full device surfaces as.
func fullDevice() error {
	return &fs.PathError{Op: "write", Path: "segment", Err: syscall.ENOSPC}
}

// checkAnswer fails t unless err is the answer to refusal want: a retry-later
// of cause delay, or no space for the device, and never the journal's error.
func checkAnswer(t *testing.T, name string, err error, want error) {
	t.Helper()

	var ns *journal.NoSpaceError

	if errors.As(err, &ns) || errors.Is(err, journal.ErrNoSpace) {
		t.Errorf("%s: the answer %v carries the journal's refusal", name, err)
	}

	var wd *vfserr.Delay

	if errors.As(want, &wd) {
		if !isDelay(err, wd.Cause) {
			t.Errorf("%s: answered %v, want ErrDelay with cause %v", name, err, wd.Cause)
		}

		return
	}

	if !errors.Is(err, want) {
		t.Errorf("%s: answered %v, want %v", name, err, want)
	}
}

// Each call that can be refused answers every refusal RFC 1 lets it meet by
// the table, at every site: the cause of its limit, no space for a full
// device, and never the journal's error.
func TestEachSiteAnswersByTheTable(t *testing.T) {
	type site struct {
		name string
		call enginetest.Call
		errs []error
		run  func(t *testing.T, ctx context.Context, u *unit, src, dst entity.FileID) error
	}

	write := func(t *testing.T, ctx context.Context, u *unit, src, _ entity.FileID) error {
		_, err := u.e.Write(ctx, u.share.ID, src, 0, []byte("abc"),
			engine.WriteOpts{Done: func() {}})

		return err
	}
	clone := func(t *testing.T, ctx context.Context, u *unit, src, dst entity.FileID) error {
		return u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}
	headroomVersion := []error{refusalErr(journal.LimitHeadroom), refusalErr(journal.LimitVersion)}
	withIndex := append(
		append([]error{}, headroomVersion...), refusalErr(journal.LimitIndex),
	)

	var all []error

	for l := journal.LimitShare; !strings.HasPrefix(l.String(), "Limit("); l++ {
		all = append(all, refusalErr(l))
	}

	sites := []site{
		{"Write", enginetest.CallWriteAt, append(append([]error{}, all...), fullDevice()), write},
		{
			"Draw", enginetest.CallDraw,
			[]error{refusalErr(journal.LimitVersion)},
			func(t *testing.T, ctx context.Context, u *unit, src, _ entity.FileID) error {
				_, err := u.e.Draw(ctx, u.share.ID, src)

				return err
			},
		},
		{
			"Truncate", enginetest.CallTruncate, headroomVersion,
			func(t *testing.T, ctx context.Context, u *unit, src, _ entity.FileID) error {
				return u.e.Truncate(ctx, u.share.ID, src, 5)
			},
		},
		{
			"Deallocate", enginetest.CallDeallocate, withIndex,
			func(t *testing.T, ctx context.Context, u *unit, src, _ entity.FileID) error {
				return u.e.Deallocate(ctx, u.share.ID, src, 0, 4)
			},
		},
		{
			"Delete", enginetest.CallDelete, headroomVersion,
			func(t *testing.T, ctx context.Context, u *unit, src, _ entity.FileID) error {
				_, err := u.e.Delete(ctx, u.share.ID, src)

				return err
			},
		},
		{"CloneTarget", enginetest.CallCloneTarget, withIndex, clone},
		{"Reserve", enginetest.CallReserve, []error{
			refusalErr(journal.LimitShare), refusalErr(journal.LimitJournal),
			refusalErr(journal.LimitIndex), refusalErr(journal.LimitVersion),
			refusalErr(journal.LimitSpare),
		}, clone},
	}

	for _, s := range sites {
		for _, ref := range s.errs {
			t.Run(s.name+"/"+ref.Error(), func(t *testing.T) {
				u := clonedUnit(t)
				src := u.committedFile(t, "src", fill(mib, 's'))
				dst := u.committedFile(t, "dst", fill(mib, 'd'))

				if err := u.j.Refuse(s.call, ref); err != nil {
					t.Fatalf("Refuse(%v): %v", ref, err)
				}

				// A version refusal waits for the raise unless the deadline has
				// passed on the engine's clock; none of these calls may wait.
				base, cancel := context.WithCancel(t.Context())

				defer cancel()

				ctx := context.Context(base)
				if s.call != enginetest.CallCloneTarget {
					ctx = clockCtx{Context: base, now: u.clock.Now()}
				}

				failIfWaits(t, u.e, cancel)

				if s.call == enginetest.CallCloneTarget {
					// The clone runs on the loop's context, which a test cannot cancel:
					// a wait there ends by clearing the refusal and waking it.
					u.e.OnRefused(func() {
						t.Error("the call waited for the raise")

						if err := u.j.Refuse(s.call, nil); err != nil {
							t.Error(err)
						}

						u.e.NotifyVersionLimitForTest(u.share.ID)
					})
				}

				checkAnswer(t, "answer", s.run(t, ctx, u, src, dst), wantOf(ref))
			})
		}
	}
}

// wantOf is the answer the table must give the refusal ref: retry-later of its
// limit's cause, or no space for a full device.
func wantOf(ref error) error {
	var ns *journal.NoSpaceError

	if errors.As(ref, &ns) {
		return &vfserr.Delay{Cause: answerOf(ns.Limit)}
	}

	return vfserr.ErrNoSpace
}

// refusedWrite is one write of a file that the journal refuses with err.
func refusedWrite(t *testing.T, u *unit, ctx context.Context, f entity.FileID) error {
	t.Helper()

	_, err := u.e.Write(ctx, u.share.ID, f, 0, []byte("abc"), engine.WriteOpts{Done: func() {}})

	return err
}

// A spare pool refusal is retry-later for capacity and answers at once: the
// write is not staged, the journal issued no version, and it is counted once.
func TestSpareRefusalIsDelayAtOnce(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	next := u.j.Stats().NextVersion

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitSpare)); err != nil {
		t.Fatal(err)
	}

	err := refusedWrite(t, u, t.Context(), f)
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a write refused by the spare pool: %v, want ErrDelay(DelayCapacity)", err)
	}

	if len(u.e.Pending(f)) != 0 || u.j.Stats().NextVersion != next {
		t.Errorf("a refused write staged %v, next version %v was %v",
			u.e.Pending(f), u.j.Stats().NextVersion, next)
	}

	if n := u.e.Stats().WriteRefusals; n != 1 {
		t.Errorf("%d refusals counted, want 1", n)
	}
}

// A full device is no space at once, not an I/O error: the write is not
// staged, the journal issued no version, and it is counted once.
func TestFullDeviceAnswersNoSpaceAtOnce(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	next := u.j.Stats().NextVersion

	if err := u.j.Refuse(enginetest.CallWriteAt, fullDevice()); err != nil {
		t.Fatal(err)
	}

	err := refusedWrite(t, u, t.Context(), f)
	if !errors.Is(err, vfserr.ErrNoSpace) || errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a write on a full device: %v, want ErrNoSpace", err)
	}

	if len(u.e.Pending(f)) != 0 || u.j.Stats().NextVersion != next {
		t.Errorf("a refused write staged %v, next version %v was %v",
			u.e.Pending(f), u.j.Stats().NextVersion, next)
	}

	if n := u.e.Stats().WriteRefusals; n != 1 {
		t.Errorf("%d refusals counted, want 1", n)
	}
}

// A context that ends while a refusal stands answers the refusal's own
// ErrDelay, never the context's error, whether it was canceled or its deadline
// passed; a full device is no space whatever the context. Write meets every
// refusal, Draw the version limit's.
func TestContextEndKeepsTheRefusalsCause(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	var refusals []error

	for l := journal.LimitShare; !strings.HasPrefix(l.String(), "Limit("); l++ {
		refusals = append(refusals, refusalErr(l))
	}

	refusals = append(refusals, fullDevice())

	for _, c := range []struct {
		name string
		ctx  func(u *unit) (context.Context, context.CancelFunc)
	}{
		{"canceled", func(*unit) (context.Context, context.CancelFunc) {
			return canceled, func() {}
		}},
		{"deadline passed", func(u *unit) (context.Context, context.CancelFunc) {
			base, cancel := context.WithCancel(t.Context())

			return clockCtx{Context: base, now: u.clock.Now()}, cancel
		}},
	} {
		for _, ref := range refusals {
			t.Run("Write/"+c.name+"/"+ref.Error(), func(t *testing.T) {
				u := newUnit(t)
				f := u.create(t, "a")

				if err := u.j.Refuse(enginetest.CallWriteAt, ref); err != nil {
					t.Fatal(err)
				}

				ctx, cancel := c.ctx(u)

				defer cancel()

				failIfWaits(t, u.e, cancel)

				err := refusedWrite(t, u, ctx, f)
				checkAnswer(t, "answer", err, wantOf(ref))

				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("answered the context's error %v", err)
				}
			})
		}

		t.Run("Draw/"+c.name, func(t *testing.T) {
			u := newUnit(t)

			version := refusalErr(journal.LimitVersion)
			if err := u.j.Refuse(enginetest.CallDraw, version); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := c.ctx(u)

			defer cancel()

			failIfWaits(t, u.e, cancel)

			_, err := u.e.Draw(ctx, u.share.ID, u.create(t, "d"))
			checkAnswer(t, "answer", err, &vfserr.Delay{Cause: vfserr.DelayCommit})

			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("answered the context's error %v", err)
			}
		})
	}
}

// A refused call changes nothing: no Done, no operation, no entry, the file's
// overlay as it was, the version counter and the verifier unmoved. The old
// per-case checks live here: a share past its limit, a full device and the
// version limit.
func TestARefusedCallChangesNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(t *testing.T, u *unit) engine.Share
		want error
	}{
		{"share limit", func(t *testing.T, u *unit) engine.Share {
			small := enginetest.AddShare(t, u.fs.Store, "small", 2)
			if err := u.e.Bind(t.Context(), small); err != nil {
				t.Fatal(err)
			}

			return small
		}, &vfserr.Delay{Cause: vfserr.DelayCapacity}},
		{"full device", func(t *testing.T, u *unit) engine.Share {
			if err := u.j.Refuse(enginetest.CallWriteAt, fullDevice()); err != nil {
				t.Fatal(err)
			}

			return u.share
		}, vfserr.ErrNoSpace},
		{"version limit", func(t *testing.T, u *unit) engine.Share {
			version := refusalErr(journal.LimitVersion)
			if err := u.j.Refuse(enginetest.CallWriteAt, version); err != nil {
				t.Fatal(err)
			}

			return u.share
		}, &vfserr.Delay{Cause: vfserr.DelayCommit}},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnit(t)
			sh := c.arm(t, u)
			f := entity.NewFileID()

			before, _, err := u.e.Overlay(t.Context(), sh.ID, f)
			if err != nil {
				t.Fatal(err)
			}

			next := u.j.Stats().NextVersion

			verifier, err := u.e.Write(t.Context(), sh.ID, f, 0, nil, engine.WriteOpts{})
			if err != nil {
				t.Fatal(err)
			}

			called := false

			base, cancel := context.WithCancel(t.Context())

			defer cancel()

			failIfWaits(t, u.e, cancel)

			ctx := clockCtx{Context: base, now: u.clock.Now()}

			_, err = u.e.Write(ctx, sh.ID, f, 0, []byte("abc"),
				engine.WriteOpts{Done: func() {
					called = true
				}})
			checkAnswer(t, "answer", err, c.want)

			after, pending, err := u.e.Overlay(t.Context(), sh.ID, f)
			if err != nil {
				t.Fatal(err)
			}

			if called || len(u.e.Pending(f)) != 0 || u.e.HasEntry(f) {
				t.Errorf("Done called %v, operations %v, entry %v", called,
					u.e.Pending(f), u.e.HasEntry(f))
			}

			if pending || !reflect.DeepEqual(before, after) {
				t.Errorf("overlay %+v (pending %v) after the refusal, was %+v",
					after, pending, before)
			}

			got, _ := u.e.Write(t.Context(), sh.ID, f, 0, nil, engine.WriteOpts{})
			if got != verifier {
				t.Errorf("verifier %v after the refusal, was %v", got, verifier)
			}

			if got := u.j.Stats().NextVersion; got != next {
				t.Errorf("next version %v after the refusal, was %v", got, next)
			}
		})
	}
}

// Every refused call is counted, and logged at Warn at most once a minute for
// a share, with the count of those suppressed; another share has its own line.
func TestRefusalsAreCountedAndLoggedOncePerMinutePerShare(t *testing.T) {
	var logged lockedBuffer

	u := newUnit(t, func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	other := enginetest.AddShare(t, u.fs.Store, "other", 1<<30)

	if err := u.e.Bind(t.Context(), other); err != nil {
		t.Fatal(err)
	}

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitShare)); err != nil {
		t.Fatal(err)
	}

	lines := func() []string {
		var out []string

		for _, l := range strings.Split(logged.String(), "\n") {
			if strings.Contains(l, "write refused") {
				out = append(out, l)
			}
		}

		return out
	}
	refuse := func(sh engine.Share) {
		_, err := u.e.Write(t.Context(), sh.ID, entity.NewFileID(), 0, []byte("a"),
			engine.WriteOpts{Done: func() {}})
		if !isDelay(err, vfserr.DelayCapacity) {
			t.Fatalf("a refused write: %v, want ErrDelay(DelayCapacity)", err)
		}
	}

	for range 1000 {
		refuse(u.share)
	}

	if n := u.e.Stats().WriteRefusals; n != 1000 {
		t.Errorf("%d refusals counted, want 1000", n)
	}

	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "level=WARN") {
		t.Errorf("%d refusal lines after 1000 refusals in a second, want 1 at Warn: %v",
			len(got), got)
	}

	u.clock.Advance(59 * time.Second)
	refuse(u.share)

	if n := len(lines()); n != 1 {
		t.Errorf("%d refusal lines 59 seconds later, want 1", n)
	}

	u.clock.Advance(time.Second)
	refuse(u.share)

	got := lines()
	if len(got) != 2 || !strings.Contains(got[1], "suppressed=1000") {
		t.Errorf("refusal lines a minute later: %v, want 2, the second with suppressed=1000", got)
	}

	refuse(other)

	if n := len(lines()); n != 3 {
		t.Errorf("%d refusal lines after another share's refusal, want 3", n)
	}

	// A third minute: the count restarts at each line, not at the first.
	for range 5 {
		refuse(u.share)
	}

	u.clock.Advance(time.Minute)
	refuse(u.share)

	got = lines()
	if len(got) != 4 || !strings.HasSuffix(strings.TrimSpace(got[3]), "suppressed=5") {
		t.Errorf("refusal lines in the third minute: %v, want 4, the last with suppressed=5", got)
	}

	if n := u.e.Stats().WriteRefusals; n != 1009 {
		t.Errorf("%d refusals counted at the end, want 1009", n)
	}
}

// A read-only journal and a plain I/O error are no capacity refusal: the
// answer is journalErr's, the refusal counter stays at 0 and no refusal line
// is logged.
func TestAReadOnlyRefusalIsNoCapacityRefusal(t *testing.T) {
	enospc := fullDevice()

	for _, c := range []struct {
		name string
		arm  func(t *testing.T, u *unit)
		want error
	}{
		{"read-only, EROFS", func(_ *testing.T, u *unit) {
			u.j.ReadOnly(fmt.Errorf("write segment: %w", syscall.EROFS))
		}, vfserr.ErrReadOnly},
		{
			"read-only, EIO", func(_ *testing.T, u *unit) {
				u.j.ReadOnly(syscall.EIO)
			},
			&vfserr.Delay{Cause: vfserr.DelayJournal},
		},
		{
			"read-only wrapping ENOSPC", func(_ *testing.T, u *unit) {
				u.j.ReadOnly(enospc)
			},
			&vfserr.Delay{Cause: vfserr.DelayJournal},
		},
		{"plain EIO", func(t *testing.T, u *unit) {
			if err := u.j.Fail(enginetest.CallWriteAt, syscall.EIO); err != nil {
				t.Fatal(err)
			}
		}, vfserr.ErrUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged lockedBuffer

			u := newUnit(t, func(d *engine.Deps) {
				d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
			})
			f := u.create(t, "a")
			c.arm(t, u)

			err := refusedWrite(t, u, t.Context(), f)
			checkAnswer(t, "answer", err, c.want)

			if n := u.e.Stats().WriteRefusals; n != 0 {
				t.Errorf("%d refusals counted, want 0", n)
			}

			if strings.Contains(logged.String(), "write refused") {
				t.Errorf("a refusal line was logged: %s", logged.String())
			}
		})
	}
}

// unknownLimit refuses every write by a limit the journal does not name.
type unknownLimit struct {
	engine.Handle
}

func (unknownLimit) WriteAt(
	context.Context,
	journal.FileID,
	int64,
	[]byte,
	int64,
) (journal.Version, error) {
	return journal.Version{}, &journal.NoSpaceError{
		Limit: journal.Limit(99), Missing: 1, Unit: journal.UnitBytes,
	}
}

// A limit the table has no row for is retry-later for capacity, never an I/O
// error and never a false disk full, and is logged once at Error however many
// writes it refuses.
func TestAnUnknownLimitIsRetryLaterAndLoggedOnce(t *testing.T) {
	var logged lockedBuffer

	u := newUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){func(d *engine.Deps) {
			d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
		}},
		wrap: func(x engine.Handle) engine.Handle {
			return unknownLimit{Handle: x}
		},
	})
	f := u.create(t, "a")

	for range 2 {
		err := refusedWrite(t, u, t.Context(), f)
		checkAnswer(t, "unknown limit", err, &vfserr.Delay{Cause: vfserr.DelayCapacity})
	}

	if n := strings.Count(logged.String(), "unknown limit"); n != 1 {
		t.Errorf("the unknown limit was logged %d times, want once", n)
	}

	if !strings.Contains(logged.String(), "level=ERROR") {
		t.Error("the unknown limit was not logged at Error")
	}

	if n := u.e.Stats().WriteRefusals; n != 2 {
		t.Errorf("%d refusals counted, want 2", n)
	}
}

// A clone retried by the removal loop meets a refusal by the version limit at
// its second reservation: the loop does not wait for the raise, nor count the
// refusal; it retries at its backoff and the clone completes.
func TestLoopReserveRefusalIsRetriedNotWaited(t *testing.T) {
	var logged lockedBuffer

	u, h := clonedHookedUnit(t, func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")

	var waits atomic.Int64

	u.e.OnRefused(func() {
		waits.Add(1)
		t.Error("the removal loop waits for the raise")
		// The loop's context cannot be canceled by a test: wake its wait, which
		// then retries against the one-shot refusal already spent.
		u.e.NotifyVersionLimitForTest(u.share.ID)
	})
	h.cloneTarget.set(func() {
		u.lose(src, mib/4, mib/4) // the copies now outrun the reservation
		u.j.RefuseReserve(refusalErr(journal.LimitVersion))
	})

	before := u.e.LoopSteps(u.share.ID)
	errc := u.startClone(context.Background(), src, dst, vfserr.Extent{Len: mib})

	// The loop's first step is the resize the journal refuses; its retry waits
	// one backoff on the engine's clock. The loop's own step counter, not the
	// clock's pending timers, says when each has run: the group commit's age
	// timer can arm in that window and is as pending as the loop's.
	for u.e.LoopSteps(u.share.ID) == before {
		runtime.Gosched()
	}

	refused := u.e.LoopSteps(u.share.ID)

	for u.e.LoopSteps(u.share.ID) == refused {
		u.clock.Advance(time.Second)
		runtime.Gosched()
	}

	if got := <-errc; got != nil {
		t.Fatalf("a clone refused once by the version limit at its resize: %v", got)
	}

	if n := waits.Load(); n != 0 {
		t.Errorf("the loop waited for the raise %d times, want none", n)
	}

	if n := u.e.Stats().WriteRefusals; n != 0 {
		t.Errorf("%d refusals counted, want 0: the loop's refusal answers no client", n)
	}

	// The loop's retry is logged with the table's answer, not the journal's
	// error (a copy past its reservation is the reservation's, and stays raw), and
	// the refusal is not warned.
	if !strings.Contains(logged.String(), "retry shortly: commit") ||
		strings.Contains(logged.String(), "refused by the version limit") ||
		strings.Contains(logged.String(), "write refused") {
		t.Errorf("the loop's retry was logged as %q, want the table's answer, no refusal line",
			logged.String())
	}
}
