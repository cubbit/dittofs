package identity

import (
	"container/heap"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The replay cache's bound, and the most entries one ticket client holds: a
// sixty-fourth of the bound.
const (
	replayBound = 1 << 20
	replayShare = replayBound / 64
)

var (
	// errReplay is an authenticator seen before: a logon failure whose
	// outcome is replay.
	errReplay = fmt.Errorf("%w: kerberos: authenticator replayed", vfserr.ErrLogonFailure)
	// errReplayFull is a cache, or a client's share of it, with no room for
	// one more entry.
	errReplayFull = fmt.Errorf("%w: kerberos: replay cache full", vfserr.ErrUnavailable)
)

// replayKey names one authenticator (RFC 4120 §3.2.3) by what a client
// cannot change without the session key: the SHA-256 of the AP-REQ's
// encrypted authenticator, which seals its client, time and microseconds. The
// ticket's service name stays out, since it travels in cleartext and every
// SPN of an account holds the same key, so a replay naming another one, or
// re-spelling it, still decrypts. client is kept for its per-client share.
type replayKey struct {
	client string
	auth   [sha256.Size]byte
}

type replayEntry struct {
	key     replayKey
	expires time.Time
}

// replayCache remembers each authenticator until its time plus the clock
// skew, past which the skew check refuses it anyway.
//
// ponytail: the replay cache is in memory, per process. A restart forgets it,
// so an AP-REQ captured within the clock skew before a restart can be accepted
// once after it, and each node keeps its own. A replay yields no session key,
// so it can sign no SMB request and compute no RPCSEC_GSS verifier. Refuse
// authenticators stamped before the process start, or share the cache between
// nodes, when a replay across a restart or a fail-over is shown to reach
// anything.
//
// ponytail: the bound, 1 048 576 entries, and each client's share of it, a
// sixty-fourth, are fixed counts. An entry lives at most 10 minutes, so
// filling a share takes over 27 sign-ins a second from one principal, and the
// whole cache fills only when 64 ticket holders flood at once, which then
// refuses every client, each flooder named in its logon events. Take the
// counts from RFC 24's memory pool, or shrink the share as the cache fills,
// once such a flood is seen.
type replayCache struct {
	mu        sync.Mutex
	bound     int
	share     int
	entries   map[replayKey]struct{}
	byExpiry  expiryHeap
	perClient map[string]int
}

func newReplayCache(bound, share int) *replayCache {
	return &replayCache{bound: bound, share: share, entries: map[replayKey]struct{}{}, perClient: map[string]int{}}
}

// insert records t's authenticator at now. An authenticator already held is
// errReplay; a full cache, or a client holding its whole share, is
// errReplayFull, for that client alone. Expired entries are dropped first,
// inside the call, so the cache runs no goroutine.
func (c *replayCache) insert(t *ticket, now time.Time) error {
	a := &t.req.Authenticator
	k := replayKey{
		client: a.CName.PrincipalNameString() + "@" + a.CRealm,
		auth:   sha256.Sum256(t.req.EncryptedAuthenticator.Cipher),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.byExpiry) > 0 && now.After(c.byExpiry[0].expires) {
		e := heap.Pop(&c.byExpiry).(replayEntry)
		delete(c.entries, e.key)
		if c.perClient[e.key.client]--; c.perClient[e.key.client] == 0 {
			delete(c.perClient, e.key.client)
		}
	}
	if _, seen := c.entries[k]; seen {
		return errReplay
	}
	if len(c.entries) >= c.bound || c.perClient[k.client] >= c.share {
		return errReplayFull
	}
	c.entries[k] = struct{}{}
	c.perClient[k.client]++
	heap.Push(&c.byExpiry, replayEntry{k, a.CTime.Add(clockSkew)})
	return nil
}

// expiryHeap orders entries by expiry, the soonest first.
type expiryHeap []replayEntry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)        { *h = append(*h, x.(replayEntry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}
