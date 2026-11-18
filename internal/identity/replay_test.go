package identity

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// replayFixture is the test realm, its State, a clock at t0 and a cache of
// the given bound and share.
type replayFixture struct {
	t     *testing.T
	realm *identitytest.Realm
	st    *State
	clock *sim.TestClock
	cache *replayCache
}

func newReplayFixture(t *testing.T, bound, share int) *replayFixture {
	r, st := testRealmAndState(t)
	return &replayFixture{t, r, st, sim.NewTestClock(t0), newReplayCache(bound, share)}
}

// apReq is an AP-REQ for cifs/nas.test from client, its authenticator stamped
// authTime with cusec, issued at t0.
func (f *replayFixture) apReq(client string, authTime time.Time, cusec int) []byte {
	return f.realm.Ticket(identitytest.TicketOpts{Service: "cifs/nas.test", Client: client, Now: t0, AuthTime: authTime, Cusec: cusec})
}

// present verifies b at the clock's time and inserts its authenticator.
func (f *replayFixture) present(b []byte) error {
	f.t.Helper()
	tk, err := verifyAPReq(f.st, b, identity.ServiceCIFS, identity.TransportSMB, addr1, f.clock.Now())
	if err != nil {
		f.t.Fatalf("verifyAPReq at %v: %v", f.clock.Now(), err)
	}
	return f.cache.insert(&tk, f.clock.Now())
}

// The same AP-REQ twice: the second is a replay, and so is the first with its
// service name re-spelled, which the ring accepts.
func TestReplaySameAPReqTwice(t *testing.T) {
	f := newReplayFixture(t, replayBound, replayShare)
	b := f.apReq("alice", t0, 1)
	if err := f.present(b); err != nil {
		t.Fatalf("first presentation: %v, want accepted", err)
	}
	if err := f.present(b); !errors.Is(err, errReplay) || !errors.Is(err, vfserr.ErrLogonFailure) {
		t.Fatalf("second presentation: %v, want the ErrLogonFailure with outcome replay", err)
	}
	req, err := parseAPReq(b)
	if err != nil {
		t.Fatal(err)
	}
	req.Ticket.SName.NameString = []string{"CIFS", "NAS.TEST"}
	respelled, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.present(respelled); !errors.Is(err, errReplay) {
		t.Fatalf("the AP-REQ with its service name re-spelled: %v, want the replay refusal", err)
	}
}

// An authenticator stamped 4 minutes ahead, replayed 6 minutes after it was
// first seen, is still held, since its entry lives until its own time plus
// the skew, not its arrival's.
func TestReplayEntryKeptFromAuthenticatorTime(t *testing.T) {
	f := newReplayFixture(t, replayBound, replayShare)
	b := f.apReq("alice", t0.Add(4*time.Minute), 1)
	if err := f.present(b); err != nil {
		t.Fatalf("first presentation: %v, want accepted", err)
	}
	f.clock.Advance(6 * time.Minute)
	if err := f.present(b); !errors.Is(err, errReplay) {
		t.Fatalf("replayed 6 minutes later: %v, want the replay refusal", err)
	}
}

// Once the clock passes an entry's time plus the skew, the next insert drops
// it: the cache holds the new entry alone.
func TestReplayEntryDroppedOnceExpired(t *testing.T) {
	f := newReplayFixture(t, replayBound, replayShare)
	if err := f.present(f.apReq("alice", t0, 1)); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(clockSkew + time.Second)
	if err := f.present(f.apReq("alice", f.clock.Now(), 2)); err != nil {
		t.Fatal(err)
	}
	if n := f.cache.Len(); n != 1 {
		t.Errorf("entries after the first expired = %d, want 1", n)
	}
}

// A full cache refuses another entry ErrUnavailable, never evicting a live
// one, and takes it once the first entry expires.
func TestReplayFullCacheRefuses(t *testing.T) {
	f := newReplayFixture(t, 2, 2)
	for i, at := range []time.Time{t0, t0.Add(2 * time.Minute)} {
		if err := f.present(f.apReq("alice", at, i)); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	third := f.apReq("bob", t0.Add(4*time.Minute), 3)
	if err := f.present(third); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("third authenticator into a full cache: %v, want ErrUnavailable", err)
	}
	f.clock.Advance(clockSkew + time.Second)
	if err := f.present(third); err != nil {
		t.Fatalf("third authenticator once the first expired: %v, want accepted", err)
	}
}

// One client past its share is refused ErrUnavailable while another still
// inserts, and its share is released as its entries expire.
func TestReplayClientShare(t *testing.T) {
	f := newReplayFixture(t, 8, 2)
	for i := range 2 {
		if err := f.present(f.apReq("carol", t0.Add(time.Duration(i)*time.Minute), i)); err != nil {
			t.Fatalf("carol's entry %d: %v", i, err)
		}
	}
	if err := f.present(f.apReq("carol", t0.Add(2*time.Minute), 2)); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("carol's third authenticator: %v, want ErrUnavailable", err)
	}
	if err := f.present(f.apReq("alice", t0, 0)); err != nil {
		t.Fatalf("alice's first authenticator while carol is at her share: %v, want accepted", err)
	}
	f.clock.Advance(clockSkew + time.Second)
	if err := f.present(f.apReq("carol", f.clock.Now(), 3)); err != nil {
		t.Fatalf("carol's next authenticator once her first expired: %v, want accepted", err)
	}
}

// 64 goroutines presenting one AP-REQ at once: exactly one is accepted.
func TestReplayConcurrentPresentations(t *testing.T) {
	f := newReplayFixture(t, replayBound, replayShare)
	b := f.apReq("alice", t0, 1)
	tickets := make([]ticket, 64)
	for i := range tickets {
		tk, err := verifyAPReq(f.st, b, identity.ServiceCIFS, identity.TransportSMB, addr1, t0)
		if err != nil {
			t.Fatal(err)
		}
		tickets[i] = tk
	}
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		accepted = make(chan struct{}, len(tickets))
	)
	for i := range tickets {
		wg.Go(func() {
			<-start
			if f.cache.insert(&tickets[i], t0) == nil {
				accepted <- struct{}{}
			}
		})
	}
	close(start)
	wg.Wait()
	if n := len(accepted); n != 1 {
		t.Errorf("presentations accepted = %d, want 1", n)
	}
}
