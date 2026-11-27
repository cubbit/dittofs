package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/health"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const (
	dc1, dc2 = "dc1:135", "dc2:135"
	pw2      = "Nas-Passw0rd-2"
	pw3      = "Nas-Passw0rd-3"
	pw4      = "Nas-Passw0rd-4"
)

var alicePW = []byte("Alice-Passw0rd")

// chanFixture is a server over a domain of two controllers, dc1 and dc2,
// holding the computer account NAS with pw2 and the user alice, reached
// through the fault wrapper, on a clock at mt0.
type chanFixture struct {
	s     *identity.Server
	d     *identitytest.DomainController
	f     *identitytest.FaultyDomainController
	clock *sim.TestClock
}

func newChannelServer(t *testing.T, rules ...sim.Rule) *chanFixture {
	t.Helper()
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(mt0)
	d := identitytest.NewDomainController("CORP", machineRealm, "DC=corp,DC=example", dc1, dc2)
	d.StageComputer(t, identitytest.StagedComputer{Name: "NAS", Password: []byte(pw2), EncTypes: 0x18})
	d.AddUser(t, "alice", alicePW)
	sched, err := sim.NewSchedule(1, clock, rules...)
	if err != nil {
		t.Fatal(err)
	}
	f := identitytest.NewFaultyDomainController(d, sched)
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: clock, DomainController: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return &chanFixture{s: s, d: d, f: f, clock: clock}
}

// apply applies a joined State naming the controller addr, "" for none,
// confirming version confirmed at mt0.
func (fx *chanFixture) apply(t *testing.T, addr string, confirmed uint64, versions ...secrets.Version) {
	t.Helper()
	st := joinedState(confirmed, versions...)
	st.Directory.DCAddress = addr
	if err := fx.s.Apply(st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// logon passes user's response under password through the node's channel.
func (fx *chanFixture) logon(ctx context.Context, user string, password []byte) error {
	challenge, client := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, [8]byte{8, 7, 6, 5, 4, 3, 2, 1}
	nt, lm, _ := identitytest.NTLMv2Response(password, user, "CORP", challenge, client)
	_, err := fx.s.Logon(ctx, netlogon.LogonRequest{Domain: "CORP", User: user, Workstation: "WS1",
		Challenge: challenge, NTResponse: nt, LMResponse: lm})
	return err
}

// signIn wants alice's logon answered and the channel set up to addr with
// version.
func (fx *chanFixture) signIn(t *testing.T, addr string, version uint64) {
	t.Helper()
	if err := fx.logon(context.Background(), "alice", alicePW); err != nil {
		t.Fatalf("alice's logon: %v, want answered", err)
	}
	if a, v, ok := fx.s.ChannelAt(); !ok || a != addr || v != version {
		t.Errorf("channel at %q with version %d (set up %v), want %q with version %d", a, v, ok, addr, version)
	}
}

// noSignIn wants alice's logon to answer ErrUnavailable with no channel set
// up, never ErrRejected, which would read as alice's password refused.
func (fx *chanFixture) noSignIn(t *testing.T) {
	t.Helper()
	if err := fx.logon(context.Background(), "alice", alicePW); !errors.Is(err, vfserr.ErrUnavailable) || errors.Is(err, netlogon.ErrRejected) {
		t.Errorf("alice's logon: %v, want ErrUnavailable alone, not ErrRejected", err)
	}
	if a, v, ok := fx.s.ChannelAt(); ok {
		t.Errorf("channel at %q with version %d, want none", a, v)
	}
}

func unhealthy(s *identity.Server) bool {
	for _, c := range s.Conditions(nil) {
		if c.Name == "machine_account_unhealthy" && c.Subject.Scope == health.ScopeNode {
			return true
		}
	}
	return false
}

var errDown = errors.New("connection refused")

// machine_account_unhealthy is raised while the channel to every controller
// fails, and cleared by the next channel set up.
func TestChannelHealth(t *testing.T) {
	fx := newChannelServer(t,
		sim.Rule{Op: "dial", Key: dc1, Count: 1, Action: sim.FaultAction{Err: errDown}},
		sim.Rule{Op: "dial", Key: dc2, Count: 1, Action: sim.FaultAction{Err: errDown}})
	fx.apply(t, "", 2, ver(2, 2, pw2, false))
	if unhealthy(fx.s) {
		t.Fatal("machine_account_unhealthy raised before any channel was tried")
	}
	fx.noSignIn(t)
	if !unhealthy(fx.s) {
		t.Errorf("both controllers refused: machine_account_unhealthy not raised")
	}
	fx.signIn(t, dc1, 2)
	if unhealthy(fx.s) {
		t.Errorf("dc1 restored and signed in: machine_account_unhealthy still raised")
	}
}

// A controller that cannot be asked is passed over for the next one, and the
// condition is not raised.
func TestChannelFailOver(t *testing.T) {
	fx := newChannelServer(t, sim.Rule{Op: "dial", Key: dc1, Action: sim.FaultAction{Err: errDown}})
	fx.apply(t, "", 2, ver(2, 2, pw2, false))
	fx.signIn(t, dc2, 2)
	if unhealthy(fx.s) {
		t.Errorf("signed in on dc2: machine_account_unhealthy raised")
	}
}

// A controller that has taken version 3 is signed in to with it, and one that
// has not with version 2.
func TestChannelFallback(t *testing.T) {
	fx := newChannelServer(t)
	fx.d.HoldReplication(dc2)
	fx.d.SetComputerPassword(t, "NAS", []byte(pw3)) // at dc1, the controller first dialled
	for _, tc := range []struct {
		addr string
		want uint64
	}{{dc1, 3}, {dc2, 2}} {
		fx.apply(t, tc.addr, 3, ver(3, 3, pw3, false), ver(2, 2, pw2, false))
		fx.signIn(t, tc.addr, tc.want)
	}
}

// A HandedOver version, current beside a confirmed one as a crash between the
// API's two writes leaves it, signs in nowhere: dc1, holding the confirmed
// version, is signed in to with it, and dc2, holding only the handed-over
// one, refuses every version the channel tries.
func TestChannelHandedOver(t *testing.T) {
	fx := newChannelServer(t)
	ctx := context.Background()
	fx.d.SetComputerPassword(t, "NAS", []byte(pw3))
	fx.d.HoldReplication(dc1)
	dc, err := fx.d.Dial(ctx, directory.Record{}, dc2) // the seeder acts at dc2
	if err != nil {
		t.Fatal(err)
	}
	_ = dc.Close()
	fx.d.SetComputerPassword(t, "NAS", []byte(pw4))

	fx.apply(t, dc1, 3, ver(4, 4, pw4, true), ver(3, 3, pw3, false))
	fx.signIn(t, dc1, 3)
	fx.apply(t, dc2, 3, ver(4, 4, pw4, true), ver(3, 3, pw3, false))
	fx.noSignIn(t)
}

// A record naming a controller's address is that controller alone: dc2,
// named and refusing the version, is not passed over for dc1, which would
// accept it, and the domain's controllers are never looked up.
func TestChannelNamedController(t *testing.T) {
	fx := newChannelServer(t, sim.Rule{Op: "lookup", Key: "*", Action: sim.FaultAction{Err: errDown}}) // never asked
	fx.d.HoldReplication(dc2)
	fx.d.SetComputerPassword(t, "NAS", []byte(pw3)) // at dc1
	fx.apply(t, dc2, 3, ver(3, 3, pw3, false))
	fx.noSignIn(t)
	fx.apply(t, dc1, 3, ver(3, 3, pw3, false))
	fx.signIn(t, dc1, 3)
}

// No channel is set up while the installation is not joined, nor with no
// domain controller to dial.
func TestChannelNotJoined(t *testing.T) {
	fx := newChannelServer(t)
	st := joinedState(2, ver(2, 2, pw2, false))
	st.Join.Joined = false
	if err := fx.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	fx.noSignIn(t)
	if unhealthy(fx.s) {
		t.Errorf("not joined: machine_account_unhealthy raised")
	}

	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: fx.clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(joinedState(2, ver(2, 2, pw2, false))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Logon(context.Background(), netlogon.LogonRequest{User: "alice"}); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("no Deps.DomainController: logon %v, want ErrUnavailable", err)
	}
}

// The previous version signs in to a controller that has not taken the
// current one until 10 hours plus the 5-minute skew after the confirm, and
// not past it, the channel it set up included.
func TestChannelRetention(t *testing.T) {
	fx := newChannelServer(t)
	fx.apply(t, "", 3, ver(3, 3, pw3, false), ver(2, 2, pw2, false))
	fx.clock.Advance(10*time.Hour + 4*time.Minute)
	fx.signIn(t, dc1, 2)
	fx.clock.Advance(time.Minute + time.Second)
	fx.noSignIn(t)
}

// A new principals generation, or a failed rotation's step, keeps the channel,
// and a change to the machine password sets it up again: every set-up after
// the first fails here.
func TestChannelKeptAcrossGenerations(t *testing.T) {
	fx := newChannelServer(t, sim.Rule{Op: "channel", Key: "*", From: 2, Action: sim.FaultAction{Err: errDown}})
	fx.apply(t, "", 2, ver(2, 2, pw2, false))
	fx.signIn(t, dc1, 2)
	st := joinedState(2, ver(2, 2, pw2, false))
	st.Generation = 2
	if err := fx.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	fx.signIn(t, dc1, 2)
	st.Join.LastRotation.At = mt0.Add(time.Hour) // a failed rotation's step
	st.Join.LastRotation.Failed = true
	if err := fx.s.Apply(st); err != nil {
		t.Fatal(err)
	}
	fx.signIn(t, dc1, 2)
	fx.apply(t, "", 2, ver(3, 3, pw3, false), ver(2, 2, pw2, false))
	fx.noSignIn(t)
}

// wait polls cond until it holds, failing the test after a generous backstop.
func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// recv is the next value on ch, failing the test after a generous backstop.
func recv(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// Ten logons the controller holds take every slot: at the 5 s bound they
// answer ErrUnavailable but keep their slots, so an eleventh, started at 1 s,
// answers ErrUnavailable at 6 s without reaching the controller; once the ten
// return, a twelfth signs in.
func TestChannelBounds(t *testing.T) {
	fx := newChannelServer(t, sim.Rule{Op: "logon", Key: "held*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
	fx.apply(t, dc1, 2, ver(2, 2, pw2, false)) // one controller: its set-up arms no controller's bound
	ctx := context.Background()
	ten := make(chan error, 10)
	for i := range 10 {
		go func() { ten <- fx.logon(ctx, fmt.Sprintf("held%d", i), alicePW) }()
	}
	wait(t, "ten held logons", func() bool { return fx.f.Holding() == 10 })
	if n := fx.clock.Pending(); n != 10 {
		t.Fatalf("timers pending with ten held logons = %d, want 10: one call bound each", n)
	}

	fx.clock.Advance(time.Second)
	eleventh := make(chan error, 1)
	go func() { eleventh <- fx.logon(ctx, "alice", []byte("wrong")) }()
	wait(t, "the eleventh waiting for a slot", func() bool { return fx.clock.Pending() == 11 })

	fx.clock.Advance(4 * time.Second)
	for i := range 10 {
		if err := recv(t, ten, "a held logon's answer"); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("held logon %d at 5 s: %v, want ErrUnavailable", i, err)
		}
	}
	fx.clock.Advance(time.Second)
	if err := recv(t, eleventh, "the eleventh's answer"); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("eleventh logon at 6 s: %v, want ErrUnavailable", err)
	}
	if n := fx.d.BadPwdCount(t, "alice"); n != 0 {
		t.Errorf("alice's badPwdCount after the eleventh = %d, want 0: it reached the controller", n)
	}

	fx.f.Release()
	if err := fx.logon(ctx, "alice", alicePW); err != nil {
		t.Errorf("twelfth logon after the release: %v, want answered", err)
	}
}

// Stop closes the channel and ends a call the controller holds; the leak
// check finds nothing left running.
func TestChannelStop(t *testing.T) {
	fx := newChannelServer(t, sim.Rule{Op: "logon", Key: "held*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
	ctx := context.Background()
	fx.apply(t, "", 2, ver(2, 2, pw2, false))
	fx.signIn(t, dc1, 2)
	if err := fx.s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _, ok := fx.s.ChannelAt(); ok {
		t.Errorf("channel to %q open after Stop, want closed", a)
	}

	fx.apply(t, "", 2, ver(2, 2, pw2, false))
	done := make(chan error, 1)
	go func() { done <- fx.logon(ctx, "held1", alicePW) }()
	wait(t, "the held logon", func() bool { return fx.f.Holding() == 1 })
	if err := fx.s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// The bounded wait is the assertion: nothing but Stop ends the hold, and
	// no clock bounds it.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("held logon after Stop: %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("held logon still running 10 s after Stop, want it ended")
	}
}

// A call that read the State before Stop, and reaches the channel only after
// it, sets up no channel. The race is run twenty times, since without the
// check under the channel's lock the call's context could still end first.
func TestChannelStopRace(t *testing.T) {
	fx := newChannelServer(t)
	ctx := context.Background()
	for i := range 20 {
		fx.apply(t, dc1, 2, ver(2, 2, pw2, false))
		reached, resume := make(chan struct{}), make(chan struct{})
		fx.s.OnSlotTaken(func() { close(reached); <-resume })
		done := make(chan error, 1)
		go func() { done <- fx.logon(ctx, "alice", alicePW) }()
		<-reached
		if err := fx.s.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		close(resume)
		if err := recv(t, done, "the logon resumed after Stop"); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("run %d: logon resumed after Stop: %v, want ErrUnavailable", i, err)
		}
		if a, _, ok := fx.s.ChannelAt(); ok {
			t.Fatalf("run %d: channel to %q open after Stop, want none", i, a)
		}
	}
}

// A State that leaves the domain, or drops its record, closes the channel and
// clears machine_account_unhealthy.
func TestChannelLeave(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(*identity.State)
	}{
		{"not joined", func(st *identity.State) { st.Join.Joined = false }},
		{"no record", func(st *identity.State) { st.Directory = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newChannelServer(t, sim.Rule{Op: "dial", Key: dc1, Count: 1, Action: sim.FaultAction{Err: errDown}})
			leave := joinedState(2, ver(2, 2, pw2, false))
			tc.leave(&leave)
			fx.apply(t, dc1, 2, ver(2, 2, pw2, false))
			fx.noSignIn(t)
			if err := fx.s.Apply(leave); err != nil {
				t.Fatal(err)
			}
			if unhealthy(fx.s) {
				t.Errorf("left: machine_account_unhealthy still raised")
			}
			fx.apply(t, dc1, 2, ver(2, 2, pw2, false))
			fx.signIn(t, dc1, 2)
			if err := fx.s.Apply(leave); err != nil {
				t.Fatal(err)
			}
			if a, v, ok := fx.s.ChannelAt(); ok {
				t.Errorf("left: channel at %q with version %d, want none", a, v)
			}
		})
	}
}

// A logon its caller hangs up on answers context.Canceled wherever the
// hang-up lands: in the dial or the bind, which fail with the context's error
// wrapped, or in the call, where a real channel fails with its closed
// connection's error, modelled here as the controller failing just before
// the hang-up. The call's answer and the hang-up are both ready when it looks,
// and a select picks among ready cases at random, so each landing runs
// hangUps times: an answer that drops the context's error on one of the two
// passes them all with odds 2^-hangUps.
func TestChannelHangUpLanding(t *testing.T) {
	const hangUps = 32
	hold := sim.FaultAction{Err: identitytest.ErrHold}
	for _, tc := range []struct {
		name string
		rule sim.Rule
		held bool // whether the hang-up lands while the controller holds the step
	}{
		{"dial", sim.Rule{Op: "dial", Key: dc1, Count: hangUps, Action: hold}, true},
		{"bind", sim.Rule{Op: "channel", Key: "*", Count: hangUps, Action: hold}, true},
		{"call", sim.Rule{Op: "logon", Key: "alice", Action: sim.FaultAction{Err: errDown}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newChannelServer(t, tc.rule)
			fx.apply(t, dc1, 2, ver(2, 2, pw2, false))
			for i := range hangUps {
				gate := make(chan struct{})
				fx.s.OnAwaitAnswer(func() { <-gate })
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- fx.logon(ctx, "alice", alicePW) }()
				if tc.held {
					wait(t, "the held "+tc.name, func() bool { return fx.f.Holding() == 1 })
					cancel()
				}
				wait(t, "the call's own answer", func() bool { return fx.s.PassThroughs() == 0 })
				cancel()
				close(gate)
				if err := recv(t, done, "the hung-up logon"); !errors.Is(err, context.Canceled) {
					t.Fatalf("hang-up %d in the %s: logon %v, want context.Canceled", i, tc.name, err)
				}
			}
			if tc.held && unhealthy(fx.s) {
				t.Errorf("hung up in the %s: machine_account_unhealthy raised", tc.name)
			}
		})
	}
}

// A set-up its caller hangs up on raises no condition, and a call its caller
// hangs up on leaves the channel to the others.
func TestChannelCallerHangsUp(t *testing.T) {
	fx := newChannelServer(t,
		sim.Rule{Op: "dial", Key: dc1, Count: 1, Action: sim.FaultAction{Err: identitytest.ErrHold}},
		sim.Rule{Op: "logon", Key: "held*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
	fx.apply(t, dc1, 2, ver(2, 2, pw2, false))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fx.logon(ctx, "alice", alicePW) }()
	wait(t, "the held dial", func() bool { return fx.f.Holding() == 1 })
	cancel()
	if err := recv(t, done, "the hung-up logon"); !errors.Is(err, context.Canceled) {
		t.Errorf("hung-up logon: %v, want context.Canceled", err)
	}
	wait(t, "the hung-up set-up's end", func() bool { return fx.s.PassThroughs() == 0 })
	if unhealthy(fx.s) {
		t.Errorf("a set-up its caller hung up on raised machine_account_unhealthy")
	}

	fx.signIn(t, dc1, 2)
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- fx.logon(ctx, "held1", alicePW) }()
	wait(t, "the held logon", func() bool { return fx.f.Holding() == 1 })
	cancel()
	if err := recv(t, done, "the hung-up held logon"); !errors.Is(err, context.Canceled) {
		t.Errorf("hung-up held logon: %v, want context.Canceled", err)
	}
	wait(t, "the hung-up logon's end", func() bool { return fx.s.PassThroughs() == 0 })
	if a, v, ok := fx.s.ChannelAt(); !ok || a != dc1 || v != 2 {
		t.Errorf("after a hung-up logon: channel at %q with version %d (open %v), want %q with version 2", a, v, ok, dc1)
	}
}

// A controller that goes silent during a set-up is passed over at its bound
// for the next one; when the last is silent too, the call's bound ends the
// set-up and raises machine_account_unhealthy.
func TestChannelSilentController(t *testing.T) {
	hold := func(addr string) sim.Rule {
		return sim.Rule{Op: "dial", Key: addr, Action: sim.FaultAction{Err: identitytest.ErrHold}}
	}
	ctx := context.Background()
	done := make(chan error, 1)

	t.Run("dc2 healthy", func(t *testing.T) {
		fx := newChannelServer(t, hold(dc1))
		fx.apply(t, "", 2, ver(2, 2, pw2, false))
		go func() { done <- fx.logon(ctx, "alice", alicePW) }()
		wait(t, "the held dial to dc1", func() bool { return fx.f.Holding() == 1 })
		if n := fx.clock.Pending(); n != 2 {
			t.Fatalf("timers pending at the held dial = %d, want 2: the call's bound and dc1's", n)
		}
		fx.clock.Advance(identity.ChannelAttempt)
		if err := recv(t, done, "the logon past dc1"); err != nil {
			t.Errorf("dc1 silent, dc2 healthy: logon %v, want answered", err)
		}
		if a, _, ok := fx.s.ChannelAt(); !ok || a != dc2 {
			t.Errorf("dc1 silent: channel at %q (open %v), want %q", a, ok, dc2)
		}
		if unhealthy(fx.s) {
			t.Errorf("signed in on dc2: machine_account_unhealthy raised")
		}
	})

	t.Run("both silent", func(t *testing.T) {
		fx := newChannelServer(t, hold(dc1), hold(dc2))
		fx.apply(t, "", 2, ver(2, 2, pw2, false))
		go func() { done <- fx.logon(ctx, "alice", alicePW) }()
		wait(t, "the held dial to dc1", func() bool { return fx.f.Holding() == 1 })
		if n := fx.clock.Pending(); n != 2 {
			t.Fatalf("timers pending at the held dial = %d, want 2: the call's bound and dc1's", n)
		}
		fx.clock.Advance(identity.ChannelAttempt)
		fx.clock.Advance(5*time.Second - identity.ChannelAttempt)
		if err := recv(t, done, "the logon at its bound"); !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("both silent: logon %v, want ErrUnavailable", err)
		}
		// No clock bounds the set-up's end once the bound has fired, so the
		// bounded wait is the assertion.
		wait(t, "machine_account_unhealthy raised by the set-up the bound ended", func() bool { return unhealthy(fx.s) })
	})
}

// A channel replaced while a call runs on it stays open until that call
// returns.
func TestChannelReplacedUnderCall(t *testing.T) {
	fx := newChannelServer(t, sim.Rule{Op: "logon", Key: "held*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
	ctx := context.Background()
	fx.d.AddUser(t, "held1", alicePW)
	fx.apply(t, dc1, 2, ver(2, 2, pw2, false))
	done := make(chan error, 1)
	go func() { done <- fx.logon(ctx, "held1", alicePW) }()
	wait(t, "the held logon", func() bool { return fx.f.Holding() == 1 })
	fx.apply(t, dc1, 3, ver(3, 3, pw3, false), ver(2, 2, pw2, false)) // dc1 refuses 3 and takes 2 again
	fx.signIn(t, dc1, 2)
	fx.f.Release()
	if err := recv(t, done, "the held logon on the replaced channel"); err != nil {
		t.Errorf("held logon on the replaced channel: %v, want answered", err)
	}
}
