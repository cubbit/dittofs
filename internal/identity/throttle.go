package identity

import (
	"container/list"
	"crypto/sha256"
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metrics"
)

// The logon throttle's rule (RFC 18 §3.1): past throttleLimit counted
// failures within throttleWindow, a pair's attempts are refused; the table
// holds at most throttleBound pairs (§7.3).
const (
	throttleLimit  = 10
	throttleWindow = 10 * time.Minute
	throttleBound  = 1 << 16
)

// throttleKey names one (name, client address) pair.
type throttleKey [sha256.Size]byte

// throttleKeyOf is the pair user, as presented, signs in as from client.
//
// decision: the key is the SHA-256 of the client address beside the user
// part, never the name itself, so a 64 KiB name costs the table 32 bytes.
// The user part is entity.PrincipalUserPart's, the fold Lookup keys names by,
// so alice, ALICE, CORP\alice and alice@CORP.EXAMPLE are one pair. The address
// is unmapped, so ::ffff:a.b.c.d is a.b.c.d, and an IPv6 address is cut to
// its /64, as RFC 23 §5.3 keys a source, so one host cannot spread its
// failures over the 2^64 addresses it holds. Overturned if a deployment's
// clients share a /64 behind one gateway, where one guesser would then
// throttle a user for all of them.
func throttleKeyOf(user string, client netip.Addr) throttleKey {
	a := client.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		a = p.Addr()
	}
	b := a.AsSlice()
	h := sha256.New()
	h.Write([]byte{byte(len(b))}) // so no address and name run into another pair's
	h.Write(b)
	h.Write([]byte(entity.PrincipalUserPart(user)))
	return throttleKey(h.Sum(nil))
}

// evictionsSpec counts the pairs a full throttle evicted (RFC 18 §7.3).
var evictionsSpec = metrics.Spec{
	Name: "identity_logon_throttle_evictions_total",
	Help: "Logon throttle pairs evicted from a full table.",
}

// throttlePair is one pair's last throttleLimit counted failures, a ring
// whose oldest is at next once it is full. Each is in Unix seconds, a uint32
// good until 2106, so the full table takes half what int64 times would.
type throttlePair struct {
	fails   [throttleLimit]uint32
	n       uint8  // the failures held, up to throttleLimit
	next    uint8  // where the next failure goes
	pending uint16 // the attempts reserved and not yet settled
	key     throttleKey
	at      *list.Element // the pair in under or full; nil while pending
}

// counted is how many of p's failures are under throttleWindow old at now.
func (p *throttlePair) counted(now time.Time) int {
	n := 0
	for _, f := range p.fails[:p.n] {
		if now.Unix()-int64(f) < int64(throttleWindow/time.Second) {
			n++
		}
	}
	return n
}

// throttle counts each pair's logon failures and refuses a pair at its limit
// before its credential is checked. It runs no goroutine: a pair's window
// slides as its own failures are counted.
//
// ponytail: throttling is in memory, per node. A restart forgets it, each node
// counts alone, and a guesser who spreads one name across many addresses, or
// many names across one, stays under it. Keep the counts in the metadata store,
// or add a per-address limit across names, when the logon stream shows a spray
// that this rule did not slow.
//
// ponytail: the bound, 65 536 pairs, is a fixed count. Charge the table to
// RFC 24's connection-state pool once it can be charged.
type throttle struct {
	mu    sync.Mutex
	pairs map[throttleKey]*throttlePair
	under list.List // the settled pairs below their limit, least recently settled first
	full  list.List // the settled pairs that reached their limit, in the order they did
	bound int       // the most pairs held, throttleBound but in a test
	// evictions counts the pairs a full table dropped.
	evictions prometheus.Counter
}

func newThrottle(bound int, evictions prometheus.Counter) *throttle {
	return &throttle{pairs: map[throttleKey]*throttlePair{}, bound: bound, evictions: evictions}
}

// reserve admits one attempt of k at now, or refuses it when k's counted
// failures and its attempts in flight reach throttleLimit. An admitted
// attempt must call settle once its credential check returns, with whether
// it failed ErrLogonFailure; until then it holds one of k's throttleLimit
// slots, so attempts sent at once cannot all pass.
//
// decision: only an attempt whose credential was checked, or refused for its
// mechanism, is counted; a throttled one is not, since counting it would keep
// a client that retries refused for ever, the lockout RFC 18 §3.1 rules out.
// A success does not reset the count. Overturned if a guesser is seen pacing
// attempts just inside the window.
func (t *throttle) reserve(k throttleKey, now time.Time) (settle func(failed bool), ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pairs[k]
	switch {
	case p == nil:
		if len(t.pairs) >= t.bound && !t.evict(now) {
			return nil, false
		}
		p = &throttlePair{key: k}
		t.pairs[k] = p
	case p.counted(now)+int(p.pending) >= throttleLimit:
		return nil, false
	default:
		t.unlist(p)
	}
	p.pending++
	return func(failed bool) { t.settle(p, failed, now) }, true
}

// unlist takes p out of whichever of under and full holds it.
func (t *throttle) unlist(p *throttlePair) {
	if p.at != nil {
		t.under.Remove(p.at) // a no-op on the list p.at is not in
		t.full.Remove(p.at)
		p.at = nil
	}
}

// settle ends one attempt of p admitted at now, counting it when it failed.
// A pair with no attempt in flight and no failure held leaves the table, so a
// success takes no slot; one dropped meanwhile is not put back.
func (t *throttle) settle(p *throttlePair, failed bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pairs[p.key] != p {
		return
	}
	p.pending--
	if failed {
		p.fails[p.next] = uint32(now.Unix())
		p.next = (p.next + 1) % throttleLimit
		p.n = min(p.n+1, throttleLimit)
	}
	switch {
	case p.pending > 0:
	case p.n == 0:
		delete(t.pairs, p.key)
	case p.counted(now) >= throttleLimit:
		p.at = t.full.PushBack(p)
	default:
		p.at = t.under.PushBack(p)
	}
}

// evict drops one pair to make room for a new one, counted: the least
// recently settled pair below its limit, or else the first to reach its
// limit once its window has passed. It reports false, dropping nothing, when
// every pair is at its limit or has an attempt in flight.
//
// decision: a pair at its limit is never evicted, so no stream of new pairs,
// junk names from one address among them, frees a throttled name (RFC 18
// §3.1). A full table of throttled pairs instead refuses every new pair,
// throttled, until one's window passes: that takes 655 360 failures within
// the window at the full bound, and it denies new pairs, never a counted one. Overturned if the
// logon stream shows a client filling the table that way, which needs a
// per-address share of it.
//
// ponytail: only the front of full is checked, though a pair behind it may
// have aged out first, since full is ordered by when a pair reached its limit,
// not by when its oldest failure leaves the window; a new pair is then refused
// until the front ages out, at most one window later. Scan full, or order it
// by expiry, when a refusal is seen while a throttled pair had aged out.
func (t *throttle) evict(now time.Time) bool {
	e := t.under.Front()
	if e == nil {
		e = t.full.Front()
		if e == nil || e.Value.(*throttlePair).counted(now) >= throttleLimit {
			return false
		}
	}
	p := e.Value.(*throttlePair)
	t.unlist(p)
	delete(t.pairs, p.key)
	t.evictions.Inc()
	return true
}

// drop forgets every pair.
func (t *throttle) drop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pairs = map[throttleKey]*throttlePair{}
	t.under.Init()
	t.full.Init()
}
