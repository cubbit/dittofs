package identitytest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/internal/identity/netlogon"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
)

func newDomain() *identitytest.DomainController {
	return identitytest.NewDomainController("CORP", "CORP.EXAMPLE", base, "dc1:135", "dc2:135")
}

func dial(t *testing.T, d netlogon.Dialer, addr string) netlogon.DC {
	t.Helper()
	dc, err := d.Dial(context.Background(), directory.Record{}, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	return dc
}

// The in-memory domain controller answers as the suite holds a real one to.
func TestDomainControllerConformance(t *testing.T) {
	identitytest.DomainControllerConformance(t, func(t *testing.T) (netlogon.DC, identitytest.DCSeeder) {
		d := newDomain()
		return dial(t, d, "dc1:135"), d
	})
}

// Lookup answers the controllers in the order built; a controller whose
// replication is held answers an account as it was, the old key version and
// the old password, until released. One real controller cannot show this.
func TestDomainControllerReplication(t *testing.T) {
	ctx := context.Background()
	d := newDomain()
	if addrs, err := d.Lookup(ctx, "corp.example", ""); err != nil || len(addrs) != 2 || addrs[0] != "dc1:135" || addrs[1] != "dc2:135" {
		t.Fatalf("Lookup = %v, %v; want [dc1:135 dc2:135]", addrs, err)
	}
	old, next := []byte("Old1!password"), []byte("New1!password")
	d.StageComputer(t, identitytest.StagedComputer{Name: "HOST1", Password: old, EncTypes: 0x18})
	d.HoldReplication("dc2:135")
	dc1, dc2 := dial(t, d, "dc1:135"), dial(t, d, "dc2:135")
	if err := dc1.Reset(ctx, d.Admin(), "HOST1", next); err != nil {
		t.Fatal(err)
	}

	if a, err := dc2.Account(ctx, d.Admin(), "host1"); err != nil || a.KeyVersion != 1 {
		t.Errorf("held dc2: key version %d, %v; want 1, the number before the reset", a.KeyVersion, err)
	}
	if _, err := dc2.Channel(ctx, "HOST1", next); !errors.Is(err, netlogon.ErrRejected) {
		t.Errorf("held dc2, the new password: err %v, want ErrRejected", err)
	}
	if a, err := dc1.Account(ctx, d.Admin(), "HOST1"); err != nil || a.KeyVersion != 2 {
		t.Errorf("dc1: key version %d, %v; want 2", a.KeyVersion, err)
	}

	d.ReleaseReplication("dc2:135")
	if a, err := dc2.Account(ctx, d.Admin(), "HOST1"); err != nil || a.KeyVersion != 2 {
		t.Errorf("released dc2: key version %d, %v; want 2", a.KeyVersion, err)
	}
	if ch, err := dc2.Channel(ctx, "HOST1", next); err != nil {
		t.Errorf("released dc2, the new password: %v", err)
	} else {
		_ = ch.Close()
	}
}

// The fault wrapper refuses a dial before it is sent, fails a call after the
// controller has acted on it, and holds one until released or its context
// ends.
func TestFaultyDomainController(t *testing.T) {
	ctx := context.Background()
	d := newDomain()
	d.StageComputer(t, identitytest.StagedComputer{Name: "HOST1", Password: []byte("Old1!password")})
	errDown := errors.New("down")
	clock := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, clock,
		sim.Rule{Op: "dial", Key: "dc2:135", Action: sim.FaultAction{Err: errDown}},
		sim.Rule{Op: "reset", Key: "HOST1", From: 1, Count: 1, Action: sim.FaultAction{Err: errDown}},
		sim.Rule{Op: "account", Key: "HOST1", From: 1, Count: 2, Action: sim.FaultAction{Err: identitytest.ErrHold}},
	)
	if err != nil {
		t.Fatal(err)
	}
	f := identitytest.NewFaultyDomainController(d, sched)

	if _, err := f.Dial(ctx, directory.Record{}, "dc2:135"); !errors.Is(err, errDown) {
		t.Errorf("dial dc2: err %v, want %v", err, errDown)
	}
	dc := dial(t, f, "dc1:135")
	if err := dc.Reset(ctx, d.Admin(), "HOST1", []byte("New1!password")); !errors.Is(err, errDown) {
		t.Errorf("reset: err %v, want %v", err, errDown)
	}
	if ch, err := dc.Channel(ctx, "HOST1", []byte("New1!password")); err != nil {
		t.Errorf("channel with the password the failed reset sent: %v, want accepted: the controller acted", err)
	} else {
		_ = ch.Close()
	}

	hctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { _, err := dc.Account(hctx, d.Admin(), "HOST1"); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("held account read: err %v, want %v once its context ended", err, context.Canceled)
	}

	type answer struct {
		v   uint32
		err error
	}
	got := make(chan answer)
	go func() { a, err := dc.Account(ctx, d.Admin(), "HOST1"); got <- answer{a.KeyVersion, err} }()
	// Release frees only a call already held, so it is repeated until the
	// held call answers.
	deadline := time.After(10 * time.Second)
	for {
		f.Release()
		select {
		case <-deadline:
			t.Fatal("the held account read did not answer within 10s of being released")
		case a := <-got:
			if a.err != nil || a.v != 2 {
				t.Errorf("released account read: key version %d, %v; want 2", a.v, a.err)
			}
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}
