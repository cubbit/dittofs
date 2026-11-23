package identity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// coldPassThrough is a fixture inside a synctest bubble whose alice is in
// groups directory groups, every directory op matching op delayed by delay,
// and the domain controller faulted by rules.
func coldPassThrough(t *testing.T, groups int, op string, delay time.Duration, rules ...sim.Rule) *ptFixture {
	t.Helper()
	fx := newPassThroughUnwatched(t, rules...)
	for i := range groups {
		fx.dir.AddGroup(t, fmt.Sprintf("g%03d", i), map[string][][]byte{"member": attr(fx.userDN)})
	}
	s, err := sim.NewSchedule(1, fx.clock, sim.Rule{Op: op, Key: "*", Action: sim.FaultAction{Delay: delay}})
	if err != nil {
		t.Fatal(err)
	}
	fx.dirf.cur.Store(s)
	return fx
}

// timedSignIn signs alice in, advancing the clock half a second at a time once
// every goroutine of the bubble is blocked, and returns the attempt and how
// long it took on the clock. The searches and calls the attempt left behind
// are run out on the clock before it returns.
func (fx *ptFixture) timedSignIn(t *testing.T) (pid.SignInResult, string, time.Duration, error) {
	t.Helper()
	type attempt struct {
		res pid.SignInResult
		oc  string
		err error
	}
	start := fx.clock.Now()
	done := make(chan attempt, 1)
	go func() {
		res, oc, err := fx.signIn(ntlmStep(string(alicePW), "alice", "CORP", true))
		done <- attempt{res, oc, err}
	}()
	var a attempt
wait:
	for {
		synctest.Wait()
		select {
		case a = <-done:
			break wait
		default:
		}
		if fx.clock.Now().Sub(start) > time.Minute {
			t.Fatal("SignIn still running a minute in")
		}
		fx.clock.Advance(time.Second / 2)
	}
	took := fx.clock.Now().Sub(start)
	for range 60 {
		fx.clock.Advance(time.Second)
		synctest.Wait()
	}
	return a.res, a.oc, took, a.err
}

// reached passes each domain controller op to next and keeps the clock's
// time at which each op and key first reached it, before any delay of next.
type reached struct {
	clock *sim.TestClock
	next  sim.Fault
	mu    sync.Mutex
	at    map[string]time.Time
}

func (r *reached) At(op string, key []byte) (time.Duration, int, error) {
	r.mu.Lock()
	if k := op + " " + string(key); r.at[k].IsZero() {
		r.at[k] = r.clock.Now()
	}
	r.mu.Unlock()
	return r.next.At(op, key)
}

// first is when "op key" first reached the controller, and whether it did.
func (r *reached) first(opKey string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.at[opKey]
	return at, ok
}

// noneMinted fails unless no principal holds alice's SID.
func (fx *ptFixture) noneMinted(t *testing.T, what string) {
	t.Helper()
	if p, err := fx.s.Find(context.Background(), pid.Spelling{Scheme: pid.SchemeSID, SID: fx.alice}, pid.FindOpts{}); !errors.Is(err, vfserr.ErrUnknownPrincipal) {
		t.Errorf("%s: Find of alice's SID = %v, %v; want ErrUnknownPrincipal, nothing minted", what, p, err)
	}
}

// RFC 18 §5.1's bound counts a pass-through's call to the domain controller
// with its directory work. A cold sign-in of alice in 3 groups asks the
// directory five searches; each delayed 3.5 seconds, it signs in under the
// bound with the controller answering at once, and fails ErrUnavailable at
// 20 seconds on the clock, nothing minted, with the controller's answer
// delayed 4 seconds, under the call's own 5-second bound. With the
// directory's dial, bind and searches each delayed 4.5 seconds, under the
// query timeout, the call reaches the controller at 18 seconds and the bound
// ends it at 20, naming the bound; a channel set-up started then against a
// controller that never answers is ended at 20 too, and raises
// machine_account_unhealthy.
func TestPassThroughBoundCountsTheCall(t *testing.T) {
	prof.Watch(t, prof.Options{})
	late := sim.Rule{Op: "logon", Key: "alice", Action: sim.FaultAction{Delay: 4 * time.Second}}
	silent := sim.Rule{Op: "dial", Key: dc1, Action: sim.FaultAction{Err: identitytest.ErrHold}}
	for _, tc := range []struct {
		name      string
		op        string // the directory ops delayed
		delay     time.Duration
		rule      *sim.Rule // the controller's fault; nil for none
		ok        bool      // signs in, under 20s; else refused at 20s
		logonAt   time.Duration
		bound     bool // the refusal names the bound
		unhealthy bool // machine_account_unhealthy raised
	}{
		{name: "controller at once", op: "search", delay: 3500 * time.Millisecond, ok: true},
		{name: "controller 4s late", op: "search", delay: 3500 * time.Millisecond, rule: &late, logonAt: 7 * time.Second},
		{name: "call started at 18s", op: "*", delay: 4500 * time.Millisecond, rule: &late, logonAt: 18 * time.Second, bound: true},
		{name: "set-up started at 18s", op: "*", delay: 4500 * time.Millisecond, rule: &silent, bound: true, unhealthy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var rules []sim.Rule
				if tc.rule != nil {
					rules = append(rules, *tc.rule)
				}
				fx := coldPassThrough(t, 3, tc.op, tc.delay, rules...)
				start := fx.clock.Now()
				res, oc, took, err := fx.timedSignIn(t)
				if tc.ok {
					if err != nil || oc != "ok" {
						t.Fatalf("SignIn = %v, outcome %q, after %v; want ok", err, oc, took)
					}
					if took >= 20*time.Second {
						t.Errorf("SignIn took %v, want under 20s", took)
					}
				} else {
					refused(t, tc.name, res, oc, err, vfserr.ErrUnavailable, "unavailable")
					if took != 20*time.Second {
						t.Errorf("SignIn returned at %v, want 20s", took)
					}
					fx.noneMinted(t, tc.name)
				}
				if tc.logonAt != 0 {
					if at, ok := fx.reached.first("logon alice"); !ok {
						t.Errorf("the controller never saw alice's logon, want it at %v", tc.logonAt)
					} else if got := at.Sub(start); got != tc.logonAt {
						t.Errorf("the controller saw alice's logon at %v, want %v", got, tc.logonAt)
					}
				}
				if tc.bound && (err == nil || !strings.Contains(err.Error(), "20-second bound")) {
					t.Errorf("SignIn = %v, want the error to name the 20-second bound", err)
				}
				if got := unhealthy(fx.s); got != tc.unhealthy {
					t.Errorf("machine_account_unhealthy raised = %v, want %v", got, tc.unhealthy)
				}
			})
		})
	}
}

// A controller that keeps its connection up and never answers holds a
// pass-through's slot past the sign-in's answer at 5 seconds, until 30
// seconds from the call's start, the held call's ceiling, which frees it.
func TestPassThroughHeldCallCeiling(t *testing.T) {
	prof.Watch(t, prof.Options{})
	synctest.Test(t, func(t *testing.T) {
		fx := newPassThroughUnwatched(t, sim.Rule{Op: "logon", Key: "alice", Action: sim.FaultAction{Err: identitytest.ErrHold}})
		done := make(chan error, 1)
		go func() {
			_, _, err := fx.signIn(ntlmStep(string(alicePW), "alice", "CORP", true))
			done <- err
		}()
		synctest.Wait()
		fx.clock.Advance(5 * time.Second)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, vfserr.ErrUnavailable) {
				t.Fatalf("SignIn at the call's 5s bound = %v, want ErrUnavailable", err)
			}
		default:
			t.Fatal("SignIn still running at the call's 5s bound")
		}
		fx.clock.Advance(24*time.Second + 500*time.Millisecond)
		synctest.Wait()
		if h, n := fx.f.Holding(), fx.s.PassThroughs(); h != 1 || n != 1 {
			t.Errorf("at 29.5s: %d calls held, %d slots taken; want 1 and 1, the call kept past its bound", h, n)
		}
		fx.clock.Advance(time.Second / 2)
		synctest.Wait()
		if h, n := fx.f.Holding(), fx.s.PassThroughs(); h != 0 || n != 0 {
			t.Errorf("at the 30s ceiling: %d calls held, %d slots taken; want 0 and 0", h, n)
		}
	})
}

// RFC 18 §10 "directory deadline"'s sign-in clause: a cold pass-through
// sign-in of alice in 300 groups, every directory search delayed 4 seconds
// and the controller answering at once, fails ErrUnavailable at 20 seconds
// on the clock, and nothing is minted.
func TestPassThroughBoundCountsTheDirectory(t *testing.T) {
	prof.Watch(t, prof.Options{})
	synctest.Test(t, func(t *testing.T) {
		fx := coldPassThrough(t, 300, "search", 4500*time.Millisecond)
		res, oc, took, err := fx.timedSignIn(t)
		refused(t, "300 groups", res, oc, err, vfserr.ErrUnavailable, "unavailable")
		if took != 20*time.Second {
			t.Errorf("300 groups: SignIn returned at %v, want 20s", took)
		}
		fx.noneMinted(t, "300 groups")
	})
}
