package protocol

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// replayVoid is a row's request and result, nothing: the rows below exist only
// to be looked up in the cache, never to run, and their codec moves no byte.
type replayVoid struct{}

func (*replayVoid) DecodeWire(b []byte, _ core.Version) (int, error) {
	return len(b), nil
}

func (*replayVoid) EncodeWire(_ core.Version, _ *resources.Buf) error {
	return nil
}

func (*replayVoid) MaxWire(core.Version) int {
	return 0
}

func (*replayVoid) IsZero() bool {
	return true
}

// replayed is a connection's replay stage over fresh tables, whose NFSv3
// table holds a cacheable row at 0 and 7 and an uncacheable one at 6, and the
// one cache over a reply budget of ceiling. With no conn its key reads the
// zero address. It answers the budget too, so a test can hold it full.
func replayed(ceiling int64) (*replayStage, *resources.WaitBudget) {
	errs := newErrors()
	row := func(code uint32, name string, cacheable bool) *core.Op {
		r := core.Row(code, name, core.NeedsNothing,
			func(context.Context, *core.Request, replayVoid) (replayVoid, error) {
				return replayVoid{}, nil
			})
		r.Cacheable = cacheable

		return r
	}
	v3 := core.NewOpTable(
		errs,
		false,
		row(0, "null", true),
		row(6, "read", false),
		row(7, "write", true),
	)
	v3.Protocol = "nfs3"
	budget := resources.NewWaitBudget("reply", ceiling)
	s := &stage{
		errs:   errs,
		served: newServed(settings.VersionRange{Min: "3", Max: "3"}, Tables{V3: v3}),
		conn:   &core.Conn{},
	}

	return &replayStage{stage: s, cache: newReplayCache(resources.NewBufPool(budget))}, budget
}

// sysCall is an AUTH_SYS NFSv3 call of the given procedure, at the xid, with
// the given name, uid, gid, groups and arguments.
func sysCall(xid, proc uint32, name string, uid, gid uint32, gids []uint32, args []byte) []byte {
	b := nullCall(wire.ProgNFS, wire.VersNFS3, proc, wire.AuthSys, authSys(name, uid, gid, gids...),
		wire.AuthNone, nil, args)
	binary.BigEndian.PutUint32(b, xid)

	return b
}

// headOf wraps a call's bytes as the stage's decoder does: its view of its
// record.
func headOf(b []byte) core.Header {
	return header{rec: &resources.Buf{B: b}}
}

// stored keeps reply as the cache's copy for call, and answers whether the
// lookup of the same call reads it back.
func stored(t *testing.T, w *replayStage, call, reply []byte) bool {
	t.Helper()
	w.Store(headOf(call), &resources.Buf{B: reply})

	buf, err := w.Lookup(context.Background(), headOf(call))
	if err != nil {
		t.Fatalf("the stored call answers %v", err)
	}

	if buf == nil {
		return false
	}
	defer buf.Release()

	return string(buf.B) == string(reply)
}

// A call's retransmission is answered from the cache only when every field the
// key names is the same: the address, the xid, the target and the credential
// it carries, its arguments to the covered prefix. The AUTH_SYS stamp and the
// machine name are no part of it: a call that differs in them alone is the
// same client retrying the same call.
func TestTheRetryIdentityKeysEveryFieldItNames(t *testing.T) {
	w, _ := replayed(1 << 20)
	base := func() []byte {
		return sysCall(0x51, 0, "host", 1000, 100, []uint32{4, 24}, make([]byte, 300))
	}
	reply := []byte("the cached reply")
	w.Store(headOf(base()), &resources.Buf{B: reply})

	other := func(args []byte) []byte { // the same call with the arguments given
		b := base()
		argsLen := binary.BigEndian.Uint32(b[56:]) // the tail after the verifier
		b = b[:len(b)-int(argsLen)]
		b = append(b, args...)

		return b
	}

	sameTail := func(edit func([]byte)) []byte { // the same call, its tail edited
		b := base()
		edit(b[len(b)-300:])

		return b
	}
	for _, c := range []struct {
		name string
		call []byte
		hit  bool
	}{
		{"the same call", base(), true},
		{"another xid", sysCall(0x52, 0, "host", 1000, 100,
			[]uint32{4, 24}, make([]byte, 300)), false},
		{"another uid", sysCall(0x51, 0, "host", 1001, 100,
			[]uint32{4, 24}, make([]byte, 300)), false},
		{"another gid", sysCall(0x51, 0, "host", 1000, 101,
			[]uint32{4, 24}, make([]byte, 300)), false},
		{"another group list", sysCall(0x51, 0, "host", 1000, 100,
			[]uint32{4, 25}, make([]byte, 300)), false},
		{"no group list", sysCall(0x51, 0, "host", 1000, 100, nil, make([]byte, 300)), false},
		{"an AUTH_NONE credential", func() []byte {
			b := nullCall(wire.ProgNFS, wire.VersNFS3, 0,
				wire.AuthNone, nil, wire.AuthNone, nil, make([]byte, 300))
			binary.BigEndian.PutUint32(b, 0x51)

			return b
		}(), false},
		{"another procedure", sysCall(0x51, 7, "host", 1000, 100,
			[]uint32{4, 24}, make([]byte, 300)), false},
		{"the uncacheable row", sysCall(0x51, 6, "host", 1000, 100,
			[]uint32{4, 24}, make([]byte, 300)), false},
		{"the RPC of another version", func() []byte {
			b := base()
			binary.BigEndian.PutUint32(b[8:], 3)

			return b
		}(), false},
		{"the program of another service", func() []byte {
			b := base()
			binary.BigEndian.PutUint32(b[12:], wire.ProgMount)

			return b
		}(), false},
		{"longer arguments", other(make([]byte, 301)), false},
		{"shorter arguments", other(make([]byte, 200)), false},
		{"arguments inside the covered prefix", sameTail(func(tail []byte) {
			tail[5] = 1
		}), false},
		{"another stamp", func() []byte {
			b := base()
			binary.BigEndian.PutUint32(b[32:], 0x0102) // the credential's first word

			return b
		}(), true},
		{"another machine name", sysCall(0x51, 0, "otherhost", 1000, 100,
			[]uint32{4, 24}, make([]byte, 300)), true},
	} {
		buf, err := w.Lookup(context.Background(), headOf(c.call))
		if c.hit {
			if err != nil || buf == nil || string(buf.B) != string(reply) {
				t.Errorf("%s: the lookup answers %v with %v, want the cached reply %q",
					c.name, err, buf, reply)
			}
		} else if buf != nil || err != nil {
			t.Errorf("%s: the lookup answers %v with %v, want a plain miss", c.name, err, buf)
		}

		if buf != nil {
			buf.Release()
		}
	}
	// The address is the one part of the key the bytes of a call never name:
	// its identity changes with it, so another client's equal call misses.
	c := wire.Call(base())

	flavour, sys, err := checkAuth(c)
	if err != nil {
		t.Fatal(err)
	}

	here := retryIdentityOf(c, flavour, &sys, netip.MustParseAddr("127.0.0.1"), false)

	there := retryIdentityOf(c, flavour, &sys, netip.MustParseAddr("127.0.0.2"), false)
	if keyOf(here, c) == keyOf(there, c) {
		t.Error("two identities of different addresses share one key")
	}
}

// The call's TLS bit is part of its key: a reply asked over TLS is never
// answered to a plaintext retransmission, and the constant records that no
// reply is ever given over TLS before the handshake lands.
func TestTheTLSBitSeparatesTheKeys(t *testing.T) {
	w, _ := replayed(1 << 20)
	call := sysCall(0x53, 0, "host", 1000, 100, []uint32{4, 24}, nil)
	c := wire.Call(call)

	flavour, sys, err := checkAuth(c)
	if err != nil {
		t.Fatal(err)
	}

	clear := retryIdentityOf(c, flavour, &sys, netip.Addr{}, false)

	tls := retryIdentityOf(c, flavour, &sys, netip.Addr{}, true)
	if keyOf(clear, c) == keyOf(tls, c) {
		t.Fatal("the identities of the two TLS states share one key")
	}

	w.Store(headOf(call), &resources.Buf{B: []byte("plaintext")})
	w.cache.mu.Lock()
	_, plainKept := w.cache.m[keyOf(clear, c)]
	_, tlsKept := w.cache.m[keyOf(tls, c)]
	w.cache.mu.Unlock()

	if !plainKept || tlsKept {
		t.Errorf("the cache held the plaintext key %v and the TLS key %v, want the first only",
			plainKept, tlsKept)
	}
}

// A call's arguments are hashed to their covered prefix only: two calls that
// differ past it are one call, their whole bytes too large to hash beside
// every lookup.
func TestTheArgumentsAreKeyedToTheirPrefix(t *testing.T) {
	w, _ := replayed(1 << 20)
	tail := make([]byte, 300)
	base := sysCall(0x55, 0, "host", 1000, 100, []uint32{4, 24}, tail)
	past := sysCall(0x55, 0, "host", 1000, 100, []uint32{4, 24}, tail)
	past[len(past)-30] = 1

	if !stored(t, w, base, []byte("one")) {
		t.Fatal("the stored call is not answered")
	}

	buf, err := w.Lookup(context.Background(), headOf(past))
	if err != nil || buf == nil || string(buf.B) != "one" {
		t.Errorf(
			"a call differing past the covered prefix answers %v with %v, want the cached reply",
			err,
			buf,
		)
	}

	if buf != nil {
		buf.Release()
	}
}

// A call under RPCSEC_GSS is held nowhere in the cache: the adapter refuses
// the flavour, and a retransmission that carries it is answered no bytes any
// cache kept for another call.
func TestAReplyUnderRPCSECGSSIsHeldNowhere(t *testing.T) {
	w, _ := replayed(1 << 20)
	// An RPCSEC_GSS credential body (RFC 2203 section 5.3.3.1): a context
	// handle, a sequence number and a service. Nothing decodes it: the
	// flavour alone ends the call's lookup.
	cred := make([]byte, 0, 28)
	cred = append(cred, []byte("0123456789abcdef")...)
	cred = binary.BigEndian.AppendUint64(cred, 1)
	cred = binary.BigEndian.AppendUint32(cred, 3)
	call := nullCall(wire.ProgNFS, wire.VersNFS3, 0, wire.AuthGSS, cred, wire.AuthNone, nil, nil)
	w.Store(headOf(call), &resources.Buf{B: []byte("no reply")})

	buf, err := w.Lookup(context.Background(), headOf(call))
	if buf != nil || err != nil {
		t.Errorf("the RPCSEC_GSS call answers %v with %v, want a plain miss", err, buf)
	}

	w.cache.mu.Lock()
	held := len(w.cache.m)
	w.cache.mu.Unlock()

	if held != 0 {
		t.Errorf("the cache holds %d entries of the RPCSEC_GSS call, want none", held)
	}
}

// The cache holds at most replayBytes and one reply at most maxReply: the
// least recently used entry is dropped for the bound, a reply too big is kept
// nowhere, and either retransmission runs again.
func TestTheCacheBoundsItsBytesAndItsReplies(t *testing.T) {
	w, _ := replayed(1 << 20)
	first := sysCall(0xF1, 0, "host", 1000, 100, nil, nil)
	w.Store(headOf(first), &resources.Buf{B: []byte("first")})
	// Fill the cache past its bound with replies at the cap, the oldest first:
	// the first entry, one round, is the one the bound drops.
	const fills = 8100
	for i := range fills {
		w.Store(headOf(sysCall(0x10000+uint32(i), 0, "host", 1000, 100, nil, nil)),
			&resources.Buf{B: make([]byte, maxReply)})
	}

	w.cache.mu.Lock()
	held := w.cache.bytes
	w.cache.mu.Unlock()

	if held > replayBytes {
		t.Fatalf("the cache holds %d bytes, want at most %d", held, replayBytes)
	}

	buf, err := w.Lookup(context.Background(), headOf(first))
	if buf != nil || err != nil {
		t.Errorf("the oldest entry answers %v with %v, want it evicted and a plain miss", err, buf)
	}

	last := sysCall(0x10000+fills-1, 0, "host", 1000, 100, nil, nil)

	buf, err = w.Lookup(context.Background(), headOf(last))
	if err != nil || buf == nil || len(buf.B) != maxReply {
		t.Errorf("the newest entry answers %v with %v, want its %d-byte reply", err, buf, maxReply)
	}

	if buf != nil {
		buf.Release()
	}
	// A reply past the cap keeps nothing, and its call's mark ends with its
	// Store, so the retransmission runs the call again instead of waiting on
	// a mark nothing will end.
	big := sysCall(0xF2, 0, "host", 1000, 100, nil, nil)
	if buf, err := w.Lookup(context.Background(), headOf(big)); buf != nil || err != nil {
		t.Fatalf(
			"the first lookup of the big reply answers %v with %v, want a plain miss",
			err,
			buf,
		)
	}

	w.Store(headOf(big), &resources.Buf{B: make([]byte, maxReply+1)})

	buf, err = w.Lookup(context.Background(), headOf(big))
	if buf != nil || err != nil {
		t.Errorf(
			"past a reply of %d bytes the retransmission answers %v with %v, want to run again",
			maxReply+1,
			err,
			buf,
		)
	}
}

// The bound evicts by use, not by age: of two replies the older one, once a
// retransmission read it back, is kept when a fill passes the bound, while
// the newer one, never read, is dropped. A cache that dropped by age alone
// would give up the reply a client just asked for and run its call again on
// the next retransmission.
func TestTheBoundDropsTheReplyNoClientRead(t *testing.T) {
	w, _ := replayed(1 << 20)
	read := sysCall(0xF3, 0, "host", 1000, 100, nil, nil)
	w.Store(headOf(read), &resources.Buf{B: []byte("read")})

	unread := sysCall(0xF4, 0, "host", 1000, 100, nil, nil)
	w.Store(headOf(unread), &resources.Buf{B: []byte("unread")})

	// Fill the cache to one byte of its bound, so the next store has to drop
	// one of the two oldest entries: read, stored first, or unread.
	w.cache.mu.Lock()
	slack := replayBytes - w.cache.bytes
	w.cache.mu.Unlock()

	for i := 0; slack >= maxReply+2*entryOverhead+2; i++ {
		w.Store(headOf(sysCall(0x10000+uint32(i), 0, "host", 1000, 100, nil, nil)),
			&resources.Buf{B: make([]byte, maxReply)})
		slack -= maxReply + entryOverhead
	}

	if slack < entryOverhead+2 {
		t.Fatalf("the fill left %d bytes of the bound, want room for one more reply", slack)
	}

	w.Store(headOf(sysCall(0xFFF0, 0, "host", 1000, 100, nil, nil)),
		&resources.Buf{B: make([]byte, slack-entryOverhead-1)})
	w.cache.mu.Lock()
	spare := replayBytes - w.cache.bytes
	w.cache.mu.Unlock()

	if spare != 1 {
		t.Fatalf("the cache holds its bound with %d spare, want the one byte below it", spare)
	}

	// The hit on the older reply is what separates the two: it moves read
	// above unread in the cache's order, so the store past the bound drops
	// the reply no client read.
	buf, err := w.Lookup(context.Background(), headOf(read))
	if err != nil || buf == nil || !bytes.Equal(buf.B, []byte("read")) {
		t.Fatalf("the retransmission of the read call answers %v with %v, want its reply", err, buf)
	}

	buf.Release()

	w.Store(headOf(sysCall(0xFFF1, 0, "host", 1000, 100, nil, nil)),
		&resources.Buf{B: []byte("late")})

	if buf, err := w.Lookup(context.Background(), headOf(unread)); buf != nil || err != nil {
		t.Errorf(
			"the never-read reply answers %v with %v, want the bound to have dropped it",
			err,
			buf,
		)
	}

	if buf, err := w.Lookup(context.Background(), headOf(read)); err != nil || buf == nil ||
		!bytes.Equal(buf.B, []byte("read")) {
		kept := 0
		if buf != nil {
			kept = len(buf.B)
		}

		t.Errorf(
			"the read reply answers %v with a reply of %d bytes, want the bound to keep it",
			err,
			kept,
		)
	} else {
		buf.Release()
	}
}

// A cacheable call's lookup allocates nothing, on the miss that marks it in
// progress (a new xid each run, its mark dropped as its end does) and on the
// second lookup that finds the mark's place: the retransmission is answered
// from the cache the same cheap way the call itself reached its handler.
func TestALookupAllocatesNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's own bookkeeping is the count")
	}

	w, _ := replayed(1 << 20)

	call := sysCall(0x57, 0, "host", 1000, 100, []uint32{4, 24}, nil)
	if buf, err := w.Lookup(context.Background(), headOf(call)); buf != nil || err != nil {
		t.Fatalf("the priming lookup answers %v with %v, want a plain miss", err, buf)
	}

	if n := testing.AllocsPerRun(100, func() {
		buf, err := w.Lookup(context.Background(), headOf(call))
		if buf != nil || !errors.Is(err, errReplaying) {
			t.Fatalf("the marked call answers %v with %v, want the in-progress mark", err, buf)
		}
	}); n != 0 {
		t.Errorf("a marked call's lookup allocates %v, want 0", n)
	}

	fresh := sysCall(0x58, 0, "host", 1000, 100, []uint32{4, 24}, nil)
	h := headOf(fresh)

	xid := uint32(0x1000) // clear of the primed call's 0x57

	if n := testing.AllocsPerRun(100, func() {
		xid++

		binary.BigEndian.PutUint32(fresh, xid)

		if buf, err := w.Lookup(context.Background(), h); buf != nil || err != nil {
			t.Fatalf("a new call answers %v with %v, want a plain miss", err, buf)
		}

		w.Drop(h)
	}); n != 0 {
		t.Errorf("a new call's lookup and drop allocate %v, want 0", n)
	}
	// A flavour the cache never holds is refused before the hash and the map.
	cred := binary.BigEndian.AppendUint32(nil, 0) // a stamp, the least the shape needs
	gss := nullCall(wire.ProgNFS, wire.VersNFS3, 0, wire.AuthGSS, cred, wire.AuthNone, nil, nil)

	if n := testing.AllocsPerRun(100, func() {
		buf, err := w.Lookup(context.Background(), headOf(gss))
		if buf != nil || err != nil {
			t.Fatalf("the refused call answers %v with %v, want a plain miss", err, buf)
		}
	}); n != 0 {
		t.Errorf("a refused call's lookup allocates %v, want 0", n)
	}
}

// An answer handed to a retransmission waits on the reply budget: when the
// budget is full and the wait's deadline has passed, the lookup answers the
// resources delay the pipeline waits behind, and a client asks again shortly,
// never a stale reply from a freed buffer.
func TestAFullBudgetDelaysPastItsDeadline(t *testing.T) {
	w, budget := replayed(1 << 20)
	call := sysCall(0x59, 0, "host", 1000, 100, nil, nil)
	w.Store(headOf(call), &resources.Buf{B: []byte("the reply")})

	held, err := budget.Reserve(context.Background(), netip.Addr{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	buf, err := w.Lookup(ctx, headOf(call))

	var delay *vfs.Delay
	if buf != nil || !errors.As(err, &delay) || delay.Cause != vfs.DelayResources {
		t.Fatalf("past its deadline on a full budget the lookup answers %v with %v"+
			", want the resources delay", err, buf)
	}
}

// The retry identities golden records each vector's call and the identity it
// translates to, the fields only: the hash's seed is per process, so no run
// could hold a record of it. A field that changes its meaning, or a field of
// one vector that starts to feed another, fails the recorded translation.
func TestTheRetryIdentityGolden(t *testing.T) {
	for _, vec := range retryVectors(t) {
		c := wire.Call(vec.call)

		flavour, sys, err := checkAuth(c)
		if err != nil {
			t.Fatalf("%s: %v", vec.name, err)
		}

		id := retryIdentityOf(c, flavour, &sys, vec.id.addr, vec.id.tls)
		if id != vec.id {
			t.Errorf("%s translates to %+v, want %+v", vec.name, id, vec.id)
		}
	}
}

// retryVectors reads the recorded calls and their identities.
func retryVectors(t *testing.T) []retryVector {
	t.Helper()

	data, err := os.ReadFile("testdata/retry-identities.golden")
	if err != nil {
		t.Fatal(err)
	}

	var vecs []retryVector

	var name, hexBytes string

	var id retryIdentity

	var open bool

	flush := func() {
		if !open {
			return
		}

		call, err := hex.DecodeString(strings.Join(strings.Fields(hexBytes), ""))
		if err != nil {
			t.Fatal(err)
		}

		vecs = append(vecs, retryVector{name: name, call: call, id: id})
		open = false
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "#"):
			if rest, ok := strings.CutPrefix(line, "# "); ok {
				name = strings.TrimSpace(rest)
			}
		case strings.HasPrefix(line, "addr "):
			flush()

			id = retryIdentityOfFields(t, line)
			hexBytes = ""
			open = true
		default:
			hexBytes += line
		}
	}

	flush()

	return vecs
}

// retryVector is one recorded call and the identity it translates to.
type retryVector struct {
	name string
	call []byte
	id   retryIdentity
}

// retryIdentityOfFields parses a recorded identity line: field names and the
// words they hold, numbers in hex with the 0x prefix.
func retryIdentityOfFields(t *testing.T, line string) retryIdentity {
	t.Helper()

	var id retryIdentity

	f := strings.Fields(line)
	num := func(s string) uint32 {
		t.Helper()

		v, err := strconv.ParseUint(s, 0, 32)
		if err != nil {
			t.Fatal(err)
		}

		return uint32(v)
	}

	for i := 0; i+1 < len(f); {
		switch f[i] {
		case "addr":
			id.addr = netip.MustParseAddr(f[i+1])
			i += 2
		case "xid":
			id.xid = num(f[i+1])
			i += 2
		case "prog":
			id.prog = num(f[i+1])
			i += 2
		case "vers":
			id.vers = num(f[i+1])
			i += 2
		case "proc":
			id.proc = num(f[i+1])
			i += 2
		case "flavour":
			id.flavour = num(f[i+1])
			i += 2
		case "uid":
			id.uid = num(f[i+1])
			i += 2
		case "gid":
			id.gid = num(f[i+1])
			i += 2
		case "tls":
			id.tls = num(f[i+1]) == 1
			i += 2
		case "groups":
			i++
			for i < len(f) && f[i] != "tls" {
				id.gids[id.gidsN] = num(f[i])
				id.gidsN++
				i++
			}

		default:
			t.Fatalf("a known field, %q", f[i])
		}
	}

	return id
}

// The keyed fields and the left-out ones name every field of the adapter's
// Identity between them: what a call proves, the claims it carries and the
// client it came from feed the key, what protects it and the evidence behind
// it never vary over the flavours served.
func TestTheIdentityFieldListsCoverEveryField(t *testing.T) {
	in := func(list []string, field string) bool {
		return slices.Contains(list, field)
	}
	for _, field := range reflect.VisibleFields(reflect.TypeFor[vfs.Identity]()) {
		keyed, left := in(identityKeyed, field.Name), in(identityLeftOut, field.Name)
		if keyed == left {
			t.Errorf("%s is in %v of the two lists, want exactly one", field.Name, field.Name)
		}
	}

	for _, field := range slices.Concat(identityKeyed, identityLeftOut) {
		if _, ok := reflect.TypeFor[vfs.Identity]().FieldByName(field); !ok {
			t.Errorf("the list names %s, which vfs.Identity does not hold", field)
		}
	}
}

// An uncacheable row's call is looked up nowhere: no mark, no entry, no byte
// of its arguments hashed, so a retransmission of it is only ever run.
func TestAnUncacheableRowIsLookedUpNowhere(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the race detector's own bookkeeping is the count")
	}

	w, _ := replayed(1 << 20)

	read := sysCall(0x5B, 6, "host", 1000, 100, nil, make([]byte, 300))
	if buf, err := w.Lookup(context.Background(), headOf(read)); buf != nil || err != nil {
		t.Fatalf("the uncacheable row answers %v with %v, want a plain miss", err, buf)
	}

	w.cache.mu.Lock()
	held := len(w.cache.m)
	w.cache.mu.Unlock()

	if held != 0 {
		t.Errorf("the uncacheable row leaves %d cached entries, want none", held)
	}

	if n := testing.AllocsPerRun(100, func() {
		if buf, err := w.Lookup(context.Background(), headOf(read)); buf != nil || err != nil {
			t.Fatalf("the uncacheable row answers %v with %v, want a plain miss", err, buf)
		}
	}); n != 0 {
		t.Errorf("the uncacheable row's lookup allocates %v, want 0", n)
	}
}

// The cache keeps its own copy of every reply it hands out, and every handout
// is a fresh copy: the pipeline wraps and overwrites its buffer after Store,
// and a reader's writes into a copy reach no later copy nor the cached one.
func TestEveryHandoutIsAFreshCopy(t *testing.T) {
	w, _ := replayed(1 << 20)
	call := sysCall(0x5D, 0, "host", 1000, 100, nil, nil)
	reply := &resources.Buf{B: []byte("a reply the cache keeps word for word")}
	w.Store(headOf(call), reply)

	for i := range reply.B { // the pipeline overwrites and releases its buffer
		reply.B[i] = 0xEE
	}

	ctx := context.Background()
	for range 2 {
		buf, err := w.Lookup(ctx, headOf(call))
		if err != nil {
			t.Fatal(err)
		}

		if string(buf.B) != "a reply the cache keeps word for word" {
			t.Fatalf("the handout reads %q, want the cached reply unchanged", buf.B)
		}

		for i := range buf.B {
			buf.B[i] = 0xDD
		}

		buf.Release()
	}
}

// The write-heavy mix at reply sizes measured from the replies' wire layouts,
// frozen here: NFSv3's WRITE at 220 bytes, an accepted header of 24, a
// status, both wcc attributes at 88 each, the count, the stable flag and the
// verifier; CREATE and REMOVE at 188 and 204, a handle, its attributes and
// their wcc pairs; an NFSv4.0 WRITE compound at 68, the head of a tagged
// chain of one. At 2000 cacheable replies a second the cache must hold 60
// seconds of them, the retransmission window of a Linux client over TCP:
// anything the mix defeats, or the bound, drops the oldest retransmissions.
func TestTheCacheHoldsSixtySecondsOfTheWriteMix(t *testing.T) {
	const (
		rate, window = 2000, 60
		write3       = 220
		create3      = 188
		remove3      = 204
		write4       = 68
	)

	sizes := []int{write3, create3, remove3, write4}

	worst := slices.Max(sizes)
	if hold := (int64(worst) + entryOverhead) * rate * window; hold > replayBytes {
		t.Fatalf("the mix's worst reply needs %d bytes of cache for %ds at %d a second"+
			", past the %d bound", hold, window, rate, replayBytes)
	}

	w, _ := replayed(1 << 20)

	var spent int64

	for i := range rate * window {
		size := sizes[i%len(sizes)]
		w.Store(headOf(sysCall(0x1000000+uint32(i), 7, "host", 1000, 100, nil, nil)),
			&resources.Buf{B: make([]byte, size)})
		spent += int64(size) + entryOverhead
	}

	w.cache.mu.Lock()
	held := w.cache.bytes
	w.cache.mu.Unlock()

	if held != spent {
		t.Errorf("the cache accounted %d bytes, want the %d the %d replies cost"+
			", reply and overhead each", held, spent, rate*window)
	}

	for _, xid := range []uint32{0x1000000, 0x1000000 + rate*window - 1} {
		size := sizes[int(xid)%len(sizes)]

		buf, err := w.Lookup(
			context.Background(),
			headOf(sysCall(xid, 7, "host", 1000, 100, nil, nil)),
		)
		if err != nil || buf == nil || len(buf.B) != size {
			t.Errorf("xid %x answers %v with %v, want its own %d-byte reply of the mix",
				xid, err, buf, size)
		}

		if buf != nil {
			buf.Release()
		}
	}
}

// New builds the one cache its reply budget names, and every connection's
// stages answer from that one cache: two connections' retransmissions are
// answered from the same entry, the address in the key apart. With no reply
// budget, or none at all, no stage is built and no reply is ever answered
// from another call's bytes.
func TestNewWiresTheOneCacheOverEveryConnection(t *testing.T) {
	ts, _ := tables()
	p := New(ts, &resources.Budgets{Reply: resources.NewWaitBudget("reply", 1<<20)})
	a, z := p.NewConn(nil), p.NewConn(nil)
	ra, okA := a.Replay.(*replayStage)

	rz, okZ := z.Replay.(*replayStage)
	if !okA || !okZ || ra == nil || ra.cache != p.cache || rz.cache != p.cache {
		t.Fatalf("one connection's replay %v, another's %v, want the one cache %v",
			a.Replay, z.Replay, p.cache)
	}

	for _, none := range []*resources.Budgets{nil, {}} {
		if st := New(ts, none).NewConn(nil); st.Replay != nil {
			t.Errorf("a cache built without a reply budget wires %v", st.Replay)
		}
	}
}

// The settings refuse every TLS mode but off, and the refusal names StartTLS,
// the subtask the protocol's TLS constant waits on: the day Validate accepts
// a mode, the cache must answer with a key that can tell the states apart.
func TestTheSettingsRefuseATLSModeUntilStartTLS(t *testing.T) {
	s := settings.DefaultNFS()
	if err := s.Validate(); err != nil {
		t.Fatalf("the default settings validate to %v", err)
	}

	s.TLS.Mode = "opportunistic"

	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "opportunistic") ||
		!strings.Contains(err.Error(), "StartTLS") {
		t.Fatalf("an opportunistic mode validates to %v, want its refusal to name StartTLS", err)
	}
}

// A reply with a segment lent to it is not kept: the copy holds reply.B
// alone, which would answer a retransmission with the tail of the message. Its
// call's mark ends at the Store, so the retransmission runs again.
func TestAReplyWithSegmentsIsNotStored(t *testing.T) {
	w, budget := replayed(1 << 20)
	pool := resources.NewBufPool(budget)
	ctx := context.Background()
	call := sysCall(0x59, 7, "host", 1000, 100, nil, nil)

	if buf, err := w.Lookup(ctx, headOf(call)); buf != nil || err != nil {
		t.Fatalf("the first lookup answers %v with %v, want a plain miss", err, buf)
	}

	reply, err := pool.Get(ctx, netip.Addr{}, 64)
	if err != nil {
		t.Fatal(err)
	}

	defer reply.Release()

	seg, err := pool.Get(ctx, netip.Addr{}, 64)
	if err != nil {
		t.Fatal(err)
	}

	reply.B = append(reply.B[:0], "head"...)
	seg.B = append(seg.B[:0], "lent"...)
	reply.Attach(seg)
	seg.Release()

	reply.B = append(reply.B, "tail"...)

	w.Store(headOf(call), reply)

	if buf, err := w.Lookup(ctx, headOf(call)); buf != nil || err != nil {
		var kept []byte

		if buf != nil {
			kept = bytes.Clone(buf.B)
			buf.Release()
		}

		t.Errorf("the retransmission answers %v with %q, want a miss that runs again", err, kept)
	}
}

// A reply whose header is filled and whose only segment is empty, with
// nothing after it, has one part to send but an empty reply.B: the copy would
// answer a retransmission with an empty record, so it is not kept and the
// call's mark ends at the Store.
func TestAReplyWithAnEmptySegmentIsNotStored(t *testing.T) {
	w, budget := replayed(1 << 20)
	pool := resources.NewBufPool(budget)
	ctx := context.Background()
	call := sysCall(0x5A, 7, "host", 1000, 100, nil, nil)

	if buf, err := w.Lookup(ctx, headOf(call)); buf != nil || err != nil {
		t.Fatalf("the first lookup answers %v with %v, want a plain miss", err, buf)
	}

	reply, err := pool.Get(ctx, netip.Addr{}, 64)
	if err != nil {
		t.Fatal(err)
	}

	defer reply.Release()

	seg, err := pool.Get(ctx, netip.Addr{}, 64)
	if err != nil {
		t.Fatal(err)
	}

	reply.B = append(reply.B[:0], "head"...)
	seg.B = seg.B[:0]
	reply.Attach(seg)
	seg.Release()

	w.Store(headOf(call), reply)

	w.cache.mu.Lock()
	held, marks := w.cache.bytes, len(w.cache.m)
	w.cache.mu.Unlock()

	if held != 0 || marks != 0 {
		t.Errorf(
			"the store left %d bytes and %d keys, want nothing kept and the mark cleared",
			held,
			marks,
		)
	}

	if buf, err := w.Lookup(ctx, headOf(call)); buf != nil || err != nil {
		var kept []byte

		if buf != nil {
			kept = bytes.Clone(buf.B)
			buf.Release()
		}

		t.Errorf("the retransmission answers %v with %q, want a miss that runs again", err, kept)
	}
}
