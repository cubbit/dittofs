package testproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

var (
	errNoSessionID = errors.New("testproto: a Session that names no session")
	errViolation   = errors.New("testproto: the Violate operation")
	errMember      = errors.New("testproto: a compound member that runs past the record")
)

// registry is a protocol's sessions. A session spans connections: it lives
// while one carries it, and ends at a Logoff or when the last closes.
type registry struct {
	mu sync.Mutex
	m  map[uint32]*session
}

// session is one session: when its credential ends, its trees, and the
// connections carrying it, each with the release of the keep it took, nil
// for none.
type session struct {
	expires time.Time
	trees   map[uint32]bool
	conns   map[*connState]func()
}

func newRegistry() *registry { return &registry{m: map[uint32]*session{}} }

// join sets up session id, or joins it, on cs, with the credential's end and
// the release of the keep cs took for it.
func (g *registry) join(id uint32, cs *connState, expires time.Time, release func()) {
	g.mu.Lock()
	s := g.m[id]
	if s == nil {
		s = &session{trees: map[uint32]bool{}, conns: map[*connState]func(){}}
		g.m[id] = s
	}
	s.expires = expires
	old := s.conns[cs]
	s.conns[cs] = release
	g.mu.Unlock()
	if old != nil {
		old()
	}
}

// look reports whether cs carries session id, when its credential ends, and
// whether the session has tree.
func (g *registry) look(id uint32, cs *connState, tree uint32) (carried bool, expires time.Time, hasTree bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.m[id]
	if s == nil {
		return false, time.Time{}, false
	}
	if _, ok := s.conns[cs]; !ok {
		return false, time.Time{}, false
	}

	return true, s.expires, s.trees[tree]
}

// addTree connects tree in session id.
func (g *registry) addTree(id, tree uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.m[id]; s != nil {
		s.trees[tree] = true
	}
}

// end ends session id on every connection, releasing each keep.
func (g *registry) end(id uint32) {
	g.mu.Lock()
	s := g.m[id]
	delete(g.m, id)
	g.mu.Unlock()
	if s == nil {
		return
	}
	for _, release := range s.conns {
		if release != nil {
			release()
		}
	}
}

// leave drops cs's reference to every session it carried; a session no
// connection carries any more ends.
func (g *registry) leave(cs *connState) {
	g.mu.Lock()
	var releases []func()
	for id, s := range g.m {
		release, ok := s.conns[cs]
		if !ok {
			continue
		}
		if release != nil {
			releases = append(releases, release)
		}
		delete(s.conns, cs)
		if len(s.conns) == 0 {
			delete(g.m, id)
		}
	}
	g.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

// sessionReq is Session's arguments: whether to keep the connection past the
// idle timeout, and when the credential ends.
type sessionReq struct {
	keep    bool
	expires time.Time
}

func decodeSession(b []byte, _ core.Version, req *sessionReq) (int, error) {
	if len(b) < 9 {
		return 0, errors.New("testproto: a Session with short arguments")
	}
	req.keep = b[0] != 0
	if ns := int64(binary.BigEndian.Uint64(b[1:])); ns != 0 {
		req.expires = time.Unix(0, ns)
	}

	return 9, nil
}

// session sets up, or joins, the session the header names on this connection,
// which ends the handshake.
func (p *Protocol) session(_ context.Context, r *core.Request, req *sessionReq) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	if s.sid == 0 || s.sid == inherited {
		return nil, errNoSessionID
	}
	// At the seam a request has no Conn: the handshake and the keep are
	// the connection's, which the seam does not build.
	if r.Conn != nil {
		r.Conn.Established()
	}
	if s.conn.p.bare {
		return &empty{}, nil // Bare carries no session: it has no OnClose to drop one
	}
	var release func()
	if req.keep && r.Conn != nil {
		release = r.Conn.Keep()
	}
	p.reg.join(s.sid, s.conn, req.expires, release)

	return &empty{}, nil
}

// logoff ends the request's session and its keeps.
func (p *Protocol) logoff(_ context.Context, r *core.Request, _ *empty) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	p.reg.end(s.sid)
	if s.conn.replay != nil {
		s.conn.replay.forget(s.sid)
	}

	return &empty{}, nil
}

// tree connects the tree the header names in the request's session.
func (p *Protocol) tree(_ context.Context, r *core.Request, _ *empty) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	p.reg.addTree(s.sid, s.tid)

	return &empty{}, nil
}

// park parks the request and then makes its one service call, a Create of the
// name, whose answer completes it.
//
// decision: a parked request waits on a held service call, not on a lock or a
// notification. A test parks one by having the service it supplied hold that
// Create, completes it by releasing the hold, and reads a withdrawal from the
// outcome: a cancelled park answers the cancel status and no file of the name
// exists, and a refused park makes no call. The service's own waits are
// proven by NFS's and SMB's handler tests. Withdraw it if core's parking
// comes to depend on what the waiter is.
func (p *Protocol) park(ctx context.Context, r *core.Request, req *nameReq) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done, err := r.Park(cancel)
	if err != nil {
		cancel()
		return nil, err
	}
	// The name lies in the request's record, which core may release once the
	// handler returns; the call outlives it.
	call, dir, name := r.Call, s.fh, bytes.Clone(req.name)
	go func() {
		defer cancel()
		_, err := p.d.VFS.Create(callCtx, call, dir, name, vfs.CreateArgs{})
		done(err)
	}()

	return &empty{}, core.ErrParked
}

// targetReq is Notify's arguments: the message ID of the request it names.
type targetReq struct{ target uint64 }

func decodeTarget(b []byte, _ core.Version, req *targetReq) (int, error) {
	if len(b) < 8 {
		return 0, errors.New("testproto: a Notify with no target")
	}
	req.target = binary.BigEndian.Uint64(b)

	return 8, nil
}

// notify sends a message of its own on the connection, ahead of the reply to
// the request its target names.
//
// ponytail: the message is reserved under no client's key, as the replay
// cache's copy is, and is left to Conn.Send on every path, an error included.
// Release it here on an error once core states that a refused Send leaves the
// message with its caller.
func (p *Protocol) notify(ctx context.Context, r *core.Request, req *targetReq) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	if p.pool == nil {
		return nil, errors.New("testproto: a Notify and no reply budget to send it under")
	}
	msg, err := p.pool.Get(ctx, netip.Addr{}, headerLen)
	if err != nil {
		return nil, err
	}
	var zero header
	msg.B = append(msg.B[:0], zero[:]...)
	h := (*header)(msg.B[:headerLen])
	h[offFlags] = flagReply
	h[offVersion] = uint8(r.Version)
	binary.BigEndian.PutUint32(h[offOp:], OpNotify)
	binary.BigEndian.PutUint64(h[offID:], req.target)
	binary.BigEndian.PutUint32(h[offSession:], s.sid)
	if err := r.Conn.Send(h, msg, req.target|1<<63); err != nil {
		return nil, err
	}

	return &empty{}, nil
}

// violate closes the connection, as a protocol does on a violation it finds.
func violate(_ context.Context, r *core.Request, _ *empty) (*empty, error) {
	r.Conn.Close(errViolation)

	return &empty{}, nil
}

// maxMembers is the compound row's cap.
const maxMembers = 8

// compound is version 1's compound row over members, its members' table.
func compound(members *core.OpTable) *core.Op {
	return &core.Op{
		Code: OpCompound, Name: "Compound", Versions: core.Range{First: 1, Last: 1}, Needs: core.NeedsNothing,
		Decode: decodeCompound, Members: members, MaxMembers: maxMembers,
	}
}

// decodeCompound reads the count of members that follow and the tag the
// reply's head echoes, and takes those bytes alone: the runner decodes the
// members after them.
func decodeCompound(b []byte, _ core.Version) (core.Invocation, int, error) {
	if len(b) < 8 {
		return core.Invocation{}, 0, errMember
	}
	count, n := binary.BigEndian.Uint32(b), binary.BigEndian.Uint32(b[4:])
	if count > maxBody || int64(n) > int64(len(b)-8) {
		return core.Invocation{}, 0, errMember // more members, or tag bytes, than a record has
	}
	tag := b[8 : 8+n]

	return core.Invocation{
		Count:   int(count),
		Bound:   func() int { return 0 },
		Resolve: func(*core.Request) error { return nil },
		Call:    func(context.Context, *core.Request) error { return nil },
		Encode:  func(*resources.Buf, core.Version) error { return nil },
		EncodeHead: func(out *resources.Buf, s core.Status, n int, _ core.Version) error {
			out.B = appendHead(out.B, s, tag, n)
			return nil
		},
	}, 8 + int(n), nil
}

// readOp reads the code at the head of a compound member.
func readOp(b []byte) (uint32, int, error) {
	if len(b) < 4 {
		return 0, 0, errMember
	}

	return binary.BigEndian.Uint32(b), 4, nil
}

// memberRows is rows as a compound's members decode them: each member's
// arguments follow a 4-byte length, after the code readOp took.
//
// Negotiate is left out: a connection negotiates in a request of its own, as
// the Router gives only that one the highest version it may pick.
func memberRows(rows []*core.Op) []*core.Op {
	var out []*core.Op
	for _, row := range rows {
		if row.Code == OpNegotiate {
			continue
		}
		m := *row
		whole := row.Decode
		m.Decode = func(b []byte, v core.Version) (core.Invocation, int, error) {
			if len(b) < 4 {
				return core.Invocation{}, 0, errMember
			}
			length := binary.BigEndian.Uint32(b)
			if uint64(length) > uint64(len(b)-4) {
				return core.Invocation{}, 0, errMember
			}
			n := int(length)
			inv, _, err := whole(b[4:4+n], v)
			if err == nil {
				inv = asResult(inv)
			}

			return inv, 4 + n, err
		}
		out = append(out, &m)
	}

	return out
}

// resultHead is a member's result head: its code, its status and its body's
// length, 4 bytes each.
const resultHead = 12

// appendHead appends a compound reply's head: its status, its tag's length,
// the tag and its count of results.
func appendHead(b []byte, s core.Status, tag []byte, n int) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(s))
	b = binary.BigEndian.AppendUint32(b, uint32(len(tag)))
	b = append(b, tag...)
	return binary.BigEndian.AppendUint32(b, uint32(n))
}

// asResult is a member's invocation as a compound's result: its body follows
// the result head encodeResult wrote, whose length it sets, so a reader finds
// the next result; and its bound counts that head.
func asResult(inv core.Invocation) core.Invocation {
	bound, enc := inv.Bound, inv.Encode
	inv.Bound = func() int { return resultHead + bound() }
	inv.Encode = func(out *resources.Buf, v core.Version) error {
		at := len(out.B)
		if at < 4 {
			return errMember
		}
		if err := enc(out, v); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(out.B[at-4:], uint32(len(out.B)-at))

		return nil
	}

	return inv
}

// encodeResult writes a member's result head, its body's length 0 until the
// member's own Encode sets it: a member that did not decode has none.
func encodeResult(out *resources.Buf, op uint32, s core.Status, _ core.Version) error {
	out.B = binary.BigEndian.AppendUint32(out.B, op)
	out.B = binary.BigEndian.AppendUint32(out.B, uint32(s))
	out.B = binary.BigEndian.AppendUint32(out.B, 0)

	return nil
}
