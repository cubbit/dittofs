package auth

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

var t0 = time.Unix(1_800_000_000, 0).UTC()

func src(s string) netip.Prefix { return sourceOf(netip.MustParseAddr(s), false) }

// Past its free attempts a count makes each further attempt wait twice as
// long as the last, from 1 s up to 5 min after its last counted attempt: a
// pair's and a source's from their fifth, an account's from its hundredth.
// A last attempt ahead of now, as store time is ahead of a node clock stepped
// back, owes no more than the backoff itself.
func TestDecideWaits(t *testing.T) {
	alice := entity.NewUser()
	for _, tc := range []struct {
		name  string
		k     kind
		count uint32
		since time.Duration // since the count's last counted attempt
		want  time.Duration
	}{
		{"pair at 4", pairCount, 4, 0, 0},
		{"pair at 5", pairCount, 5, 0, time.Second},
		{"pair at 5, 1 s on", pairCount, 5, time.Second, 0},
		{"pair at 6", pairCount, 6, 0, 2 * time.Second},
		{"pair at 6, 1 s on", pairCount, 6, time.Second, time.Second},
		{"pair at 7", pairCount, 7, 0, 4 * time.Second},
		{"pair at 13", pairCount, 13, 0, 256 * time.Second},
		{"pair at 14", pairCount, 14, 0, 5 * time.Minute},
		{"pair at 1000", pairCount, 1000, 0, 5 * time.Minute},
		{"source at 4", sourceCount, 4, 0, 0},
		{"source at 5", sourceCount, 5, 0, time.Second},
		{"source at 8", sourceCount, 8, 0, 8 * time.Second},
		{"account at 5", accountCount, 5, 0, 0},
		{"account at 99", accountCount, 99, 0, 0},
		{"account at 100", accountCount, 100, 0, time.Second},
		{"account at 101", accountCount, 101, 0, 2 * time.Second},
		{"pair at 5, last 1 h ahead", pairCount, 5, -time.Hour, time.Second},
		{"pair at 14, last 1 h ahead", pairCount, 14, -time.Hour, 5 * time.Minute},
	} {
		now := t0.Add(tc.since)
		var c counts
		c[tc.k] = entity.SignInFailures{Count: tc.count, Last: t0, Failed: t0, Nonce: 7}
		a := attempt{account: alice, source: src("192.0.2.1")}
		wait, raised := decide(a, c, now)
		if wait != tc.want {
			t.Errorf("%s: wait %v, want %v", tc.name, wait, tc.want)
		}
		if wait == 0 && raised[tc.k].Count != tc.count+1 {
			t.Errorf("%s: raised to %d, want %d", tc.name, raised[tc.k].Count, tc.count+1)
		}
		if wait > 0 && raised != (counts{}) {
			t.Errorf("%s: a wait raised %+v, want nothing", tc.name, raised)
		}
	}
}

// A caller on the local socket has no source, and so no source count and no
// pair; any other is counted on entity.SourceKey's source.
func TestSourceOf(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.1")
	if got := sourceOf(addr, true); got.IsValid() {
		t.Errorf("source of a local-socket caller: got %v, want none", got)
	}
	if got, want := sourceOf(addr, false), entity.SourceKey(addr); got != want {
		t.Errorf("source of %v: got %v, want %v", addr, got, want)
	}
	local := attempt{account: entity.NewUser(), source: sourceOf(addr, true)}
	for _, k := range []struct {
		k    kind
		name string
	}{{sourceCount, "source"}, {pairCount, "pair"}} {
		if _, ok := local.key(k.k); ok {
			t.Errorf("a local-socket attempt has a %s count, want none", k.name)
		}
	}
	if _, ok := local.key(accountCount); !ok {
		t.Errorf("a local-socket attempt has no account count, want one")
	}
}

// Every counted attempt moves a count's last attempt; a source's last
// settled failure moves only when an attempt creates the count or a failure
// settles, never by another raise or a takeback; an expired count is
// counted afresh.
func TestCountTimes(t *testing.T) {
	a := attempt{account: entity.NewUser(), source: src("192.0.2.1")}
	_, c := decide(a, counts{}, t0)
	if want := (entity.SignInFailures{Count: 1, Last: t0, Failed: t0}); c[sourceCount] != want {
		t.Errorf("source count created: got %+v, want %+v", c[sourceCount], want)
	}
	c[sourceCount].Nonce = 7 // as the write draws it
	if c[pairCount].Failed != (time.Time{}) || c[pairCount].Nonce != 0 {
		t.Errorf("pair count created: got %+v, want no Failed and no Nonce", c[pairCount])
	}
	t1 := t0.Add(time.Minute)
	_, c = decide(a, c, t1)
	if want := (entity.SignInFailures{Count: 2, Last: t1, Failed: t0, Nonce: 7}); c[sourceCount] != want {
		t.Errorf("source count raised: got %+v, want %+v", c[sourceCount], want)
	}
	t2 := t1.Add(time.Minute)
	if f, ok := settle(c[sourceCount], 7, t2); !ok || f.Failed != t2 || f.Last != t1 || f.Count != 2 {
		t.Errorf("failure settled: got %+v, %v; want Failed %v, Last %v, Count 2", f, ok, t2, t1)
	}
	for _, tc := range []struct {
		name string
		k    kind
		f    entity.SignInFailures
		now  time.Time
		live bool
	}{
		{"source 15 min less 1 ns after its failure", sourceCount, entity.SignInFailures{Count: 9, Last: t0.Add(14 * time.Minute), Failed: t0, Nonce: 7}, t0.Add(15*time.Minute - 1), true},
		{"source 15 min after its failure", sourceCount, entity.SignInFailures{Count: 9, Last: t0.Add(14 * time.Minute), Failed: t0, Nonce: 7}, t0.Add(15 * time.Minute), false},
		{"pair 24 h less 1 ns after its attempt", pairCount, entity.SignInFailures{Count: 9, Last: t0}, t0.Add(24*time.Hour - 1), true},
		{"pair 24 h after its attempt", pairCount, entity.SignInFailures{Count: 9, Last: t0}, t0.Add(24 * time.Hour), false},
		{"account a year on", accountCount, entity.SignInFailures{Count: 9, Last: t0}, t0.Add(365 * 24 * time.Hour), true},
	} {
		var c counts
		c[tc.k] = tc.f
		_, raised := decide(a, c, tc.now)
		want := uint32(1)
		if tc.live {
			want = 10
		}
		if raised[tc.k].Count != want {
			t.Errorf("%s: raised to %d, want %d", tc.name, raised[tc.k].Count, want)
		}
	}
}

// Two successes counted on one source after three failures, each counted
// after the other's count and taken back in either order, leave the count at
// 3 and its last settled failure at the third failure's.
func TestOverlappingSuccesses(t *testing.T) {
	failed := entity.SignInFailures{Count: 3, Last: t0, Failed: t0, Nonce: 7}
	a := attempt{source: src("192.0.2.1")}
	_, first := decide(a, counts{sourceCount: failed}, t0.Add(time.Second))
	_, second := decide(a, first, t0.Add(2*time.Second))
	for _, order := range [][2]int{{3, 4}, {4, 3}} {
		f := second[sourceCount]
		for _, at := range order {
			var ok bool
			if f, ok = takeBack(f, 7, t0.Add(time.Duration(at)*time.Second)); !ok {
				t.Fatalf("takeback at %ds: refused, want applied", at)
			}
		}
		if f.Count != 3 || f.Failed != t0 || f.Last != t0.Add(2*time.Second) {
			t.Errorf("takebacks at %v s: got %+v, want Count 3, Failed %v, Last %v", order, f, t0, t0.Add(2*time.Second))
		}
	}
}

// A settle or a takeback on a count other than the one its attempt raised,
// recreated since or expired, leaves it as found.
func TestSettleOnAnotherCount(t *testing.T) {
	fresh := entity.SignInFailures{Count: 1, Last: t0.Add(16 * time.Minute), Failed: t0.Add(16 * time.Minute), Nonce: 9}
	old := entity.SignInFailures{Count: 2, Last: t0, Failed: t0, Nonce: 7}
	for _, tc := range []struct {
		name  string
		f     entity.SignInFailures
		nonce uint64
		now   time.Time
	}{
		{"recreated", fresh, 7, t0.Add(17 * time.Minute)},
		{"expired", old, 7, t0.Add(15 * time.Minute)},
	} {
		if got, ok := settle(tc.f, tc.nonce, tc.now); ok || got != tc.f {
			t.Errorf("settle on a %s count: got %+v, %v; want %+v unwritten", tc.name, got, ok, tc.f)
		}
		if got, ok := takeBack(tc.f, tc.nonce, tc.now); ok || got != tc.f {
			t.Errorf("takeback on a %s count: got %+v, %v; want %+v unwritten", tc.name, got, ok, tc.f)
		}
	}
}

// fix is the throttle over a real store whose clock only the test moves,
// counted and faultable.
type fix struct {
	t     *testing.T
	clk   *sim.TestClock
	cp    kvstore.ControlPlane
	cnt   *kv.Counting
	fault *kvtest.FaultKV
	users int
}

// stores runs fn over the store on Badger in memory and under t.TempDir().
func stores(t *testing.T, fn func(t *testing.T, f *fix)) {
	for _, name := range []string{"memory", "disk"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clk := sim.NewTestClock(t0)
			// The ceiling reaches past every advance a test makes, so store
			// time follows the clock at once.
			opts := badgerkv.Options{Clock: clk.Now, CeilingAhead: 48 * time.Hour}
			if name == "disk" {
				opts.Dir = t.TempDir()
			}
			db, err := badgerkv.Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			cnt := &kv.Counting{KV: db}
			fault := kvtest.NewFaultKV(cnt)
			if err := kvstore.Init(t.Context(), fault, uuid.New()); err != nil {
				t.Fatal(err)
			}
			s, err := kvstore.Open(t.Context(), fault)
			if err != nil {
				t.Fatal(err)
			}
			fn(t, &fix{t: t, clk: clk, cp: s.ControlPlane(), cnt: cnt, fault: fault})
		})
	}
}

// advance moves the clock, and with it store time, d on.
func (f *fix) advance(d time.Duration) { f.clk.Advance(d) }

// user stores a user and returns its principal, an account a count may name.
func (f *fix) user() entity.Principal {
	f.t.Helper()
	f.users++
	u := entity.User{Principal: entity.NewUser(), Name: fmt.Sprintf("user%d", f.users)}
	if _, err := f.cp.PutUser(f.t.Context(), u, 0, entity.Alloc{}); err != nil {
		f.t.Fatal(err)
	}
	return u.Principal
}

func (f *fix) reserve(a attempt) (counted, error) { return reserve(f.t.Context(), f.cp, f.clk, a) }

// fail counts a and settles it as a failure.
func (f *fix) fail(a attempt) {
	f.t.Helper()
	c, err := f.reserve(a)
	if err != nil {
		f.t.Fatalf("failing attempt: %v", err)
	}
	f.write(func(tx entity.ControlPlaneTx) error { return settleFailure(tx, c) })
}

// succeed counts a and settles it as a success.
func (f *fix) succeed(a attempt) {
	f.t.Helper()
	c, err := f.reserve(a)
	if err != nil {
		f.t.Fatalf("succeeding attempt: %v", err)
	}
	f.write(func(tx entity.ControlPlaneTx) error { return takeBackSuccess(tx, c) })
}

func (f *fix) write(fn func(entity.ControlPlaneTx) error) {
	f.t.Helper()
	if err := f.cp.Write(f.t.Context(), entity.WriteMeta{}, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fix) count(k entity.SignInKey) (entity.SignInFailures, bool) {
	f.t.Helper()
	c, ok, err := f.cp.SignInFailures(f.t.Context(), k)
	if err != nil {
		f.t.Fatal(err)
	}
	return c, ok
}

// together runs n attempts at once and returns how many were let through;
// every other must be throttled.
func (f *fix) together(n int, a func(i int) attempt) int {
	f.t.Helper()
	var mu sync.Mutex
	var wg sync.WaitGroup
	passed := 0
	for i := range n {
		wg.Go(func() {
			_, err := f.reserve(a(i))
			var th *throttled
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				passed++
			case !errors.As(err, &th):
				f.t.Errorf("attempt %d: %v, want let through or throttled", i, err)
			}
		})
	}
	wg.Wait()
	return passed
}

// barrier holds the first n sign-in count writes until all n have reached
// theirs, so each has read the counts before any commits.
type barrier struct {
	t       *testing.T
	mu      sync.Mutex
	n       int
	release chan struct{}
}

func (b *barrier) At(op string, key []byte) (time.Duration, int, error) {
	if op != kvtest.OpSet || !strings.HasPrefix(string(key), "SF") {
		return 0, 0, nil
	}
	b.mu.Lock()
	held := b.n > 0
	if held {
		if b.n--; b.n == 0 {
			close(b.release)
		}
	}
	b.mu.Unlock()
	if held {
		select {
		case <-b.release:
		case <-time.After(10 * time.Second):
			b.t.Error("the held writes never all reached the barrier")
		}
	}
	return 0, 0, nil
}

// Five attempts at once on a pair whose wait has just passed, each holding
// its read of the counts until all five have read them: one is let through,
// as the counts allow, and the rest wait.
func TestConcurrentAttemptsPastTheBound(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{account: f.user(), source: src("192.0.2.1")}
		for range 5 {
			f.fail(a)
		}
		f.advance(time.Second + time.Millisecond)
		f.fault.SetFault(&barrier{t: t, n: 5, release: make(chan struct{})})
		if got := f.together(5, func(int) attempt { return a }); got != 1 {
			t.Errorf("attempts let through: got %d, want 1", got)
		}
		if p, _ := f.count(entity.SignInKey{Account: a.account, Source: a.source}); p.Count != 6 {
			t.Errorf("pair's count: got %d, want %d", p.Count, 6)
		}
	})
}

// 200 attempts at once on one account from 200 addresses: 100 are let
// through before the account's own wait holds the rest.
func TestAccountWaitAcrossSources(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		alice := f.user()
		got := f.together(200, func(i int) attempt {
			return attempt{account: alice, source: src(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}).String())}
		})
		if got != 100 {
			t.Errorf("attempts let through: got %d, want %d", got, 100)
		}
	})
}

// Three failures from one source, the clock moved 10 min on, then 50
// attempts at once for 50 accounts from it: two are let through, the fourth
// and the fifth, and the rest wait on the source.
func TestSourceWaitAcrossAccounts(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		s := src("192.0.2.1")
		for range 3 {
			f.fail(attempt{account: f.user(), source: s})
		}
		f.advance(10 * time.Minute)
		var accounts []entity.Principal
		for range 50 {
			accounts = append(accounts, f.user())
		}
		if got := f.together(50, func(i int) attempt { return attempt{account: accounts[i], source: s} }); got != 2 {
			t.Errorf("attempts let through: got %d, want 2", got)
		}
	})
}

// 50 attempts within a pair's wait are each throttled with the wait left,
// and none makes an Update or raises a count.
func TestThrottledAttemptWritesNothing(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{account: f.user(), source: src("192.0.2.1")}
		for range 5 {
			f.fail(a)
		}
		keys := []entity.SignInKey{{Account: a.account}, {Source: a.source}, {Account: a.account, Source: a.source}}
		var before []entity.SignInFailures
		for _, k := range keys {
			c, _ := f.count(k)
			before = append(before, c)
		}
		f.cnt.TakeUpdates()
		for i := range 50 {
			_, err := f.reserve(a)
			var th *throttled
			if !errors.As(err, &th) || th.RetryAfter() <= 0 || th.RetryAfter() > time.Second+time.Millisecond {
				t.Fatalf("attempt %d within the wait: %v, want throttled for about %v", i+1, err, time.Second)
			}
		}
		if u := f.cnt.TakeUpdates(); u.Updates != 0 {
			t.Errorf("Updates by throttled attempts: got %d, want 0", u.Updates)
		}
		for i, k := range keys {
			if c, _ := f.count(k); c != before[i] {
				t.Errorf("count %+v after the throttled attempts: got %+v, want %+v", k, c, before[i])
			}
		}
	})
}

// A write that fails refuses the attempt, and raises nothing.
func TestFailedWriteRefuses(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{account: f.user(), source: src("192.0.2.1")}
		boom := errors.New("commit lost")
		sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: kvtest.OpUpdate, Key: "*", Action: sim.FaultAction{Err: boom}})
		if err != nil {
			t.Fatal(err)
		}
		f.fault.SetFault(sched)
		if _, err := f.reserve(a); !errors.Is(err, boom) {
			t.Fatalf("attempt over a failing write: %v, want %v", err, boom)
		}
		f.fault.SetFault(nil)
		if c, ok := f.count(entity.SignInKey{Source: a.source}); ok {
			t.Errorf("source's count after the refused attempt: %+v, want absent", c)
		}
	})
}

// An attempt for a name that resolves to no account raises its source's
// count, one count and its expiry row, and no account's or pair's.
func TestUnknownNameCountsOnItsSource(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{source: src("192.0.2.1")}
		f.cnt.Take()
		if _, err := f.reserve(a); err != nil {
			t.Fatal(err)
		}
		if sets := f.cnt.Take().Sets; sets != 2 {
			t.Errorf("keys written: got %d, want 2, the source's count and its expiry row", sets)
		}
		if c, _ := f.count(entity.SignInKey{Source: a.source}); c.Count != 1 {
			t.Errorf("source's count: got %d, want 1", c.Count)
		}
	})
}

// A success resets its account's count and its pair's and takes back its
// raise of its source's; ResetAccount resets the account's alone.
func TestSuccessAndReset(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		alice := f.user()
		a := attempt{account: alice, source: src("192.0.2.1")}
		account, source, pair := entity.SignInKey{Account: alice}, entity.SignInKey{Source: a.source}, entity.SignInKey{Account: alice, Source: a.source}
		for range 3 {
			f.fail(a)
		}
		f.succeed(a)
		for _, k := range []entity.SignInKey{account, pair} {
			if c, ok := f.count(k); ok {
				t.Errorf("count %+v after a success: %+v, want absent", k, c)
			}
		}
		if c, _ := f.count(source); c.Count != 3 {
			t.Errorf("source's count after a success: got %d, want 3", c.Count)
		}
		f.fail(a)
		f.write(func(tx entity.ControlPlaneTx) error { return ResetAccount(tx, alice) })
		if c, ok := f.count(account); ok {
			t.Errorf("account's count after its reset: %+v, want absent", c)
		}
		if c, _ := f.count(pair); c.Count != 1 {
			t.Errorf("pair's count after the account's reset: got %d, want 1", c.Count)
		}
	})
}

// Three failures from one source, then successes from it 10 min on: the
// count keeps its last settled failure at the third failure's, and is
// forgotten 15 min after it, the next attempt counted afresh.
func TestSourceForgottenWhileSuccessesCome(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		s := src("192.0.2.1")
		for range 3 {
			f.fail(attempt{account: f.user(), source: s})
		}
		third, _ := f.count(entity.SignInKey{Source: s})
		bob := attempt{account: f.user(), source: s}
		f.advance(10 * time.Minute)
		f.succeed(bob)
		if c, _ := f.count(entity.SignInKey{Source: s}); c.Count != 3 || c.Failed != third.Failed || c.Nonce != third.Nonce {
			t.Errorf("source's count after a success: got %+v, want Count 3, Failed %v, Nonce %d", c, third.Failed, third.Nonce)
		}
		f.advance(5*time.Minute + time.Millisecond)
		f.succeed(bob)
		if c, _ := f.count(entity.SignInKey{Source: s}); c.Count != 0 || c.Nonce == third.Nonce {
			t.Errorf("source's count 15 min after its last failure: got %+v, want counted afresh and taken back to 0", c)
		}
	})
}

// A source's count whose attempt never settles expires 15 min after the
// attempt that created it; a pair's 24 h after its last attempt.
func TestUnsettledAndPairCountsExpire(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		unsettled := attempt{source: src("192.0.2.1")}
		if _, err := f.reserve(unsettled); err != nil {
			t.Fatal(err)
		}
		a := attempt{account: f.user(), source: src("192.0.2.2")}
		for range 5 {
			f.fail(a)
		}
		f.advance(15*time.Minute + time.Millisecond)
		if _, err := f.reserve(unsettled); err != nil {
			t.Fatal(err)
		}
		if c, _ := f.count(entity.SignInKey{Source: unsettled.source}); c.Count != 1 {
			t.Errorf("unsettled source's count 15 min after its creation: got %d, want counted afresh at 1", c.Count)
		}
		if _, err := f.reserve(a); err != nil {
			t.Fatal(err)
		}
		if c, _ := f.count(entity.SignInKey{Account: a.account, Source: a.source}); c.Count != 6 {
			t.Errorf("pair's count 15 min after its last attempt: got %d, want %d", c.Count, 6)
		}
		f.advance(24*time.Hour + time.Millisecond)
		if _, err := f.reserve(a); err != nil {
			t.Fatalf("pair's attempt 24 h after its last: %v, want let through", err)
		}
		if c, _ := f.count(entity.SignInKey{Account: a.account, Source: a.source}); c.Count != 1 {
			t.Errorf("pair's count 24 h after its last attempt: got %d, want counted afresh at 1", c.Count)
		}
	})
}

// A source's second failure 10 min after its first moves its last settled
// failure, so 5 min past the first's 15 the count is still live.
func TestSettledFailureKeepsSourceLive(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{source: src("192.0.2.1")}
		f.fail(a)
		f.advance(10 * time.Minute)
		f.fail(a)
		f.advance(5*time.Minute + time.Millisecond)
		if _, err := f.reserve(a); err != nil {
			t.Fatal(err)
		}
		if c, _ := f.count(entity.SignInKey{Source: a.source}); c.Count != 3 {
			t.Errorf("source's count 15 min after its first failure, 5 min after its second: got %d, want 3", c.Count)
		}
	})
}

// An attempt the store could not answer takes back its raise of its source's
// count and leaves its account's and its pair's as raised.
func TestTakeBackRaiseLeavesOtherCounts(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{account: f.user(), source: src("192.0.2.1")}
		for range 3 {
			f.fail(a)
		}
		c, err := f.reserve(a)
		if err != nil {
			t.Fatal(err)
		}
		f.write(func(tx entity.ControlPlaneTx) error { return takeBackRaise(tx, c) })
		for _, tc := range []struct {
			name string
			k    entity.SignInKey
			want uint32
		}{
			{"source's", entity.SignInKey{Source: a.source}, 3},
			{"account's", entity.SignInKey{Account: a.account}, 4},
			{"pair's", entity.SignInKey{Account: a.account, Source: a.source}, 4},
		} {
			if got, _ := f.count(tc.k); got.Count != tc.want {
				t.Errorf("%s count after a raise taken back: got %d, want %d", tc.name, got.Count, tc.want)
			}
		}
	})
}

// The write decides at store time: with the node clock an hour ahead, the
// untracked check passes the sixth attempt and the write still refuses it.
func TestWriteDecidesAtStoreTime(t *testing.T) {
	stores(t, func(t *testing.T, f *fix) {
		a := attempt{account: f.user(), source: src("192.0.2.1")}
		for range 5 {
			f.fail(a)
		}
		_, err := reserve(t.Context(), f.cp, sim.NewTestClock(f.clk.Now().Add(time.Hour)), a)
		var th *throttled
		if !errors.As(err, &th) {
			t.Errorf("sixth attempt with the node clock an hour ahead: %v, want throttled by store time", err)
		}
	})
}
