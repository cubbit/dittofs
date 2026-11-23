package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	pid "github.com/cubbit/dittofs/pkg/identity"
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
func (fx *ptFixture) timedSignIn(t *testing.T) (pid.SignInResult, string, error, time.Duration) {
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
	return a.res, a.oc, a.err, took
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
// query timeout, the call starts at 18 seconds and the bound ends it at 20.
func TestPassThroughBoundCountsTheCall(t *testing.T) {
	prof.Watch(t, prof.Options{})
	synctest.Test(t, func(t *testing.T) {
		fx := coldPassThrough(t, 3, "search", 3500*time.Millisecond)
		_, oc, err, took := fx.timedSignIn(t)
		if err != nil || oc != "ok" {
			t.Fatalf("controller at once: SignIn = %v, outcome %q, after %v; want ok", err, oc, took)
		}
		if took >= 20*time.Second {
			t.Errorf("controller at once: SignIn took %v, want under 20s", took)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		fx := coldPassThrough(t, 3, "search", 3500*time.Millisecond, sim.Rule{Op: "logon", Key: "alice", Action: sim.FaultAction{Delay: 4 * time.Second}})
		res, oc, err, took := fx.timedSignIn(t)
		refused(t, "controller 4s late", res, oc, err, vfserr.ErrUnavailable, "unavailable")
		if took != 20*time.Second {
			t.Errorf("controller 4s late: SignIn returned at %v, want 20s", took)
		}
		fx.noneMinted(t, "controller 4s late")
	})
	synctest.Test(t, func(t *testing.T) {
		fx := coldPassThrough(t, 3, "*", 4500*time.Millisecond, sim.Rule{Op: "logon", Key: "alice", Action: sim.FaultAction{Delay: 4 * time.Second}})
		res, oc, err, took := fx.timedSignIn(t)
		refused(t, "call started at 18s", res, oc, err, vfserr.ErrUnavailable, "unavailable")
		if took != 20*time.Second {
			t.Errorf("call started at 18s: SignIn returned at %v, want 20s", took)
		}
		fx.noneMinted(t, "call started at 18s")
	})
}

// RFC 18 §10 "directory deadline"'s sign-in clause: a cold pass-through
// sign-in of alice in 300 groups, every directory search delayed 4 seconds
// and the controller answering at once, fails ErrUnavailable at 20 seconds
// on the clock, and nothing is minted.
func TestPassThroughBoundCountsTheDirectory(t *testing.T) {
	prof.Watch(t, prof.Options{})
	synctest.Test(t, func(t *testing.T) {
		fx := coldPassThrough(t, 300, "search", 4*time.Second)
		res, oc, err, took := fx.timedSignIn(t)
		refused(t, "300 groups", res, oc, err, vfserr.ErrUnavailable, "unavailable")
		if took != 20*time.Second {
			t.Errorf("300 groups: SignIn returned at %v, want 20s", took)
		}
		fx.noneMinted(t, "300 groups")
	})
}
