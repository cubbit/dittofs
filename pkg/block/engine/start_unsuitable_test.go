package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// retryEvery is the interval of the retried open of an unsuitable journal
// (RFC 8 §2.5, RFC 1 §5.4).
const retryEvery = 30 * time.Second

// openScript is a fault wrapper around Deps.Open: its n-th answer is the
// n-th error of the script, and an open past the script, or a nil error,
// goes through to the starter's journal.
type openScript struct {
	mu      sync.Mutex
	answers []error
}

// next is the error the next Open answers instead of the journal, if any.
func (o *openScript) next() error {
	o.mu.Lock()

	defer o.mu.Unlock()

	if len(o.answers) == 0 {
		return nil
	}

	err := o.answers[0]
	o.answers = o.answers[1:]

	return err
}

// withOpens scripts the answers of the first Open calls.
func withOpens(answers ...error) starterOption {
	o := &openScript{answers: answers}

	return withStartDeps(func(d *engine.Deps) {
		inner := d.Open
		d.Open = func(
			ctx context.Context,
			dir string,
			floor journal.Version,
			expect journal.JournalID,
		) (engine.Journal, error) {
			if err := o.next(); err != nil {
				return nil, err
			}

			return inner(ctx, dir, floor, expect)
		}
	})
}

// raiseScript is a fault wrapper around Deps.Lifecycle: its n-th RaiseAtStart
// answers the n-th error of the script, and one past it, or a nil error,
// goes through to the store. With after set, the call goes through first and
// answers the error as well: the raise committed and its answer was lost.
type raiseScript struct {
	engine.Lifecycle

	after   bool
	mu      sync.Mutex
	answers []error
}

func (r *raiseScript) RaiseAtStart(
	ctx context.Context,
	shards []entity.ShardID,
) (map[entity.ShardID]uint64, error) {
	r.mu.Lock()

	var err error

	if len(r.answers) > 0 {
		err, r.answers = r.answers[0], r.answers[1:]
	}

	r.mu.Unlock()

	if err != nil && !r.after {
		return nil, err
	}

	incs, rerr := r.Lifecycle.RaiseAtStart(ctx, shards)

	if err != nil {
		return nil, err
	}

	return incs, rerr
}

// withRaises scripts the answers of the first RaiseAtStart calls.
func withRaises(answers ...error) starterOption {
	return withStartDeps(func(d *engine.Deps) {
		d.Lifecycle = &raiseScript{Lifecycle: d.Lifecycle, answers: answers}
	})
}

// withLostRaises is withRaises for raises that commit and then answer the
// error.
func withLostRaises(answers ...error) starterOption {
	return withStartDeps(func(d *engine.Deps) {
		d.Lifecycle = &raiseScript{Lifecycle: d.Lifecycle, answers: answers, after: true}
	})
}

// closeAtEnd closes the engine when the test ends, which stops the retried
// open a refused Start leaves running.
func (s *starter) closeAtEnd(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		if err := s.e.Close(context.Background()); err != nil {
			t.Errorf("Close = %v", err)
		}
	})
}

// retry waits until the retried open's timer is set, then advances the
// clock by d: a timer armed while Advance runs waits for the next Advance.
func (s *starter) retry(t *testing.T, d time.Duration) {
	t.Helper()

	eventually(t, "the retried open's timer is set", func() bool {
		return s.clock.Pending() == 1
	})
	s.clock.Advance(d)
}

// refused is why Stats says share id is not attached, nil when it is.
func (s *starter) refused(id entity.ShareID) error {
	return s.e.Stats().Shares[id].Refused
}

// attached waits until Stats names neither share as refused.
func (s *starter) attached(t *testing.T) {
	t.Helper()

	eventually(t, "the shares are attached", func() bool {
		return s.refused(s.one.ID) == nil && s.refused(s.two.ID) == nil
	})
}

// unsuitable is the refusal a journal answers for a directory the platform
// profile refuses: the sentinel, wrapped with the rule and the directory.
func unsuitable() error {
	return fmt.Errorf(
		"journal: directory %s refused: mount has no barriers: %w",
		startDevice,
		journal.ErrUnsuitableStorage,
	)
}

// conditionOf is the condition named name that st raises for the start
// device, and whether there is one.
func conditionOf(st engine.Stats, name string) (engine.Condition, bool) {
	for _, c := range st.Journals[startDevice].Conditions {
		if c.Name == name {
			return c, true
		}
	}

	return engine.Condition{}, false
}

// A journal whose Open refuses the directory as unsuitable does not fail
// Start: its shares are not attached, Stats names each with the device and
// the rule, and journal_unsuitable is raised (RFC 8 §2.5).
func TestStartLeavesAnUnsuitableJournalsSharesUnattached(t *testing.T) {
	s := newStarter(t)
	s.openErr = unsuitable()
	s.closeAtEnd(t)

	if err := s.start(t); err != nil {
		t.Fatalf("Start = %v, want nil: a refused journal does not stop the node", err)
	}

	st := s.e.Stats()

	for _, id := range []engine.Share{s.one, s.two} {
		got := st.Shares[id.ID]
		if got.Device != startDevice || !errors.Is(got.Refused, journal.ErrUnsuitableStorage) {
			t.Errorf(
				"share %v: device %q, refused %v; want %q and the rule",
				id.ID, got.Device, got.Refused, startDevice,
			)
		}

		if err := s.write(t, id.ID); !isDelay(err, vfserr.DelayFrozen) {
			t.Errorf("a write on share %v = %v, want ErrDelay(DelayFrozen)", id.ID, err)
		}
	}

	c, ok := conditionOf(st, "journal_unsuitable")
	if !ok || !errors.Is(c.Cause, journal.ErrUnsuitableStorage) {
		t.Errorf("journal_unsuitable = %+v (raised %t), want one naming the rule", c, ok)
	}

	if got := len(st.Journals[startDevice].Conditions); got != 1 {
		t.Errorf("%d conditions on the device, want exactly journal_unsuitable", got)
	}
}

// A removed share's tag is still in the journal waiting for its retried
// open, so its Drop, which forgets the tag there, answers retry-later too:
// ErrUnavailable would not be retried, and the tag would stay in the journal.
func TestADropOfARemovedSharesTagWaitsForTheRetriedOpen(t *testing.T) {
	s := newStarter(t)
	removed := entity.NewShareID()
	s.lc.removed = map[entity.ShareID]entity.TagRecord{
		removed: {
			Tag:   91,
			State: entity.TagOffloaded,
		},
	}
	s.openErr = unsuitable()
	s.closeAtEnd(t)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if err := s.e.Drop(t.Context(), removed); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Drop of a removed share while unattached = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Unbind(t.Context(), removed, false); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Unbind of a removed share while unattached = %v, want ErrDelay(DelayFrozen)", err)
	}
}

// Until the journal's shares attach, a Bind, Unbind or Drop naming a share
// the journal would serve answers retry-later: an Unbind or Drop that
// answered nil would report a share ended while its tag is still in the
// journal (RFC 8 §2.5).
func TestAnUnsuitableJournalsSharesAnswerRetryLaterToLifecycleCalls(t *testing.T) {
	s := newStarter(t, withSpare())
	s.openErr = unsuitable()
	s.closeAtEnd(t)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if err := s.e.Unbind(t.Context(), s.one.ID, false); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Unbind of a refused share = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Drop(t.Context(), s.one.ID); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Drop of a refused share = %v, want ErrDelay(DelayFrozen)", err)
	}

	if err := s.e.Bind(t.Context(), s.spare); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Bind of a share the journal would serve = %v, want ErrDelay(DelayFrozen)", err)
	}
}

// The open is retried every 30 s: 29 s on the clock leave the shares
// unattached and the 30th second attaches them.
func TestAnUnsuitableJournalIsRetriedEvery30Seconds(t *testing.T) {
	s := newStarter(t, withOpens(unsuitable(), unsuitable()))

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	s.retry(t, retryEvery-time.Second)

	if s.refused(s.one.ID) == nil || s.refused(s.two.ID) == nil {
		t.Fatal("the shares are attached 29 s after Start, want unattached until the 30th second")
	}

	// The 30th second retries and the journal refuses again: the timer set
	// again is the proof the retry ran, and the shares and the one condition
	// stay as they were.
	s.retry(t, time.Second)
	eventually(t, "the second refusal re-arms the retry", func() bool {
		return s.clock.Pending() == 1
	})

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
		if got := s.refused(id); !errors.Is(got, journal.ErrUnsuitableStorage) {
			t.Errorf(
				"share %v refused with %v after a second refusal, want ErrUnsuitableStorage",
				id,
				got,
			)
		}
	}

	st := s.e.Stats()

	c, ok := conditionOf(st, "journal_unsuitable")
	if !ok || len(st.Journals[startDevice].Conditions) != 1 ||
		!errors.Is(c.Cause, journal.ErrUnsuitableStorage) {
		t.Errorf("conditions after a second refusal = %+v, want one journal_unsuitable",
			st.Journals[startDevice].Conditions)
	}

	s.retry(t, retryEvery)
	s.attached(t)

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
		if err := s.write(t, id); err != nil {
			t.Errorf("a write on share %v after the attach = %v, want nil", id, err)
		}
	}
}

// A retried open that finds no journal recorded for the device ends the
// retry: the content is intact and a later start records it, so the shares
// stay refused and no condition is raised (RFC 8 §2.5).
func TestARetriedOpenThatFindsNoJournalRecordedEndsTheRetry(t *testing.T) {
	s := newStarter(t, noRecord(), withOpens(unsuitable(), journal.ErrIdentityMismatch))
	s.closeAtEnd(t)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	s.retry(t, retryEvery)

	eventually(t, "the retry ends refused as not recorded", func() bool {
		got := s.refused(s.one.ID)

		return got != nil && !errors.Is(got, journal.ErrUnsuitableStorage)
	})

	// Start logged the unsuitable refusal of each share; the retry that
	// ends logs each again, so an operator sees why the timer stopped.
	var ended int

	for _, r := range s.logs.warnings() {
		if strings.Contains(r.Message, "not retried again") {
			ended++
		}
	}

	if ended != 2 {
		t.Errorf("%d warnings of the retry's end, want one for each of the two shares", ended)
	}

	for range 2 {
		for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
			got := s.refused(id)
			if !errors.Is(got, engine.ErrNotRecorded) {
				t.Errorf("share %v refused with %v, want it refused as not recorded", id, got)
			}
		}

		if got := s.e.Stats().Journals[startDevice].Conditions; len(got) != 0 {
			t.Errorf("conditions %+v, want none: no journal is recorded, so none is lost", got)
		}

		if got := s.clock.Pending(); got != 0 {
			t.Errorf("%d timers pending after the retry ended, want none", got)
		}

		s.clock.Advance(retryEvery)
	}
}

// A retried open that succeeds attaches the shares as Bind does: step 1 for
// the journal, each share's handle, the shard's incarnation raised in the
// start form (a new grace instance, as the restart lost the shard's open
// state), and the condition cleared (RFC 8 §2.5, RFC 16 §3).
func TestARetriedOpenAttachesTheSharesAndClearsTheCondition(t *testing.T) {
	s := newStarter(t, withOpens(unsuitable()))

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	before := s.shardOf(t, s.one.Shard)

	s.retry(t, retryEvery)
	s.attached(t)

	if _, ok := conditionOf(s.e.Stats(), "journal_unsuitable"); ok {
		t.Error("journal_unsuitable is still raised after the attach")
	}

	for _, id := range []entity.ShareID{s.one.ID, s.two.ID} {
		if err := s.write(t, id); err != nil {
			t.Errorf("a write on share %v after the attach = %v, want nil", id, err)
		}
	}

	got := s.shardOf(t, s.one.Shard)
	if got.Incarnation != before.Incarnation+1 || got.GraceInstance != got.Incarnation {
		t.Errorf(
			"shard after the attach: incarnation %d, grace %d; want %d and %d from %d and %d",
			got.Incarnation, got.GraceInstance,
			before.Incarnation+1, before.Incarnation+1,
			before.Incarnation, before.GraceInstance,
		)
	}
}

// A late attach leaves the engine as a Start that opened the journal leaves
// it: a share Start never bound is bound after it, with no refusal left
// over from the retry (RFC 8 §2.5).
func TestABindAfterTheLateAttachSucceeds(t *testing.T) {
	s := newStarter(t, withOpens(unsuitable()), withSpare())
	s.closeAtEnd(t)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	s.retry(t, retryEvery)
	s.attached(t)

	if err := s.e.Bind(t.Context(), s.spare); err != nil {
		t.Errorf("Bind after the late attach = %v, want nil", err)
	}
}

// A retried open that Close overtook, after it rebuilt a clone, takes the
// rebuild back: the clone's registrations on both files and the guard of its
// destination are gone once Close returns.
func TestCloseTakesBackTheCloneALateAttachRebuilt(t *testing.T) {
	m := newMarkerCrash(t, 0, withOpens(unsuitable()))
	g := newGate(t)

	m.begin(t)

	m.starter.gate = g

	// The clock also drives open state's timers, so the retry's is not the
	// only one pending: advance until the retried open arrives.
	m.until(t, "the retried open reaches Open", func() bool {
		select {
		case <-g.entered:
			return true
		default:
			return false
		}
	})

	// The rig holds a removal loop of its own, which Close would wait for.
	m.run()

	closed := closing(m.e, t.Context())

	g.open()

	if err := <-closed; err != nil {
		t.Fatalf("Close = %v", err)
	}

	if src, dst := m.e.Registrations(m.src); src != 0 || dst != 0 {
		t.Errorf("source file holds %d source and %d destination registrations after Close, want none", src, dst)
	}

	if src, dst := m.e.Registrations(m.dst); src != 0 || dst != 0 {
		t.Errorf("destination file holds %d source and %d destination registrations after Close, want none", src, dst)
	}

	if m.e.GuardHeld(m.dst) {
		t.Error("the destination's guard is held after Close")
	}
}

// One retry timer waits for a refused journal, and Close leaves none running:
// the engine arms no other timer before a write, so the one pending after a
// refused Start is the retry's.
func TestOneRetryTimerWaitsForARefusedJournal(t *testing.T) {
	s := newStarter(t, withOpens(unsuitable()))

	if got := s.clock.Pending(); got != 0 {
		t.Fatalf("%d timers pending before Start, want none", got)
	}

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the retry's timer is the one pending", func() bool {
		return s.clock.Pending() == 1
	})

	if err := s.e.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// A Start that opens the journal arms no timer: what a refused Start leaves
// pending is the retry's.
func TestAStartThatOpensTheJournalArmsNoTimer(t *testing.T) {
	s := newStarter(t)

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if got := s.clock.Pending(); got != 0 {
		t.Errorf("%d timers pending after a Start that opened the journal, want none", got)
	}
}

// A share the journal serves answers retry-later until it attaches, then its
// Unbind succeeds and its Drop forgets the tag in the journal.
func TestLifecycleCallsSucceedOnceTheRetriedOpenAttaches(t *testing.T) {
	s := newStarter(t, withOpens(unsuitable()))

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	if err := s.e.Unbind(t.Context(), s.one.ID, false); !isDelay(err, vfserr.DelayFrozen) {
		t.Fatalf("Unbind while unattached = %v, want ErrDelay(DelayFrozen)", err)
	}

	s.retry(t, retryEvery)
	s.attached(t)

	if err := s.write(t, s.one.ID); err != nil {
		t.Fatalf("a write on the attached share = %v", err)
	}

	if s.j.Stats().Shares[s.one.Tag].UsedBytes == 0 {
		t.Fatal("the journal holds nothing for the share's tag before the Drop")
	}

	if err := s.e.Unbind(t.Context(), s.one.ID, false); err != nil {
		t.Errorf("Unbind once attached = %v, want nil", err)
	}

	if err := s.e.Drop(t.Context(), s.one.ID); err != nil {
		t.Errorf("Drop once attached = %v, want nil", err)
	}

	if got := s.j.Stats().Shares[s.one.Tag].UsedBytes; got != 0 {
		t.Errorf("the journal still holds %d bytes for the dropped share's tag", got)
	}
}

// No file is served before step 3 has run for it, on a late attach as at
// Start: while the replay's commit is held every call on the share answers
// ErrDelay, and the shard's incarnation is raised already.
func TestNoFileIsServedBeforeALateAttachsRecoveryRuns(t *testing.T) {
	r := newRig(t, withOpens(unsuitable()))
	_, f := r.newFile(t, "a")
	r.write(t, f, 0, 4096, writeMtime)
	r.atRestart()

	if err := r.start(t); err != nil {
		t.Fatalf("Start: %v", err)
	}

	before := r.shardOf(t, r.one.Shard)
	g := newGate(t)
	r.ex.gate = g

	r.retry(t, retryEvery)

	select {
	case <-g.entered:
	case <-t.Context().Done():
		t.Fatal("the late attach never reached the replay's commit")
	}

	if _, _, err := r.e.Overlay(t.Context(), r.one.ID, f); !isDelay(err, vfserr.DelayFrozen) {
		t.Errorf("Overlay while the commit is held = %v, want ErrDelay(DelayFrozen)", err)
	}

	if got := r.shardOf(t, r.one.Shard); got.Incarnation != before.Incarnation+1 {
		t.Errorf(
			"incarnation %d while the commit is held, want %d",
			got.Incarnation, before.Incarnation+1,
		)
	}

	g.open()
	r.attached(t)

	if _, _, err := r.e.Overlay(t.Context(), r.one.ID, f); err != nil {
		t.Errorf("Overlay once attached = %v, want nil", err)
	}
}

// A retried open that fails for another cause leaves the shares unattached,
// journal_unsuitable names that cause, and the next retry attaches them.
func TestARetriedOpenThatFailsForAnotherCauseIsRetried(t *testing.T) {
	cause := errors.New("read the segment: input/output error")
	s := newStarter(t, withOpens(unsuitable(), cause))

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	s.retry(t, retryEvery)

	eventually(t, "journal_unsuitable names the new cause", func() bool {
		c, ok := conditionOf(s.e.Stats(), "journal_unsuitable")

		return ok && errors.Is(c.Cause, cause)
	})

	if got := s.refused(s.one.ID); !errors.Is(got, cause) {
		t.Errorf("share refused with %v, want the latest cause %v", got, cause)
	}

	s.retry(t, retryEvery)
	s.attached(t)
}

// A late attach that fails after the journal opened closes what it opened,
// the handles before the journal, leaves the shares unattached and is retried
// (RFC 8 §2.5).
func TestAFailedLateAttachClosesWhatItOpenedInReverse(t *testing.T) {
	cause := errors.New("raise the incarnations: store unavailable")
	s := newStarter(t, freshJournals(), withOpens(unsuitable()), withRaises(nil, cause))

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	first := s.j

	s.retry(t, retryEvery)

	eventually(t, "journal_unsuitable names the raise's error", func() bool {
		c, ok := conditionOf(s.e.Stats(), "journal_unsuitable")

		return ok && errors.Is(c.Cause, cause)
	})

	// The order of the closes is the requirement, so the check reads what the
	// logging journal recorded of its own calls and forwarded to the fake
	// journal, which RFC 29 §4.11 allows for a call that is itself the
	// requirement.
	log := first.log()
	closeJournal := slices.Index(log, "close journal")

	for _, tag := range []journal.ShareTag{s.one.Tag, s.two.Tag} {
		h := slices.Index(log, fmt.Sprintf("close handle %d", tag))
		if h < 0 || closeJournal < 0 || h > closeJournal {
			t.Errorf("journal log %v: handle %d must close before the journal", log, tag)
		}
	}

	s.retry(t, retryEvery)
	s.attached(t)
}

// A raise that committed and whose answer was lost leaves the shard raised
// once more than its attach knows: the next retry raises it again in the start
// form, and a shard is not recorded as raised before it is served.
func TestAFailedLateAttachRaisesAgainInTheStartForm(t *testing.T) {
	s := newStarter(t, freshJournals(), withOpens(unsuitable()), withLostRaises(nil, errInjected))

	if err := s.start(t); err != nil {
		t.Fatal(err)
	}

	before := s.shardOf(t, s.one.Shard)

	s.retry(t, retryEvery)

	eventually(t, "the raise's error is named", func() bool {
		c, ok := conditionOf(s.e.Stats(), "journal_unsuitable")

		return ok && errors.Is(c.Cause, errInjected)
	})

	got := s.shardOf(t, s.one.Shard)
	if got.Incarnation != before.Incarnation+1 || got.GraceInstance != got.Incarnation {
		t.Fatalf(
			"shard after the lost raise: %d and %d, want %d and %d",
			got.Incarnation, got.GraceInstance, before.Incarnation+1, before.Incarnation+1,
		)
	}

	s.retry(t, retryEvery)
	s.attached(t)

	got = s.shardOf(t, s.one.Shard)
	if want := before.Incarnation + 2; got.Incarnation != want || got.GraceInstance != want {
		t.Errorf(
			"shard after the attach: %d and %d, want %d and %d: the start form again",
			got.Incarnation, got.GraceInstance, want, want,
		)
	}
}
