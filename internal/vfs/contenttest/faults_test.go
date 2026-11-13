package contenttest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/contenttest"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
)

// Faults with no fault set passes every call through, so Memory behind it
// still passes the suite.
func TestFaultsOverMemory(t *testing.T) {
	contenttest.Run(t, func(t *testing.T) contenttest.Subject {
		clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		m := contenttest.New(clock)
		share, other := entity.NewShareID(), entity.NewShareID()
		m.Bind(share)
		m.Bind(other)
		return contenttest.Subject{
			Content: contenttest.Faults(m),
			Share:   share,
			Other:   other,
			File:    func(*testing.T) entity.FileID { return entity.NewFileID() },
			Clock:   clock,
			Lose:    func(_ *testing.T, f entity.FileID) { m.Lose(share, f) },
		}
	})
}

// A fault set by On sees each call of its method as it arrives: an error
// it answers is the call's, the call never reaching Memory, and nil passes
// the call through; clearing it passes every call. A Read reads first, so
// one that fails answers the bytes it read.
func TestOnFailsTheCallsOfItsMethod(t *testing.T) {
	ctx := context.Background()
	m := contenttest.New(sim.NewTestClock(time.Unix(0, 0)))
	share, f := entity.NewShareID(), entity.NewFileID()
	m.Bind(share)
	c := contenttest.Faults(m)
	refused := errors.New("refused")
	var seen []contenttest.Call
	c.On(contenttest.OpWrite, func(_ context.Context, call contenttest.Call) error {
		seen = append(seen, call)
		if call.Extent.Offset == 0 {
			return refused
		}
		return nil
	})
	if _, err := c.Write(ctx, share, f, 0, []byte("ab"), engine.WriteOpts{}); err != refused {
		t.Errorf("the failed write answered %v, want %v", err, refused)
	}
	if _, err := c.Write(ctx, share, f, 4, []byte("cd"), engine.WriteOpts{}); err != nil {
		t.Errorf("the passed write answered %v", err)
	}
	want := []contenttest.Call{
		{Op: contenttest.OpWrite, Share: share, File: f, Extent: entity.Extent{Offset: 0, Length: 2}},
		{Op: contenttest.OpWrite, Share: share, File: f, Extent: entity.Extent{Offset: 4, Length: 2}},
	}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("the fault saw %+v, want %+v", seen, want)
	}
	buf := make([]byte, 6)
	if n, err := m.Read(ctx, share, f, 0, buf); n != 6 || err != nil || string(buf) != "\x00\x00\x00\x00cd" {
		t.Errorf("Memory holds %q (%d, %v), want only the passed write's bytes", buf[:n], n, err)
	}
	c.On(contenttest.OpWrite, nil)
	if _, err := c.Write(ctx, share, f, 0, []byte("ab"), engine.WriteOpts{}); err != nil || len(seen) != 2 {
		t.Errorf("a write once the fault was cleared: %v, the fault called %d times; want nil, 2", err, len(seen))
	}

	lost := errors.New("lost")
	c.On(contenttest.OpRead, func(context.Context, contenttest.Call) error { return lost })
	if n, err := c.Read(ctx, share, f, 0, buf); n != 6 || err != lost || string(buf) != "ab\x00\x00cd" {
		t.Errorf("a failed read answered %q, %v; want its 6 bytes and %v", buf[:n], err, lost)
	}
}

// After runs once Memory has answered: a write it stages is staged after
// the call's own.
func TestAfterRunsOnceTheCallIsAnswered(t *testing.T) {
	ctx := context.Background()
	m := contenttest.New(sim.NewTestClock(time.Unix(0, 0)))
	share, f := entity.NewShareID(), entity.NewFileID()
	m.Bind(share)
	c := contenttest.Faults(m)
	var dones []string
	write := func(name string) {
		if _, err := m.Write(ctx, share, f, 0, []byte(name), engine.WriteOpts{Done: func() { dones = append(dones, name) }}); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	write("before")
	c.After(contenttest.OpCommitExistence, func(context.Context, contenttest.Call) { write("after") })
	if err := c.CommitExistence(ctx, share, f); err != nil {
		t.Fatal(err)
	}
	if len(dones) != 1 || dones[0] != "before" {
		t.Errorf("the commit called the Dones of %v, want only the write before it", dones)
	}
	if _, pending, err := m.Overlay(ctx, share, f); !pending || err != nil {
		t.Errorf("the write After staged: pending %v, %v; want still pending", pending, err)
	}
}

// Pending answers the next Delete its retry channel without reaching
// Memory, which still holds the file; the Delete after it passes through.
func TestPendingLeavesTheNextReleasePending(t *testing.T) {
	ctx := context.Background()
	m := contenttest.New(sim.NewTestClock(time.Unix(0, 0)))
	share, f := entity.NewShareID(), entity.NewFileID()
	m.Bind(share)
	c := contenttest.Faults(m)
	if _, err := m.Write(ctx, share, f, 0, []byte("x"), engine.WriteOpts{}); err != nil {
		t.Fatal(err)
	}
	held := func() bool {
		n, err := m.Read(ctx, share, f, 0, make([]byte, 1))
		return err == nil && n == 1
	}
	retry := make(chan error)
	c.Pending(retry)
	if got, err := c.Delete(ctx, share, f); got != retry || err != nil || !held() {
		t.Errorf("the pending Delete answered %v, %v, the file held %v; want the retry channel, nil, held", got, err, held())
	}
	if got, err := c.Delete(ctx, share, f); got != nil || err != nil || held() {
		t.Errorf("the next Delete answered %v, %v, the file held %v; want nil, nil, released", got, err, held())
	}
}
