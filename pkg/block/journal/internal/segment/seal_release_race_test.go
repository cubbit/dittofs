package segment_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// The two rows below pin that a sealed segment's write reference — which the
// seal releases once its own sync covered the segment whole — is released
// EXACTLY once, whatever races the seal. Ref.Release is a bare decrement: a
// second release of one handle drives the entry's count negative and shuts
// the drain out of its zero crossing, so both rows read the count through the
// cache's own probe and fail on it directly.

// TestASealFinishingUnderARollingWaitsRefOnce: a roll can read the segment it
// is leaving while a seal of it is still running — the defense-in-depth wait
// the roll's own predecessor check is built for — and the seal it waits on
// then finishes and releases the old segment's write reference itself. The
// woken roll must not release the same handle a second time at its install
// tail: the reference it carries away is read after the wait, never
// snapshotted under one, so a handle the parked roll read is never the one it
// releases.
func TestASealFinishingUnderARollingWaitsRefOnce(t *testing.T) {
	e := streamOver(t, streamCap, func(streamEnv) {})
	fid := core.NewFileID([16]byte{7})
	head, body := streamWriteHeader(fid, 0, make([]byte, 32<<10), 1, core.Version{Counter: 1})
	loc := streamAppendWrite(t, e, head, body)
	seg := loc.Loc.Segment

	// The append that cannot place its record in the active segment's
	// remainder arms the roll gate and runs the roll itself, and the roll's
	// take is held at the hook: the successor taken, the old segment's state
	// not yet read, and the stream's append lock free — the staging a racing
	// seal needs. The seal runs while the roll sits there, parked at its own
	// storage sync, so the segment the roll is leaving answers sealing by
	// the time the roll reads it.
	// Two pause points, staged in order: the roll takes its successor and
	// sits after the take with the append lock free — the staging the seal
	// needs to start mid-roll — and once let through, it parks on the
	// drain's cond over the sealing segment the seal is mid-write of. The
	// wait-entry hook confirms the park, and the sync is released only
	// after the confirmation, so the seal's sealed tail runs only once the
	// roll is on the drain: the release-and-link order below is forced,
	// never raced — 20 of its 20 runs read one handle, one release,
	// whatever the scheduler interleaves outside the parked state.
	taken := make(chan struct{})
	letThrough := make(chan struct{})
	rolledToWait := make(chan struct{})
	e.s.SetRollTakeHook(func() {
		close(taken)
		<-letThrough
	})
	t.Cleanup(func() { e.s.SetRollTakeHook(nil) })
	e.s.SetRollWaitHook(func() {
		select {
		case rolledToWait <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() { e.s.SetRollWaitHook(nil) })
	appendDone := make(chan error, 1)
	go func() {
		h, b := streamWriteHeader(fid, 32<<10, make([]byte, 60<<10), 1, core.Version{Counter: 2})
		_, err := e.s.AppendRoll(h, b, segment.Roll{Whole: true})
		appendDone <- err
	}()
	select {
	case <-taken:
	case <-time.After(10 * time.Second):
		t.Fatal("the roll never reached its take")
	}
	syncHold := e.seam.Hold(t, kit.OpSync, streamSegName(seg))
	t.Cleanup(syncHold.Release)
	sealDone := make(chan error, 1)
	go func() {
		_, err := e.s.Seal(false)
		sealDone <- err
	}()
	select {
	case <-syncHold.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the seal's storage sync never reached the seam's hold")
	}

	// The roll takes the lock it parked without, reads the sealing state
	// twice over — the wait's entry hooks the first — and only when it is
	// ON the drain does the sync go free: the seal's remaining steps
	// finish, the sealed tail takes the lock the roll gave up at the wait
	// and releases the write reference, and the woken roll carries on
	// over a segment already sealed. Should the seal's tail finish
	// anywhere else — before the roll's park, after its snapshot — the
	// row loses its forced order, and the handle count shows it.
	close(letThrough)
	select {
	case <-rolledToWait:
	case <-time.After(10 * time.Second):
		t.Fatal("the roll never parked on the drain over the sealing segment")
	}
	syncHold.Release()
	for name, ch := range map[string]chan error{"the seal": sealDone, "the rolling append": appendDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s after its parked sync went free: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never returned once its parked sync went free", name)
		}
	}
	if got, want := segment.Refs(e.cache, seg), 0; got != want {
		t.Fatalf("the sealed segment's reference count after the roll = %d, want %d: the woken roll released the ref the finishing seal already had", got, want)
	}
}

// TestACloseRacingTheSealsStorageStepsKeepsTheRefsRelease: a close can take
// the stream's write reference while the seal runs its storage steps without
// the append lock — the seal is one of the close's in-flight operations, and
// the pair's release belongs to the close that took it. The seal answers
// sealed all the same — its bytes are durable — but releases only when the
// stream still holds the reference it read; the close's own release stays
// the one and only.
func TestACloseRacingTheSealsStorageStepsKeepsTheRefsRelease(t *testing.T) {
	e := streamOver(t, streamCap, func(streamEnv) {})
	fid := core.NewFileID([16]byte{7})
	head, body := streamWriteHeader(fid, 0, make([]byte, 4<<10), 1, core.Version{Counter: 1})
	loc := streamAppendWrite(t, e, head, body)
	seg := loc.Loc.Segment

	// The seal parked at its own storage sync, no append lock held. Any
	// later word of the stream sees the close, not the seal: a read that
	// the close has begun can no longer recall nor publish a byte, so the
	// read's refusal is how the row tells the close has taken the pair.
	syncHold := e.seam.Hold(t, kit.OpSync, streamSegName(seg))
	t.Cleanup(syncHold.Release)
	sealDone := make(chan error, 1)
	go func() {
		_, err := e.s.Seal(false)
		sealDone <- err
	}()
	select {
	case <-syncHold.Reached():
	case <-time.After(10 * time.Second):
		t.Fatal("the seal's storage sync never reached the seam's hold")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- e.s.Close() }()
	buf := make([]byte, format.RecordHeaderSize)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, err := e.s.ReadAt(seg, buf, 0); err != nil {
			if !errors.Is(err, segment.ErrStreamClosed) {
				t.Fatalf("a read of the closing stream = %v, want ErrStreamClosed", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the close never began: the stream's reads kept succeeding")
		}
	}

	// The read's refusal says the close's own locked section ran, and the
	// seal's ref word below takes that same lock — so the seal can only see
	// the pair as the close took it. Whatever the seal does now runs beside
	// the close, and exactly one of the two releases.
	syncHold.Release()
	for name, ch := range map[string]chan error{"the seal": sealDone, "the close": closeDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s after the parked sync went free: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never returned once the parked sync went free", name)
		}
	}
	if got, want := segment.Refs(e.cache, seg), 0; got != want {
		t.Fatalf("the sealed segment's reference count after the close = %d, want %d: the seal released the ref the close had taken", got, want)
	}
}
