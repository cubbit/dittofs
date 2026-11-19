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
	fails [throttleLimit]uint32
	n     uint8 // the failures held, up to throttleLimit
	next  uint8 // where the next failure goes
}

// throttle counts each pair's logon failures and refuses a pair past its
// limit before its credential is checked. It runs no goroutine: a pair's
// window slides as its own failures are counted.
//
// ponytail: throttling is in memory, per node. A restart forgets it, each node
// counts alone, and a guesser who spreads one name across many addresses, or
// many names across one, stays under it. Keep the counts in the metadata store,
// or add a per-address limit across names, when the logon stream shows a spray
// that this rule did not slow.
//
// ponytail: a full throttle evicts its oldest pair, which frees a guesser who
// can fill it — but one who can fill it can more cheaply rotate addresses, which
// the in-memory note above already concedes. Refuse new pairs while every pair
// is at its limit when the logon stream shows a spray that eviction let through.
//
// ponytail: the bound, 65 536 pairs, about 12 MiB with their times, is a
// fixed count. Charge the table to RFC 24's connection-state pool once it can
// be charged.
type throttle struct {
	mu        sync.Mutex
	pairs     map[throttleKey]throttlePair
	order     []throttleKey // the pairs as first counted, a ring from head once full
	head      int
	evictions prometheus.Counter
}

func newThrottle(evictions prometheus.Counter) *throttle {
	return &throttle{pairs: map[throttleKey]throttlePair{}, evictions: evictions}
}

// throttled reports whether k has throttleLimit counted failures, the oldest
// under throttleWindow old at now.
func (t *throttle) throttled(k throttleKey, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pairs[k]
	return p.n == throttleLimit && now.Unix()-int64(p.fails[p.next]) < int64(throttleWindow/time.Second)
}

// fail counts one failure of k at now. A pair new to a full table evicts the
// pair first counted, counted.
//
// decision: only an attempt whose credential was checked, or refused for its
// mechanism, is counted; a throttled one is not, since counting it would keep
// a client that retries refused for ever, the lockout RFC 18 §3.1 rules out.
// A success does not reset the count. Overturned if a guesser is seen pacing
// attempts just inside the window.
func (t *throttle) fail(k throttleKey, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pairs[k]
	if !ok {
		if len(t.order) < throttleBound {
			t.order = append(t.order, k)
		} else {
			delete(t.pairs, t.order[t.head])
			t.order[t.head] = k
			t.head = (t.head + 1) % throttleBound
			t.evictions.Inc()
		}
	}
	p.fails[p.next] = uint32(now.Unix())
	p.next = (p.next + 1) % throttleLimit
	p.n = min(p.n+1, throttleLimit)
	t.pairs[k] = p
}

// drop forgets every pair.
func (t *throttle) drop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pairs, t.order, t.head = map[throttleKey]throttlePair{}, nil, 0
}
