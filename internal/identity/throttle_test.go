package identity_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metrics"
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

// A full table evicts its oldest pair, counted: alice's throttled pair is
// the first of 65 537, and once evicted she signs in from it. A 64 KiB name
// costs the table no more than a short one.
func TestThrottleEvictsTheOldestPair(t *testing.T) {
	r, p := ntlmServer(t, "NAS01")
	r.ntlmUser(t, p, "alice", "1500", alicePassword)
	reg := metrics.New("test", "test")
	var err error
	if r.srv, err = identity.New(identity.Deps{Store: r.st, Clock: r.clock, Secrets: p, Metrics: reg}); err != nil {
		t.Fatal(err)
	}
	r.reg = reg
	r.applyNTLM(t, "NAS01", true)
	r.fails(t, 10, wrong(addrA), vfserr.ErrLogonFailure)

	short := r.srv.ThrottleFootprint()
	long := strings.Repeat("x", 64<<10)
	r.fails(t, 1, func() pid.SignInStep { return from(ntlmStep("wrong", long, "", false), addrA) }, vfserr.ErrLogonFailure)
	if got := r.srv.ThrottleFootprint() - short; got != short {
		t.Errorf("a 64 KiB name's pair holds %d bytes, want a short name's %d", got, short)
	}

	// The remaining pairs are refused for NTLM being off, which reads
	// nothing, from one address each.
	r.applyNTLM(t, "NAS01", false)
	for i := range identity.ThrottleBound - 1 {
		c := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		r.fails(t, 1, func() pid.SignInStep { return from(ntlmStep("wrong", "bob", "", false), c) }, vfserr.ErrLogonFailure)
	}
	if got := r.total(t, "identity_logon_throttle_evictions_total"); got != 1 {
		t.Errorf("evictions after %d pairs = %v, want 1", identity.ThrottleBound+1, got)
	}
	r.applyNTLM(t, "NAS01", true)
	r.signsIn(t, "alice from A once her pair is evicted", from(ntlmStep(alicePassword, "alice", "", false), addrA), false)
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
