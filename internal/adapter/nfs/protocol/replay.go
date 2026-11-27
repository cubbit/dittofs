package protocol

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/maphash"
	"net"
	"net/netip"
	"sync"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The duplicate-request cache's bounds (RFC 21 section 2.6): it holds at
// most replayBytes over every connection, one reply at most maxReply, and
// charges each entry its reply's length and entryOverhead for the key, the
// slot and its map node, so the bound holds the memory an entry really
// costs.
const (
	replayBytes   int64 = 64 << 20
	maxReply            = 8 << 10
	entryOverhead       = 128
	prefixBound         = 256
)

// decision: the key's TLS bit is written constant false. While
// settings.Validate still refuses every TLS mode but off (nfs.go), no reply
// is ever given over TLS that a plaintext retransmission could be answered
// with. The subtask that records a connection's TLS state replaces the
// constant with that state. Withdraw the marker with it.
const replayTLS = false

// identityKeyed names the fields of vfs.Identity a call's key is built from,
// and identityLeftOut the ones it is not: Protection and Evidence are
// constant over the two flavours served, AUTH_NONE and AUTH_SYS, and vary
// only under RPCSEC_GSS, which the adapter refuses and never caches.
// A field of vfs.Identity found in neither list breaks the reflection test.
var (
	identityKeyed   = []string{"Proof", "Claims", "Client"}
	identityLeftOut = []string{"Protection", "Evidence"}
)

// errReplaying names a request that runs while its own retransmission
// arrived: core answers it nothing through a NoReply row, so the client
// finds the reply at its next try once the first has ended.
var errReplaying = errors.New("nfs: a duplicate of a call that is still running")

// replaySeed is the per-process seed the key's hash is built with, so a
// crafted run of calls cannot steer the cache's map into one bucket.
var replaySeed = maphash.MakeSeed()

// retryIdentity is a call's translation into the fields its retransmission
// is recognized by: the client's address, the xid, the target the call names
// and the credential it carries. The AUTH_SYS stamp and machine name
// are not part of it: two calls that differ in them alone are one client
// retrying one call. Its TLS bit names the shape the reply may be answered
// in, plaintext or TLS.
type retryIdentity struct {
	addr    netip.Addr
	xid     uint32
	prog    uint32
	vers    uint32
	proc    uint32
	flavour uint32
	uid     uint32
	gid     uint32
	gids    [16]uint32
	gidsN   int
	tls     bool
}

// replayKey is the map key a retryIdentity is recognized by: the address and
// the xid whole, and one 64-bit hash over the rest of the identity and the
// first prefix of the call's arguments. No byte of the call is ever
// compared again: the hash is what a retransmission must match, so it can
// collide, answering one call's reply to bytes the call never sent, which
// the address and the xid pin to one client's one retry window.
type replayKey struct {
	addr netip.Addr
	xid  uint32
	hash uint64
}

// keyOf is id's key, hashing id's fields beside the address and the xid, its
// arguments' length and their first prefixBound bytes, so the hash tells
// retransmissions of the call from its neighbours without comparing any
// byte of the call again. It allocates nothing: every write goes
// into the hash from the record's own bytes.
func keyOf(id retryIdentity, c wire.Call) replayKey {
	var h maphash.Hash
	h.SetSeed(replaySeed)

	words := [...]uint32{id.prog, id.vers, id.proc, id.flavour, id.uid, id.gid, uint32(id.gidsN)}

	var b [4]byte

	for _, v := range words {
		binary.BigEndian.PutUint32(b[:], v)
		_, _ = h.Write(b[:])
	}

	for _, g := range id.gids[:id.gidsN] {
		binary.BigEndian.PutUint32(b[:], g)
		_, _ = h.Write(b[:])
	}

	args := c.Args()

	binary.BigEndian.PutUint32(b[:], uint32(len(args)))

	_, _ = h.Write(b[:])
	_, _ = h.Write(args[:min(len(args), prefixBound)])

	if id.tls {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}

	return replayKey{addr: id.addr, xid: id.xid, hash: h.Sum64()}
}

// retryIdentityOf is the call's identity under the credential checkAuth
// decoded, which the caller passes: uid, gid and groups only under AUTH_SYS,
// nothing claimed under AUTH_NONE. It allocates nothing.
func retryIdentityOf(
	c wire.Call, flavour uint32, sys *wire.AuthSysParms, addr netip.Addr, tls bool,
) retryIdentity {
	id := retryIdentity{
		addr: addr, xid: c.XID(), prog: c.Program(), vers: c.Version(),
		proc: c.Procedure(), flavour: flavour,
	}
	if flavour == wire.AuthSys {
		id.uid, id.gid = sys.UID, sys.GID
		id.gidsN = copy(id.gids[:], sys.Groups())
	}

	id.tls = tls

	return id
}

// replayEntry is one cached reply: its key beside its own copy of the
// reply's bytes, linked into the cache's recent list by index, which
// recycles the slot after eviction. Its reply array is never reused by a
// later entry: a lookup reading an entry's bytes while the entry is evicted
// and its slot recycled would race, and a slot recycled onto a fresh array
// cannot.
type replayEntry struct {
	key        replayKey
	reply      []byte
	prev, next int32
}

// replayCache is the one duplicate-request cache every connection shares:
// its entries in one map and one recent list, the least recently used
// evicted for the byte bound whenever a reply is kept. A map entry
// holds the index of its reply's slot, or -1 while the call runs, which is
// its in-progress mark: a retransmission that finds the mark answers nothing
// while the first runs, and the mark is cleared by Store or Drop,
// whichever ends the request, so it cannot outlive its call.
type replayCache struct {
	pool *resources.BufPool // the copies a lookup hands out, charged to the reply budget
	// ponytail: one lock over the whole cache, held for a map lookup and a
	// list move. Shard by key when a mutex profile shows
	// contention.
	mu      sync.Mutex
	m       map[replayKey]int32
	entries []replayEntry
	free    []int32 // recycled slots of evicted entries
	head    int32   // the most recently used, -1 none
	tail    int32
	bytes   int64 // charged for live entries, replies and overhead
}

// newReplayCache is the cache whose copies the pool hands out: the adapter's
// reply budget, so a retransmission's copy is charged as its first call's
// reply was.
func newReplayCache(pool *resources.BufPool) *replayCache {
	return &replayCache{pool: pool, m: make(map[replayKey]int32), head: -1, tail: -1}
}

// replayStage is one connection's view of the shared cache: the stage whose
// judgement of a header and whose tables Route shares with it, and the
// connection whose remote address the key is recognized by.
type replayStage struct {
	*stage
	cache *replayCache
}

// retryOf judges the call the way Route judges it, through the same refusal
// rows and the same table, and answers the identity its retransmission is
// known by when the call is one the cache serves: an NFSv3 call on a
// Cacheable row, or an NFSv4 COMPOUND of minor version 0, whose slot in the
// later minors' sessions is another stage.
//
// decision: the cache serves the NFS program alone: an NFSv3 call on a
// Cacheable row and an NFSv4.0 COMPOUND, and never MOUNT, NLM or NSM, whose
// calls reach no row a lookup of theirs could answer from, nor an NFSv4
// minor past 0, whose session's slot answers its retries. Overturned if a
// retransmission of a call of those programs must not run twice.
//
// decision: an NFSv4.0 call's program and version range are left to the key,
// not judged here: a header is judged like Route judges it only as far as the
// RPC version, the flavour and the credential, so a retransmission whose reply
// is stored may be answered from the cache after a Reconfigure narrowed the
// range, until the entry leaves by the bound. An NFSv3 call is judged by its
// table: one the connection does not serve is no row, and the call is a
// miss. Overturned if a Reconfigure that
// narrows the range must withdraw stored replies at once.
//
// Anything else is answered
// a zero identity and false: looked up nowhere, so a call refused at its
// credential, an uncacheable row and MOUNT's, NLM's and NSM's calls mark
// nothing and are not answered from the cache. Credential and header
// refusals come before the map, so a retransmission answered RPC_MISMATCH or
// AUTH_ERROR is refused its own answer again, never one the cache kept for
// a call that ran.
func (r *replayStage) retryOf(hd header) (retryIdentity, wire.Call, bool) {
	c := hd.call()

	if _, err := wire.ParseCall(c); errors.Is(err, wire.ErrNotCall) {
		return retryIdentity{}, c, false
	}

	if c.RPCVersion() != 2 {
		return retryIdentity{}, c, false
	}

	flavour, sys, err := checkAuth(c)
	if err != nil {
		return retryIdentity{}, c, false
	}

	if c.Program() != wire.ProgNFS {
		return retryIdentity{}, c, false
	}

	if minor, compound := c.MinorVersion(); compound {
		if minor != 0 {
			return retryIdentity{}, c, false
		}
	} else {
		if c.Version() != wire.VersNFS3 {
			return retryIdentity{}, c, false
		}

		table := r.served.table(wire.ProgNFS, wire.VersNFS3)

		if table == nil {
			return retryIdentity{}, c, false
		}

		row, ok := table.Ops[c.Procedure()]
		if !ok || !row.Cacheable {
			return retryIdentity{}, c, false
		}
	}

	var addr netip.Addr

	if r.conn != nil {
		addr = r.conn.Remote()
	}

	return retryIdentityOf(c, flavour, &sys, addr, replayTLS), c, true
}

// lookup answers h's cached reply as a fresh copy, reserved from the reply
// budget under ctx, whose deadline passing answers the resources delay the
// pipeline waits behind; or no buffer and no error when none is cached
// and the call just started, marking it in progress so a retransmission
// running the same call twice is answered nothing.
func (r *replayStage) Lookup(ctx context.Context, h core.Header) (*resources.Buf, error) {
	hd, ok := h.(header)
	if !ok {
		return nil, errors.New("nfs: a replay lookup for a header of another protocol")
	}

	id, c, serve := r.retryOf(hd)

	if !serve {
		return nil, nil
	}

	key := keyOf(id, c)
	_, compound := c.MinorVersion()

	r.cache.mu.Lock()

	i, found := r.cache.m[key]

	if !found {
		r.cache.m[key] = -1 // the call's in-progress mark

		r.cache.mu.Unlock()

		return nil, nil
	}

	var reply []byte

	if i >= 0 {
		e := &r.cache.entries[i]
		reply = e.reply

		r.cache.touch(i)
	}
	r.cache.mu.Unlock()

	if i < 0 {
		// An NFSv4 COMPOUND answered nothing is severed for the client to
		// retry on a fresh connection (RFC 7530 section 3.1.1).
		if compound && r.conn != nil {
			r.conn.Close(errReplaying)
		}

		return nil, errReplaying
	}

	// decision: a COMPOUND whose copy cannot be charged at once is severed,
	// where an NFSv3 call parks behind the pool: core asks first under an ended
	// context, which tries the charge once, so this arm runs on that first ask
	// and the COMPOUND never waits for the attempt's deadline. The client
	// retries on a fresh connection, which RFC 7530 section 3.1.1 allows
	// where a silent drop on TCP is forbidden. Overturned if a COMPOUND's
	// retransmission must wait for the reply pool like an NFSv3 call's.
	buf, err := r.cache.pool.Get(ctx, id.addr, len(reply))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if compound && r.conn != nil {
				r.conn.Close(ctx.Err())
			}

			return nil, &vfs.Delay{Cause: vfs.DelayResources}
		}

		return nil, err
	}

	buf.B = append(buf.B[:0], reply...)

	return buf, nil
}

// Store keeps the cache's own copy of the reply, its caller keeping the
// buffer to wrap, which overwrites it: through the same judgement
// Lookup marks with, so the entry lands at the call's mark, and never for a
// reply past maxReply or one whose parts do not sum to reply.B, as a reply
// with segments lent to it, whose mark it clears without keeping anything: the
// copy holds reply.B alone, which such a reply would leave short of the message.
func (r *replayStage) Store(h core.Header, reply *resources.Buf) {
	hd, ok := h.(header)
	if !ok {
		return
	}

	id, c, serve := r.retryOf(hd)

	if !serve {
		return
	}

	key := keyOf(id, c)

	b := reply.B

	// The copy is the whole message exactly when the parts a writer would send
	// sum to reply.B: a segment lent to the reply, even an empty one that
	// leaves one part, adds bytes ahead of B or leaves B short of the message.
	sent := 0
	for _, part := range reply.AppendBuffers(make(net.Buffers, 0, 1)) {
		sent += len(part)
	}

	r.cache.mu.Lock()

	defer r.cache.mu.Unlock()

	if len(b) > maxReply || sent != len(b) {
		// A reply the cache will not keep ends its call's mark here, since
		// no other end comes to a Store that keeps nothing.
		if r.cache.m[key] == -1 {
			delete(r.cache.m, key)
		}

		return
	}

	// The store that runs was always preceded by its own request's miss, so
	// the map holds that request's mark here, never another reply: a request
	// whose key holds an entry is answered from it and never runs.
	delete(r.cache.m, key) // the mark ends where its entry begins

	idx := r.cache.slot()
	e := &r.cache.entries[idx]

	e.key, e.prev, e.next = key, -1, -1

	e.reply = append(make([]byte, 0, len(b)), b...)

	r.cache.push(idx)

	r.cache.m[key] = idx

	r.cache.bytes += int64(len(b)) + entryOverhead
	r.cache.evict(key)
}

// Drop clears h's in-progress mark, which Store kept over, and is no answer
// for a refusal, an ended call or a reply that was never kept: the request
// ends without one, so the mark cannot hold its retransmission answering
// nothing forever.
func (r *replayStage) Drop(h core.Header) {
	hd, ok := h.(header)
	if !ok {
		return
	}

	id, c, serve := r.retryOf(hd)

	if !serve {
		return
	}

	key := keyOf(id, c)

	r.cache.mu.Lock()

	defer r.cache.mu.Unlock()

	if r.cache.m[key] == -1 {
		delete(r.cache.m, key)
	}
}

// touch moves entry i to the head of the recent list. The cache's mutex is
// held.
func (p *replayCache) touch(i int32) {
	if p.head == i {
		return
	}

	p.unlink(i)
	p.push(i)
}

// unlink takes entry i off the recent list. The cache's mutex is held.
func (p *replayCache) unlink(i int32) {
	e := &p.entries[i]
	if e.prev >= 0 {
		p.entries[e.prev].next = e.next
	} else {
		p.head = e.next
	}

	if e.next >= 0 {
		p.entries[e.next].prev = e.prev
	} else {
		p.tail = e.prev
	}

	e.prev, e.next = -1, -1
}

// push puts entry i at the head of the recent list. The cache's mutex is
// held.
func (p *replayCache) push(i int32) {
	e := &p.entries[i]
	if p.head >= 0 {
		p.entries[p.head].prev = i
		e.next = p.head
	}

	if p.tail < 0 {
		p.tail = i
	}

	e.prev = -1

	p.head = i
}

// slot answers an unused entry index, a recycled one first. The cache's
// mutex is held.
func (p *replayCache) slot() int32 {
	if n := len(p.free); n > 0 {
		i := p.free[n-1]

		p.free = p.free[:n-1]

		return i
	}

	p.entries = append(p.entries, replayEntry{})

	return int32(len(p.entries) - 1)
}

// evict drops the least recently used entries until the byte bound is met
// again, keep the entry just stored. The cache's mutex is held.
func (p *replayCache) evict(keep replayKey) {
	for p.bytes > replayBytes && p.tail >= 0 {
		i := p.tail
		e := &p.entries[i]

		if e.key == keep {
			return
		}

		p.unlink(i)
		delete(p.m, e.key)
		p.bytes -= int64(len(e.reply)) + entryOverhead
		e.reply, e.key = nil, replayKey{}

		p.free = append(p.free, i)
	}
}
