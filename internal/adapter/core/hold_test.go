package core_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"strings"
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
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
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
// PutFH's success and the Create's interim, keyed by its message ID. It
// returns once the pipeline has settled: a hold arms its re-drive after its
// interim goes out, so a clock moved at once could pass the re-drive's time
// before it is armed.
func (s *served) interimOf(id uint64) testproto.Reply {
	s.t.Helper()
	rs, err := s.client.Recv()
	if err != nil || len(rs) != 2 || rs[0].Status != testproto.StatusOK || !rs[1].Interim || rs[1].ID != keyOf(id) {
		s.t.Fatalf("the chain is answered %+v (%v), want the PutFH's success and the Create's interim keyed %d", rs, err, id)
	}
	synctest.Wait()
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

// A held Create whose interim does not encode closes its connection as a
// wiring fault, as the same stage failing off the held path does, never as a
// violation, which §10.1 keeps for a protocol's own Conn.Close.
func TestAnInterimThatDoesNotEncodeClosesAsWiring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk, func(st *core.Stages) { st.Hold = failingInterim{st.Hold} })
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.heldChain(1, "f")...)
		<-hold.Reached()
		clk.Advance(time.Second) // the interim's interval
		if got := core.CloseReason(s.closeErr(t)); got != "wiring" {
			t.Errorf("the close counts under %s, want wiring", got)
		}
	})
}

// failingInterim is a HoldCodec whose Interim fails: a protocol's stage
// faulting on the held path, by fault injection.
type failingInterim struct{ core.HoldCodec }

func (failingInterim) Interim(core.Header, bool, []core.Header, *resources.Buf) error {
	return errors.New("the interim does not encode")
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
// table's lock is never held across a call (§6.6). Its mutant, that lock held
// across the call, shows as the go test timeout, since synctest cannot settle
// a goroutine blocked on a mutex.
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

// A held request observes reply_wait and resolve_ids once, however many
// attempts run them, its runs summed: a PutFH and a Create the service
// answers ErrDelay twice, on a clock that steps 1 ms a read, observe each
// twice, once per request, summing 4 ms, the PutFH's 1 ms and the Create's
// three attempts' 1 ms each (RFC 20 §10.1).
func TestAHeldRequestObservesEachStageOnceItsAttemptsSummed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk, m := newManualClock(), metrics.New("v", "c")
		s := newHeld(t, 1<<20, adapter.Deps{Metrics: m}, clk)
		before, _ := scrape(t, m)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		clk.step = time.Millisecond
		s.serve(s.heldChain(1, "f")...)
		s.interimOf(2)
		clk.Advance(100 * time.Millisecond)
		synctest.Wait()
		clk.Advance(100 * time.Millisecond)
		if r := s.finalOf(2); r.Status != testproto.StatusOK {
			t.Fatalf("the held Create's third attempt is answered %+v, want success", r)
		}
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, _ := scrape(t, m)
		c0, s0 := wireSeconds(before)
		c1, s1 := wireSeconds(after)
		for _, st := range []string{"reply_wait", "resolve_ids"} {
			if n, sum := c1[st]-c0[st], s1[st]-s0[st]; n != 2 || math.Abs(sum-0.004) > 1e-9 {
				t.Errorf("%s observed %v times, %v s; want 2, 0.004 s", st, n, sum)
			}
		}
	})
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

// A cancel that comes while a re-drive waits for its reply buffer is
// remembered, and ends the hold once that attempt, answered ErrDelay again,
// holds: its final reply is the cancel status, no re-drive is left and no
// file is made (§4.7).
func TestACancelDuringAReDriveEndsItWhenItHoldsAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, smallCharge, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.heldChain(1, "f")...)
		s.recvOne() // the reply pool's one buffer splits the PutFH's reply off
		if r := s.recvOne(); !r.Interim || r.ID != keyOf(2) {
			t.Fatalf("the Create is answered %+v, want its interim", r)
		}
		s.serve(echoIn(3, 7)) // its reply, unread, holds the one buffer
		redrive := start(func() { clk.Advance(10 * time.Millisecond) })
		synctest.Wait()
		s.serve(cancelOf(2, 7))
		synctest.Wait()
		if r := s.recvOne(); r.ID != 3 {
			t.Fatalf("read %+v, want the Echo's reply", r)
		}
		final := s.recvLater()
		if !final.arrived() {
			t.Fatalf("the hold is not answered once its re-drive holds again, %d timers armed", clk.pending())
		}
		<-redrive.done
		if final.err != nil || len(final.rs) != 1 || final.rs[0].Interim || final.rs[0].ID != keyOf(2) || final.rs[0].Status != testproto.StatusCancelled {
			t.Errorf("the hold is answered %+v (%v), want its final cancel status", final.rs, final.err)
		}
		if n, files := clk.pending(), s.creates(); n != 0 || files != 0 {
			t.Errorf("%d timers left armed and %d files made, want none", n, files)
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

// pendChain is a version 2 chain of three under session 7 and tree 1: a PutFH
// of the root's handle, a related Create of "f", message id+1, with the pend
// flag where pend, and an unrelated Echo, message id+2.
func (s *served) pendChain(id uint64, pend bool) []testproto.Msg {
	msgs := append(s.heldChain(id, "f"), echoIn(id+2, 7))
	msgs[1].Pend = pend
	return msgs
}

// A chain's member before the last that the service answers ErrDelay goes
// pending where Pends says so: the PutFH's reply and its interim go out as
// one record, the Echo after it runs only once the Create is answered, and
// the re-drive's success goes out with the Echo's reply (§4.7).
func TestAPendingMemberRunsTheMembersAfterItOnItsAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.pendChain(1, true)...)
		s.interimOf(2)
		next := s.recvLater()
		if next.arrived() {
			t.Fatalf("the chain sent %+v before its Create was answered", next.rs)
		}
		clk.Advance(10 * time.Millisecond)
		<-next.done
		if next.err != nil || len(next.rs) != 2 || next.rs[0].Interim || next.rs[0].ID != keyOf(2) || next.rs[0].Status != testproto.StatusOK || next.rs[1].ID != 3 || next.rs[1].Status != testproto.StatusOK {
			t.Errorf("the re-drive is answered %+v (%v), want the Create's final success and the Echo's reply in one record", next.rs, next.err)
		}
		if n := s.creates(); n != 1 {
			t.Errorf("%d files made, want 1", n)
		}
	})
}

// The credits of the PutFH, answered before the pending Create, are granted
// in full in its interim's record, which grants the Create's in full too; its
// final reply then grants none, and the Echo's reply its own (§4.6, §4.7).
func TestTheCreditsBeforeAPendingMemberRideOnItsInterim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		msgs := s.pendChain(1, true)
		msgs[0].Credits, msgs[1].Credits = 2, 2
		before := s.client.Balance()
		s.serve(msgs...)
		s.interimOf(2)
		if got := s.client.Balance() - before; got != 4 {
			t.Errorf("the PutFH's reply and the interim granted %d credits, want two each", got)
		}
		clk.Advance(10 * time.Millisecond)
		if _, err := s.client.Recv(); err != nil {
			t.Fatal(err)
		}
		if got := s.client.Balance() - before; got != 5 {
			t.Errorf("the chain granted %d credits, want five: none more for the Create, one for the Echo", got)
		}
	})
}

// Without the pend flag, a chain's member before the last that the service
// answers ErrDelay is answered the not-last status at once, with no interim,
// since the delay row's status would speak for a hold that does not happen,
// and the Echo after it runs; its span's result is the delay the service
// returned (§4.7, §10.2).
func TestAMemberThatDoesNotPendIsAnsweredNotLast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		s := newHeld(t, 1<<20, adapter.Deps{Tracer: tracer, SampleRatio: 1}, newManualClock())
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(s.pendChain(1, false)...)
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 3 || rs[1].Interim || rs[1].ID != 2 || rs[1].Status != testproto.StatusNotLast || rs[2].ID != 3 || rs[2].Status != testproto.StatusOK {
			t.Errorf("the chain is answered %+v (%v), want the Create's not-last status at once and the Echo's success", rs, err)
		}
		synctest.Wait()
		var result string
		for _, kv := range requestSpan(t, spans, "Create").Attributes() {
			if kv.Key == "result" {
				result = kv.Value.AsString()
			}
		}
		if result != "delay" {
			t.Errorf("the Create's span carries result %q, want the service's delay", result)
		}
	})
}

// A chain's member before the last still in the service a second after it
// reached it goes pending without the pend flag, since its call may still
// apply: its interim goes out with the PutFH's reply, and its final success
// with the Echo's (§4.6, §4.7).
func TestAMemberInTheServicePendsWithoutTheFlag(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.pendChain(1, false)...)
		<-hold.Reached()
		clk.Advance(time.Second)
		s.interimOf(2)
		hold.Release()
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Interim || rs[0].ID != keyOf(2) || rs[0].Status != testproto.StatusOK || rs[1].ID != 3 || rs[1].Status != testproto.StatusOK {
			t.Errorf("the Create's call is answered %+v (%v), want its final success and the Echo's reply in one record", rs, err)
		}
		if n := s.creates(); n != 1 {
			t.Errorf("%d files made, want 1", n)
		}
	})
}

// With the reply pool full when the interim timer fires, a chain's Create
// still in the service gets no interim, not even once the pool has room, and
// goes on unannounced: answered success, its reply carries its own header,
// not Final's; answered ErrDelay, it is asked Pends as any member is and,
// without the pend flag, answered the not-last status at once. Either way
// the Echo after it runs and the chain goes out as one record (§4.6, §4.7).
func TestWithTheReplyPoolFullAtTheInterval(t *testing.T) {
	for _, c := range []struct {
		name string
		fail bool
		want core.Status
	}{
		{"the call answered success", false, testproto.StatusOK},
		{"the call answered ErrDelay", true, testproto.StatusNotLast},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The Create waits in the service on the outer fault's hold
				// and, let through, is answered by the inner one.
				svc := memory.New(t)
				root, _, err := svc.Root(t.Context(), svc.Admin(), vfs.ShareRef{Name: svc.Store.Share.Name})
				if err != nil {
					t.Fatal(err)
				}
				inner := vfstest.Faults(svc)
				outer := vfstest.Faults(inner)
				d := adapter.Deps{VFS: outer}
				s := &served{seam: newSeamWith(t, testproto.New(d).WithCall(svc.Admin()), 1<<20, d), svc: svc, faults: outer, root: root}
				s.setUp(7)
				synctest.Wait() // the last set-up reply's buffer is given back after its send
				clk := newManualClock()
				core.SetClock(s.sh, clk)
				if c.fail {
					inner.Fail(vfs.OpCreate, vfs.ErrDelay)
				}
				hold := outer.Hold(vfs.OpCreate)
				defer hold.Release()
				s.serve(s.pendChain(1, false)...)
				<-hold.Reached()
				synctest.Wait()
				full, err := s.budgets.Reply.Reserve(context.Background(), peer, (1<<20)-s.budgets.Reply.Reserved())
				if err != nil {
					t.Fatal(err)
				}
				next := s.recvLater()
				fired := start(func() { clk.Advance(time.Second) })
				synctest.Wait()
				full.Release()
				<-fired.done
				clk.Advance(time.Second)
				if next.arrived() {
					t.Fatalf("with no reply buffer at the interval the chain got %+v, want no interim", next.rs)
				}
				hold.Release()
				if !next.arrived() {
					t.Fatal("the chain is unanswered once the Create's call returned")
				}
				if rs := next.rs; next.err != nil || len(rs) != 3 || rs[1].Interim || rs[1].ID != 2 || rs[1].Status != c.want || rs[2].ID != 3 || rs[2].Status != testproto.StatusOK {
					t.Errorf("the chain is answered %+v (%v), want the Create's status %d under its own header and the Echo's success", rs, next.err, c.want)
				}
			})
		})
	}
}

// A chain whose members hold in turn, past a floor of none, moves its record's
// charge to the hold pool for each: the Create the interim timer finds in the
// service pends and is answered, and the related Create after it holds on
// ErrDelay. Once the chain ends, the hold pool's gauge and its budget read as
// before the chain (§4.6, §4.7; RFC 24 §4.3).
func TestAChainWhoseMembersHoldInTurnGivesTheHoldPoolBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		budgets, m := holdBudgets(64<<20), metrics.New("v", "c")
		s := newHeld(t, 0, adapter.Deps{Budgets: budgets, Metrics: m}, clk)
		core.SetFloor(s.sh, 0, 0)
		gauge, reserved := gauges(t, m)[2], budgets.Hold.Reserved()
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.faults.Fail(vfs.OpCreate, vfs.ErrDelay)
		s.serve(append(s.heldChain(1, "f"), testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 3, Session: inherited, Tree: 1, Name: "g", Related: true})...)
		<-hold.Reached()
		clk.Advance(time.Second)
		s.interimOf(2)
		hold.Release()
		rs, err := s.client.Recv()
		if err != nil || len(rs) != 2 || rs[0].Interim || rs[0].ID != keyOf(2) || rs[0].Status != testproto.StatusOK || !rs[1].Interim || rs[1].ID != keyOf(3) {
			t.Fatalf("the first Create is answered %+v (%v), want its final success and the second's interim", rs, err)
		}
		synctest.Wait() // the second Create's re-drive is armed after its interim goes out
		if got := budgets.Hold.Reserved(); got <= reserved {
			t.Fatalf("the second Create holds with %d bytes in the hold pool, want its record's charge there", got)
		}
		clk.Advance(10 * time.Millisecond)
		if r := s.finalOf(3); r.Status != testproto.StatusOK {
			t.Fatalf("the second Create's final reply is %+v, want success", r)
		}
		synctest.Wait()
		if got, rsv := gauges(t, m)[2], budgets.Hold.Reserved(); got != gauge || rsv != reserved {
			t.Errorf("once the chain ends the hold pool's gauge reads %v and its budget %d, want %v and %d as before it", got, rsv, gauge, reserved)
		}
	})
}

// parkChain is a version 2 chain under session 7 and tree 1: a PutFH of the
// root's handle, then a related Park of name, whose message ID is id+1.
func (s *served) parkChain(id uint64, name string) []testproto.Msg {
	msgs := s.heldChain(id, name)
	msgs[1].Op = testproto.OpPark
	return msgs
}

// parkWait holds a park's wait, its second read of the current filehandle:
// the first, made before Park, passes.
func (s *served) parkWait() *vfstest.Hold {
	s.faults.Hold(vfs.OpGetAttr).Release()
	return s.faults.Hold(vfs.OpGetAttr)
}

// exists says whether the root holds a file of name.
func (s *served) exists(name string) bool {
	_, _, err := s.svc.Lookup(context.Background(), s.svc.Admin(), s.root, []byte(name))
	return err == nil
}

// A parked request gets its interim at once; while it waits the receive
// budget holds nothing and the reply budget one small buffer; released, it
// is answered under its key, with its name, and its file is made (§4.7;
// RFC 24 §3.3).
func TestAParkedRequestKeepsOneSmallReplyAndNoRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		synctest.Wait()
		if recv, reply := s.budgets.Receive.Reserved(), s.budgets.Reply.Reserved(); recv != 0 || reply != smallCharge {
			t.Errorf("while parked the receive budget holds %d and the reply budget %d, want 0 and one small buffer, %d", recv, reply, smallCharge)
		}
		hold.Release()
		if r := s.finalOf(2); r.Status != testproto.StatusOK || string(r.Body) != "f" || !s.exists("f") {
			t.Errorf("released, the park is answered %+v, file made %v; want success with its name, and the file", r, s.exists("f"))
		}
	})
}

// A park has no deadline: left while the clock passes 30 s and the hold's
// bound it is still parked, with no second interim, and is answered when
// released (§4.7, §6.4).
func TestAParkOutlivesTheDeadlineAndTheHoldsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		next := s.recvLater()
		clk.Advance(30*time.Second + testproto.HoldFor)
		if next.arrived() {
			t.Fatalf("the park got %+v past 30 s and the hold's bound, want nothing", next.rs)
		}
		hold.Release()
		<-next.done
		if next.err != nil || len(next.rs) != 1 || next.rs[0].Interim || next.rs[0].ID != keyOf(2) || next.rs[0].Status != testproto.StatusOK {
			t.Errorf("released, the park is answered %+v (%v), want its final success", next.rs, next.err)
		}
	})
}

// A parked request's map_error and encode are timed from done: the 30 s it
// waits parked lands in no stage, and its answer observes map_error and
// encode once each (RFC 20 §10.1).
func TestAParksWaitIsNoStages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk, m := newManualClock(), metrics.New("v", "c")
		s := newHeld(t, 1<<20, adapter.Deps{Metrics: m}, clk)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		next := s.recvLater()
		synctest.Wait()
		before, _ := scrape(t, m)
		wasN, wasS := wireSeconds(before)
		clk.Advance(30 * time.Second)
		hold.Release()
		<-next.done
		if next.err != nil || len(next.rs) != 1 || next.rs[0].Status != testproto.StatusOK {
			t.Fatalf("the park is answered %+v (%v), want its final success", next.rs, next.err)
		}
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		after, _ := scrape(t, m)
		counts, sums := wireSeconds(after)
		for _, st := range []string{"map_error", "encode"} {
			if got := counts[st] - wasN[st]; got != 1 {
				t.Errorf("%s observed %v times at the park's answer, want once", st, got)
			}
		}
		for st, v := range sums {
			if got := v - wasS[st]; got != 0 {
				t.Errorf("%s gained %v s over the park's wait, want none", st, got)
			}
		}
	})
}

// A park whose name does not fit the small class keeps a small buffer, not
// its own, while it waits, and is answered the overflow status alone (§4.7;
// RFC 24 §3.3).
func TestALongParkedNameIsAnsweredTheOverflow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		name := strings.Repeat("n", 5000)
		s.serve(s.parkChain(1, name)...)
		s.interimOf(2)
		synctest.Wait()
		if reply := s.budgets.Reply.Reserved(); reply != smallCharge {
			t.Errorf("while parked the reply budget holds %d, want one small buffer, %d", reply, smallCharge)
		}
		hold.Release()
		if r := s.finalOf(2); r.Status != testproto.StatusOverflow || len(r.Body) != 0 {
			t.Errorf("released, the park is answered %d with %d bytes, want the overflow status alone", r.Status, len(r.Body))
		}
	})
}

// parkEight parks eight requests, each waiting in its held Create.
func (s *served) parkEight() {
	s.t.Helper()
	for i := range 8 {
		s.faults.Hold(vfs.OpCreate)
		s.serve(s.parkChain(uint64(10*i+1), "f"+strconv.Itoa(i))...)
		s.interimOf(uint64(10*i + 2))
	}
}

// bigCharge is what a reply of a directory-class buffer reserves: 64 KiB,
// its 4 KiB of header room and the per-request overhead.
const bigCharge = 76 << 10

// A park before the last member, a ninth park, a park while draining and a
// park with the reply budget full each make no call and send no interim: each
// is answered its status at once, after the PutFH's reply, the drain's
// refusal its interim alone, and nothing more is sent (§4.7). The full
// budget's park, its name past the small class, finds no room for a small
// buffer: the pool is filled to the byte by unread replies on another
// connection, at four times what the chain holds, so the chain does not split
// at a quarter of it.
func TestARefusedParkMakesNoCallAndSendsNoInterim(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply int64
		park  func(s *served) ([]testproto.Msg, func()) // set up: the chain to serve, and what frees the budget after
		want  testproto.Reply                           // the Park's reply, after the PutFH's
	}{
		{"before the last", 1 << 20, func(s *served) ([]testproto.Msg, func()) {
			return append(s.parkChain(1, "f"), echoIn(3, 7)), func() {}
		}, testproto.Reply{ID: 2, Status: testproto.StatusNotLast}},
		{"the ninth", 1 << 20, func(s *served) ([]testproto.Msg, func()) {
			s.parkEight()
			return s.parkChain(91, "g"), func() {}
		}, testproto.Reply{ID: 92, Status: testproto.StatusTooManyPending}},
		{"while draining", 1 << 20, func(s *served) ([]testproto.Msg, func()) {
			read := s.faults.Hold(vfs.OpGetAttr)
			go func() {
				<-read.Reached()
				s.p.Drain()
				read.Release()
			}()
			return s.parkChain(1, "f"), func() {}
		}, testproto.Reply{ID: keyOf(2), Interim: true}},
		{"with the reply budget full", 4 * (smallCharge + bigCharge), func(s *served) ([]testproto.Msg, func()) {
			o := s.another(netip.MustParseAddr("192.0.2.2"), 8)
			n := 3 * (smallCharge + bigCharge) / smallCharge
			for i := range n {
				o.serve(echoIn(uint64(100+i), 8))
			}
			synctest.Wait()
			return s.parkChain(1, strings.Repeat("n", 5000)), func() {
				for range n {
					o.recvOne()
				}
			}
		}, testproto.Reply{ID: 2, Status: testproto.StatusDelay}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newHeld(t, c.reply, adapter.Deps{}, newManualClock())
				msgs, free := c.park(s)
				s.serve(msgs...)
				next := s.recvLater()
				if !next.arrived() {
					t.Fatal("the refused park is not answered at once")
				}
				if next.err != nil || len(next.rs) < 2 || next.rs[0].Status != testproto.StatusOK {
					t.Fatalf("the chain is answered %+v (%v), want the PutFH's success first", next.rs, next.err)
				}
				if r := next.rs[1]; r.ID != c.want.ID || r.Interim != c.want.Interim || r.Status != c.want.Status {
					t.Errorf("the Park is answered %+v, want %+v", r, c.want)
				}
				if ops := s.svc.Ops(); len(ops) != 0 {
					t.Errorf("the refused park made %v, want no call", ops)
				}
				free()
				if !s.sentNothing() {
					t.Error("more was sent after the refusal")
				}
			})
		})
	}
}

// Eight parks are not held requests: the held gauges read nothing (§4.6).
func TestEightParksAreNotCountedAsHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newHeld(t, 1<<20, adapter.Deps{Metrics: m}, newManualClock())
		s.parkEight()
		if got := gauges(t, m); got != [4]float64{} {
			t.Errorf("with eight parked, held, their bytes, the pool's and the refusals read %v, want zero", got)
		}
	})
}

// A park its client cancels during its wait is answered the cancel status,
// the error done reports replaced, and makes no file; the cancel gets no
// reply of its own (§4.7, §6.5).
func TestACancelledParkIsAnsweredTheCancelStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		wait := s.parkWait()
		defer wait.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		<-wait.Reached()
		s.serve(cancelOf(2, 7))
		if r := s.finalOf(2); r.Status != testproto.StatusCancelled || s.exists("f") {
			t.Errorf("the cancelled park is answered %+v, file made %v; want the cancel status and no file", r, s.exists("f"))
		}
		if !s.sentNothing() {
			t.Error("the cancel got a reply of its own")
		}
	})
}

// A park whose call the service applies as its client cancels it, done
// reporting no error, is answered success, and its file exists: a call that
// took effect is never reported cancelled (§6.5).
func TestAParkCancelledAsItsCallCompletesIsAnsweredSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		<-hold.Reached()
		s.serve(cancelOf(2, 7))
		if r := s.finalOf(2); r.Status != testproto.StatusOK || !s.exists("f") {
			t.Errorf("the park is answered %+v, file made %v; want success and the file", r, s.exists("f"))
		}
	})
}

// A cancel that comes before its target parks is remembered, and ends the
// park as cancelled: its handler's cancel is called as it parks, and no file
// is made (§4.7).
func TestACancelBeforeItsTargetParksEndsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		s.serve(cancelOf(2, 7))
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		if r := s.finalOf(2); r.Status != testproto.StatusCancelled || s.exists("f") {
			t.Errorf("the park is answered %+v, file made %v; want the cancel status and no file", r, s.exists("f"))
		}
	})
}

// The start of a drain cancels a park during its wait, so it does not wait
// for the drain's deadline: the drain's error, whose row answers nothing,
// replaces the error done reports, nothing more is sent, no file is made and
// the park holds nothing (§3.1, §4.7).
func TestADrainCancelsAPark(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		wait := s.parkWait()
		defer wait.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		<-wait.Reached()
		s.p.Drain()
		synctest.Wait()
		if n, reply := s.p.Runs(), s.budgets.Reply.Reserved(); n != 0 || reply != 0 {
			t.Errorf("once the drain began %d run and the reply budget holds %d, want the park ended", n, reply)
		}
		if s.exists("f") || !s.sentNothing() {
			t.Errorf("the drained park made its file (%v) or was answered", s.exists("f"))
		}
	})
}

// A park whose call the service applies as the drain cancels it, done
// reporting no error, is answered success, and its file exists (§3.1, §6.5).
func TestAParkDrainedAsItsCallCompletesIsAnsweredSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		<-hold.Reached()
		s.p.Drain()
		if r := s.finalOf(2); r.Status != testproto.StatusOK || !s.exists("f") {
			t.Errorf("the park is answered %+v, file made %v; want success and the file", r, s.exists("f"))
		}
	})
}

// close ends a park at once: close returns while the park waits, its
// handler's cancel is called, nothing is sent, its request span ends unsent,
// no error is counted as one no row maps, and every budget is back at zero
// once wait returns (§4.7, §6.5, §10.1).
func TestCloseEndsAPark(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		spans := tracetest.NewSpanRecorder()
		tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test")
		s := newHeld(t, 1<<20, adapter.Deps{Metrics: m, Tracer: tracer, SampleRatio: 1}, newManualClock())
		wait := s.parkWait()
		defer wait.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		<-wait.Reached()
		closing := start(s.p.Close)
		synctest.Wait()
		if !closing.returned() {
			t.Fatal("close waited for the park it ends")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.p.Wait(ctx); err != nil {
			t.Fatalf("the park outlived its connection: %v", err)
		}
		if recv, reply := s.budgets.Receive.Reserved(), s.budgets.Reply.Reserved(); recv != 0 || reply != 0 {
			t.Errorf("after the close the receive budget holds %d and the reply budget %d, want 0", recv, reply)
		}
		if sd := requestSpan(t, spans, "Park"); slices.ContainsFunc(sd.Attributes(), func(kv attribute.KeyValue) bool { return kv.Key == "reply_bytes" }) {
			t.Errorf("the closed park's span ended as sent: %v", sd.Attributes())
		}
		got, _ := scrape(t, m)
		if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 0 {
			t.Errorf("%s = %v, want 0", k, got[k])
		}
		if s.exists("f") || !s.sentNothing() {
			t.Errorf("the closed park made its file (%v) or was answered", s.exists("f"))
		}
	})
}

// A request whose read before Park is in the service as its connection closes
// parks on the closed connection and is dropped as a close drops a park:
// Park calls its cancel at once, so no file is made, nothing is sent, no error
// is counted as one no row maps, and every budget is back at zero once wait
// returns (§4.7, §10.1).
func TestAParkOnAClosedConnectionIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newHeld(t, 1<<20, adapter.Deps{Metrics: m}, newManualClock())
		read := s.faults.Hold(vfs.OpGetAttr)
		defer read.Release()
		s.serve(s.parkChain(1, "f")...)
		<-read.Reached()
		s.p.Close()
		read.Release()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.p.Wait(ctx); err != nil {
			t.Fatalf("the park outlived its connection: %v", err)
		}
		synctest.Wait()
		got, _ := scrape(t, m)
		if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 0 {
			t.Errorf("%s = %v, want 0", k, got[k])
		}
		if recv, reply := s.budgets.Receive.Reserved(), s.budgets.Reply.Reserved(); recv != 0 || reply != 0 {
			t.Errorf("after the close the receive budget holds %d and the reply budget %d, want 0", recv, reply)
		}
		if s.exists("f") || !s.sentNothing() {
			t.Errorf("the park on a closed connection made its file (%v) or was answered", s.exists("f"))
		}
	})
}

// Core calls a park's cancel once: a client's cancel, its withdrawal held in
// the service, then a drain or a close, leaves the service one CancelLock and
// no Create (§3.1, §4.7, §6.5).
func TestAParksCancelIsCalledOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		then func(*served)
	}{
		{"then a drain", func(s *served) { s.p.Drain() }},
		{"then a close", func(s *served) { s.p.Close() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newHeld(t, 1<<20, adapter.Deps{}, newManualClock())
				wait := s.parkWait()
				defer wait.Release()
				withdraw := s.faults.Hold(vfs.OpCancelLock)
				defer withdraw.Release()
				s.serve(s.parkChain(1, "f")...)
				s.interimOf(2)
				<-wait.Reached()
				s.serve(cancelOf(2, 7))
				<-withdraw.Reached()
				c.then(s)
				synctest.Wait()
				withdraw.Release()
				synctest.Wait()
				if ops := s.svc.Ops(); !slices.Equal(ops, []vfs.Op{vfs.OpCancelLock}) {
					t.Errorf("the service saw %v, want one CancelLock", ops)
				}
			})
		})
	}
}

// A request held on its read, then parked on its re-drive, has received one
// interim: its slot moves from held to parked, its record goes, and its
// final reply comes when released (§4.6, §4.7).
func TestARequestHeldThenParkedHasOneInterim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk)
		s.faults.Fail(vfs.OpGetAttr, vfs.ErrDelay)
		hold := s.faults.Hold(vfs.OpCreate)
		defer hold.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		next := s.recvLater()
		clk.Advance(10 * time.Millisecond)
		<-hold.Reached()
		if next.arrived() {
			t.Fatalf("the re-drive that parked sent %+v, want nothing", next.rs)
		}
		if recv, reply := s.budgets.Receive.Reserved(), s.budgets.Reply.Reserved(); recv != 0 || reply != smallCharge {
			t.Errorf("parked on its re-drive the receive budget holds %d and the reply budget %d, want 0 and one small buffer", recv, reply)
		}
		hold.Release()
		<-next.done
		if next.err != nil || len(next.rs) != 1 || next.rs[0].Interim || next.rs[0].ID != keyOf(2) || next.rs[0].Status != testproto.StatusOK || string(next.rs[0].Body) != "f" {
			t.Errorf("released, the park is answered %+v (%v), want its final success with its name", next.rs, next.err)
		}
	})
}

// A request held at MaxPending of one, re-driven while a call the interim
// timer found takes the connection past it, parks: its slot moves from held
// to parked, so it is not refused, and it is answered when its wait ends
// (§4.6, §4.7).
func TestAHeldRequestParksPastMaxPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clk := newManualClock()
		s := newHeld(t, 1<<20, adapter.Deps{}, clk, func(st *core.Stages) { st.MaxPending = 1 })
		s.faults.Fail(vfs.OpGetAttr, vfs.ErrDelay)
		read := s.faults.Hold(vfs.OpGetAttr)
		defer read.Release()
		create := s.faults.Hold(vfs.OpCreate)
		defer create.Release()
		s.serve(s.parkChain(1, "f")...)
		s.interimOf(2)
		redrive := start(func() { clk.Advance(10 * time.Millisecond) })
		<-read.Reached()
		s.serve(s.heldChain(11, "g")...)
		<-create.Reached()
		clk.Advance(time.Second)
		s.interimOf(12)
		read.Release()
		<-redrive.done
		if r := s.finalOf(2); r.Status != testproto.StatusOK || string(r.Body) != "f" {
			t.Errorf("the held request that parked is answered %+v, want its final success with its name", r)
		}
		create.Release()
		s.finalOf(12)
	})
}

// A park whose own reply buffer is of the small class keeps it, and is not
// refused while another request waits on the reply pool: its interim waits
// for a status's buffer behind that request, and its final reply goes out in
// the buffer it kept (§4.7; RFC 24 §3.3).
func TestASmallParkKeepsItsReplyWhileARequestWaitsOnThePool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newHeld(t, 3*smallCharge, adapter.Deps{}, newManualClock())
		o := s.another(netip.MustParseAddr("192.0.2.2"), 8)
		o.serve(echoIn(100, 8)) // its reply, unread, holds one buffer
		synctest.Wait()
		read := s.faults.Hold(vfs.OpGetAttr)
		s.serve(s.parkChain(1, "f")...)
		<-read.Reached() // the PutFH's reply, unread, and the Park's own hold the other two
		o.serve(echoIn(101, 8))
		synctest.Wait() // the second Echo waits on the pool
		read.Release()
		synctest.Wait() // the Park has parked, its interim waiting behind the Echo
		if r := s.recvOne(); r.ID != 1 || r.Status != testproto.StatusOK {
			t.Fatalf("read %+v, want the PutFH's success", r)
		}
		if r := o.recvOne(); r.ID != 100 {
			t.Fatalf("read %+v, want the first Echo's reply", r)
		}
		if r := s.recvOne(); !r.Interim || r.ID != keyOf(2) {
			t.Fatalf("the park is answered %+v, want its interim", r)
		}
		if r := s.finalOf(2); r.Status != testproto.StatusOK || string(r.Body) != "f" {
			t.Errorf("the park's final reply is %+v, want success with its name", r)
		}
		if r := o.recvOne(); r.ID != 101 || r.Status != testproto.StatusOK {
			t.Errorf("the waiting Echo is answered %+v, want its success", r)
		}
		synctest.Wait() // each reply's buffer is given back after its send
	})
}

// With the reply pool filled by two parks and an unread reply on other
// connections, a request whose reply reservation's wait ends at 30 s gets its
// delay status once one park is released and its reply read; with none
// released, its connection is closed with the reply-wait error 30 s after
// its status began to wait, and the request no longer runs (RFC 24 §3.3;
// §6.4).
func TestAStatusWaitingOnAPoolOfParks(t *testing.T) {
	for _, released := range []bool{true, false} {
		t.Run("one released "+strconv.FormatBool(released), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := newManualClock()
				s := newHeld(t, 3*smallCharge, adapter.Deps{}, clk)
				w := s.another(netip.MustParseAddr("192.0.2.2"), 8)
				fill := s.another(netip.MustParseAddr("192.0.2.3"), 9)
				var holds []*vfstest.Hold
				for i := range 2 {
					holds = append(holds, s.faults.Hold(vfs.OpCreate))
					s.serve(s.parkChain(uint64(10*i+1), "f"+strconv.Itoa(i))...)
					s.recvOne() // the PutFH's, split off at a quarter of the pool
					if r := s.recvOne(); !r.Interim {
						t.Fatalf("park %d is answered %+v, want its interim", i, r)
					}
					// Holds pair with calls in their arrival order: park i's
					// Create takes holds[i] before the next park's is armed.
					<-holds[i].Reached()
				}
				defer func() {
					for _, h := range holds {
						h.Release()
					}
				}()
				fill.serve(echoIn(300, 9)) // its reply, unread, holds the last buffer
				synctest.Wait()
				w.serve(echoIn(200, 8))
				synctest.Wait()
				clk.Advance(30 * time.Second)
				synctest.Wait()
				begin := time.Now()
				if released {
					holds[0].Release()
					if r := s.finalOf(2); r.Status != testproto.StatusOK {
						t.Fatalf("the released park is answered %+v, want success", r)
					}
					if r := w.recvOne(); r.ID != 200 || r.Status != testproto.StatusDelay {
						t.Errorf("the request is answered %+v, want its delay status", r)
					}
				} else {
					select {
					case err := <-w.closed:
						if !errors.Is(err, core.ErrReplyWait) {
							t.Errorf("the connection was closed with %v, want the reply-wait error", err)
						}
					case <-time.After(time.Minute):
						t.Fatal("the status still waits a minute on, want its connection closed at 30 s")
					}
					if waited := time.Since(begin); waited != 30*time.Second {
						t.Errorf("the connection was closed %v after its status began to wait, want 30s", waited)
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					if err := w.p.Wait(ctx); err != nil {
						t.Errorf("the request still runs after its connection closed: %v", err)
					}
				}
				s.p.Close()
				fill.recvOne()
				synctest.Wait()
			})
		})
	}
}
