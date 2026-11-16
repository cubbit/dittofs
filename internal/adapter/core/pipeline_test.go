package core_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var peer = netip.MustParseAddr("192.0.2.1")

// smallCharge is what a small-class reply buffer reserves: 4 KiB and the
// per-request overhead.
const smallCharge = 12 << 10

// started is a call run on its own goroutine; done is closed once it returns.
type started struct{ done chan struct{} }

func start(f func()) started {
	s := started{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		f()
	}()
	return s
}

func (s started) returned() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// admit is an Admit that must return a place at once.
func admit(t *testing.T, ctx context.Context, f core.FlowControl) func() {
	t.Helper()
	release, err := f.Admit(ctx, nil)
	if err != nil {
		t.Fatalf("Admit with a place free = %v, want a place", err)
	}
	return release
}

// The cap admits 64 requests; the 65th waits until a release frees a place.
func TestCapAdmits64(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := core.NewCapped()
		releases := make([]func(), 64)
		for i := range releases {
			releases[i] = admit(t, t.Context(), f)
		}
		var last func()
		s := start(func() { last, _ = f.Admit(t.Context(), nil) })
		synctest.Wait()
		if s.returned() {
			t.Fatal("the 65th Admit returned with 64 in flight, want it waiting")
		}
		releases[0]()
		synctest.Wait()
		if !s.returned() || last == nil {
			t.Fatal("the 65th Admit still waits after a release, want it admitted")
		}
		last()
		for _, release := range releases[1:] {
			release()
		}
	})
}

// A release called twice frees one place: after it, one Admit is let in and
// the next waits.
func TestCapReleaseFreesOnePlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := core.NewCapped()
		releases := make([]func(), 64)
		for i := range releases {
			releases[i] = admit(t, t.Context(), f)
		}
		releases[0]()
		releases[0]()
		releases[0] = admit(t, t.Context(), f)
		s := start(func() {
			if release, err := f.Admit(t.Context(), nil); err == nil {
				release()
			}
		})
		synctest.Wait()
		if s.returned() {
			t.Fatal("a second Admit returned after one release called twice, want it waiting: the release freed two places")
		}
		for _, release := range releases {
			release()
		}
	})
}

// Under a context already ended, Admit takes a free place, and with none
// returns the context's error at once.
func TestCapUnderAnEndedContextTriesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		late, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer stop()
		for _, ctx := range []context.Context{cancelled, late} {
			f := core.NewCapped()
			releases := make([]func(), 64)
			for i := range releases {
				releases[i] = admit(t, ctx, f)
			}
			var err error
			s := start(func() { _, err = f.Admit(ctx, nil) })
			synctest.Wait()
			if !s.returned() {
				t.Error("Admit at the cap under an ended context waits, want it refused at once")
			} else if !errors.Is(err, ctx.Err()) {
				t.Errorf("Admit at the cap under an ended context = %v, want %v", err, ctx.Err())
			}
			for _, release := range releases {
				release()
			}
			<-s.done
		}
	})
}

// reserving is a reserveReply run on its own goroutine.
type reserving struct {
	started
	buf *resources.Buf
	err error
}

func startReserving(ctx context.Context, pool *resources.BufPool, bound int) *reserving {
	r := &reserving{}
	r.started = start(func() { r.buf, r.err = core.ReserveReply(ctx, pool, peer, bound) })
	return r
}

// With the reply budget full, a reply reservation waits: its context's
// deadline ends it with the adapter's ErrDelay of DelayResources, a cancel
// with the context's error, and room freeing first with the buffer. The
// budget holds nothing for a wait that ended.
func TestReserveReplyWaitsForRoom(t *testing.T) {
	for _, end := range []string{"deadline", "cancel", "room"} {
		t.Run(end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget := resources.NewWaitBudget("reply", smallCharge)
				pool := resources.NewBufPool(budget)
				held, ok := pool.TryGet(peer, 0)
				if !ok {
					t.Fatal("TryGet on an empty budget refused")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				r := startReserving(ctx, pool, 0)
				synctest.Wait()
				if r.returned() {
					t.Fatalf("reserveReply on a full budget returned (%v), want it waiting", r.err)
				}

				switch end {
				case "deadline":
					<-r.done
					if !errors.Is(r.err, &vfs.Delay{Cause: vfs.DelayResources}) {
						t.Errorf("at the deadline reserveReply = %v, want ErrDelay of DelayResources", r.err)
					}
				case "cancel":
					cancel()
					<-r.done
					if !errors.Is(r.err, context.Canceled) || errors.Is(r.err, vfs.ErrDelay) {
						t.Errorf("cancelled, reserveReply = %v, want the context's error", r.err)
					}
				case "room":
					held.Release()
					held = nil
					<-r.done
					if r.err != nil || r.buf == nil || budget.Reserved() != smallCharge {
						t.Fatalf("with room, reserveReply = %v with %d reserved, want the buffer charged %d", r.err, budget.Reserved(), smallCharge)
					}
					r.buf.Release()
				}
				if held != nil {
					if r.buf != nil || budget.Reserved() != smallCharge {
						t.Errorf("after the wait ended: buffer %v, %d reserved; want none and the held %d", r.buf != nil, budget.Reserved(), smallCharge)
					}
					held.Release()
				}
				if got := budget.Reserved(); got != 0 {
					t.Errorf("at the end %d reserved, want 0", got)
				}
			})
		})
	}
}

// A bound past the reply pool's ceiling is ErrReplyTooBig at once, under a
// context with no deadline, with nothing reserved.
func TestReserveReplyPastTheCeilingIsTooBig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := resources.NewWaitBudget("reply", 16<<20)
		pool := resources.NewBufPool(budget)
		r := startReserving(t.Context(), pool, 16<<20)
		synctest.Wait()
		if !r.returned() {
			t.Fatal("reserveReply past the ceiling still waits, want it refused at once")
		}
		if !errors.Is(r.err, core.ErrReplyTooBig) || r.buf != nil || budget.Reserved() != 0 {
			t.Errorf("reserveReply past the ceiling = %v with %d reserved, want ErrReplyTooBig and 0", r.err, budget.Reserved())
		}
	})
}

// With the reply budget full, a status's buffer waits under the connection's
// context: it is handed out once room frees; at 30 s from the start of its
// wait the connection is closed with the reply-wait error; when the
// connection's context ends first the wait returns at once without closing.
// The budget holds nothing for a wait that ended.
func TestReserveStatusWaitsAtMost30s(t *testing.T) {
	for _, end := range []string{"room", "30 s", "closed"} {
		t.Run(end, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				budget := resources.NewWaitBudget("reply", smallCharge)
				pool := resources.NewBufPool(budget)
				held, ok := pool.TryGet(peer, 0)
				if !ok {
					t.Fatal("TryGet on an empty budget refused")
				}
				conn, cancel := context.WithCancel(t.Context())
				defer cancel()
				var closed []error
				var buf *resources.Buf
				begin := time.Now()
				s := start(func() { buf = core.ReserveStatus(conn, pool, peer, func(err error) { closed = append(closed, err) }) })
				synctest.Wait()
				if s.returned() {
					t.Fatal("reserveStatus on a full budget returned, want it waiting")
				}

				switch end {
				case "room":
					held.Release()
					held = nil
					<-s.done
					if buf == nil || budget.Reserved() != smallCharge {
						t.Fatalf("with room, reserveStatus handed out %v with %d reserved, want a buffer charged %d", buf != nil, budget.Reserved(), smallCharge)
					}
					buf.Release()
				case "30 s":
					select {
					case <-s.done:
					case <-time.After(time.Minute):
						t.Fatal("reserveStatus still waits a minute on, want it ended at 30 s")
					}
					if waited := time.Since(begin); waited != 30*time.Second {
						t.Errorf("reserveStatus ended after %v, want 30s", waited)
					}
					if len(closed) != 1 || !errors.Is(closed[0], core.ErrReplyWait) {
						t.Errorf("at 30 s the connection was closed with %v, want once with the reply-wait error", closed)
					}
				case "closed":
					cancel()
					synctest.Wait()
					if !s.returned() || time.Since(begin) != 0 {
						t.Fatalf("after the connection's end reserveStatus returned %v at %v, want at once", s.returned(), time.Since(begin))
					}
				}
				if end != "30 s" && len(closed) != 0 {
					t.Errorf("reserveStatus closed the connection with %v, want no close", closed)
				}
				if held != nil {
					if buf != nil || budget.Reserved() != smallCharge {
						t.Errorf("after the wait ended: buffer %v, %d reserved; want none and the held %d", buf != nil, budget.Reserved(), smallCharge)
					}
					held.Release()
				}
				if got := budget.Reserved(); got != 0 {
					t.Errorf("at the end %d reserved, want 0", got)
				}
			})
		})
	}
}
