package core_test

import (
	"bytes"
	"context"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// newHeld is a chained seam in version 2, the version that holds, whose
// server reads clk from here on.
func newHeld(t *testing.T, reply int64, d adapter.Deps, clk core.Clock, edit ...func(*core.Stages)) *served {
	t.Helper()
	s := newChained(t, reply, d, edit...)
	synctest.Wait() // the last set-up reply's buffer is given back after its send
	core.SetClock(s.sh, clk)
	return s
}

// heldChain is a version 2 chain under session 7 and tree 1: a PutFH of the
// root's handle, then a related Create of name, whose message ID is id+1.
func (s *served) heldChain(id uint64, name string) []testproto.Msg {
	return []testproto.Msg{
		{Op: testproto.OpPutFH, Version: 2, ID: id, Session: 7, Tree: 1, Handle: s.root},
		{Op: testproto.OpCreate, Version: 2, ID: id + 1, Session: inherited, Tree: 1, Name: name, Related: true},
	}
}

// keyOf is the ID the interim and final replies to message id carry.
func keyOf(id uint64) uint64 { return id | 1<<63 }

// pendingRecv is a read of the next record, started before the test moves
// the clock, so it can say whether the record has come.
type pendingRecv struct {
	started
	rs  []testproto.Reply
	err error
}

func (s *served) recvLater() *pendingRecv {
	r := &pendingRecv{}
	r.started = start(func() { r.rs, r.err = s.client.Recv() })
	return r
}

// arrived says, once everything in the bubble has settled, whether the
// record has come.
func (r *pendingRecv) arrived() bool {
	synctest.Wait()
	return r.returned()
}

// interimOf reads the record a held chain's first answer goes out in: the
// PutFH's success and the Create's interim, keyed by its message ID.
func (s *served) interimOf(id uint64) testproto.Reply {
	s.t.Helper()
	rs, err := s.client.Recv()
	if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusOK || !rs[1].Interim || rs[1].ID != keyOf(id) {
		s.t.Fatalf("the chain is answered %+v (%v), want the PutFH's success and the Create's interim keyed %d", rs, err, id)
	}
	return rs[1]
}

// finalOf reads a held Create's final reply, keyed by its message ID.
func (s *served) finalOf(id uint64) testproto.Reply {
	s.t.Helper()
	r := s.recvOne()
	if r.Interim || r.ID != keyOf(id) {
		s.t.Fatalf("the held Create's final reply is %+v, want a final keyed %d", r, id)
	}
	return r
}

// In version 2 a Create the service answers ErrDelay twice gets one interim
// and is re-driven on the backoff, at 10 ms and 20 ms after that, the third
// attempt answered success; in version 1 it is answered the delay status at
// once (§4.6).
func TestADelayedCreateIsReDrivenOnTheBackoff(t *testing.T) {
	t.Run("version 2", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			clk := newManualClock()
			s := newHeld(t, 1<<20, adapter.Deps{}, clk)
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			s.serve(s.heldChain(1, "f")...)
			s.interimOf(2)
			clk.Advance(10 * time.Millisecond)
			next := s.recvLater()
			clk.Advance(20*time.Millisecond - time.Nanosecond)
			if next.arrived() {
				t.Fatalf("the held Create got %+v before its third attempt", next.rs)
			}
			clk.Advance(time.Nanosecond)
			<-next.done
			if next.err != nil || len(next.rs) != 1 || next.rs[0].Interim || next.rs[0].Status != testproto.StatusOK || next.rs[0].ID != keyOf(2) {
				t.Errorf("the third attempt is answered %+v (%v), want its final success", next.rs, next.err)
			}
			if n := s.creates(); n != 1 {
				t.Errorf("%d files made, want 1", n)
			}
		})
	})
	t.Run("version 1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newServed(t, 1<<20, adapter.Deps{})
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			s.serve(s.fhThenCreate(1, "f")...)
			if st := s.statuses(); !slices.Equal(st, []core.Status{testproto.StatusOK, testproto.StatusDelay}) {
				t.Errorf("the chain is answered %v, want the PutFH's success and the delay status", st)
			}
		})
	})
}

// A re-drive runs Resolve again: a session logged off between attempts ends
// the hold with the no-session status, and no file is made (§4.6).
func TestALogoffBetweenAttemptsEndsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "f")...)
		s.interimOf(2)
		s.serve(testproto.Msg{Op: testproto.OpLogoff, Version: 2, ID: 3, Session: 7})
		if r := s.recvOne(); r.Status != testproto.StatusOK {
			t.Fatalf("the Logoff is answered %+v", r)
		}
		clk.Advance(10 * time.Millisecond)
		if r := s.finalOf(2); r.Status != testproto.StatusNoSession || s.creates() != 0 {
			t.Errorf("the re-drive is answered %d with %d files made, want the no-session status and none", r.Status, s.creates())
		}
	})
}

// A re-drive runs the expiry check again: a credential that expires between
// attempts ends the hold with the expired status, and the Create does not
// reach the service (§4.4, §4.6).
func TestACredentialExpiredBetweenAttemptsEndsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.serve(testproto.Msg{Op: testproto.OpSession, Version: 2, ID: 1, Session: 7, Expires: t0.Add(5 * time.Millisecond)})
		if r := s.recvOne(); r.Status != testproto.StatusOK {
			t.Fatalf("the Session is answered %+v", r)
		}
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(2, "f")...)
		s.interimOf(3)
		clk.Advance(10 * time.Millisecond)
		if r := s.finalOf(3); r.Status != testproto.StatusExpired || s.creates() != 0 {
			t.Errorf("the re-drive is answered %d with %d files made, want the expired status and none", r.Status, s.creates())
		}
	})
}

// A Create the service keeps answering ErrDelay is answered the delay status
// when the clock reaches its hold's bound, and not before; with the bound
// shortened between attempts, at the new bound: the row's Hold is read
// afresh (§4.6).
func TestAHoldEndsAtItsBound(t *testing.T) {
	for _, c := range []struct {
		name    string
		shorten time.Duration // the bound set at 1.5 s; 0: none
		at      time.Duration // the answer, after t0
	}{
		{"left failing", 0, testproto.HoldFor},
		{"its bound shortened between attempts", 2270 * time.Millisecond, 2270 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				s := newHeld(t, 1<<20, adapter.Deps{}, clk)
				for range 20 {
					s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
				}
				s.serve(s.heldChain(1, "f")...)
				s.interimOf(2)
				next := s.recvLater()
				// Every re-drive falls on a multiple of 10 ms: the backoff
				// doubles from 10 ms to 1 s.
				for d := 10 * time.Millisecond; d < c.at; d += 10 * time.Millisecond {
					clk.Advance(10 * time.Millisecond)
					if d == 1500*time.Millisecond && c.shorten != 0 {
						s.tp.SetHold(c.shorten)
					}
				}
				if next.arrived() {
					t.Fatalf("the held Create got %+v before its bound", next.rs)
				}
				clk.Advance(10 * time.Millisecond)
				if !next.arrived() {
					t.Fatal("the held Create is not answered at its bound")
				}
				if next.err != nil || len(next.rs) != 1 || next.rs[0].Status != testproto.StatusDelay || next.rs[0].ID != keyOf(2) {
					t.Errorf("at its bound the Create is answered %+v (%v), want its final delay status", next.rs, next.err)
				}
			})
		})
	}
}

// Between attempts a held request keeps its record, so its decoded
// arguments stay valid, and holds no reply reservation: each attempt takes
// its own (§4.6; RFC 24 §4.3).
func TestBetweenAttemptsAHeldRequestKeepsItsRecordAndNoReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "f")...)
		s.interimOf(2)
		for _, after := range []string{"its interim", "its second attempt"} {
			synctest.Wait()
			if recv, reply := s.budgets.Receive.Reserved(), s.budgets.Reply.Reserved(); recv == 0 || reply != 0 {
				t.Errorf("after %s the receive budget holds %d and the reply budget %d, want the record and no reply", after, recv, reply)
			}
			clk.Advance(10 * time.Millisecond)
		}
		clk.Advance(10 * time.Millisecond)
		if r := s.finalOf(2); r.Status != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("the third attempt is answered %d with %d files, want success and the file", r.Status, s.creates())
		}
	})
}

// Each attempt reaches the service with its own deadline: a re-drive at
// 10 ms held in the service is answered 30 s from its own start, and, 5 s
// before its hold's bound, at the bound, not before (§4.6, §6.4).
func TestEachAttemptHasItsOwnDeadline(t *testing.T) {
	for _, c := range []struct {
		name string
		hold time.Duration
		at   time.Duration // the answer, after t0
	}{
		{"30 s from its start", time.Minute, 10*time.Millisecond + 30*time.Second},
		{"the hold's bound, 5 s on", 5*time.Second + 10*time.Millisecond, 5*time.Second + 10*time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				s := newHeld(t, 1<<20, adapter.Deps{}, clk)
				s.tp.SetHold(c.hold)
				s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
				hold := s.faults.Hold(vfs.OpCreate)
				defer hold.Release()
				s.serve(s.heldChain(1, "f")...)
				s.interimOf(2)
				redrive := start(func() { clk.Advance(10 * time.Millisecond) })
				<-hold.Reached()
				next := s.recvLater()
				clk.Advance(c.at - 10*time.Millisecond - time.Nanosecond)
				if next.arrived() {
					t.Fatalf("the re-drive got %+v before its deadline", next.rs)
				}
				clk.Advance(time.Nanosecond)
				if !next.arrived() {
					t.Fatal("the re-drive is not answered at its deadline")
				}
				<-redrive.done
				if next.err != nil || len(next.rs) != 1 || next.rs[0].Status != testproto.StatusOK || next.rs[0].ID != keyOf(2) {
					t.Errorf("at its deadline the re-drive is answered %+v (%v), want the service's success", next.rs, next.err)
				}
			})
		})
	}
}

// ErrDelay of DelayResources from the service is a node pool's refusal,
// answered the delay status at once in version 2, with no interim (§6.3).
func TestTheServicesResourcesDelayIsNotHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		s.faults.Fail(vfs.OpCreate, &vfs.Delay{Cause: vfs.DelayResources})
		s.serve(s.heldChain(1, "f")...)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[1].Interim || rs[1].Status != testproto.StatusDelay || rs[1].ID != 2 {
			t.Errorf("the chain is answered %+v (%v), want the Create's delay status at once", rs, err)
		}
	})
}

// A Create still in the service a second after it reached it gets its
// interim from the adapter's timer, with the PutFH's reply before it, and
// its final reply carries its key (§4.6).
func TestACallStillInTheServiceGetsAnInterim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.heldChain(1, "f")...)
		<-hold.Reached()
		next := s.recvLater()
		clk.Advance(time.Second - time.Nanosecond)
		if next.arrived() {
			t.Fatalf("the chain got %+v before the interval", next.rs)
		}
		clk.Advance(time.Nanosecond)
		<-next.done
		if next.err != nil || len(next.rs) != 2 || next.rs[0].Status != testproto.StatusOK || !next.rs[1].Interim || next.rs[1].ID != keyOf(2) {
			t.Fatalf("at the interval the chain got %+v (%v), want the PutFH's success and the Create's interim", next.rs, next.err)
		}
		hold.Release()
		if r := s.finalOf(2); r.Status != testproto.StatusOK {
			t.Errorf("the Create's final reply is %+v, want success", r)
		}
	})
}

// A held last member's interim grants its own credits alone, though the
// PutFH answered before it is in its record: it is handed no headers, which
// testproto would grant the credits of too (§4.7).
func TestAHeldMembersInterimGrantsItsOwnCredits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		msgs := s.heldChain(1, "f")
		msgs[0].Credits, msgs[1].Credits = 1, 1
		before := s.client.Balance()
		s.serve(msgs...)
		s.interimOf(2)
		if got := s.client.Balance() - before; got != 2 {
			t.Errorf("the PutFH's reply and the interim granted %d credits, want one each", got)
		}
		clk.Advance(10 * time.Millisecond)
		if r := s.finalOf(2); r.Credits != 0 {
			t.Errorf("the final reply grants %d credits, want none: its interim granted them", r.Credits)
		}
	})
}

// Past Stages.MaxPending, a hold starting on the service's error is answered
// the too-many-pending status at once; a call the interim timer finds in the
// service goes pending all the same, its interim granting one credit of its
// three and its final reply the other two (§4.6).
func TestPastMaxPending(t *testing.T) {
	none := func(st *core.Stages) { st.MaxPending = 0 }
	t.Run("a hold on the service's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock(), none)
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			s.serve(s.heldChain(1, "f")...)
			rs, err := s.client.Recv()
			if err != nil || len(rs) != 2 || rs[1].Interim || rs[1].Status != testproto.StatusTooManyPending {
				t.Errorf("the chain is answered %+v (%v), want the Create's too-many-pending status at once", rs, err)
			}
		})
	})
	t.Run("a call in the service", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			clk := newManualClock()
			s := newHeld(t, 1<<20, adapter.Deps{}, clk, none)
			hold := s.faults.Hold(vfs.OpCreate)
			defer hold.Release()
			msgs := s.heldChain(1, "f")
			msgs[1].Credits = 3
			s.serve(msgs...)
			<-hold.Reached()
			clk.Advance(time.Second)
			if r := s.interimOf(2); r.Credits != 1 {
				t.Errorf("the interim grants %d credits, want one", r.Credits)
			}
			hold.Release()
			if r := s.finalOf(2); r.Status != testproto.StatusOK || r.Credits != 2 {
				t.Errorf("the final reply is %+v, want success granting the other two credits", r)
			}
		})
	})
}

// holdBudgets is a seam's budgets with a hold pool of hold bytes.
func holdBudgets(hold int64) *resources.Budgets {
	return &resources.Budgets{Receive: resources.NewWaitBudget("receive", 64<<20), Reply: resources.NewWaitBudget("reply", 1<<20), Hold: resources.NewBudget("hold", hold)}
}

// gauges reads the held requests', their bytes', the hold pool's and the
// hold refusals' series.
func gauges(t *testing.T, m *metrics.Registry) [4]float64 {
	t.Helper()
	got, _ := scrape(t, m)
	const p = "{protocol=" + testproto.Name + "}"
	return [4]float64{got["adapter_held_requests"+p], got["adapter_held_bytes"+p], got["adapter_hold_pool_bytes{}"], got["adapter_hold_refusals_total"+p]}
}

// With the floor lowered to two requests, the third held request moves its
// record's whole charge from the receive budget to the hold pool; the held
// gauges count all three and fall back to zero as they are answered (§4.6;
// RFC 24 §4.3).
func TestPastTheFloorAHoldMovesItsChargeToTheHoldPool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		budgets, m := holdBudgets(64<<20), metrics.New("v", "c")
		s := newHeld(t, 0, adapter.Deps{Budgets: budgets, Metrics: m}, clk)
		core.SetFloor(s.sh, 2, 32<<20)
		var charge int64
		for i := range 3 {
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			before := budgets.Receive.Reserved()
			s.serve(s.heldChain(uint64(10*i+1), "f"+strconv.Itoa(i))...)
			s.interimOf(uint64(10*i + 2))
			synctest.Wait()
			if i == 0 {
				charge = budgets.Receive.Reserved() - before
			}
		}
		if recv, hold := budgets.Receive.Reserved(), budgets.Hold.Reserved(); charge == 0 || recv != 2*charge || hold != charge {
			t.Errorf("three held of %d bytes each: the receive budget holds %d and the hold pool %d, want two and one", charge, recv, hold)
		}
		if got, want := gauges(t, m), [4]float64{3, float64(3 * charge), float64(charge), 0}; got != want {
			t.Errorf("held, their bytes, the pool's and the refusals read %v, want %v", got, want)
		}
		clk.Advance(10 * time.Millisecond)
		for range 3 {
			s.recvOne()
		}
		synctest.Wait()
		if got := gauges(t, m); got != [4]float64{} || budgets.Hold.Reserved() != 0 {
			t.Errorf("once answered the gauges read %v and the hold pool %d, want all zero", got, budgets.Hold.Reserved())
		}
	})
}

// With the floor at zero and the hold pool full, a hold starting on the
// service's error is refused at once, the delay status with no interim, and
// counted; a call the interim timer finds in the service goes pending
// keeping its receive reservation, its interim withholding its credits
// (§4.6; RFC 24 §4.3).
func TestWithTheHoldPoolFull(t *testing.T) {
	t.Run("a hold on the service's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := metrics.New("v", "c")
			s := newHeld(t, 0, adapter.Deps{Budgets: holdBudgets(0), Metrics: m}, newManualClock())
			core.SetFloor(s.sh, 0, 0)
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			s.serve(s.heldChain(1, "f")...)
			rs, err := s.client.Recv()
			if err != nil || len(rs) != 2 || rs[1].Interim || rs[1].Status != testproto.StatusDelay {
				t.Errorf("the chain is answered %+v (%v), want the Create's delay status at once", rs, err)
			}
			if got := gauges(t, m); got != [4]float64{0, 0, 0, 1} {
				t.Errorf("held, their bytes, the pool's and the refusals read %v, want one refusal alone", got)
			}
		})
	})
	t.Run("a call in the service", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			clk := newManualClock()
			budgets, m := holdBudgets(0), metrics.New("v", "c")
			s := newHeld(t, 0, adapter.Deps{Budgets: budgets, Metrics: m}, clk)
			core.SetFloor(s.sh, 0, 0)
			hold := s.faults.Hold(vfs.OpCreate)
			defer hold.Release()
			msgs := s.heldChain(1, "f")
			msgs[1].Credits = 3
			s.serve(msgs...)
			<-hold.Reached()
			recv := budgets.Receive.Reserved()
			clk.Advance(time.Second)
			if r := s.interimOf(2); r.Credits != 1 {
				t.Errorf("the interim grants %d credits, want one", r.Credits)
			}
			if got := budgets.Receive.Reserved(); got != recv || got == 0 {
				t.Errorf("the pending call's receive budget holds %d, want its %d kept", got, recv)
			}
			if got := gauges(t, m); got != [4]float64{1, float64(recv), 0, 0} {
				t.Errorf("held, their bytes, the pool's and the refusals read %v, want the one call", got)
			}
			hold.Release()
			if r := s.finalOf(2); r.Status != testproto.StatusOK || r.Credits != 2 {
				t.Errorf("the final reply is %+v, want success granting the other two credits", r)
			}
		})
	})
}

// close ends a held request at once, between attempts and while a re-drive
// is in the service: close returns without waiting for it, wait returns once
// it has ended, no timer is left, nothing more is sent, its request span
// ends unsent, and every budget is back at zero (§4.6, §6.5).
func TestCloseEndsAHold(t *testing.T) {
	for _, inCall := range []bool{false, true} {
		t.Run("in a re-drive's call "+strconv.FormatBool(inCall), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				m := metrics.New("v", "c")
				spans := tracetest.NewSpanRecorder()
				tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
				s := newHeld(t, 1<<20, adapter.Deps{Metrics: m, Tracer: tracer, SampleRatio: 1}, clk)
				s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
				hold := s.faults.Hold(vfs.OpCreate)
				defer hold.Release()
				s.serve(s.heldChain(1, "f")...)
				s.interimOf(2)
				redrive := started{done: make(chan struct{})}
				if inCall {
					redrive = start(func() { clk.Advance(10 * time.Millisecond) })
					<-hold.Reached()
				}
				closing := start(s.p.Close)
				synctest.Wait()
				if !closing.returned() {
					t.Fatal("close waited for the hold it ends")
				}
				wait, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := s.p.Wait(wait); err != nil {
					t.Fatalf("the hold outlived its connection: %v", err)
				}
				if inCall {
					<-redrive.done
				}
				if n := clk.pending(); n != 0 {
					t.Errorf("%d timers left armed", n)
				}
				if got := gauges(t, m); got != [4]float64{} {
					t.Errorf("held, their bytes, the pool's and the refusals read %v, want zero", got)
				}
				if sd := requestSpan(t, spans, "Create"); slices.ContainsFunc(sd.Attributes(), func(kv attribute.KeyValue) bool { return kv.Key == "reply_bytes" }) {
					t.Errorf("the closed hold's span ended as sent: %v", sd.Attributes())
				}
				if !s.sentNothing() {
					t.Error("the closed hold was answered")
				}
			})
		})
	}
}

// Eight requests held between attempts make no call until the clock moves,
// and none has a goroutine: no stack is in the hold table's code (§4.6;
// RFC 24 §6.1).
func TestEightHeldRequestsHaveNoGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		for i := range 8 {
			s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
			s.serve(s.heldChain(uint64(10*i+1), "f"+strconv.Itoa(i))...)
			s.interimOf(uint64(10*i + 2))
		}
		synctest.Wait()
		stacks := make([]byte, 1<<20)
		stacks = stacks[:runtime.Stack(stacks, true)]
		if bytes.Contains(stacks, []byte("/core/hold.go:")) {
			t.Errorf("a goroutine waits in the hold table's code:\n%s", stacks)
		}
		if n := s.creates(); n != 0 {
			t.Errorf("%d files made before the clock moved, want none", n)
		}
		clk.Advance(10 * time.Millisecond)
		for range 8 {
			s.recvOne()
		}
		if n := s.creates(); n != 8 {
			t.Errorf("%d files made once re-driven, want 8", n)
		}
	})
}

// requestSpan is the ended request span of op's request, the one with no
// parent.
func requestSpan(t *testing.T, spans *tracetest.SpanRecorder, op string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, sd := range spans.Ended() {
		if sd.Name() == "adapter."+testproto.Name+"."+op && !sd.Parent().IsValid() {
			return sd
		}
	}
	t.Fatalf("no request span ended for the %s", op)
	return nil
}

// A held request keeps its request span across its re-drives, with one
// child per attempt (§10.2).
func TestEachAttemptOfAHeldRequestHasAChildSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{Tracer: tracer, SampleRatio: 1}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "f")...)
		s.interimOf(2)
		clk.Advance(10 * time.Millisecond)
		clk.Advance(20 * time.Millisecond)
		s.finalOf(2)
		synctest.Wait()
		request := requestSpan(t, spans, "Create")
		children := 0
		for _, sd := range spans.Ended() {
			if sd.Parent().SpanID() == request.SpanContext().SpanID() {
				children++
			}
		}
		if children != 3 {
			t.Errorf("the Create's request span has %d children, want one per attempt, three", children)
		}
	})
}

// A second chain is answered while the first is in the service: the hold
// table's lock is never held across a call (§6.6).
func TestASecondChainIsAnsweredWhileTheFirstIsInTheService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "a")...)
		<-hold.Reached()
		s.serve(s.heldChain(11, "b")...)
		next := s.recvLater()
		if !next.arrived() {
			t.Fatal("the second chain is not answered while the first is in the service")
		}
		if len(next.rs) != 2 || !next.rs[1].Interim || next.rs[1].ID != keyOf(12) {
			t.Errorf("the second chain is answered %+v, want its Create's interim", next.rs)
		}
		hold.Release()
		if st := s.statuses(); !slices.Equal(st, []core.Status{testproto.StatusOK, testproto.StatusOK}) {
			t.Errorf("the first chain is answered %v, want two successes", st)
		}
		clk.Advance(10 * time.Millisecond)
		s.finalOf(12)
	})
}

// cancelOf is a version 2 Cancel, sealed under session, naming message id.
func cancelOf(id uint64, session uint32) testproto.Msg {
	return testproto.Msg{Op: testproto.OpCancel, Version: 2, Session: session, Target: id}
}

// A cancel ends a hold between attempts at once, alone or as a chain's
// first or later member: its final reply is the cancel status, the cancel
// gets no reply of its own, no re-drive is left and no file is made. core
// matches the protocol's key alone, so a cancel under a second session of the
// connection ends it too (§4.7, §6.5).
func TestACancelEndsAHoldBetweenAttempts(t *testing.T) {
	for _, c := range []struct {
		name    string
		session uint32 // the cancel's, set up where not 7
		msgs    []testproto.Msg
	}{
		{"alone", 7, []testproto.Msg{cancelOf(2, 7)}},
		{"under a second session", 8, []testproto.Msg{cancelOf(2, 8)}},
		{"first in a chain", 7, []testproto.Msg{cancelOf(2, 7), cancelOf(92, 7)}},
		{"later in a chain", 7, []testproto.Msg{cancelOf(92, 7), cancelOf(2, 7)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				s := newHeld(t, 1<<20, adapter.Deps{}, clk)
				if c.session != 7 {
					s.serve(testproto.Msg{Op: testproto.OpSession, Version: 2, Session: c.session})
					if r := s.read(); r.Status != testproto.StatusOK {
						t.Fatalf("the second session is answered %+v", r)
					}
				}
				s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
				s.serve(s.heldChain(1, "f")...)
				s.interimOf(2)
				s.serve(c.msgs...)
				if r := s.finalOf(2); r.Status != testproto.StatusCancelled {
					t.Errorf("the cancelled hold is answered %+v, want the cancel status", r)
				}
				if n, files := clk.pending(), s.creates(); n != 0 || files != 0 {
					t.Errorf("%d timers left armed and %d files made, want none", n, files)
				}
				if !s.sentNothing() {
					t.Error("the cancel got a reply of its own")
				}
			})
		})
	}
}

// A cancel sent while an attempt is in the service, where the interim timer
// found it, lets the attempt finish: its final reply is its own success, and
// the file is made (§6.5).
func TestACancelLetsARunningAttemptFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.heldChain(1, "f")...)
		<-hold.Reached()
		clk.Advance(time.Second)
		s.interimOf(2)
		s.serve(cancelOf(2, 7))
		synctest.Wait()
		hold.Release()
		if r := s.finalOf(2); r.Status != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("the attempt is answered %+v with %d files made, want its success and the file", r, s.creates())
		}
	})
}

// A cancel that finds nothing is remembered: its target, sent next and
// answered ErrDelay, ends as cancelled once it holds, right after its
// interim, with no re-drive (§4.7).
func TestACancelBeforeItsTargetHoldsEndsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		s.serve(cancelOf(2, 7))
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "f")...)
		s.interimOf(2)
		if r := s.finalOf(2); r.Status != testproto.StatusCancelled || s.creates() != 0 {
			t.Errorf("the target is answered %+v with %d files made, want the cancel status and none", r, s.creates())
		}
	})
}

// A cancel sent clear under its session fails Unwrap and is refused there,
// never asked of Cancels: the hold it names goes on, and its re-drive's
// success is its final reply (§4.7, §5.5).
func TestAnUnsealedCancelEndsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "f")...)
		s.interimOf(2)
		unsealed := cancelOf(2, 7)
		unsealed.Clear = true
		s.serve(unsealed)
		if r := s.recvOne(); r.Status != testproto.StatusChecksum {
			t.Fatalf("the unsealed cancel is answered %+v, want the checksum status", r)
		}
		clk.Advance(10 * time.Millisecond)
		if r := s.finalOf(2); r.Status != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("the hold is answered %+v with %d files made, want its re-drive's success and the file", r, s.creates())
		}
	})
}

// A remembered cancel is forgotten 5 s after it came, on the clock, and at
// most MaxPending of them, eight in testproto, are kept, the oldest dropped
// first: a target that holds past either is re-driven, not cancelled (§4.7).
func TestARememberedCancelIsBounded(t *testing.T) {
	for _, c := range []struct {
		name      string
		cancels   int           // naming messages 2, 12, 22 and on
		after     time.Duration // on the clock, before the targets come
		cancelled []bool        // per target, messages 2, 12 and on
	}{
		{"just under 5 s on", 1, 5*time.Second - time.Nanosecond, []bool{true}},
		{"5 s on", 1, 5 * time.Second, []bool{false}},
		{"the ninth", 9, 0, []bool{false, true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				s := newHeld(t, 1<<20, adapter.Deps{}, clk)
				for i := range c.cancels {
					s.serve(cancelOf(uint64(10*i+2), 7))
				}
				clk.Advance(c.after)
				for i, want := range c.cancelled {
					id := uint64(10*i + 2)
					s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
					s.serve(s.heldChain(id-1, "f"+strconv.Itoa(i))...)
					s.interimOf(id)
					if !want {
						continue
					}
					if r := s.finalOf(id); r.Status != testproto.StatusCancelled {
						t.Errorf("target %d is answered %+v, want the cancel status", id, r)
					}
				}
				clk.Advance(10 * time.Millisecond)
				for i, want := range c.cancelled {
					if want {
						continue
					}
					if r := s.finalOf(uint64(10*i + 2)); r.Status != testproto.StatusOK {
						t.Errorf("target %d is answered %+v, want its re-drive's success", 10*i+2, r)
					}
				}
			})
		})
	}
}
