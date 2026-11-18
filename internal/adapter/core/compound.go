package core

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// member is one decoded member of a compound: its row and its invocation.
type member struct {
	op  *Op
	inv Invocation
}

// members decodes the members of the compound row's request inv from rest,
// the bytes after the compound's own arguments. It refuses a Count past the
// row's MaxMembers with ErrTooLong before it reads a member. Each member's
// code is read with the members table's ReadOp, which takes at least one
// byte, and its arguments with that row's Decode, which takes no more than
// are left; the walk moves on by the bytes each took.
//
// It returns the members that decoded and, where one did not or names a code
// the table lacks, that member's index, its code where it was read, and its
// error, the members before it to run first. With every member decoded, bytes
// left after the last are the compound's decode error, and no member is
// returned. A refusal of the compound whole, its cap or its trailing bytes,
// comes back with at -1.
func members(row *Op, inv Invocation, rest []byte, v Version) (ms []member, at int, code uint32, err error) {
	if inv.Count > row.MaxMembers {
		return nil, -1, 0, fmt.Errorf("%w: %d members, past the cap of %d", ErrTooLong, inv.Count, row.MaxMembers)
	}
	if inv.Count < 0 {
		return nil, -1, 0, fmt.Errorf("%w: a count of %d members", ErrDecode, inv.Count)
	}
	tb := row.Members
	ms = make([]member, 0, inv.Count)
	for i := range inv.Count {
		c, n, err := tb.ReadOp(rest)
		if err == nil && (n < 1 || n > len(rest)) {
			err = fmt.Errorf("a code of %d bytes, %d left", n, len(rest))
		}
		if err != nil {
			return ms, i, 0, fmt.Errorf("%w: member %d: %v", ErrDecode, i, err)
		}
		rest = rest[n:]
		op := tb.Ops[c]
		if op == nil {
			return ms, i, c, fmt.Errorf("%w: member %d's code %d", ErrNotSupported, i, c)
		}
		minv, m, err := op.Decode(rest, v)
		if err == nil && (m < 0 || m > len(rest)) {
			err = fmt.Errorf("arguments of %d bytes, %d left", m, len(rest))
		}
		if err != nil {
			return ms, i, c, fmt.Errorf("%w: member %d: %v", ErrDecode, i, err)
		}
		rest = rest[m:]
		ms = append(ms, member{op, minv})
	}
	if len(rest) > 0 {
		return nil, -1, 0, fmt.Errorf("%w: %d bytes after the last member", ErrDecode, len(rest))
	}
	return ms, len(ms), 0, nil
}

// roleOf is the role of a request to op, decoded as inv: its row's, unless
// its Invocation's Role, over the decoded arguments, says otherwise.
func roleOf(op *Op, inv Invocation, r *Request) Role {
	if inv.Role != nil {
		return inv.Role(r)
	}
	return op.Role
}

// judge says whether a compound runs whole while draining: each member's
// role, its row's or its Invocation's over its decoded arguments, asked
// before Resolve, is one the drain serves, and every member decoded. It
// collects every filehandle the members name, from which the drain's error
// is decided once for the whole compound.
func judge(ms []member, decoded bool, r *Request) (whole bool, handles []vfs.Handle) {
	whole = decoded
	for _, m := range ms {
		if roleOf(m.op, m.inv, r) == RoleNone {
			whole = false
		}
		if m.inv.Handle != nil {
			if h, ok := m.inv.Handle(); ok {
				handles = append(handles, h)
			}
		}
	}
	return whole, handles
}

// compound runs a compound row's request from its members' walk to out:
// every member decoded first; while draining, the compound judged whole, the
// drain read once, before any member runs; one reply buffer of the members'
// bounds and the compound's header and head, refused past a quarter of the
// reply pool's ceiling; then each member in turn, through the row's checks,
// Resolve and its chain, carrying r between them, the first failure ending
// the reply, and one a NoReply row maps leaving it unanswered. The head, which
// the compound's own Invocation encodes, goes first and again, in place, with
// the final status and count. A compound refused whole is answered its
// status alone, whose head, with no results, its header's codec writes.
func (p *pipeline) compound(tr *track, h Header, r *Request, tb *OpTable, row *Op, inv Invocation, rest []byte, release func()) {
	ctx, v, errs := tr.ctx, h.Version(), tb.Errors
	if inv.EncodeHead == nil {
		// A compound row whose Decode sets no head is a fault of the
		// protocol's wiring, since no reply could carry the compound's
		// status: the connection closes naming the row (RFC 20 §4.7).
		release()
		p.drop(tr, nil, fmt.Errorf("core: the %s row's Decode sets no EncodeHead", row.Name))
		return
	}
	ms, at, code, err := members(row, inv, rest, v)
	if at < 0 {
		p.refused(tr, stageDecodeArgs)
		p.finish(tr, h, tb, errs, row, Invocation{}, nil, err, release, alone{p})
		return
	}
	if p.draining.Load() {
		// ponytail: a compound the drain does not serve whole is refused
		// whole, since drainErr answers ErrDraining, a NoReply row. Once it
		// can answer ErrMoved, a refusal with a reply, the compound runs to
		// the moved member and is answered there (§4.7).
		if whole, handles := judge(ms, err == nil, r); !whole {
			p.refused(tr, stageResolveIDs)
			p.finish(tr, h, tb, errs, row, Invocation{}, nil, drainErr(ctx, handles), release, alone{p})
			return
		}
	}
	n := p.st.Header.ReplyLen(h)
	for _, m := range ms {
		n += m.inv.Bound()
	}
	// decision: one reply buffer for the whole compound, its charge refused
	// past a quarter of the reply pool's ceiling: held across every member's
	// call, a charge the whole ceiling could hold would let one compound
	// stall every reply on the node until it ends. The ceiling: a compound
	// whose bounds pass the transfer class at the 16 MiB floor, a GETATTR of
	// ACLs beside a 1 MiB READ, is refused. Charge the exact size past the
	// transfer class when such a compound is seen refused.
	//
	// ponytail: one reservation of the summed bounds, held whole while the
	// compound runs, as §4.3 has it, so four such compounds still fill the
	// pool until each client's turns bound its share. Check what is left of
	// the session's reply size when the slots bring that limit.
	if p.sh.reply.Charge(n) > p.sh.reply.Ceiling()/4 {
		p.refused(tr, stageReplyTooBig)
		p.finish(tr, h, tb, errs, row, Invocation{}, nil, fmt.Errorf("%w: a compound's reply of %d bytes", ErrReplyTooBig, n), release, alone{p})
		return
	}
	mts, serr := p.sh.table(row.Members)
	if serr != nil {
		release()
		p.drop(tr, nil, serr)
		return
	}
	start := p.sh.clock.Now()
	reply, rerr := p.sh.reserveReply(ctx, p.l.remote, n)
	if rerr != nil {
		if st, ok := reservationStage(rerr); ok {
			p.refused(tr, st)
		}
		p.finish(tr, h, tb, errs, row, Invocation{}, nil, rerr, release, alone{p})
		return
	}
	mts.observe(stageReplyWait, p.sh.clock.Now().Sub(start))
	reply.B = reply.B[:0]
	mt := row.Members
	if encErr := p.st.Header.Encode(h, 0, reply); encErr != nil {
		reply.Release()
		release()
		p.drop(tr, nil, encErr)
		return
	}
	head := len(reply.B)
	if encErr := inv.EncodeHead(reply, 0, 0, v); encErr != nil {
		reply.Release()
		release()
		p.drop(tr, nil, encErr)
		return
	}
	headLen := len(reply.B) - head

	var (
		status Status
		count  int
		last   error
	)
	for _, m := range ms {
		s, send, merr := p.member(tr, mts, mt.Errors, m, r, v)
		if !send {
			p.unanswered(tr, h, tb, reply, merr, release)
			return
		}
		if encErr := p.result(mt, reply, m.op.Code, s, m.inv.Encode, v); encErr != nil {
			reply.Release()
			release()
			p.drop(tr, nil, encErr)
			return
		}
		status, last, count = s, merr, count+1
		if s != 0 {
			break
		}
	}
	if err != nil && status == 0 {
		// The member that did not decode, or names a code the table lacks,
		// once the members before it have run, counted where a request
		// alone's would be: a code the table lacks at route, the rest at
		// decode_args.
		if errors.Is(err, ErrNotSupported) {
			mts.refused(stageRoute)
		} else {
			mts.refused(stageDecodeArgs)
		}
		var send bool
		if status, send = p.mapped(tr, mt.Errors, err, nil, v, true); !send {
			p.unanswered(tr, h, tb, reply, err, release)
			return
		}
		if encErr := p.result(mt, reply, code, status, nil, v); encErr != nil {
			reply.Release()
			release()
			p.drop(tr, nil, encErr)
			return
		}
		last, count = err, count+1
	}
	again := &resources.Buf{B: reply.B[head : head : head+headLen]}
	if encErr := inv.EncodeHead(again, status, count, v); encErr != nil || len(again.B) != headLen {
		reply.Release()
		release()
		p.drop(tr, nil, fmt.Errorf("core: a compound's head of %d bytes rewritten as %d (%v)", headLen, len(again.B), encErr))
		return
	}
	release()
	if row.Cacheable {
		p.st.Replay.Store(h, reply)
	}
	var key uint64
	if tb.Holds {
		key = p.st.Hold.Key(h)
	}
	sent := len(reply.B)
	p.send(h, reply, key)
	p.end(tr, true, status, sent, last)
}

// member runs one member of a compound to its status: the row's checks,
// Resolve, the expired credential and its chain, under r as the member
// before left it. A refusal counts under the stage that refused it, in the
// members' table's series. send is false for an error a NoReply row maps,
// which leaves the whole compound unanswered (RFC 20 §4.3): a results array
// cut short under a success is malformed; and for a call the connection's
// close ended, which is mapped as no error.
func (p *pipeline) member(tr *track, mts *tableSeries, errs *ErrorTable, m member, r *Request, v Version) (s Status, send bool, err error) {
	err = p.check(m.op, r)
	if err != nil {
		mts.refused(stageRoute)
	}
	if err == nil {
		r.resolveStart = p.sh.clock.Now()
		if err = m.inv.Resolve(r); err != nil {
			mts.refused(stageResolveIDs)
		}
	}
	if err == nil && p.expired(r) {
		if !m.op.Expired && roleOf(m.op, m.inv, r) != RoleRelease {
			err = ErrExpired
			mts.refused(stageResolveIDs)
		}
	}
	if err == nil {
		r.invoke = m.inv.Call
		tr.ran = true
		err = mts.chains[m.op.Code](tr.ctx, r)
	}
	switch {
	case err == nil:
		return 0, true, nil
	case errors.Is(err, errPanicked):
		return errs.fault, true, err
	case p.endedByClose(err):
		// A call the close ended is no error: the compound ends unsent, as
		// unanswered ends it.
		return 0, false, err
	}
	s, send = p.mapped(tr, errs, err, m.op, v, true)
	return s, send, err
}

// result encodes one member's result into reply: its head, then, where the
// member decoded, its own reply.
func (p *pipeline) result(mt *OpTable, reply *resources.Buf, code uint32, s Status, enc func(*resources.Buf, Version) error, v Version) error {
	if err := mt.EncodeResult(reply, code, s, v); err != nil {
		return err
	}
	if enc == nil {
		return nil
	}
	return enc(reply, v)
}
