package core

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// member is one decoded member of a compound: its row, its invocation, its
// reply bound, as its room in the compound's record left it, and whether the
// drain refuses it.
type member struct {
	op      *Op
	inv     Invocation
	bound   int
	refused bool // judged while draining: its role is none (§4.7)
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
		ms = append(ms, member{op: op, inv: minv})
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
// before Resolve, is one the drain serves, and every member decoded. It marks
// each member whose role is none as refused, and collects every filehandle
// the members name, from which the drain's error is decided once for the
// whole compound.
func judge(ms []member, decoded bool, r *Request) (whole bool, handles []vfs.Handle) {
	whole = decoded
	for i := range ms {
		m := &ms[i]
		if roleOf(m.op, m.inv, r) == RoleNone {
			m.refused, whole = true, false
		}
		handles = append(handles, handlesOf(m.inv)...)
	}
	return whole, handles
}

// compound runs a compound row's request from its members' walk to out:
// every member decoded first; each member's bound handed its room within the
// protocol's record cap; one reply buffer of the members' bounds and the
// compound's header and head, refused past the record cap or a quarter of
// the reply pool's ceiling; while draining, the compound judged whole, the
// drain read once, after that buffer is reserved and before any member runs,
// and the drain's error its refused members get decided once; then each
// member in turn, through the row's checks, Resolve, the drain and its chain,
// carrying r between them, the first failure ending the reply, a NotFailure
// row's status being none (RFC 20 §4.3), and one a NoReply row maps leaving
// it unanswered. The head, which the compound's own Invocation encodes, goes
// first and again, in place, with the final status and count. A compound
// refused whole is answered its status alone, whose head, with no results,
// its header's codec writes.
func (p *pipeline) compound(tr *track, h Header, r *Request, tb *OpTable, row *Op, inv Invocation, rest []byte, release func()) {
	ctx, v, errs := tr.ctx, h.Version(), tb.Errors
	if inv.EncodeHead == nil {
		// A compound row whose Decode sets no head is a fault of the
		// protocol's wiring, since no reply could carry the compound's
		// status: the connection closes naming the row (RFC 20 §4.7).
		release()
		p.drop(tr, nil, fmt.Errorf("%w: the %s row's Decode sets no EncodeHead", errWiring, row.Name))
		return
	}
	ms, at, code, err := members(row, inv, rest, v)
	if at < 0 {
		p.refused(tr, stageDecodeArgs)
		p.finish(tr, h, tb, errs, row, Invocation{}, nil, err, release, alone{p}, nil)
		return
	}
	// Each member's bound is first taken for the record cap less the
	// compound's ReplyLen, as a request's is, then handed its room: what the
	// cap leaves after ReplyLen, the members before it at their bounds and
	// those after it. A READ's hook lowers its count to fit, one the room
	// leaves no byte refuses the compound, and so do bounds no hook lowers
	// that pass the cap, all before any member runs (RFC 20 §4.3).
	//
	// ponytail: each hook is asked its room before any member runs, and not
	// again before its member runs, so no hook can yet cap a READ at a budget
	// a member before it resolves, as an NFSv4.1 session's SEQUENCE does
	// (§4.3). Ask a READ's hook again before it runs once a row reads such a
	// budget.
	rl, after := p.st.Header.ReplyLen(h), 0
	for i := range ms {
		ms[i].bound, _ = ms[i].inv.Bound(r, p.l.record-rl)
		after += ms[i].bound
	}
	n, shortened := rl, 0
	var big error
	for i := range ms {
		m := &ms[i]
		after -= m.bound
		b, ok := m.inv.Bound(r, p.l.record-n-after)
		if !ok {
			big = fmt.Errorf("%w: member %d has no byte of room within the record cap of %d", ErrReplyTooBig, i, p.l.record)
			break
		}
		if b < m.bound {
			shortened++
		}
		m.bound, n = b, n+b
	}
	switch {
	case big != nil:
	case n > p.l.record:
		big = fmt.Errorf("%w: a compound's reply of %d bytes past the record cap of %d", ErrReplyTooBig, n, p.l.record)
	// decision: one reply buffer for the whole compound, its charge refused
	// past a quarter of the reply pool's ceiling: held across every member's
	// call, a charge the whole ceiling could hold would let one compound
	// stall every reply on the node until it ends. The ceiling: a compound
	// within the record cap whose charge passes a quarter is refused, not
	// shortened; at the 16 MiB floor an NFS compound, within its transfer
	// class, never is. Charge the exact size past the transfer class when
	// such a compound is seen refused.
	//
	// ponytail: one reservation of the summed bounds, held whole while the
	// compound runs, as §4.3 has it, so four such compounds still fill the
	// pool until each client's turns bound its share. Check what is left of
	// the session's reply size when the slots bring that limit.
	case p.sh.reply.Charge(n) > p.sh.reply.Ceiling()/4:
		big = fmt.Errorf("%w: a compound's reply of %d bytes", ErrReplyTooBig, n)
	}
	if big != nil {
		p.refused(tr, stageReplyTooBig)
		p.finish(tr, h, tb, errs, row, Invocation{}, nil, big, release, alone{p}, nil)
		return
	}
	mts := p.sh.tables[row.Members] // resolved with its parent's, at NewServer
	start := p.sh.clock.Now()
	reply, rerr := p.sh.reserveReply(ctx, p.l.remote, n)
	if rerr != nil {
		if st, ok := reservationStage(rerr); ok {
			p.refused(tr, st)
		}
		p.finish(tr, h, tb, errs, row, Invocation{}, nil, rerr, release, alone{p}, nil)
		return
	}
	mts.observe(stageReplyWait, p.sh.clock.Now().Sub(start))
	// The drain is read once the reply is reserved, which can wait: a
	// compound that waited through the drain's start has not been handed to
	// the service, so it is a new request, which the drain judges (§3.1).
	var drained error // the drain's error, which every member it refuses gets
	if p.draining.Load() {
		if whole, handles := judge(ms, err == nil, r); !whole {
			// A member that does not decode counts as refused with a NoReply
			// row, and a compound with any member refused with one answers
			// nothing and runs nothing; any other runs to its first refused
			// member and is answered there (§4.7).
			silent := err != nil
			drained = ErrDraining
			if !silent {
				// decision: asked before any member resolves, under r.Call,
				// which the Router builds once for the compound and every
				// member passes to the service: Resolve maps wire IDs alone,
				// and MoveTarget's admission resolves the identity on each
				// share. Overturned if a member's Resolve may change the call.
				drained = p.drainErr(ctx, r.Call, handles)
			}
			for _, m := range ms {
				if _, reply, _ := row.Members.Errors.Map(drained, m.op, v); m.refused && !reply {
					silent = true
				}
			}
			if silent {
				p.refused(tr, stageDecodeArgs) // judged whole before any member resolves its IDs (§10.1)
				p.finish(tr, h, tb, errs, row, Invocation{}, reply, ErrDraining, release, alone{p}, nil)
				return
			}
		}
	}
	mts.readsShortened(shortened)
	reply.B = reply.B[:0]
	mt := row.Members
	if encErr := p.st.Header.Encode(h, 0, reply); encErr != nil {
		reply.Release()
		release()
		p.drop(tr, nil, stageFault(encErr))
		return
	}
	head := len(reply.B)
	if encErr := inv.EncodeHead(reply, 0, 0, v); encErr != nil {
		reply.Release()
		release()
		p.drop(tr, nil, stageFault(encErr))
		return
	}
	headLen := len(reply.B) - head

	var (
		status Status
		count  int
		last   error
		failed bool
	)
	for _, m := range ms {
		s, send, notFailure, merr := p.member(tr, mts, mt.Errors, m, r, v, drained)
		if !send {
			p.unanswered(tr, h, tb, reply, merr, release)
			return
		}
		if encErr := p.result(mt, reply, m.op.Code, s, m.inv.Encode, v); encErr != nil {
			reply.Release()
			release()
			p.drop(tr, nil, stageFault(encErr))
			return
		}
		status, last, count = s, merr, count+1
		if failed = s != 0 && !notFailure; failed {
			break
		}
	}
	if err != nil && !failed {
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
		if status, send, _ = p.mapped(tr, mt.Errors, err, nil, v, true); !send {
			p.unanswered(tr, h, tb, reply, err, release)
			return
		}
		if encErr := p.result(mt, reply, code, status, nil, v); encErr != nil {
			reply.Release()
			release()
			p.drop(tr, nil, stageFault(encErr))
			return
		}
		last, count = err, count+1
	}
	again := &resources.Buf{B: reply.B[head : head : head+headLen]}
	if encErr := inv.EncodeHead(again, status, count, v); encErr != nil || len(again.B) != headLen {
		reply.Release()
		release()
		p.drop(tr, nil, fmt.Errorf("%w: a compound's head of %d bytes rewritten as %d (%v)", errWiring, headLen, len(again.B), encErr))
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
	p.end(tr, p.send(h, reply, key), status, sent, last)
}

// member runs one member of a compound to its status: the row's checks,
// Resolve, the expired credential, the drain, which answers a refused member
// drained, and its chain, under r as the member before left it. A refusal
// counts under the stage that refused it, in the members' table's series.
// send is false for an error a NoReply row maps, which leaves the whole
// compound unanswered (RFC 20 §4.3): a results array cut short under a
// success is malformed; and once the connection's close has cancelled the
// request's context, whatever the member answered, so the compound is
// dropped with no member after it run (RFC 20 §6.5). The close's own cancel
// is mapped as no error. notFailure is a NotFailure row's status, past which
// the compound runs its next member.
func (p *pipeline) member(tr *track, mts *tableSeries, errs *ErrorTable, m member, r *Request, v Version, drained error) (s Status, send, notFailure bool, err error) {
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
	if err == nil && m.refused {
		err = drained
		mts.refused(stageResolveIDs)
	}
	if err == nil {
		r.invoke = m.inv.Call
		tr.ran = true
		err = mts.chains[m.op.Code](tr.ctx, r)
	}
	send = true
	switch {
	case err == nil, p.endedByClose(err):
	case errors.Is(err, errPanicked):
		s = errs.fault
	default:
		s, send, notFailure = p.mapped(tr, errs, err, m.op, v, true)
	}
	return s, send && tr.ctx.Err() == nil, notFailure, err
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
