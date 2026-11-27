package protocol

import (
	"container/list"
	"crypto/rand"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// Clock is the time the GSS context table reads for idle eviction and a
// ticket's end, an input so a test drives both rather than sleeping.
type Clock interface{ Now() time.Time }

// ponytail: the context cap and the idle timeout are constants. Make them
// settings when a deployment's client count, or its clients' idle pattern, is
// shown to need others.
const (
	gssMaxContexts = 10_000        // contexts per node, the table's cap
	gssIdle        = 8 * time.Hour // a context idle this long is evicted
)

const (
	gssWindow    = 128        // the sequence window RFC 2203 section 5.3.3.1 slides
	gssMaxSeq    = 0x80000000 // RFC 2203's MAXSEQ: a number at or above it ends the context
	gssIdleSweep = 16         // idle contexts one insert or get evicts at most
	// gssMaxInitToken bounds an INIT token: only the record cap does
	// otherwise, and the context's charge is sized from it. A ticket with a
	// large PAC fits, Windows' MaxTokenSize being at most 64 KiB.
	gssMaxInitToken = 64 << 10
)

// The charge of one context, from what it holds: the fixed part, its list
// elements and map entry, and an allowance for the AP-REP, the one key
// buffer SignIn builds rather than decodes from the INIT token.
const (
	gssFixedCharge = int64(unsafe.Sizeof(gssContext{})+unsafe.Sizeof(gss.Context{})+2*unsafe.Sizeof(list.Element{})) + 64
	gssAPRepCharge = 512
	gssSIDCharge   = int64(unsafe.Sizeof(vfs.Evidence{}.SIDs[0]))
	gssSIDWireMin  = 8 // a SID's smallest encoding: revision, count and authority
	gssHandleLen   = 16
)

// verdict is what check decides for a call whose header MIC verified.
type verdict uint8

const (
	verdictRun        verdict = iota // in the window and new: the call runs
	verdictDrop                      // below the window or seen: dropped unanswered
	verdictCtxProblem                // past MAXSEQ or the ticket's end: the context is deleted
)

// gssContext is one RPCSEC_GSS context: its handle, the gss.Context SignIn
// made, its principal, the sequence window and its idle time.
type gssContext struct {
	t         *gssTable
	handle    [gssHandleLen]byte
	g         *gss.Context
	principal vfs.Principal

	// mu guards the window alone; the table's mutex guards the rest below.
	mu      sync.Mutex
	seen    bool      // a number has been taken, so highest is one
	highest uint32    // the highest number taken
	bitmap  [2]uint64 // the numbers taken in the window, by number modulo 128

	res      resources.Reservation
	lastUsed time.Time
	global   *list.Element // in t.global
	own      *list.Element // in t.byPrincipal[principal]
}

// gssTable holds the adapter's GSS contexts, shared by every connection and
// keyed by handle, never by connection. Each context is charged to the
// connection-state budget; at the cap an insert evicts the inserting
// principal's least recently used context, else the least recently used
// overall, and an idle context is evicted lazily by insert and get.
//
// ponytail: one mutex over the whole table, held for a map lookup and two
// list moves. Shard it by handle when a mutex profile taken without -race
// shows contention on it.
type gssTable struct {
	budget *resources.Budget
	clock  Clock
	max    int

	mu          sync.Mutex
	m           map[[gssHandleLen]byte]*gssContext
	global      *list.List // every context, most recently used first
	byPrincipal map[vfs.Principal]*list.List

	// afterWindow, when set, runs in check between the window taking a
	// number and the table's lists moving; a test sets it per instance.
	afterWindow func(*gssContext)
}

// newGSSTable is an empty table of at most max contexts charged to b. A max
// below one panics: insert evicts one context to make room for the next, and
// an empty table has none to evict.
func newGSSTable(b *resources.Budget, c Clock, max int) *gssTable {
	if max < 1 {
		panic(fmt.Sprintf("nfs: a GSS context table of at most %d contexts", max))
	}
	return &gssTable{budget: b, clock: c, max: max, m: map[[gssHandleLen]byte]*gssContext{}, global: list.New(), byPrincipal: map[vfs.Principal]*list.List{}}
}

// reservation is one INIT's charge, taken before SignIn and handed to insert
// or released when the exchange fails.
type reservation struct{ r resources.Reservation }

// release returns the charge of an INIT that made no context.
func (r *reservation) release() { r.r.Release() }

// reserve charges a context's bound for an INIT token of tokenLen bytes: the
// fixed part, and the evidence's name, SIDs and keys, which are decoded from
// the ticket inside the token, so their size is bounded by its length.
//
// decision: a budget that refuses refuses the INIT here, before SignIn sees
// it, and evicts nothing: no principal is known until SignIn decrypts the
// ticket, and the budget is shared with other connection state, so a starved
// budget must not cost another principal its context. The cap's eviction runs
// in insert, once SignIn has named the principal. Answering rather than
// dropping lets the client's next attempt, a fresh AP-REQ, succeed as soon as
// the budget frees. Overturned if a supported client is shown to resend the
// same AP-REQ after a drop.
func (t *gssTable) reserve(tokenLen int) (*reservation, error) {
	if tokenLen < 0 || tokenLen > gssMaxInitToken {
		return nil, fmt.Errorf("nfs: an INIT token of %d bytes, past %d", tokenLen, gssMaxInitToken)
	}
	n := int64(tokenLen)
	r, err := t.budget.Reserve(gssFixedCharge + gssAPRepCharge + n + n/gssSIDWireMin*gssSIDCharge)
	if err != nil {
		return nil, err
	}
	return &reservation{r: r}, nil
}

// charge is what a context of g holds, which insert trims its reservation
// to.
//
// decision: a context whose real size passes the bound reserve charged, an
// evidence name longer than the ticket that named it or an AP-REP past its
// allowance, keeps the bound and holds the excess uncharged, since insert
// cannot refuse a context SignIn has already accepted. Overturned if SignIn's
// evidence is ever seen outgrowing the token it came from.
func charge(g *gss.Context) int64 {
	n := gssFixedCharge + int64(len(g.Evidence.Name)+len(g.Keys.Session)+len(g.Keys.Subkey)+len(g.Keys.APRep))
	for _, sid := range g.Evidence.SIDs {
		n += gssSIDCharge + 4*int64(len(sid.SubAuthorities))
	}
	for _, sid := range g.Evidence.DenyOnly {
		n += gssSIDCharge + 4*int64(len(sid.SubAuthorities))
	}
	return n
}

// insert adds the context SignIn made for g under r, which it takes and
// trims to the context's real size, after evicting idle contexts and, at the
// cap, the principal's least recently used context or else the least
// recently used overall. The handle is 16 random bytes, so a restarted node
// never hands out one a client still holds.
func (t *gssTable) insert(r *reservation, g *gss.Context) (handle [gssHandleLen]byte) {
	c := &gssContext{t: t, g: g, principal: g.Evidence.Principal, res: r.r}
	r.r = resources.Reservation{}
	c.res.Shrink(charge(g))
	rand.Read(c.handle[:])

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock.Now()
	t.evictIdleLocked(now)
	if len(t.m) >= t.max {
		victim := t.global.Back()
		if own := t.byPrincipal[c.principal]; own != nil {
			victim = own.Back()
		}
		t.removeLocked(victim.Value.(*gssContext))
	}
	c.lastUsed = now
	t.m[c.handle] = c
	c.global = t.global.PushFront(c)
	own := t.byPrincipal[c.principal]
	if own == nil {
		own = list.New()
		t.byPrincipal[c.principal] = own
	}
	c.own = own.PushFront(c)
	return c.handle
}

// get finds the context of handle, after evicting idle contexts. It moves
// nothing: the handle travels in the clear, so finding a context neither
// keeps it alive nor protects it from eviction. A context it finds idle is
// evicted and not served.
func (t *gssTable) get(handle []byte) (*gssContext, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock.Now()
	t.evictIdleLocked(now)
	if len(handle) != gssHandleLen {
		return nil, false
	}
	c, ok := t.m[[gssHandleLen]byte(handle)]
	if !ok {
		return nil, false
	}
	if now.Sub(c.lastUsed) >= gssIdle {
		t.removeLocked(c)
		return nil, false
	}
	return c, true
}

// delete removes the context of handle, if the table holds it.
func (t *gssTable) delete(handle []byte) {
	if len(handle) != gssHandleLen {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.m[[gssHandleLen]byte(handle)]; ok {
		t.removeLocked(c)
	}
}

// evictIdleLocked evicts from the global tail while its context has been idle
// gssIdle, at most gssIdleSweep of them, so one call never pays for a whole
// table gone idle. The global list is ordered by idle time, since only check
// moves a context and it moves it to the front.
func (t *gssTable) evictIdleLocked(now time.Time) {
	for range gssIdleSweep {
		e := t.global.Back()
		if e == nil || now.Sub(e.Value.(*gssContext).lastUsed) < gssIdle {
			return
		}
		t.removeLocked(e.Value.(*gssContext))
	}
}

// removeLocked unlinks c from the map and both lists and releases its
// charge.
func (t *gssTable) removeLocked(c *gssContext) {
	delete(t.m, c.handle)
	t.global.Remove(c.global)
	own := t.byPrincipal[c.principal]
	own.Remove(c.own)
	if own.Len() == 0 {
		delete(t.byPrincipal, c.principal)
	}
	c.res.Release()
}

// check judges a call on c whose header MIC verified: a sequence number at or
// above MAXSEQ, or a time at or past the ticket's end, deletes the context;
// otherwise the window drops a number below it or already taken, and takes a
// new one, which alone moves the context's idle time and its place in both
// lists.
//
// The window is under c's mutex and the lists under the table's, never held
// together. Between the two another call may delete c; then the move is
// skipped, so a deleted context is never linked back, and this call's verdict
// is still run: it was verified while the context lived.
//
// decision: the context ends at the ticket's end time, not one tick after:
// GSS_Context_time (RFC 2743 section 2.2.5) is the end less now, and a
// context whose lifetime is zero is expired, and RFC 2203 leaves a context's
// lifetime to the mechanism. RFC 4120 section 3.2.3 would honour the ticket
// through its end and a clock skew past it; that applies when the AP-REQ is
// accepted, not to the context it made. Overturned if a supported client is
// shown to send its last call at the end time exactly and expect it to run.
func (c *gssContext) check(seq uint32, now time.Time) verdict {
	if end := c.g.Evidence.Expires; seq >= gssMaxSeq || !end.IsZero() && !now.Before(end) {
		c.t.delete(c.handle[:])
		return verdictCtxProblem
	}
	c.mu.Lock()
	ok := c.take(seq)
	c.mu.Unlock()
	if !ok {
		return verdictDrop
	}
	if c.t.afterWindow != nil {
		c.t.afterWindow(c)
	}
	t := c.t
	t.mu.Lock()
	if t.m[c.handle] == c {
		if now.After(c.lastUsed) {
			c.lastUsed = now
		}
		t.global.MoveToFront(c.global)
		t.byPrincipal[c.principal].MoveToFront(c.own)
	}
	t.mu.Unlock()
	return verdictRun
}

// take is RFC 2203 section 5.3.3.1's window over c's bitmap: it reports
// whether seq is new and inside the window, marking it taken. The first
// number taken is the window's base, 0 included. Called with c.mu held.
func (c *gssContext) take(seq uint32) bool {
	bit := func(n uint32) (*uint64, uint64) { return &c.bitmap[n%gssWindow/64], 1 << (n % 64) }
	if !c.seen {
		c.seen, c.highest = true, seq
		w, b := bit(seq)
		*w |= b
		return true
	}
	switch {
	case seq > c.highest:
		if shift := seq - c.highest; shift >= gssWindow {
			c.bitmap = [2]uint64{}
		} else {
			for n := c.highest + 1; n != seq+1; n++ {
				w, b := bit(n)
				*w &^= b
			}
		}
		c.highest = seq
	case c.highest-seq >= gssWindow:
		return false
	default:
		if w, b := bit(seq); *w&b != 0 {
			return false
		}
	}
	w, b := bit(seq)
	*w |= b
	return true
}
