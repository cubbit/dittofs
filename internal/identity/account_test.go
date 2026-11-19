package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// Every spelling of alice's name gives her principal, an API-only account
// is found too, and a name no account holds gives the zero principal; none
// raises the principals generation.
func TestLookup(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	admin := entity.User{Principal: entity.NewUser(), Name: "admin"}
	r.user(t, admin, 0)
	before := r.pgen(t)

	for _, c := range []struct {
		name string
		want entity.Principal
	}{
		{"alice", alice.Principal},
		{"ALICE", alice.Principal},
		{`CORP\alice`, alice.Principal},
		{"alice@CORP.EXAMPLE", alice.Principal},
		{"admin", admin.Principal},
		{"nobody", entity.Principal{}},
	} {
		if got, err := r.srv.Lookup(ctx, c.name); err != nil || got != c.want {
			t.Errorf("Lookup(%q) = %v, %v; want %v, nil", c.name, got, err, c.want)
		}
	}
	if after := r.pgen(t); after != before {
		t.Errorf("principals generation after the lookups = %d, want %d unchanged", after, before)
	}
}

// A store that cannot answer is ErrUnavailable, never the zero principal of
// a name no account holds.
func TestLookupStoreOutage(t *testing.T) {
	r := naming(t)
	r.fail(t, "*", "*")
	if p, err := r.srv.Lookup(context.Background(), "alice"); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Lookup over a failing store = %v, %v; want ErrUnavailable", p, err)
	}
}

// Account reads the store on every call: a user disabled with no Apply since
// is refused, as are the zero principal and a machine's, and an outage is
// ErrUnavailable.
func TestAccount(t *testing.T) {
	r := naming(t)
	ctx := context.Background()
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	gen := r.user(t, alice, 0)

	if u, err := r.srv.Account(ctx, alice.Principal); err != nil || u.Name != "alice" {
		t.Fatalf("Account(alice) = %+v, %v; want her record", u, err)
	}
	if u, err := r.srv.Account(ctx, entity.Principal{}); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("Account(zero principal) = %+v, %v; want ErrLogonFailure", u, err)
	}
	m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()},
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/build01.corp.example@CORP.EXAMPLE"}}}
	r.user(t, m, 0)
	if u, err := r.srv.Account(ctx, m.Principal); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("Account(a machine) = %+v, %v; want ErrLogonFailure", u, err)
	}
	alice.Disabled = true
	r.user(t, alice, gen)
	if u, err := r.srv.Account(ctx, alice.Principal); !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Errorf("Account(alice) once disabled = %+v, %v; want ErrLogonFailure", u, err)
	}
	r.fail(t, "*", "*")
	if u, err := r.srv.Account(ctx, alice.Principal); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("Account over a failing store = %+v, %v; want ErrUnavailable", u, err)
	}
}

// A server built without a secret provider refuses every password
// ErrUnavailable, never ErrLogonFailure and never a panic.
func TestVerifyPasswordWithoutSecrets(t *testing.T) {
	r := naming(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1500")}
	r.user(t, alice, 0)
	if err := r.srv.VerifyPassword(context.Background(), alice.Principal, []byte("Password")); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("VerifyPassword with no secret provider = %v, want ErrUnavailable", err)
	}
}

// With every hashing slot held, a verification waits 5 s on the clock, not
// less, then answers ErrUnavailable and leaves no goroutine behind.
func TestHashSlotWaitIsBounded(t *testing.T) {
	s, clock := slotServer(t)
	defer s.HoldHashSlots(0)()

	done := make(chan error, 1)
	go func() { done <- s.HashSlot(context.Background()) }()
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the verification never waited on the clock for a slot")
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(hashWait - time.Nanosecond)
	if n := clock.Pending(); n != 1 {
		t.Fatalf("waits pending just before 5 s = %d, want 1", n)
	}
	clock.Advance(time.Nanosecond)
	select {
	case err := <-done:
		if !errors.Is(err, vfserr.ErrUnavailable) {
			t.Errorf("HashSlot with every slot held = %v, want ErrUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HashSlot did not return once the clock passed 5 s")
	}
}

// slotServer builds a server over a test clock, under the leak check.
func slotServer(t *testing.T) (*identity.Server, *sim.TestClock) {
	t.Helper()
	prof.Watch(t, prof.Options{})
	clock := sim.NewTestClock(time.Unix(0, 0))
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: clock})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, clock
}

// A slot is given back: with one free, two verifications in turn each take
// it, and the second never waits on the clock.
func TestHashSlotIsReleased(t *testing.T) {
	s, clock := slotServer(t)
	defer s.HoldHashSlots(1)()

	for i := range 2 {
		done := make(chan error, 1)
		go func() { done <- s.HashSlot(context.Background()) }()
		deadline := time.Now().Add(10 * time.Second)
	wait:
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("HashSlot %d with one slot free = %v, want nil", i+1, err)
				}
				break wait
			default:
			}
			if clock.Pending() != 0 {
				clock.Advance(hashWait)
				t.Fatalf("HashSlot %d waited on the clock for a slot and answered %v, want the one free slot back", i+1, <-done)
			}
			if time.Now().After(deadline) {
				t.Fatalf("HashSlot %d neither returned nor waited on the clock", i+1)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A wait for a slot ended by its context answers the context's error.
func TestHashSlotWaitEndsWithContext(t *testing.T) {
	s, clock := slotServer(t)
	defer s.HoldHashSlots(0)()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.HashSlot(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the verification never waited on the clock for a slot")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("HashSlot cancelled while waiting = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		clock.Advance(hashWait)
		t.Fatalf("HashSlot did not return once its context was cancelled; past the wait it answered %v", <-done)
	}
}

// hashWait is how long a verification waits for a slot.
const hashWait = 5 * time.Second
