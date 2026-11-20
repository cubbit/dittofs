package identity_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

var (
	addrA = netip.MustParseAddr("192.0.2.10")
	addrB = netip.MustParseAddr("192.0.2.11")
)

// from is step signed in from client.
func from(step pid.SignInStep, client netip.Addr) pid.SignInStep {
	step.Client = client
	return step
}

// fails signs in n times with step, each answering want.
func (r *resolving) fails(t *testing.T, n int, step func() pid.SignInStep, want error) {
	t.Helper()
	for i := range n {
		if _, err := r.srv.SignIn(context.Background(), step()); !errors.Is(err, want) {
			t.Fatalf("attempt %d: SignIn = %v, want %v", i+1, err, want)
		}
	}
}

// signsIn checks that step signs in, or is throttled when throttled is set.
func (r *resolving) signsIn(t *testing.T, what string, step pid.SignInStep, throttled bool) {
	t.Helper()
	res, oc, err := r.srv.SignInOutcome(context.Background(), step)
	switch {
	case throttled:
		refused(t, what, res, oc, err, vfserr.ErrLogonFailure, "throttled")
	case err != nil:
		t.Errorf("%s: SignIn = %v, outcome %q; want a sign-in", what, err, oc)
	}
}

func wrong(client netip.Addr) func() pid.SignInStep {
	return func() pid.SignInStep { return from(ntlmStep("wrong", "alice", "", false), client) }
}

// §10 "logon throttle", local half: 10 wrong passwords for alice from one
// address, then the right one, is refused ErrLogonFailure, outcome
// throttled, reaching no store read, which the secret provider's reads go
// through too; with every read failing it is still that, not ErrUnavailable.
// Catches a throttle asked after a read, or one that still checks the
// credential.
func TestThrottleRefusesBeforeTheCredential(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	right := from(ntlmStep(alicePassword, "alice", "", false), addrA)
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)
	r.reads.TakeReads()
	r.signsIn(t, "the right password after 10 wrong ones", right, true)
	if n := r.reads.TakeReads(); n != 0 {
		t.Errorf("a throttled sign-in made %d store reads, want 0", n)
	}
	r.fail(t, "*", "*")
	r.signsIn(t, "the right password after 10 wrong ones, every read failing", right, true)
}

// The limit's lower edge: after 9 wrong passwords the right one still signs
// in, its credential checked, and one more wrong one then throttles the pair,
// since a success does not reset the count. Catches a limit below 10.
func TestThrottleChecksTheTenthAttempt(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	right := from(ntlmStep(alicePassword, "alice", "", false), addrA)
	r.fails(t, 9, wrong(addrA), vfserr.ErrLogonFailure)
	r.signsIn(t, "the right password after 9 wrong ones", right, false)
	r.fails(t, 1, wrong(addrA), vfserr.ErrLogonFailure)
	r.signsIn(t, "the right password after 10 wrong ones and a success", right, true)
}

// Wrong passwords for one pair sent at once have at most 10 credentials
// checked: every attempt that reaches the check is held there until each
// of the others has either reached it too or been answered, so all of them
// are in flight together. Catches the check and the count taken apart.
func TestThrottleReservesConcurrentAttempts(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	const n = 50
	var checked atomic.Int32
	arrived, gate := make(chan struct{}, n), make(chan struct{})
	r.srv.OnNTHash(func([16]byte) {
		checked.Add(1)
		arrived <- struct{}{}
		<-gate
	})
	done := make(chan error, n)
	for range n {
		go func() {
			_, err := r.srv.SignIn(context.Background(), wrong(addrA)())
			done <- err
		}()
	}
	var errs []error
	for held := 0; held+len(errs) < n; {
		select {
		case <-arrived:
			held++
		case err := <-done:
			errs = append(errs, err)
		case <-time.After(time.Minute):
			t.Fatalf("after a minute %d attempts were held and %d answered, want %d in all", held, len(errs), n)
		}
	}
	close(gate)
	for len(errs) < n {
		errs = append(errs, <-done)
	}
	for i, err := range errs {
		if !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Errorf("attempt %d: SignIn = %v, want %v", i+1, err, vfserr.ErrLogonFailure)
		}
	}
	if got := checked.Load(); got > 10 {
		t.Errorf("%d wrong passwords sent at once had %d credentials checked, want at most 10", n, got)
	}
}

// The refusals decided from the message count, since the throttle is asked
// before them: 10 NTLMv1 responses, or 10 attempts while NTLM is off, leave
// the right password refused; a malformed response from a throttled pair is
// throttled, not ErrInvalid. Catches the throttle asked after one of those
// refusals.
func TestThrottleCountsMessageRefusals(t *testing.T) {
	right := func() pid.SignInStep { return from(ntlmStep(alicePassword, "alice", "", false), addrA) }
	t.Run("NTLMv1", func(t *testing.T) {
		r, p := ntlmServer(t, "NAS01")
		r.ntlmUser(t, p, "alice", "1500", alicePassword)
		r.fails(t, 10, func() pid.SignInStep {
			s := right()
			s.NTLM.NTResponse = s.NTLM.NTResponse[:24]
			return s
		}, vfserr.ErrLogonFailure)
		r.signsIn(t, "the right password after 10 NTLMv1 responses", right(), true)
		malformed := right()
		malformed.NTLM.NTResponse[16] = 2 // RespType
		res, oc, err := r.srv.SignInOutcome(context.Background(), malformed)
		refused(t, "a malformed response from a throttled pair", res, oc, err, vfserr.ErrLogonFailure, "throttled")
	})
	t.Run("NTLM off", func(t *testing.T) {
		r, p := ntlmServer(t, "NAS01")
		r.ntlmUser(t, p, "alice", "1500", alicePassword)
		r.applyNTLM(t, "NAS01", false)
		r.fails(t, 10, right, vfserr.ErrLogonFailure)
		r.applyNTLM(t, "NAS01", true)
		r.signsIn(t, "the right password after 10 attempts while NTLM was off", right(), true)
	})
}

// 10 failures spread over alice, ALICE, CORP\alice (unknown_domain here) and
// alice@CORP.EXAMPLE leave alice's right password refused. Catches a
// spelling that counts apart.
func TestThrottleCountsEverySpelling(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	spellings := []string{"alice", "ALICE", `CORP\alice`, "alice@CORP.EXAMPLE"}
	i := 0
	r.fails(t, 10, func() pid.SignInStep {
		i++
		return from(ntlmStep("wrong", spellings[i%len(spellings)], "", false), addrA)
	}, vfserr.ErrLogonFailure)
	r.signsIn(t, "alice's right password after 10 failures over her spellings", from(ntlmStep(alicePassword, "alice", "", false), addrA), true)
}

// alice and ALICE with an empty domain, and alice with the NetBIOS name,
// each sign in as alice, Lookup finds her as NAS01\alice too, and the
// throttle keys all of them as one pair. Catches a sign-in that finds the
// literal name, making one string two principals.
func TestThrottleKeysWhatSignInFinds(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	alice := r.ntlmUser(t, p, "alice", "1500", alicePassword)
	ctx := context.Background()
	for _, c := range []struct{ user, domain string }{{"alice", ""}, {"ALICE", ""}, {"alice", "NAS01"}} {
		res, err := r.srv.SignIn(ctx, from(ntlmStep(alicePassword, c.user, c.domain, false), addrA))
		if err != nil || res.Identity.Evidence.Principal != alice.Principal {
			t.Errorf("SignIn as %q in %q = %v, %v; want alice's %v", c.user, c.domain, res.Identity.Evidence.Principal, err, alice.Principal)
		}
	}
	for _, name := range []string{"alice", "ALICE", `NAS01\alice`} {
		if got, err := r.srv.Lookup(ctx, name); err != nil || got != alice.Principal {
			t.Errorf("Lookup(%q) = %v, %v; want alice's %v", name, got, err, alice.Principal)
		}
		if identity.ThrottleKey(name, addrA) != identity.ThrottleKey("alice", addrA) {
			t.Errorf("the throttle's key for %q differs from alice's", name)
		}
	}
}

// 10 sign-ins answered ErrUnavailable, the user's read failing, count
// nothing: with the store back, the right password signs in. Catches a store
// fault counted, which would lock a right password out in an outage.
func TestThrottleCountsNoOutage(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	right := func() pid.SignInStep { return from(ntlmStep(alicePassword, "alice", "", false), addrA) }
	r.fail(t, kvtest.OpGet, "U‖*")
	r.fails(t, 10, right, vfserr.ErrUnavailable)
	r.fault.SetFault(nil)
	r.signsIn(t, "the right password after 10 outages", right(), false)
}

// The count is per pair, not per account: alice throttled from A signs in
// from B, and is still refused from A's IPv4-mapped IPv6 form. Two addresses
// in one IPv6 /64 are one pair. Catches a per-account lockout, an unmapped
// key and a key on the whole IPv6 address.
func TestThrottleKeysTheClientAddress(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	right := func(c netip.Addr) pid.SignInStep { return from(ntlmStep(alicePassword, "alice", "", false), c) }
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)
	r.signsIn(t, "alice from B", right(addrB), false)
	r.signsIn(t, "alice from A's IPv4-mapped form", right(netip.AddrFrom16(addrA.As16())), true)

	v6a, v6b := netip.MustParseAddr("2001:db8:1:2::a"), netip.MustParseAddr("2001:db8:1:2:ffff::b")
	r.fails(t, 10, wrong(v6a), vfserr.ErrLogonFailure)
	r.signsIn(t, "alice from another address of the same /64", right(v6b), true)
}

// The window slides: once the oldest of 10 failures is 10 minutes old, the
// right password signs in, though a throttled attempt was refused once a
// minute meanwhile, from 30 seconds in. Catches a throttled attempt counted,
// which would leave the oldest counted 9.5 minutes old then.
func TestThrottleWindowPasses(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	right := from(ntlmStep(alicePassword, "alice", "", false), addrA)
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)
	r.clock.Advance(30 * time.Second)
	for i := range 10 {
		if i > 0 {
			r.clock.Advance(time.Minute)
		}
		r.signsIn(t, "the right password inside the window", right, true)
	}
	r.clock.Advance(30 * time.Second)
	r.signsIn(t, "the right password once the oldest failure is 10 minutes old", right, false)
}

// junk is an NTLMv1 attempt as name from client: counted, and refused
// before any store read.
func junk(name string, client netip.Addr) func() pid.SignInStep {
	return func() pid.SignInStep {
		s := from(ntlmStep("wrong", name, "", false), client)
		s.NTLM.NTResponse = s.NTLM.NTResponse[:24]
		return s
	}
}

// net10 is the i-th address of 10.0.0.0/8.
func net10(i int) netip.Addr {
	return netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
}

// A full table evicts its least recently admitted pair, counted: alice, 9
// failures from A, is the first of 129 pairs in a table of 128, and once
// evicted her count starts again, so one more wrong password leaves her right
// one signing in. A 64 KiB name costs the table no more than a short one.
// Catches a full table that evicts another pair, or none.
func TestThrottleEvictsTheOldestPair(t *testing.T) {
	r, _ := evictionServer(t)
	r.srv.BoundThrottle(128)
	r.fails(t, 9, wrong(addrA), vfserr.ErrLogonFailure)

	short := r.srv.ThrottleFootprint()
	long := strings.Repeat("x", 64<<10)
	r.fails(t, 1, func() pid.SignInStep { return from(ntlmStep("wrong", long, "", false), addrA) }, vfserr.ErrLogonFailure)
	if got := r.srv.ThrottleFootprint() - short; got != short {
		t.Errorf("a 64 KiB name's pair holds %d bytes, want a short name's %d", got, short)
	}

	for i := range 127 {
		r.fails(t, 1, junk("bob", net10(i)), vfserr.ErrLogonFailure)
	}
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions after 129 pairs = %v, want 1", got)
	}
	r.fails(t, 1, wrong(addrA), vfserr.ErrLogonFailure)
	r.signsIn(t, "alice from A, evicted at 9 failures, after 1 more", from(ntlmStep(alicePassword, "alice", "", false), addrA), false)
}

// A table full of throttled pairs still admits a new one: 64 addresses each
// fill their share, 2 of 128 pairs, with junk names at the limit, and bob's
// right password from another address signs in, evicting one pair, counted.
// Catches a full table that refuses new pairs, which locks every user out.
func TestThrottleFullOfThrottledPairsAdmitsANewOne(t *testing.T) {
	r, p := evictionServer(t)
	r.ntlmUser(t, p, "bob", "1501", alicePassword)
	r.srv.BoundThrottle(128)
	for i := range 64 {
		for _, name := range []string{"junk0", "junk1"} {
			r.fails(t, 10, junk(name, net10(i)), vfserr.ErrLogonFailure)
		}
	}
	r.signsIn(t, "bob from B while every pair is at its limit", from(ntlmStep(alicePassword, "bob", "", false), addrB), false)
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions = %v, want 1", got)
	}
}

// Junk names never free a throttled pair: alice, 10 wrong passwords from A,
// then 8 junk names from A and 8 from B, each address holding a share of 4
// of 256 pairs. Past its share each address evicts its own junk, counted, 5
// from A and 4 from B, and alice's right password from A stays throttled.
// Catches an address past its share evicting a throttled pair of its own, or
// another address's pair, and a share not kept.
func TestThrottleKeepsAPairAtItsLimit(t *testing.T) {
	r, _ := evictionServer(t)
	r.srv.BoundThrottle(256)
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)
	alice := from(ntlmStep(alicePassword, "alice", "", false), addrA)
	evicted := 0.0
	for _, c := range []struct {
		addr    netip.Addr
		evicted float64
	}{{addrA, 5}, {addrB, 4}} {
		for i := range 8 {
			r.fails(t, 1, junk(fmt.Sprintf("junk%d", i), c.addr), vfserr.ErrLogonFailure)
		}
		evicted += c.evicted
		if got := r.total(t, "identity_logon_throttle_evictions_total"); got != evicted {
			t.Errorf("evictions after 8 junk names from %v = %v, want %v", c.addr, got, evicted)
		}
		r.signsIn(t, fmt.Sprintf("alice from A after 8 junk names from %v", c.addr), alice, true)
	}
}

// A full table evicts a pair below its limit before a throttled one: alice,
// 10 wrong passwords from A, then 64 /64s inside one /56 each send their
// share of junk names, one attempt per name, filling a table of 128 past its
// bound. The junk evicts junk, counted, and alice's right password from A
// stays throttled. Catches a full table that evicts its least recently
// admitted pair whatever its count.
func TestThrottleFullTableKeepsAThrottledPair(t *testing.T) {
	r, _ := evictionServer(t)
	r.srv.BoundThrottle(128)
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)
	for i := range 64 {
		unit := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, byte(i), 15: 1})
		for j := range 2 {
			r.fails(t, 1, junk(fmt.Sprintf("junk%d", j), unit), vfserr.ErrLogonFailure)
		}
	}
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions after 129 pairs = %v, want 1", got)
	}
	r.signsIn(t, "alice from A after the table filled with junk", from(ntlmStep(alicePassword, "alice", "", false), addrA), true)
}

// An address past its share evicts its own oldest pair below its limit:
// alice, 9 wrong passwords from A, then 4 junk names from A, its share of 256
// pairs being 4, and her count starts again. Catches an address evicting its
// newest pair.
func TestThrottleEvictsAnAddresssOldestPair(t *testing.T) {
	r, _ := evictionServer(t)
	r.srv.BoundThrottle(256)
	r.fails(t, 9, wrong(addrA), vfserr.ErrLogonFailure)
	for i := range 4 {
		r.fails(t, 1, junk(fmt.Sprintf("junk%d", i), addrA), vfserr.ErrLogonFailure)
	}
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions = %v, want 1", got)
	}
	r.fails(t, 1, wrong(addrA), vfserr.ErrLogonFailure)
	r.signsIn(t, "alice from A, evicted at 9 failures, after 1 more", from(ntlmStep(alicePassword, "alice", "", false), addrA), false)
}

// An address whose whole share is at the limit is refused a new pair,
// throttled, its credential unchecked, while another address signs in; once
// the first of its pairs' window passes, the new pair evicts it, counted.
// Catches an address evicting its own throttled pair, and one that stays
// shut once its pairs age out.
func TestThrottleRefusesAnAddressWhoseShareIsThrottled(t *testing.T) {
	r, _ := evictionServer(t)
	r.srv.BoundThrottle(256)
	for i := range 4 {
		r.fails(t, 10, junk(fmt.Sprintf("junk%d", i), addrA), vfserr.ErrLogonFailure)
	}
	alice := func(c netip.Addr) pid.SignInStep { return from(ntlmStep(alicePassword, "alice", "", false), c) }
	var checked int
	r.srv.OnNTHash(func([16]byte) { checked++ })
	r.signsIn(t, "alice from A while A's share is at its limit", alice(addrA), true)
	if checked != 0 {
		t.Errorf("a new pair refused for its address's share had %d credentials checked, want 0", checked)
	}
	r.signsIn(t, "alice from B while A's share is at its limit", alice(addrB), false)
	r.clock.Advance(10 * time.Minute)
	r.signsIn(t, "alice from A once the first pair's window has passed", alice(addrA), false)
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions = %v, want 1", got)
	}
}

// Attempts in flight count against the share and are evicted with their
// pair: in a table of 64 pairs, a share of 1, 64 addresses each hold one
// attempt in flight at the credential check. A second name from one of them
// is throttled, and alice's right password from B signs in, evicting one
// pair in flight, counted. Catches a share that leaves attempts in flight
// out, and a full table that cannot evict a pair in flight.
func TestThrottleCountsAttemptsInFlight(t *testing.T) {
	r, _ := evictionServer(t)
	r.srv.BoundThrottle(64)
	const n = 64
	var calls atomic.Int32
	arrived, gate := make(chan struct{}, n), make(chan struct{})
	r.srv.OnNTHash(func([16]byte) {
		if calls.Add(1) <= n {
			arrived <- struct{}{}
			<-gate
		}
	})
	done := make(chan error, n)
	for i := range n {
		go func() {
			_, err := r.srv.SignIn(context.Background(), from(ntlmStep("wrong", fmt.Sprintf("u%d", i), "", false), net10(i)))
			done <- err
		}()
	}
	for held := 0; held < n; held++ {
		select {
		case <-arrived:
		case err := <-done:
			t.Fatalf("an attempt answered %v before every attempt was held", err)
		case <-time.After(time.Minute):
			t.Fatalf("after a minute %d attempts were held, want %d", held, n)
		}
	}
	right := func(c netip.Addr) pid.SignInStep { return from(ntlmStep(alicePassword, "alice", "", false), c) }
	r.signsIn(t, "alice from an address whose share is in flight", right(net10(0)), true)
	r.signsIn(t, "alice from B while the table is full of attempts in flight", right(addrB), false)
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions = %v, want 1", got)
	}
	close(gate)
	for range n {
		if err := <-done; !errors.Is(err, vfserr.ErrLogonFailure) {
			t.Errorf("an attempt held in flight: SignIn = %v, want %v", err, vfserr.ErrLogonFailure)
		}
	}
}

// evictionServer is a signing fixture with alice and a metrics registry.
func evictionServer(t *testing.T) (*resolving, *secrets.Provider) {
	t.Helper()
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	reg := metrics.New("test", "test")
	var err error
	if r.srv, err = identity.New(identity.Deps{Store: r.st, Clock: r.clock, Secrets: p, Metrics: reg}); err != nil {
		t.Fatal(err)
	}
	r.reg = reg
	r.applyNTLM(t, "NAS01", true)
	return r, p
}

// total is the named counter's value, summed over its labels.
func (r *resolving) total(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := r.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() == metrics.Namespace+"_"+name {
			for _, m := range mf.GetMetric() {
				n += m.GetCounter().GetValue()
			}
		}
	}
	return n
}

// Stop drops the throttle's table. Catches a table kept across Stop.
func TestThrottleDroppedAtStop(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	if err := r.srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)
	if err := r.srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.srv.ThrottleFootprint(); got != 0 {
		t.Errorf("the throttle holds %d bytes after Stop, want 0", got)
	}
}
