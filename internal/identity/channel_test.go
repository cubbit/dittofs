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
// up.
func (fx *chanFixture) noSignIn(t *testing.T) {
	t.Helper()
	if err := fx.logon(context.Background(), "alice", alicePW); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("alice's logon: %v, want ErrUnavailable", err)
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
// accept it.
func TestChannelNamedController(t *testing.T) {
	fx := newChannelServer(t)
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

// A new principals generation alone keeps the channel, and a change to the
// machine password sets it up again: every set-up after the first fails here.
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
	fx.apply(t, "", 2, ver(2, 2, pw2, false))
	ctx := context.Background()
	ten := make(chan error, 10)
	for i := range 10 {
		go func() { ten <- fx.logon(ctx, fmt.Sprintf("held%d", i), alicePW) }()
	}
	wait(t, "ten held logons", func() bool { return fx.f.Holding() == 10 && fx.clock.Pending() == 10 })

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
	if err := recv(t, done, "the held logon after Stop"); err == nil {
		t.Errorf("held logon after Stop answered, want an error")
	}
}
