package testproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

// The stages a connection of the full protocol sets.
var (
	_ core.HeaderCodec = headerCodec{}
	_ core.Router      = (*router)(nil)
	_ core.Security    = security{}
	_ core.FlowControl = (*flow)(nil)
	_ core.ReplayCache = (*replay)(nil)
	_ core.HoldCodec   = hold{}
)

var errNoPrev = errors.New("testproto: a member that inherits its session with none before it")

// offsetOf is where h lies in body, a header a walk of body's chain reaches;
// false for one it does not.
func offsetOf(body []byte, h *header) (int, bool) {
	for off := 0; ; {
		at, err := headerAt(body, off)
		if err != nil {
			return 0, false
		}
		if at == h {
			return off, true
		}
		if off = at.Next(); off == 0 {
			return 0, false
		}
	}
}

// security checks and opens a request sealed under a session, and seals its
// reply.
type security struct{}

// Unwrap resolves the session a related member inherits from prev, checks the
// message's checksum under it, opens its arguments in place, and writes the
// session it resolved into the header, so every later stage reads a real one.
func (security) Unwrap(h core.Header, rec *resources.Buf, prev core.Header) (core.Header, error) {
	hd, ok := h.(*header)
	if !ok {
		return nil, errors.New("testproto: unwrap a header of another protocol")
	}
	off, ok := offsetOf(rec.B, hd)
	if !ok {
		return nil, errShort
	}
	s := hd.session()
	if s == inherited {
		ph, ok := prev.(*header)
		if !ok || ph == nil || ph.session() == inherited {
			return nil, errNoPrev
		}
		s = ph.session()
	}
	if s != 0 {
		args := arguments(rec.B, off, hd)
		if checksum(rec.B[off:off+headerLen+len(args)], s) != hd.checksum() {
			return nil, errChecksum
		}
		xorKey(args, s)
	}
	binary.BigEndian.PutUint32(hd[offSession:], s)

	return hd, nil
}

// Wrap seals every reply in reply under its session, in place: once for a
// chain, since a second pass would XOR the arguments back and sum them clear.
func (security) Wrap(_ core.Header, reply *resources.Buf) error {
	for off := 0; ; {
		h, err := headerAt(reply.B, off)
		if err != nil {
			return err
		}
		if s := h.session(); s != 0 && s != inherited {
			args := arguments(reply.B, off, h)
			xorKey(args, s)
			binary.BigEndian.PutUint32(h[offChecksum:], checksum(reply.B[off:off+headerLen+len(args)], s))
		}
		if off = h.Next(); off == 0 {
			return nil
		}
	}
}

// creditTarget is the most credits a connection holds at once.
const creditTarget = 64

var errOverdraw = errors.New("testproto: a message that charges more credits than the connection holds")

// flow is the connection's credits: one to start with, charged by each
// request and granted back by each reply. It follows the client's balance on
// every path that admits a request. A reply to a request refused before Admit,
// one with a bad checksum, grants credits that were never charged here, and a
// Cancel's credits are charged by its client and never here, so there the two
// counts part.
type flow struct {
	mu      sync.Mutex
	balance int
}

func newFlow() *flow { return &flow{balance: 1} }

// Admit charges the header's credits, refusing a message that would overdraw.
func (f *flow) Admit(_ context.Context, h core.Header) (func(), error) {
	hd, ok := h.(*header)
	if !ok {
		return nil, errors.New("testproto: admit a header of another protocol")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := int(hd.credits()); c > f.balance {
		return nil, errOverdraw
	} else {
		f.balance -= c
	}

	return func() {}, nil
}

// charge takes n credits, as many as the connection holds: those of a message
// its client sent and charged, and Admit never saw.
func (f *flow) charge(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balance = max(f.balance-n, 0)
}

// grant adds n credits, as many as the target leaves room for, and answers
// how many it added.
func (f *flow) grant(n uint16) uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := min(int(n), max(creditTarget-f.balance, 0))
	f.balance += g

	return uint16(g)
}

// asHeader is h as testproto's header: a request's, or the one Final
// returned for it.
func asHeader(h core.Header) (*header, bool) {
	switch h := h.(type) {
	case *header:
		return h, true
	case *final:
		return &h.header, true
	}

	return nil, false
}

// keyOf is the key a held or parked request is known by: its message ID with
// the top bit set, so it is never 0.
func keyOf(h *header) uint64 { return h.id() | 1<<63 }

// hold is the stage version 2's table needs.
type hold struct{ flow *flow }

func (hold) Key(h core.Header) uint64 {
	hd, ok := asHeader(h)
	if !ok {
		return 1 << 63
	}

	return keyOf(hd)
}

// Interim appends the interim reply to h: it grants h's credits if grant,
// else one, and the credits of every header in after, the sum stopping at the
// credit target.
//
// ponytail: the credits of chain members answered before h are not added
// here, since each of those members' own replies grants them. Add them here
// if core comes to answer such a member with no reply of its own.
func (s hold) Interim(h core.Header, grant bool, after []core.Header, out *resources.Buf) error {
	hd, ok := h.(*header)
	if !ok {
		return errors.New("testproto: an interim for a header of another protocol")
	}
	want := 1
	if grant {
		want = int(hd.credits())
	}
	charged := 0
	for _, a := range after {
		if ah, ok := a.(*header); ok {
			charged += int(ah.credits())
		}
	}
	// The members after do not run, so Admit never charged what their client
	// did: charge it here, then grant it back with the rest.
	s.flow.charge(charged)
	want += charged
	granted := s.flow.grant(uint16(min(want, creditTarget)))

	at := len(out.B)
	out.B = append(out.B, hd[:]...)
	rep := (*header)(out.B[at : at+headerLen])
	binary.BigEndian.PutUint32(rep[offNext:], 0)
	rep[offFlags] = flagReply | flagInterim
	binary.BigEndian.PutUint16(rep[offCredits:], granted)
	binary.BigEndian.PutUint64(rep[offID:], keyOf(hd))
	binary.BigEndian.PutUint32(rep[offStatus:], 0)
	binary.BigEndian.PutUint32(rep[offChecksum:], 0)

	return nil
}

// Final is the header the final reply to a held request is encoded from: h's
// with its key as the message ID. If grant, its interim granted one credit of
// h's, and this grants the rest; otherwise none.
func (hold) Final(h core.Header, grant bool) core.Header {
	hd, ok := h.(*header)
	if !ok {
		return h
	}
	f := &final{header: *hd}
	var credits uint16
	if grant && hd.credits() > 0 {
		credits = hd.credits() - 1
	}
	binary.BigEndian.PutUint16(f.header[offCredits:], credits)
	binary.BigEndian.PutUint64(f.header[offID:], keyOf(hd))

	return f
}

// Cancels names the request a Cancel carries the message ID of.
func (hold) Cancels(h core.Header) (uint64, bool) {
	hd, ok := h.(*header)
	if !ok || hd.Op() != OpCancel {
		return 0, false
	}

	return keyOf(hd), true
}

// Pends answers the message's pend flag.
func (hold) Pends(h core.Header, _ error) bool {
	hd, ok := asHeader(h)

	return ok && hd.flags()&flagPend != 0
}

// replayKey names a cached reply: its session and sequence.
type replayKey struct{ session, seq uint32 }

// replay answers a request it has cached the reply of.
type replay struct {
	pool *resources.BufPool // over the reply budget; nil: no budgets were given
	flow *flow              // the connection's credits; nil: grants are not counted
	mu   sync.Mutex
	m    map[replayKey][]byte
}

// Lookup returns a fresh copy of the reply cached for h's session and
// sequence, in a buffer reserved from the reply budget under ctx; or nil, nil
// when none is cached. The copy grants afresh, what the replayed request
// charged and at least one, through the connection's credits: the grant the
// stored reply carried was counted when that reply was encoded.
//
// ponytail: the copy is reserved under no client's key, since a stage is
// handed no client address, so a client replaying many requests takes no turn
// behind the others. Key it by the client once core hands the stage one.
func (r *replay) Lookup(ctx context.Context, h core.Header) (*resources.Buf, error) {
	hd, ok := h.(*header)
	if !ok {
		return nil, errors.New("testproto: a replay lookup for a header of another protocol")
	}
	r.mu.Lock()
	cached, found := r.m[replayKey{hd.session(), hd.seq()}]
	r.mu.Unlock()
	if !found {
		return nil, nil
	}
	if r.pool == nil {
		return nil, errors.New("testproto: a cached reply and no reply budget to copy it under")
	}
	buf, err := r.pool.Get(ctx, netip.Addr{}, len(cached))
	if err != nil {
		return nil, err
	}
	buf.B = append(buf.B[:0], cached...)
	if r.flow != nil && len(buf.B) >= headerLen {
		binary.BigEndian.PutUint16(buf.B[offCredits:], r.flow.grant(max(hd.credits(), 1)))
	}

	return buf, nil
}

// Store keeps its own copy of reply, which the caller then wraps in place.
func (r *replay) Store(h core.Header, reply *resources.Buf) {
	hd, ok := asHeader(h)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[replayKey{hd.session(), hd.seq()}] = bytes.Clone(reply.B)
}
