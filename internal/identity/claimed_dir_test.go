package identity_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity/directory"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// hostA is the client address the claims below come from unless a test
// names another.
var hostA = netip.MustParseAddr("192.0.2.1")

// host is the client address 10.0.i/256.i%256, one throttle unit each.
func host(i int) netip.Addr { return netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i % 256)}) }

// claimFrom is claim, sent from client.
func claimFrom(client netip.Addr, uid, gid uint32, gids ...uint32) pid.Identity {
	id := claim(uid, gid, gids...)
	id.Client = client
	return id
}

// resolveClaim resolves id on a share squashing nobody.
func (e *dirPrincipals) resolveClaim(id pid.Identity) (*pid.Caller, error) {
	return e.s.Resolve(context.Background(), id, fileShare(), pid.ReadOnlyNotNarrowed)
}

// sent is how many lookups of question a query answered, found or not.
func (e *dirPrincipals) sent(t *testing.T, question string) float64 {
	t.Helper()
	mfs, err := e.reg.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	n := 0.0
	for _, mf := range mfs {
		if mf.GetName() != metrics.Namespace+"_identity_directory_lookups_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["question"] == question && (labels["result"] == "queried" || labels["result"] == "not_found") {
				n += m.GetCounter().GetValue()
			}
		}
	}
	return n
}

// numbered adds the directory user name holding uidNumber uid, attrs set on
// it.
func (e *dirPrincipals) numbered(t *testing.T, name string, uid uint32, attrs map[string][][]byte) string {
	t.Helper()
	all := map[string][][]byte{"uidNumber": attr(strconv.FormatUint(uint64(uid), 10))}
	for k, v := range attrs {
		all[k] = v
	}
	return e.d.AddUser(t, name, all)
}

// mints is identity_mints_total{kind}.
func (e *dirPrincipals) mints(t *testing.T, kind string) float64 {
	t.Helper()
	return e.counter(t, "identity_mints_total", "kind", kind)
}

// RFC 18 §10's "AUTH_SYS from the directory": a claimed uid no record holds
// whose uidNumber a directory entry holds makes the caller that entry's
// minted principal, holding the entry's uid, unauthenticated; a claimed gid
// a directory group holds is minted as that group, the caller's group. A
// uid the directory does not hold acts as anonymous and writes nothing.
// Catches a directory never asked for a claimed uid or gid, a mint on the
// client's word, and a minted caller authenticated.
func TestClaimedUIDFromTheDirectory(t *testing.T) {
	e := newAD(t)
	e.numbered(t, "bob", 5000, nil)
	e.d.AddGroup(t, "ops", map[string][][]byte{"gidNumber": attr("7000")})

	before := e.pgen(t)
	c, err := e.resolveClaim(claimFrom(hostA, 4242, 0))
	if err != nil || c.Mapped != entity.AnonymousPrincipal {
		t.Fatalf("a uid the directory lacks = %+v, %v; want Mapped anonymous", c, err)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d after a uid the directory lacks, want %d: it wrote", got, before)
	}
	if got := e.sent(t, "uid"); got != 1 {
		t.Errorf("uid questions sent %v, want 1", got)
	}

	c, err = e.resolveClaim(claimFrom(hostA, 5000, 7000))
	if err != nil {
		t.Fatalf("bob's uidNumber: %v", err)
	}
	bob := e.mustFind(t, qualified("bob"), false)
	ops := e.groupOf(t, "ops")
	if c.Principal != bob || c.Authenticated || c.Group != ops {
		t.Errorf("bob's claimed caller: Principal %v, Authenticated %v, Group %v; want bob %v, false, ops %v", c.Principal, c.Authenticated, c.Group, bob, ops)
	}
	if got := e.num(t, bob, pid.SchemeUID); got != 5000 {
		t.Errorf("bob minted with uid %d, want the entry's 5000", got)
	}
	if got := e.num(t, ops, pid.SchemeGID); got != 7000 {
		t.Errorf("ops minted with gid %d, want the entry's 7000", got)
	}
	if got := e.sent(t, "gid"); got != 1 {
		t.Errorf("gid questions sent %v, want 1", got)
	}
}

// RFC 18 §10's "lookup cap": one address sending 101 distinct unknown uids
// within a minute sends 100 uid queries; the 101st is refused ErrUnavailable,
// counted lookup_cap, sends nothing, and is not anonymous. A uid the cache
// answers fresh costs nothing, so a repeated unknown uid is not refused.
// Another address is not refused. Catches the cap removed, a refusal mapped
// to anonymous, and a cached answer charged.
func TestClaimedLookupCap(t *testing.T) {
	e := newAD(t)
	for i := range 100 {
		if c, err := e.resolveClaim(claimFrom(hostA, uint32(10000+i), 0)); err != nil || c.Mapped != entity.AnonymousPrincipal {
			t.Fatalf("unknown uid %d of 100: %+v, %v; want anonymous", i, c, err)
		}
	}
	queries := len(e.f.Queries())
	c, err := e.resolveClaim(claimFrom(hostA, 10100, 0))
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("the 101st unknown uid within the minute = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := len(e.f.Queries()); got != queries {
		t.Errorf("queries sent for the refused uid %d, want 0", got-queries)
	}
	if got := e.sent(t, "uid"); got != 100 {
		t.Errorf("uid questions sent %v, want 100", got)
	}
	if got := e.refusals(t, "lookup_cap"); got != 1 {
		t.Errorf("refusals{lookup_cap} = %v, want 1", got)
	}
	if c, err := e.resolveClaim(claimFrom(hostA, 10000, 0)); err != nil || c.Mapped != entity.AnonymousPrincipal {
		t.Errorf("a repeated unknown uid the cache answers = %+v, %v; want anonymous, uncharged", c, err)
	}
	if c, err := e.resolveClaim(claimFrom(netip.MustParseAddr("192.0.2.2"), 10100, 0)); err != nil || c.Mapped != entity.AnonymousPrincipal {
		t.Errorf("the same uid from another address = %+v, %v; want anonymous", c, err)
	}
}

// The lookup cap admits a lookup only when fewer than 100 fall in the last
// 60 seconds: one at 0 s and 99 at 59 s leave room for exactly one at 60 s,
// and for the next 99 only from 119 s. Catches a fixed window or a token
// bucket, which admit more than 100 within some 60 seconds.
func TestClaimedLookupCapSlides(t *testing.T) {
	e := newAD(t)
	uid := uint32(20000)
	ask := func() error {
		uid++
		_, err := e.resolveClaim(claimFrom(hostA, uid, 0))
		return err
	}
	expect := func(at string, n int, want error) {
		t.Helper()
		for i := range n {
			if err := ask(); !errors.Is(err, want) && (want != nil || err != nil) {
				t.Fatalf("lookup %d of %d at %s: %v, want %v", i+1, n, at, err, want)
			}
		}
	}
	expect("0 s", 1, nil)
	e.clock.Advance(59 * time.Second)
	expect("59 s", 99, nil)
	expect("59 s, past 100", 1, vfserr.ErrUnavailable)
	e.clock.Advance(time.Second)
	expect("60 s", 1, nil)
	expect("60 s, the second", 1, vfserr.ErrUnavailable)
	e.clock.Advance(58 * time.Second)
	expect("118 s", 1, vfserr.ErrUnavailable)
	e.clock.Advance(time.Second)
	expect("119 s", 99, nil)
	expect("119 s, past 99", 1, vfserr.ErrUnavailable)
}

// A mint's closure query, and the entry read that follows it, are charged
// to the client's lookup cap: 34 directory users sharing one group, from one
// address, mint the first 33, each costing a uid, a closure and an entry
// query, and the 34th, its uid the address's 100th lookup, is refused
// ErrUnavailable at its closure query, the 101st, minting nothing. Catches
// the closure query or the entry read uncharged, which lets one address send
// about 200 or 300 queries a minute.
func TestClaimedLookupCapChargesTheClosure(t *testing.T) {
	e := newAD(t)
	var dns [][]byte
	for i := range 34 {
		dns = append(dns, []byte(e.numbered(t, fmt.Sprintf("u%02d", i), uint32(30000+i), nil)))
	}
	e.d.AddGroup(t, "shared", map[string][][]byte{"member": dns})
	for i := range 33 {
		if c, err := e.resolveClaim(claimFrom(hostA, uint32(30000+i), 0)); err != nil || c.Mapped == entity.AnonymousPrincipal {
			t.Fatalf("directory user %d of 33: %+v, %v; want its minted principal", i, c, err)
		}
	}
	users := e.mints(t, "user")
	if c, err := e.resolveClaim(claimFrom(hostA, 30033, 0)); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("the 34th directory user, its closure the 101st lookup = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := e.mints(t, "user"); got != users {
		t.Errorf("users minted by the refused call %v, want 0", got-users)
	}
	for q, want := range map[string]float64{"uid": 34, "groups": 33, "entry_id": 33} {
		if got := e.sent(t, q); got != want {
			t.Errorf("%s questions sent %v, want %v", q, got, want)
		}
	}
}

// Past the lookup cap, a claim of a minted directory principal's uid with
// ten supplementary gids no principal holds is refused ErrUnavailable,
// counted lookup_cap, no query sent for them, as RFC 18 §7.3 refuses the
// uid and the primary gid; once the address's window has room, the same
// claim asks for them and is admitted. Catches a supplementary gid left out
// past the cap, which drops a group the directory holds under that number,
// minted under another, and so a deny entry naming it.
func TestClaimedSupplementaryGIDsPastTheCap(t *testing.T) {
	e := newAD(t)
	e.numbered(t, "bob", 5000, nil)
	bob := e.mustFind(t, qualified("bob"), true)
	gids := make([]uint32, 10)
	for i := range gids {
		gids[i] = uint32(40000 + i)
	}
	// bob's first claim reads his entry, one lookup, so the claims past the
	// cap find it fresh and only the gids can refuse them.
	if _, err := e.resolveClaim(claimFrom(hostA, 5000, 0)); err != nil {
		t.Fatalf("bob's first claim: %v", err)
	}
	for i := range 99 {
		if _, err := e.resolveClaim(claimFrom(hostA, uint32(10000+i), 0)); err != nil {
			t.Fatalf("unknown uid %d of 99: %v", i, err)
		}
	}
	c, err := e.resolveClaim(claimFrom(hostA, 5000, 0, gids...))
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Fatalf("bob's claim with ten unknown gids past the cap = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := e.refusals(t, "lookup_cap"); got != 1 {
		t.Errorf("refusals{lookup_cap} = %v, want 1", got)
	}
	if got := e.sent(t, "gid"); got != 0 {
		t.Errorf("gid questions sent past the cap %v, want 0", got)
	}
	if c, err := e.resolveClaim(claimFrom(hostA, 5000, 0)); err != nil || c.Principal != bob {
		t.Errorf("bob's claim past the cap with no unknown gid = %+v, %v; want bob, his entry fresh", c, err)
	}
	e.clock.Advance(time.Minute)
	c, err = e.resolveClaim(claimFrom(hostA, 5000, 0, gids...))
	if err != nil || c.Principal != bob {
		t.Fatalf("bob's claim once the window has room = %+v, %v; want bob", c, err)
	}
	if got := e.sent(t, "gid"); got != 10 {
		t.Errorf("gid questions sent once the window has room %v, want 10", got)
	}
}

// A minted directory principal's entry read, once its answer is past the
// TTL, is a lookup its Claimed caller triggers, charged to the client's cap:
// with the address's 100 lookups spent, a claim of the principal's uid is
// refused ErrUnavailable, counted lookup_cap, and sends no entry query.
// Catches the entry read uncharged, which lets an address read one entry per
// minted directory user every TTL on top of its 100.
func TestClaimedLookupCapChargesTheEntryRead(t *testing.T) {
	e := newAD(t)
	e.numbered(t, "bob", 5000, nil)
	if c, err := e.resolveClaim(claimFrom(hostA, 5000, 0)); err != nil || c.Mapped == entity.AnonymousPrincipal {
		t.Fatalf("bob's first claim = %+v, %v; want bob minted", c, err)
	}
	e.clock.Advance(5*time.Minute + time.Second)
	for i := range 100 {
		if _, err := e.resolveClaim(claimFrom(hostA, uint32(10000+i), 0)); err != nil {
			t.Fatalf("unknown uid %d of 100: %v", i, err)
		}
	}
	queries := len(e.f.Queries())
	c, err := e.resolveClaim(claimFrom(hostA, 5000, 0))
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("bob's claim past the TTL, the address's 101st lookup = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := e.refusals(t, "lookup_cap"); got != 1 {
		t.Errorf("refusals{lookup_cap} = %v, want 1", got)
	}
	if got := len(e.f.Queries()); got != queries {
		t.Errorf("queries sent for the refused entry read %d, want 0", got-queries)
	}
}

// The node's mint cap: within 60 seconds, the 101st record minted for
// claims, whatever their addresses, is refused ErrUnavailable and writes
// nothing; 60 seconds later room is freed and it mints. Catches the mint cap
// removed, or kept per address.
func TestClaimedMintCap(t *testing.T) {
	e := newAD(t)
	for i := range 100 {
		e.numbered(t, fmt.Sprintf("m%03d", i), uint32(50000+i), nil)
	}
	// The first user's batch holds Domain Users too, so 99 users mint 100
	// records.
	for i := range 99 {
		if _, err := e.resolveClaim(claimFrom(host(i), uint32(50000+i), 0)); err != nil {
			t.Fatalf("user %d of 99: %v", i, err)
		}
	}
	if got := e.mints(t, "user") + e.mints(t, "group"); got != 100 {
		t.Fatalf("records minted %v, want 100", got)
	}
	before := e.pgen(t)
	if c, err := e.resolveClaim(claimFrom(host(99), 50099, 0)); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("the 101st record within the minute = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d after a mint refused at the cap, want %d", got, before)
	}
	e.clock.Advance(time.Minute)
	if c, err := e.resolveClaim(claimFrom(host(99), 50099, 0)); err != nil || c.Mapped == entity.AnonymousPrincipal {
		t.Errorf("the 101st record a minute later = %+v, %v; want minted", c, err)
	}
}

// A user in 300 directory groups, with Domain Users already minted and the
// mint cap's ring empty, has 100 of its groups minted and its call refused,
// then the next 100 and the next 100 as room frees, each refused; the call
// after that mints the user, and every group of its closure is held. No 60
// seconds mint more than 100 records, and no call resolves it before its
// whole closure is minted. Catches the cap charged per lookup rather than
// per record, a user resolved with part of its closure, or one never
// mintable.
func TestClaimedMintCapMintsAClosureInSteps(t *testing.T) {
	e := newAD(t)
	e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, "Domain Users")), true)
	carol := e.numbered(t, "carol", 6000, nil)
	var names []string
	for i := range 300 {
		names = append(names, fmt.Sprintf("g%03d", i))
		e.d.AddGroup(t, names[i], map[string][][]byte{"member": attr(carol)})
	}
	groups := e.mints(t, "group")
	for step := range 3 {
		if c, err := e.resolveClaim(claimFrom(hostA, 6000, 0)); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
			t.Errorf("carol's call %d = %+v, %v; want nil, ErrUnavailable", step+1, c, err)
		}
		if got := e.mints(t, "group") - groups; got != float64(100*(step+1)) {
			t.Errorf("groups minted after carol's call %d: %v, want %d", step+1, got, 100*(step+1))
		}
		if got := e.mints(t, "user"); got != 0 {
			t.Errorf("users minted after carol's call %d: %v, want 0", step+1, got)
		}
		e.clock.Advance(time.Minute)
	}
	c, err := e.resolveClaim(claimFrom(hostA, 6000, 0))
	if err != nil || c.Mapped == entity.AnonymousPrincipal {
		t.Fatalf("carol's fourth call = %+v, %v; want her minted principal", c, err)
	}
	if got := e.mints(t, "user"); got != 1 {
		t.Errorf("users minted after carol's fourth call: %v, want 1", got)
	}
	for _, n := range names {
		e.groupOf(t, n)
	}
}

// A Kerberos sign-in of a directory user in 300 groups mints the user and
// its whole closure at once with the mint cap's ring full, uncharged.
// Catches the cap applied to a proven sign-in through the shared mint path.
func TestSignInMintsPastTheClaimedMintCap(t *testing.T) {
	e := newAD(t)
	realm := e.kerberos(t)
	e.mustFind(t, sidOf(t, e.entry(t, directory.ByGroupName, "Domain Users")), true)
	filler := e.numbered(t, "filler", 6100, nil)
	alice := e.d.AddUser(t, "alice", nil)
	for i := range 100 {
		e.d.AddGroup(t, fmt.Sprintf("f%03d", i), map[string][][]byte{"member": attr(filler)})
	}
	for i := range 300 {
		e.d.AddGroup(t, fmt.Sprintf("a%03d", i), map[string][][]byte{"member": attr(alice)})
	}
	if _, err := e.resolveClaim(claimFrom(hostA, 6100, 0)); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("filler's claim filling the ring: %v, want ErrUnavailable", err)
	}
	groups := e.mints(t, "group")
	e.signedIn(t, realm, "alice")
	if got := e.mints(t, "group") - groups; got != 300 {
		t.Errorf("groups alice's sign-in minted with the ring full: %v, want 300", got)
	}
}

// RFC 18 §10's "mint from a claim, guarded". Each of 10^4 uids the directory
// lacks, sent from 100 addresses, 100 each within the minute, acts as
// anonymous and writes nothing. 500 uids it holds, sent from 50 addresses at
// once, mint at most the node cap's 100 records within the minute; every
// other call is refused retryably, never anonymous. Catches a mint on the
// client's word, and a scan bounded only per address.
func TestClaimedMintFromAClaimGuarded(t *testing.T) {
	e := newAD(t)
	before := e.pgen(t)
	for a := range 100 {
		for i := range 100 {
			c, err := e.resolveClaim(claimFrom(host(a), uint32(100000+a*100+i), 0))
			if err != nil || c.Mapped != entity.AnonymousPrincipal {
				t.Fatalf("unknown uid %d from %v = %+v, %v; want anonymous", 100000+a*100+i, host(a), c, err)
			}
		}
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d after 10^4 unknown uids, want %d: they wrote", got, before)
	}

	for i := range 500 {
		e.numbered(t, fmt.Sprintf("h%03d", i), uint32(200000+i), nil)
	}
	e.clock.Advance(time.Minute) // the 100 addresses' lookups behind
	var wg sync.WaitGroup
	var mu sync.Mutex
	var bad []string
	for a := range 50 {
		wg.Go(func() {
			for i := range 10 {
				uid := uint32(200000 + a*10 + i)
				c, err := e.resolveClaim(claimFrom(host(1000+a), uid, 0))
				if err == nil && c.Mapped != entity.AnonymousPrincipal || errors.Is(err, vfserr.ErrUnavailable) && c == nil {
					continue
				}
				mu.Lock()
				bad = append(bad, fmt.Sprintf("uid %d: %+v, %v", uid, c, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(bad) > 0 {
		t.Errorf("claims of held uids neither minted nor refused retryably: %s", strings.Join(bad, "; "))
	}
	if got := e.mints(t, "user") + e.mints(t, "group"); got > 100 || got == 0 {
		t.Errorf("records minted within the minute %v, want 1 to 100", got)
	}
}

// A principal minted from a claim follows its entry once the TTL has
// passed: renamed, it keeps its PrincipalID under the new name; disabled or
// deleted, its claim is refused ErrAccess. Catches a record bound to the
// entry's name, or a claimed caller that skips the directory step.
func TestClaimedMintFollowsTheEntry(t *testing.T) {
	e := newAD(t)
	dns := map[string]string{}
	for i, n := range []string{"ren", "dis", "del"} {
		dns[n] = e.numbered(t, n, uint32(7000+i), nil)
	}
	ps := map[string]entity.Principal{}
	for i, n := range []string{"ren", "dis", "del"} {
		c, err := e.resolveClaim(claimFrom(hostA, uint32(7000+i), 0))
		if err != nil {
			t.Fatalf("%s's claim: %v", n, err)
		}
		ps[n] = c.Principal
	}
	e.rename(dns["ren"], "renamed")
	e.d.Modify(dns["dis"], map[string][][]byte{"userAccountControl": attr("514")})
	e.d.Delete(dns["del"])
	e.clock.Advance(5*time.Minute + time.Second)
	c, err := e.resolveClaim(claimFrom(hostA, 7000, 0))
	if err != nil || c.Principal != ps["ren"] || e.nameOf(t, c.Principal) != "renamed" {
		t.Errorf("the renamed entry's claim = %+v, %v; want principal %v named renamed", c, err, ps["ren"])
	}
	for i, n := range []string{"dis", "del"} {
		if c, err := e.resolveClaim(claimFrom(hostA, uint32(7001+i), 0)); !errors.Is(err, vfserr.ErrAccess) || c != nil {
			t.Errorf("the %s entry's claim past the TTL = %+v, %v; want nil, ErrAccess", n, c, err)
		}
	}
}

// A directory that fails a claimed lookup refuses the call ErrUnavailable,
// counted directory_unavailable, and writes nothing: for a uid never asked,
// and for one whose "no such entry" answer has expired, which is never
// served stale. A uid two directory entries hold is refused ErrAccess, never
// minted. Catches a directory error read as "no such entry", which would act
// as anonymous.
func TestClaimedDirectoryFaults(t *testing.T) {
	e := newAD(t)
	e.numbered(t, "dup1", 8000, nil)
	e.numbered(t, "dup2", 8000, nil)
	if _, err := e.resolveClaim(claimFrom(hostA, 8001, 0)); err != nil {
		t.Fatalf("an unknown uid: %v", err)
	}
	before := e.pgen(t)
	if c, err := e.resolveClaim(claimFrom(hostA, 8000, 0)); !errors.Is(err, vfserr.ErrAccess) || c != nil {
		t.Errorf("a uid two entries hold = %+v, %v; want nil, ErrAccess", c, err)
	}
	e.fail(t, sim.Rule{Op: "search", Key: "*uidNumber*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	e.clock.Advance(31 * time.Second)
	for _, uid := range []uint32{8002, 8001} {
		if c, err := e.resolveClaim(claimFrom(hostA, uid, 0)); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
			t.Errorf("uid %d with the directory failing = %+v, %v; want nil, ErrUnavailable", uid, c, err)
		}
	}
	if got := e.refusals(t, "directory_unavailable"); got != 2 {
		t.Errorf("refusals{directory_unavailable} = %v, want 2", got)
	}
	if got := e.pgen(t); got != before {
		t.Errorf("PGEN %d after the refused calls, want %d", got, before)
	}
}

// A principal minted from a claim, its directory failing, is served from
// its last answer past the TTL within the stale grace, and refused
// ErrUnavailable past the grace, never anonymous. Catches a directory error
// read as "no such entry", which would refuse it as gone, or an answer
// served without bound.
func TestClaimedDirectoryPrincipalWithinTheGrace(t *testing.T) {
	e := newAD(t)
	e.numbered(t, "bob", 8100, nil)
	c, err := e.resolveClaim(claimFrom(hostA, 8100, 0))
	if err != nil {
		t.Fatalf("bob's claim: %v", err)
	}
	bob := c.Principal
	e.fail(t, sim.Rule{Op: "search", Key: "*", Action: sim.FaultAction{Err: errors.New("injected directory fault")}})
	e.clock.Advance(5*time.Minute + time.Second)
	if c, err := e.resolveClaim(claimFrom(hostA, 8100, 0)); err != nil || c.Principal != bob {
		t.Errorf("bob's claim past the TTL within the grace = %+v, %v; want bob from the stale answer", c, err)
	}
	e.clock.Advance(15 * time.Minute)
	if c, err := e.resolveClaim(claimFrom(hostA, 8100, 0)); !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("bob's claim past the grace = %+v, %v; want nil, ErrUnavailable", c, err)
	}
}

// Under ManageGIDs, the closure query a claimed directory principal's
// directory step sends is a lookup its address is charged for: bob, minted
// by a first claim, claims again from the same address past the TTL, after
// 99 unknown uids. His entry read is the 100th lookup, and his closure
// query, the 101st, is refused ErrUnavailable, counted lookup_cap, and never
// sent. Catches the closure read uncharged, which lets an address send one
// more query per minted directory user every TTL on top of its 100.
func TestClaimedManageGIDsChargesTheClosure(t *testing.T) {
	e := newAD(t)
	managed := fileShare()
	managed.ManageGIDs = true
	resolve := func(id pid.Identity) (*pid.Caller, error) {
		return e.s.Resolve(context.Background(), id, managed, pid.ReadOnlyNotNarrowed)
	}
	e.numbered(t, "bob", 5000, nil)
	if c, err := resolve(claimFrom(hostA, 5000, 0)); err != nil || c.Mapped == entity.AnonymousPrincipal {
		t.Fatalf("bob's first claim = %+v, %v; want bob minted", c, err)
	}
	e.clock.Advance(5*time.Minute + time.Second)
	for i := range 99 {
		if _, err := resolve(claimFrom(hostA, uint32(10000+i), 0)); err != nil {
			t.Fatalf("unknown uid %d of 99: %v", i, err)
		}
	}
	groups := e.sent(t, "groups")
	c, err := resolve(claimFrom(hostA, 5000, 0))
	if !errors.Is(err, vfserr.ErrUnavailable) || c != nil {
		t.Errorf("bob's claim past the TTL, his closure query the address's 101st lookup = %+v, %v; want nil, ErrUnavailable", c, err)
	}
	if got := e.refusals(t, "lookup_cap"); got != 1 {
		t.Errorf("refusals{lookup_cap} = %v, want 1", got)
	}
	if got := e.sent(t, "groups") - groups; got != 0 {
		t.Errorf("closure queries sent for the refused call %v, want 0", got)
	}
}
