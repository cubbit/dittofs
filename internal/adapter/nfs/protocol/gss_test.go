package protocol

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// testClock is the table's clock, which a test advances.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// start is where every test clock starts.
var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// testToken is the INIT token length tests reserve for.
const testToken = 1000

func principal(n byte) vfs.Principal { return vfs.Principal{Kind: 1, ID: [16]byte{n}} }

// gssCtx is a context for principal p whose ticket ends at end, zero for
// none.
func gssCtx(p vfs.Principal, end time.Time) *gss.Context {
	return &gss.Context{
		Evidence: vfs.Evidence{Name: fmt.Sprintf("user%d@TEST.REALM", p.ID[0]), Principal: p, Expires: end},
		Keys:     vfs.Keys{Session: make([]byte, 32), APRep: make([]byte, 120)},
	}
}

// newTable is a table of at most max contexts over a budget of ceiling
// bytes, on a fresh test clock.
func newTable(ceiling int64, max int) (*gssTable, *resources.Budget, *testClock) {
	b := resources.NewBudget("connection", ceiling)
	clk := &testClock{now: start}
	return newGSSTable(b, clk, max), b, clk
}

// add reserves for a testToken INIT and inserts g, as the control table
// does once SignIn has answered.
func add(t *testing.T, tbl *gssTable, g *gss.Context) [gssHandleLen]byte {
	t.Helper()
	r, err := tbl.reserve(testToken)
	if err != nil {
		t.Fatalf("reserve(%d) = %v, want a reservation", testToken, err)
	}
	return tbl.insert(r, g)
}

// has reports whether tbl serves handle h.
func has(tbl *gssTable, h [gssHandleLen]byte) bool {
	_, ok := tbl.get(h[:])
	return ok
}

// lists is the table's global list and the lists per principal, front first,
// read under its mutex.
func lists(tbl *gssTable) (global [][gssHandleLen]byte, own map[vfs.Principal][][gssHandleLen]byte, n int) {
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	for e := tbl.global.Front(); e != nil; e = e.Next() {
		global = append(global, e.Value.(*gssContext).handle)
	}
	own = map[vfs.Principal][][gssHandleLen]byte{}
	for p, l := range tbl.byPrincipal {
		for e := l.Front(); e != nil; e = e.Next() {
			own[p] = append(own[p], e.Value.(*gssContext).handle)
		}
	}
	return global, own, len(tbl.m)
}

// balanced fails t unless both lists hold exactly the map's contexts and
// the budget holds exactly their charges.
func balanced(t *testing.T, tbl *gssTable, b *resources.Budget) {
	t.Helper()
	global, own, n := lists(tbl)
	perPrincipal := 0
	for _, hs := range own {
		perPrincipal += len(hs)
	}
	var want int64
	tbl.mu.Lock()
	for _, c := range tbl.m {
		want += charge(c.g)
	}
	tbl.mu.Unlock()
	if len(global) != n || perPrincipal != n {
		t.Fatalf("global list %d, principals' lists %d, map %d: want all equal", len(global), perPrincipal, n)
	}
	if got := b.Reserved(); got != want {
		t.Fatalf("budget holds %d bytes, want %d, the charges of the %d contexts held", got, want, n)
	}
}

// A context is found by its handle from any caller of the one table every
// connection shares, and from no other table, even over the same budget.
// Catches a context kept per connection or per process.
func TestGSSTableFindsAHandleInItsOwnTableOnly(t *testing.T) {
	tbl, b, clk := newTable(1<<20, gssMaxContexts)
	other := newGSSTable(b, clk, gssMaxContexts)
	h := add(t, tbl, gssCtx(principal(1), time.Time{}))

	var wg sync.WaitGroup
	found := make([]bool, 8)
	for i := range found {
		wg.Go(func() { found[i] = has(tbl, h) })
	}
	wg.Wait()
	for i, ok := range found {
		if !ok {
			t.Errorf("caller %d: get(handle) found nothing, want the context", i)
		}
	}
	if has(other, h) {
		t.Error("another table found the handle, want nothing")
	}
	if _, ok := tbl.get(h[:8]); ok {
		t.Error("get of the handle's first 8 bytes found a context, want nothing")
	}
}

// At the cap a principal's insert evicts its own least recently used
// context, never another principal's, even an older one; a principal that
// holds none evicts the least recently used overall. Catches eviction by
// insertion order or by the global list alone.
func TestGSSTableCapEvictsThePrincipalsOwnFirst(t *testing.T) {
	tbl, b, clk := newTable(1<<20, 4)
	alice, bob, carol := principal(1), principal(2), principal(3)
	a1 := add(t, tbl, gssCtx(alice, time.Time{}))
	clk.advance(time.Second)
	b1 := add(t, tbl, gssCtx(bob, time.Time{}))
	clk.advance(time.Second)
	a2 := add(t, tbl, gssCtx(alice, time.Time{}))
	clk.advance(time.Second)
	a3 := add(t, tbl, gssCtx(alice, time.Time{}))
	clk.advance(time.Second)

	a4 := add(t, tbl, gssCtx(alice, time.Time{}))
	for _, c := range []struct {
		name string
		h    [gssHandleLen]byte
		want bool
	}{{"alice's oldest", a1, false}, {"bob's, older than alice's others", b1, true}, {"alice's second", a2, true}, {"alice's newest", a4, true}} {
		if got := has(tbl, c.h); got != c.want {
			t.Errorf("after alice's fifth at a cap of four, %s held = %v, want %v", c.name, got, c.want)
		}
	}

	// A call run on a2 makes a3 alice's least recently used.
	c, _ := tbl.get(a2[:])
	if v := c.check(1, clk.Now()); v != verdictRun {
		t.Fatalf("check(1) on a fresh context = %v, want run", v)
	}
	clk.advance(time.Second)
	add(t, tbl, gssCtx(alice, time.Time{}))
	if has(tbl, a3) || !has(tbl, a2) {
		t.Errorf("after a call on a2, alice's next insert kept a3 = %v and a2 = %v, want a3 evicted and a2 kept", has(tbl, a3), has(tbl, a2))
	}

	// Carol holds none, so the least recently used overall goes: bob's.
	add(t, tbl, gssCtx(carol, time.Time{}))
	if has(tbl, b1) {
		t.Error("carol's first insert at the cap kept bob's context, the least recently used overall; want it evicted")
	}
	balanced(t, tbl, b)
}

// With the budget spent below the cap, reserve refuses and evicts nothing,
// charging nothing; on an empty table a budget below one context's bound
// refuses too, as does an INIT token past 64 KiB. Catches an innocent context
// evicted by a starved budget, an eviction before SignIn has named the
// principal, and a context held uncharged.
func TestGSSReserveRefusesAndEvictsNothing(t *testing.T) {
	probe, _, _ := newTable(1<<20, 10)
	r, err := probe.reserve(testToken)
	if err != nil {
		t.Fatal(err)
	}
	boundBytes := probe.budget.Reserved()
	r.release()
	one := charge(gssCtx(principal(1), time.Time{}))

	t.Run("spent by one principal", func(t *testing.T) {
		tbl, b, _ := newTable(3*one+boundBytes-1, 10)
		var hs [][gssHandleLen]byte
		for range 3 {
			hs = append(hs, add(t, tbl, gssCtx(principal(1), time.Time{})))
		}
		before := b.Reserved()
		if r, err := tbl.reserve(testToken); err == nil {
			r.release()
			t.Fatalf("reserve with %d of %d bytes spent = a reservation, want a refusal", before, 3*one+boundBytes-1)
		}
		if got := b.Reserved(); got != before {
			t.Errorf("a refused reserve left %d bytes reserved, want %d", got, before)
		}
		for i, h := range hs {
			if !has(tbl, h) {
				t.Errorf("context %d gone after a refused reserve, want every context kept", i)
			}
		}
	})
	t.Run("below one context", func(t *testing.T) {
		tbl, b, _ := newTable(boundBytes-1, 10)
		if _, err := tbl.reserve(testToken); err == nil {
			t.Fatal("reserve on a budget below one context's bound = a reservation, want a refusal")
		}
		if got := b.Reserved(); got != 0 {
			t.Errorf("a refused reserve left %d bytes reserved, want 0", got)
		}
	})
	t.Run("token past 64 KiB", func(t *testing.T) {
		tbl, b, _ := newTable(1<<30, 10)
		if _, err := tbl.reserve(gssMaxInitToken + 1); err == nil {
			t.Fatal("reserve for a token of 64 KiB + 1 = a reservation, want a refusal")
		}
		if got := b.Reserved(); got != 0 {
			t.Errorf("a refused reserve left %d bytes reserved, want 0", got)
		}
	})
}

// insert keeps charged only the context's real size, below the bound
// reserve took, and a longer name costs exactly its extra bytes. Catches the
// bound kept as the charge.
func TestGSSInsertTrimsTheChargeToTheContext(t *testing.T) {
	tbl, b, _ := newTable(1<<20, 10)
	r, err := tbl.reserve(testToken)
	if err != nil {
		t.Fatal(err)
	}
	bound := b.Reserved()
	g := gssCtx(principal(1), time.Time{})
	tbl.insert(r, g)
	got := b.Reserved()
	if got >= bound || got != charge(g) {
		t.Fatalf("after insert %d bytes reserved, want the context's %d, below the bound %d", got, charge(g), bound)
	}
	long := gssCtx(principal(1), time.Time{})
	long.Evidence.Name += "0123456789"
	if d := charge(long) - charge(g); d != 10 {
		t.Errorf("a name 10 bytes longer costs %d bytes more, want 10", d)
	}
	sids := gssCtx(principal(1), time.Time{})
	sids.Evidence.SIDs = slices.Grow(sids.Evidence.SIDs, 2)[:2]
	sids.Evidence.SIDs[0].SubAuthorities = make([]uint32, 5)
	if d := charge(sids) - charge(g); d != 2*gssSIDCharge+20 {
		t.Errorf("two SIDs with five sub-authorities cost %d bytes more, want %d", d, 2*gssSIDCharge+20)
	}
}

// A context idle 8 hours is gone at the next get or insert, and only a
// check whose verdict is run moves its idle time: a get and a dropped number
// leave it. Catches a context served past 8 hours and a forged or replayed
// call keeping one alive.
func TestGSSIdleEviction(t *testing.T) {
	tbl, b, clk := newTable(1<<20, 100)
	quiet := add(t, tbl, gssCtx(principal(1), time.Time{}))
	used := add(t, tbl, gssCtx(principal(1), time.Time{}))
	replayed := add(t, tbl, gssCtx(principal(1), time.Time{}))
	u, _ := tbl.get(used[:])
	rp, _ := tbl.get(replayed[:])
	if v := rp.check(5, clk.Now()); v != verdictRun {
		t.Fatalf("check(5) on a fresh context = %v, want run", v)
	}

	clk.advance(gssIdle - time.Hour)
	if v := u.check(1, clk.Now()); v != verdictRun {
		t.Fatalf("check(1) = %v, want run", v)
	}
	if v := rp.check(5, clk.Now()); v != verdictDrop {
		t.Fatalf("check(5) again = %v, want drop", v)
	}
	clk.advance(time.Hour - time.Nanosecond)
	if !has(tbl, quiet) {
		t.Fatal("a context idle 8 hours less 1ns is gone, want it served")
	}
	clk.advance(time.Nanosecond)
	if has(tbl, quiet) {
		t.Error("a context idle 8 hours, found only by get, is served; want it evicted")
	}
	if has(tbl, replayed) {
		t.Error("a context idle 8 hours but for a dropped number is served; want it evicted")
	}
	if !has(tbl, used) {
		t.Error("a context with a call run 1 hour ago is gone, want it served")
	}
	balanced(t, tbl, b)
}

// One get or insert evicts at most 16 idle contexts, so no call pays for a
// whole table gone idle, and get refuses an idle context it finds beyond
// them.
func TestGSSIdleEvictionIsBounded(t *testing.T) {
	for _, op := range []string{"get", "insert"} {
		t.Run(op, func(t *testing.T) {
			tbl, b, clk := newTable(1<<20, 100)
			var newest [gssHandleLen]byte
			for range 40 {
				newest = add(t, tbl, gssCtx(principal(1), time.Time{}))
			}
			clk.advance(gssIdle)
			want := 40 - gssIdleSweep
			if op == "get" {
				// The newest is past the sweep's reach, so only get's own
				// check of what it finds refuses it.
				if has(tbl, newest) {
					t.Error("get served the newest of 40 contexts idle 8 hours, want it refused")
				}
				want--
			} else {
				add(t, tbl, gssCtx(principal(2), time.Time{}))
				want++
			}
			if _, _, n := lists(tbl); n != want {
				t.Errorf("after one %s over 40 idle contexts, %d held, want %d", op, n, want)
			}
			balanced(t, tbl, b)
		})
	}
}

// The window: a number below it or seen within it drops, one above it
// advances it, 0 is an ordinary number, and the first number taken is the
// window's base.
func TestGSSWindow(t *testing.T) {
	tbl, _, clk := newTable(1<<20, 10)
	h := add(t, tbl, gssCtx(principal(1), time.Time{}))
	c, _ := tbl.get(h[:])
	for i, s := range []struct {
		seq  uint32
		want verdict
	}{
		{0, verdictRun}, {0, verdictDrop}, {5, verdictRun}, {3, verdictRun}, {3, verdictDrop},
		{200, verdictRun}, {73, verdictRun}, {72, verdictDrop}, {5, verdictDrop}, {199, verdictRun}, {199, verdictDrop},
		{327, verdictRun}, {200, verdictDrop}, {201, verdictRun}, {199, verdictDrop}, {1000, verdictRun}, {873, verdictRun}, {872, verdictDrop},
	} {
		if got := c.check(s.seq, clk.Now()); got != s.want {
			t.Errorf("step %d: check(%d) = %v, want %v", i, s.seq, got, s.want)
		}
	}
}

// A sequence number at or above MAXSEQ, and a call at or past the ticket's
// end, delete the context and release its charge; MAXSEQ less one and a call
// before the end run.
func TestGSSCheckEndsTheContext(t *testing.T) {
	tbl, b, _ := newTable(1<<20, 10)
	end := start.Add(time.Hour)
	for _, c := range []struct {
		name string
		seq  uint32
		at   time.Duration
		want verdict
	}{
		{"MAXSEQ-1", gssMaxSeq - 1, 0, verdictRun},
		{"MAXSEQ", gssMaxSeq, 0, verdictCtxProblem},
		{"above MAXSEQ", gssMaxSeq + 7, 0, verdictCtxProblem},
		{"before the end", 1, time.Hour - time.Nanosecond, verdictRun},
		{"at the end", 1, time.Hour, verdictCtxProblem},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := add(t, tbl, gssCtx(principal(1), end))
			ctx, _ := tbl.get(h[:])
			if got := ctx.check(c.seq, start.Add(c.at)); got != c.want {
				t.Fatalf("check(%d) at start+%v = %v, want %v", c.seq, c.at, got, c.want)
			}
			if gone := !has(tbl, h); gone != (c.want == verdictCtxProblem) {
				t.Errorf("context gone = %v, want %v", gone, c.want == verdictCtxProblem)
			}
			tbl.delete(h[:])
			if got := b.Reserved(); got != 0 {
				t.Errorf("table emptied, %d bytes reserved, want 0", got)
			}
		})
	}
}

// A check that passes while another call deletes its context moves nothing
// and still runs: the first call is held between its window and its move,
// another takes the context to MAXSEQ, then the first is released. Catches a
// deleted context linked back into a list, or its charge taken again.
func TestGSSCheckRacingADeletion(t *testing.T) {
	tbl, b, clk := newTable(1<<20, 10)
	h := add(t, tbl, gssCtx(principal(1), time.Time{}))
	c, _ := tbl.get(h[:])
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tbl.pauseAfterWindow(func(*gssContext) {
		once.Do(func() {
			close(reached)
			<-release
		})
	})
	first := make(chan verdict)
	go func() { first <- c.check(1, clk.Now()) }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the first check never reached its window's end")
	}
	if v := c.check(gssMaxSeq, clk.Now()); v != verdictCtxProblem {
		t.Fatalf("check(MAXSEQ) = %v, want ctxProblem", v)
	}
	close(release)
	if v := <-first; v != verdictRun {
		t.Errorf("the held check = %v, want run: it passed while the context lived", v)
	}
	global, own, n := lists(tbl)
	if n != 0 || len(global) != 0 || len(own) != 0 {
		t.Errorf("map %d, global list %d, principals' lists %d: want all empty", n, len(global), len(own))
	}
	if got := b.Reserved(); got != 0 {
		t.Errorf("%d bytes reserved after the context's deletion, want 0", got)
	}
}

// The model of RFC 21 section 10's table: each context's principal, idle
// time, the order calls touched it in, and every number it ever took.
type modelCtx struct {
	p     vfs.Principal
	last  time.Time
	touch int
	end   time.Time
	seen  bool
	high  int64
	taken map[int64]bool
}

type model struct {
	max   int
	touch int
	ctxs  map[[gssHandleLen]byte]*modelCtx
}

// byTouch is the model's handles, least recently touched first, of p alone
// when only is set.
func (m *model) byTouch(p vfs.Principal, only bool) [][gssHandleLen]byte {
	var hs [][gssHandleLen]byte
	for h, c := range m.ctxs {
		if !only || c.p == p {
			hs = append(hs, h)
		}
	}
	slices.SortFunc(hs, func(a, b [gssHandleLen]byte) int { return m.ctxs[a].touch - m.ctxs[b].touch })
	return hs
}

func (m *model) evictIdle(now time.Time) {
	for i, h := range m.byTouch(vfs.Principal{}, false) {
		if i == gssIdleSweep || now.Sub(m.ctxs[h].last) < gssIdle {
			return
		}
		delete(m.ctxs, h)
	}
}

func (m *model) insert(h [gssHandleLen]byte, p vfs.Principal, end, now time.Time) {
	m.evictIdle(now)
	if len(m.ctxs) >= m.max {
		victims := m.byTouch(p, true)
		if len(victims) == 0 {
			victims = m.byTouch(p, false)
		}
		delete(m.ctxs, victims[0])
	}
	m.touch++
	m.ctxs[h] = &modelCtx{p: p, last: now, touch: m.touch, end: end, taken: map[int64]bool{}}
}

func (m *model) get(h [gssHandleLen]byte, now time.Time) bool {
	m.evictIdle(now)
	c, ok := m.ctxs[h]
	if ok && now.Sub(c.last) >= gssIdle {
		delete(m.ctxs, h)
		return false
	}
	return ok
}

func (m *model) check(h [gssHandleLen]byte, seq int64, now time.Time) verdict {
	c := m.ctxs[h]
	if seq >= gssMaxSeq || !c.end.IsZero() && !now.Before(c.end) {
		delete(m.ctxs, h)
		return verdictCtxProblem
	}
	switch {
	case !c.seen:
		c.seen, c.high = true, seq
	case seq > c.high:
		c.high = seq
	case c.high-seq >= gssWindow, c.taken[seq]:
		return verdictDrop
	}
	c.taken[seq] = true
	m.touch++
	c.touch = m.touch
	if now.After(c.last) {
		c.last = now
	}
	return verdictRun
}

// Random inserts, gets, checks and clock steps against a model of RFC 21
// section 10's rules, the table's map, lists and budget compared with the
// model's after every step. Catches an eviction of another principal's
// context, a context served past 8 hours, a window advanced by a drop, and a
// get that reorders a list, which leaves an idle context behind a fresher one
// at the tail.
func TestGSSTableMatchesTheModel(t *testing.T) {
	seeds, steps := 64, 3000
	if testing.Short() {
		seeds = 16
	}
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 0x6755))
		tbl, b, clk := newTable(1<<30, 6)
		m := &model{max: 6, ctxs: map[[gssHandleLen]byte]*modelCtx{}}
		var issued [][gssHandleLen]byte
		for step := range steps {
			fail := func(format string, args ...any) {
				t.Helper()
				t.Fatalf("seed %d step %d: %s", seed, step, fmt.Sprintf(format, args...))
			}
			switch op := rng.IntN(10); {
			case op < 2:
				p := principal(byte(rng.IntN(3)))
				var end time.Time
				if rng.IntN(2) == 0 {
					end = clk.Now().Add(time.Duration(rng.IntN(20)) * time.Hour)
				}
				h := add(t, tbl, gssCtx(p, end))
				m.insert(h, p, end, clk.Now())
				issued = append(issued, h)
			case op < 3:
				h := [gssHandleLen]byte{0xff}
				if len(issued) > 0 {
					h = issued[rng.IntN(len(issued))]
				}
				if got, want := has(tbl, h), m.get(h, clk.Now()); got != want {
					fail("get = %v, model %v", got, want)
				}
			case op < 8:
				if len(issued) == 0 {
					continue
				}
				h := issued[rng.IntN(len(issued))]
				c, ok := tbl.get(h[:])
				if want := m.get(h, clk.Now()); ok != want {
					fail("get before check = %v, model %v", ok, want)
				}
				if !ok {
					continue
				}
				mc := m.ctxs[h]
				seq := mc.high + int64(rng.IntN(300)) - 150
				switch rng.IntN(100) {
				case 0:
					seq = gssMaxSeq - int64(rng.IntN(3)) + 1
				case 1, 2:
					seq = mc.high + int64(rng.IntN(1000))
				}
				seq = min(max(seq, 0), 1<<32-1)
				if got, want := c.check(uint32(seq), clk.Now()), m.check(h, seq, clk.Now()); got != want {
					fail("check(%d) = %v, model %v", seq, got, want)
				}
			default:
				clk.advance([]time.Duration{0, time.Second, time.Hour, 3 * time.Hour, gssIdle}[rng.IntN(5)])
			}

			global, own, n := lists(tbl)
			if n != len(m.ctxs) {
				fail("table holds %d contexts, model %d", n, len(m.ctxs))
			}
			want := m.byTouch(vfs.Principal{}, false)
			slices.Reverse(want)
			if !slices.Equal(global, want) {
				fail("global list %x, model's order %x", global, want)
			}
			for p, hs := range own {
				want := m.byTouch(p, true)
				slices.Reverse(want)
				if !slices.Equal(hs, want) {
					fail("principal %d's list %x, model's order %x", p.ID[0], hs, want)
				}
			}
			balanced(t, tbl, b)
		}
	}
}

// 64 goroutines call over 1 000 contexts, some reaching MAXSEQ; at the end
// both lists hold the map's contexts and, once the table is emptied, the
// budget is zero. The table's mutex and block profiles go to the test's
// artifact directory: under -race they show the run's locking, and the same
// test run without -race gives the profile the table's ponytail marker reads.
func TestGSSTableUnderConcurrentCalls(t *testing.T) {
	prevMutex := runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(prevMutex)
		runtime.SetBlockProfileRate(0)
	})

	tbl, b, clk := newTable(1<<30, gssMaxContexts)
	hs := make([][gssHandleLen]byte, 1000)
	for i := range hs {
		hs[i] = add(t, tbl, gssCtx(principal(byte(i%50)), time.Time{}))
	}
	var next [1000]struct {
		sync.Mutex
		n uint32
	}
	var wg sync.WaitGroup
	for g := range 64 {
		wg.Go(func() {
			for i := range 500 {
				k := (g*31 + i*7) % len(hs)
				c, ok := tbl.get(hs[k][:])
				if !ok {
					continue
				}
				next[k].Lock()
				seq := next[k].n
				next[k].n++
				next[k].Unlock()
				if k%100 == 0 && i > 250 {
					seq = gssMaxSeq
				}
				c.check(seq, clk.Now())
			}
		})
	}
	wg.Wait()
	balanced(t, tbl, b)
	for k := 0; k < len(hs); k += 100 {
		if has(tbl, hs[k]) {
			t.Errorf("context %d, sent MAXSEQ, is still held; want it deleted", k)
		}
	}
	for _, h := range hs {
		tbl.delete(h[:])
	}
	if global, own, n := lists(tbl); n != 0 || len(global) != 0 || len(own) != 0 {
		t.Errorf("after deleting every handle: map %d, global list %d, principals' lists %d; want all empty", n, len(global), len(own))
	}
	if got := b.Reserved(); got != 0 {
		t.Errorf("table emptied, %d bytes reserved, want 0", got)
	}

	dir := t.ArtifactDir()
	for _, name := range []string{"mutex", "block"} {
		path := filepath.Join(dir, "gss-table-"+name+".pprof")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s profile: %s", name, path)
	}
}
