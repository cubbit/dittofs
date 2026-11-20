package identity

import (
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
// holds at most throttleBound pairs, and one client address at most a
// sixty-fourth of them (§7.3).
const (
	throttleLimit  = 10
	throttleWindow = 10 * time.Minute
	throttleBound  = 1 << 16
)

// throttleKey names one (name, client address) pair.
type throttleKey [sha256.Size]byte

// throttleUnit is the client address a pair is counted under: unmapped, so
// ::ffff:a.b.c.d is a.b.c.d, and an IPv6 address cut to its /64.
//
// decision: an IPv6 address is cut to its /64, as RFC 23 §5.3 keys a source,
// so one host cannot spread its failures, or its share of the table, over the
// 2^64 addresses it holds. Overturned if a deployment's clients share a /64
// behind one gateway, where one guesser would then throttle a user for all of
// them.
func throttleUnit(client netip.Addr) netip.Addr {
	a := client.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		a = p.Addr()
	}
	return a
}

// throttleKeyOf is the pair user, as presented, signs in as from client.
//
// decision: the key is the SHA-256 of the client's unit beside the user part,
// never the name itself, so a 64 KiB name costs the table 32 bytes. The user
// part is entity.PrincipalUserPart's, the fold Lookup keys names by, so
// alice, ALICE, CORP\alice and alice@CORP.EXAMPLE are one pair.
func throttleKeyOf(user string, client netip.Addr) throttleKey {
	b := throttleUnit(client).AsSlice()
	h := sha256.New()
	h.Write([]byte{byte(len(b))}) // so no address and name run into another pair's
	h.Write(b)
	h.Write([]byte(entity.PrincipalUserPart(user)))
	return throttleKey(h.Sum(nil))
}

// evictionsSpec counts the pairs the throttle evicted (RFC 18 §7.3).
var evictionsSpec = metrics.Spec{
	Name: "identity_logon_throttle_evictions_total",
	Help: "Logon throttle pairs evicted from a full table or a client address's full share of it.",
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
	addr    *throttleAddr
	links   [3]pairLink // in the table's byAge, in addr's under or full, and in the table's under
	at      *pairList   // which of addr's under and full holds the pair; nil while pending
}

// throttleAddr is one client unit's pairs.
type throttleAddr struct {
	unit  netip.Addr
	pairs int      // the unit's pairs, those with an attempt in flight among them
	under pairList // its settled pairs below their limit, least recently settled first
	full  pairList // its settled pairs that reached their limit, in the order they did
}

// The link each list of pairs runs through.
const (
	linkAge = iota
	linkAddr
	linkUnder
)

// pairList is a list of pairs threaded through each pair's links[i], so
// listing a pair allocates nothing.
type pairList struct{ front, back *throttlePair }

type pairLink struct{ prev, next *throttlePair }

// push puts p at l's back, through its links[i].
func (l *pairList) push(p *throttlePair, i int) {
	p.links[i] = pairLink{prev: l.back}
	if l.back != nil {
		l.back.links[i].next = p
	} else {
		l.front = p
	}
	l.back = p
}

// remove takes p, which l holds through its links[i], out of l.
func (l *pairList) remove(p *throttlePair, i int) {
	k := p.links[i]
	if k.prev != nil {
		k.prev.links[i].next = k.next
	} else {
		l.front = k.next
	}
	if k.next != nil {
		k.next.links[i].prev = k.prev
	} else {
		l.back = k.prev
	}
	p.links[i] = pairLink{}
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
// ponytail: the bound, 65 536 pairs, and each address's share of it, a
// sixty-fourth, are fixed counts. Charge the table to RFC 24's
// connection-state pool once it can be charged.
type throttle struct {
	mu    sync.Mutex
	pairs map[throttleKey]*throttlePair
	addrs map[netip.Addr]*throttleAddr
	byAge pairList // every pair, those in flight among them, least recently admitted first
	under pairList // every settled pair below its limit, least recently settled first
	bound int      // the most pairs held, throttleBound but in a test
	share int      // the most pairs one unit holds, a sixty-fourth of bound
	// evictions counts the pairs dropped to make room.
	evictions prometheus.Counter
}

func newThrottle(bound int, evictions prometheus.Counter) *throttle {
	return &throttle{
		pairs: map[throttleKey]*throttlePair{}, addrs: map[netip.Addr]*throttleAddr{},
		bound: bound, share: max(bound/64, 1), evictions: evictions,
	}
}

// reserve admits one attempt of user from client at now, or refuses it when
// the pair's counted failures and its attempts in flight reach
// throttleLimit, or when it is a new pair whose address holds its whole share
// and none of it can go. An admitted attempt must call settle once its
// credential check returns, with whether it failed ErrLogonFailure; until
// then it holds one of the pair's throttleLimit slots, so attempts sent at
// once cannot all pass.
//
// decision: only an attempt whose credential was checked, or refused for its
// mechanism, is counted; a throttled one is not, since counting it would keep
// a client that retries refused for ever, the lockout RFC 18 §3.1 rules out.
// A success does not reset the count. Overturned if a guesser is seen pacing
// attempts just inside the window.
func (t *throttle) reserve(user string, client netip.Addr, now time.Time) (settle func(failed bool), ok bool) {
	k, unit := throttleKeyOf(user, client), throttleUnit(client)
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pairs[k]
	switch {
	case p == nil:
		if a := t.addrs[unit]; a != nil && a.pairs >= t.share {
			if !t.evictOwn(a, now) {
				return nil, false
			}
		} else if len(t.pairs) >= t.bound {
			t.evictAny()
		}
		a := t.addrs[unit] // looked up again: an eviction may have dropped it
		if a == nil {
			a = &throttleAddr{unit: unit}
			t.addrs[unit] = a
		}
		a.pairs++
		p = &throttlePair{key: k, addr: a}
		t.byAge.push(p, linkAge)
		t.pairs[k] = p
	case p.counted(now)+int(p.pending) >= throttleLimit:
		return nil, false
	default:
		t.unlist(p)
		t.byAge.remove(p, linkAge)
		t.byAge.push(p, linkAge)
	}
	p.pending++
	return func(failed bool) { t.settle(p, failed, now) }, true
}

// unlist takes p out of whichever of its address's under and full holds it,
// and out of the table's under with the former.
func (t *throttle) unlist(p *throttlePair) {
	if p.at == &p.addr.under {
		t.under.remove(p, linkUnder)
	}
	if p.at != nil {
		p.at.remove(p, linkAddr)
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
		t.remove(p)
	case p.counted(now) >= throttleLimit:
		p.at = &p.addr.full
		p.at.push(p, linkAddr)
	default:
		p.at = &p.addr.under
		p.at.push(p, linkAddr)
		t.under.push(p, linkUnder)
	}
}

// evictOwn drops one of a's pairs to make room for a new pair of a, counted:
// its least recently settled pair below its limit, or else its first to reach
// its limit once that one's window has passed. It reports false, dropping
// nothing, when each of a's pairs is at its limit or has an attempt in flight.
//
// decision: an address at its share never evicts its own pair at its limit,
// so junk names from one address never free a name that address throttled;
// they evict its pairs below their limit instead, each a name it has guessed
// fewer than 10 times. An address whose whole share is throttled or in flight
// is refused new pairs, throttled, until one ages out or settles: that costs
// it 10 failures per pair of its share, 10 240 at the full bound, and refuses
// only the users signing in from it, which is the per-address lockout a NAT
// gateway's users share. Overturned if a gateway's users are seen refused that
// way, which needs the share keyed on more than the address.
//
// ponytail: only the front of full is checked, though a pair behind it may
// have aged out first, since full is ordered by when a pair reached its limit,
// not by when its oldest failure leaves the window; a new pair is then refused
// until the front ages out, at most one window later. Scan full, or order it
// by expiry, when a refusal is seen while a throttled pair had aged out.
func (t *throttle) evictOwn(a *throttleAddr, now time.Time) bool {
	p := a.under.front
	if p == nil {
		p = a.full.front
		if p == nil || p.counted(now) >= throttleLimit {
			return false
		}
	}
	t.evict(p)
	return true
}

// evictAny makes room in a full table for a new pair whose address is under
// its share, counted: it evicts the table's least recently settled pair below
// its limit, from any address, and only when none is left its least recently
// admitted pair, whatever its count and whether or not an attempt of it is in
// flight.
//
// decision: a full table always evicts, so no client fills it to refuse
// everyone else's new pairs. Pairs below their limit go first, so junk names
// sent one attempt each evict one another and never a throttled pair. A
// throttled pair is freed only once every pair in the table is throttled or
// in flight: 65 536 pairs at 10 counted failures each, 655 360 failures inside
// one window, spread over at least 64 address units since each holds a
// sixty-fourth of the table, or as many attempts held in flight at once. One
// such eviction frees one pair, the oldest, and evicting a pair in flight
// forgets its attempts, so its pair may have 10 more in flight. Overturned if the logon stream shows a spray across that
// many addresses, which needs the table charged to RFC 24's pool instead of a
// fixed bound.
func (t *throttle) evictAny() {
	p := t.under.front
	if p == nil {
		p = t.byAge.front
	}
	t.evict(p)
}

// evict drops p, counted.
func (t *throttle) evict(p *throttlePair) {
	t.remove(p)
	t.evictions.Inc()
}

// remove takes p out of the table, and its address with it once it holds no
// pair.
func (t *throttle) remove(p *throttlePair) {
	t.unlist(p)
	t.byAge.remove(p, linkAge)
	delete(t.pairs, p.key)
	if p.addr.pairs--; p.addr.pairs == 0 {
		delete(t.addrs, p.addr.unit)
	}
}

// drop forgets every pair.
func (t *throttle) drop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pairs = map[throttleKey]*throttlePair{}
	t.addrs = map[netip.Addr]*throttleAddr{}
	t.byAge = pairList{}
	t.under = pairList{}
}
