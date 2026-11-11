package testproto

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The statuses, one per error-table row.
const (
	StatusOK             core.Status = iota
	StatusFault                      // an error no row maps
	StatusNotSupported               // core.ErrNotSupported
	StatusVersion                    // core.ErrVersion
	StatusNotNegotiated              // core.ErrNotNegotiated
	StatusNoSession                  // core.ErrNoSession
	StatusNoTree                     // core.ErrNoTree
	StatusNoFH                       // core.ErrNoFH
	StatusTooLong                    // core.ErrTooLong: a compound past its cap
	StatusExpired                    // core.ErrExpired
	StatusNotLast                    // core.ErrNotLast
	StatusTooManyPending             // core.ErrTooManyPending
	StatusDelay                      // vfs.ErrDelay: at once in version 1, once its hold passes in version 2
	StatusCancelled                  // a parked or held request its client cancelled
)

// HoldFor is how long version 2 holds a request the service answered
// ErrDelay before it answers StatusDelay.
const HoldFor = 10 * time.Second

// Settings is testproto's shape; its Router reads it from Conn.Settings() at
// a Negotiate.
type Settings struct {
	settings.Adapter
	MaxVersion core.Version // the highest version Negotiate picks; 0: both
}

// Validate refuses a MaxVersion past 2 and checks nothing else.
//
// decision: the common part is not checked here, because package settings
// exports no check of it: NFS's and SMB's shapes run theirs from inside that
// package. A test handing core a testproto.Settings supplies a common part
// taken from settings' own defaults. Withdraw it, and call the check, if
// settings ever exports one.
func (s Settings) Validate() error {
	if s.MaxVersion > 2 {
		return fmt.Errorf("testproto: max version %d: there are versions 1 and 2", s.MaxVersion)
	}

	return nil
}

// Protocol is the test-only protocol, as core.Protocol.
//
// decision: testproto is not a test double. It is a third implementation of
// core.Protocol, and a core test's subject is core, with this as its input.
// It stays honest by doing nothing in the service's place: a handler decodes,
// makes at most one service call and encodes, and every error, delay and hold
// a test needs comes from the service the test supplies. Withdraw it, and
// hold this package to a conformance suite, if a core test ever needs
// testproto to decide something the service would.
type Protocol struct {
	d      adapter.Deps
	bare   bool
	reg    *registry
	pool   *resources.BufPool // over the reply budget; nil when d has no budgets
	tables func() [2]*core.OpTable
}

// New returns the protocol over d: its handlers call d.VFS, and the replay
// cache's copies and Notify's message are reserved from d.Budgets.Reply.
//
// decision: the tables are built at the first Route, not here, so a test that
// starts a server on testproto and routes nothing needs no table
// construction. A table that fails its checks therefore shows at the first
// request, which a test protocol can afford. Withdraw it if a test needs the
// failure at New.
func New(d adapter.Deps) *Protocol {
	p := &Protocol{d: d, reg: newRegistry()}
	if d.Budgets != nil && d.Budgets.Reply != nil {
		p.pool = resources.NewBufPool(d.Budgets.Reply)
	}
	p.tables = sync.OnceValue(p.build)

	return p
}

// Bare is the same frames with the Header and Router stages only, on version
// 1: what core's defaults for every other stage are tested on.
func (p *Protocol) Bare() core.Protocol {
	q := *p
	q.bare = true

	return &q
}

// Framer finds a record's length.
func (p *Protocol) Framer() core.Framer { return framer{} }

// NewConn returns the connection's stages: every one on the full protocol,
// the Hold stage included since the connection may reach version 2, and the
// Header and Router alone on Bare.
func (p *Protocol) NewConn(c *core.Conn) core.Stages {
	cs := &connState{p: p, c: c}
	if p.bare {
		return core.Stages{Header: headerCodec{}, Router: &router{cs}}
	}
	credits := newFlow()
	cs.replay = &replay{pool: p.pool, flow: credits, m: map[replayKey][]byte{}}

	return core.Stages{
		Header:     headerCodec{flow: credits},
		Router:     &router{cs},
		Security:   security{},
		Flow:       credits,
		Replay:     cs.replay,
		Hold:       hold{flow: credits},
		MaxPending: 8,
		OnClose:    func(context.Context) { p.reg.leave(cs) },
	}
}

// connState is one connection's state.
type connState struct {
	p       *Protocol
	c       *core.Conn
	version atomic.Uint32 // the version Negotiate picked; 0 before one
	replay  *replay       // the connection's replay cache; nil on Bare
}

// state is one request's, as Request.State: a related member shares the one
// of the member before it, so the filehandle a PutFH set is the next's.
type state struct {
	conn *connState
	sid  uint32       // the session the header names, resolved
	tid  uint32       // the tree the header names
	fh   vfs.Handle   // the current filehandle; nil: none
	max  core.Version // on a Negotiate: the highest version it may pick
}

// router chooses the version's table and builds the Request.
type router struct{ cs *connState }

// Route answers the table of the header's version: 1 or 2, and 1 alone on
// Bare.
func (rt *router) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	r, err := rt.request(h, prev)
	if err != nil {
		return nil, nil, err
	}

	return rt.cs.p.tables()[r.Version-1], r, nil
}

// request builds the Request: its level is what the connection negotiated
// and what the header's session and tree reach, and its credential ends when
// the session's does.
func (rt *router) request(h core.Header, prev *core.Request) (*core.Request, error) {
	hd, ok := h.(*header)
	if !ok {
		return nil, errors.New("testproto: route a header of another protocol")
	}
	v := h.Version()
	if v < 1 || v > 2 || (rt.cs.p.bare && v != 1) {
		return nil, fmt.Errorf("testproto: version %d: %w", v, core.ErrVersion)
	}
	// A connection keeps what it negotiated: a request in a version above it
	// is refused, one below it is served by that version's table.
	if n := rt.cs.version.Load(); n != 0 && uint32(v) > n {
		return nil, fmt.Errorf("testproto: version %d on a connection that negotiated %d: %w", v, n, core.ErrVersion)
	}
	var s *state
	if prev != nil && h.Related() {
		s, _ = prev.State.(*state)
	}
	if s == nil {
		s = &state{conn: rt.cs}
	}
	if h.Op() == OpNegotiate {
		s.max = 2
		switch set := rt.cs.c.Settings().(type) {
		case Settings:
			s.max = set.MaxVersion
		case *Settings:
			s.max = set.MaxVersion
		}
		if s.max == 0 {
			s.max = 2
		}
	}
	s.sid, s.tid = hd.session(), hd.tree()
	r := &core.Request{Version: v, Level: core.NeedsNothing, Conn: rt.cs.c, State: s}
	if rt.cs.version.Load() != 0 {
		r.Level = core.NeedsNegotiated
		if carried, expires, hasTree := rt.cs.p.reg.look(s.sid, rt.cs, s.tid); carried && !rt.cs.p.bare {
			r.Level = core.NeedsSession
			if hasTree {
				r.Level = core.NeedsTree
			}
			r.Call.Identity.Evidence.Expires = expires
		}
	}
	if s.fh != nil {
		r.Level |= core.NeedsFH
	}

	return r, nil
}

// build is both versions' tables: version 1 answers every error at once,
// version 2 holds.
func (p *Protocol) build() [2]*core.OpTable {
	errs := core.NewErrorTable(StatusFault, errorRows(false)...)
	members := core.NewOpTable(errs, false, memberRows(p.ops())...)
	members.ReadOp = readOp

	return [2]*core.OpTable{
		core.NewOpTable(errs, false, append(p.ops(), compound(members))...),
		core.NewOpTable(core.NewErrorTable(StatusFault, errorRows(true)...), true, p.ops()...),
	}
}

// errorRows is a table's rows, with the hold row's Hold set where the table
// holds.
//
// decision: the tables are not total. They map core's refusals, the service's
// ErrDelay, a cancel and the two drain errors, and nothing else, so a service
// error no row maps is reachable and answered StatusFault. Totality is a real
// protocol's property, checked on NFS's and SMB's tables. Withdraw it if a
// core test needs a service error this table leaves unmapped to have a status
// of its own.
func errorRows(holds bool) []core.ErrorRow {
	// decision: this row is testproto's only delay and only hold. A request is
	// delayed or held because the service a test supplied answered ErrDelay,
	// never because an operation here waits or answers an error itself.
	// Withdraw it if core gains a hold that no service error can cause.
	delay := core.ErrorRow{Err: vfs.ErrDelay, Status: StatusDelay}
	if holds {
		delay.Hold = func(_ error, start time.Time) time.Time { return start.Add(HoldFor) }
	}

	return []core.ErrorRow{
		{Err: core.ErrNotSupported, Status: StatusNotSupported},
		{Err: core.ErrVersion, Status: StatusVersion},
		{Err: core.ErrNotNegotiated, Status: StatusNotNegotiated},
		{Err: core.ErrNoSession, Status: StatusNoSession},
		{Err: core.ErrNoTree, Status: StatusNoTree},
		{Err: core.ErrNoFH, Status: StatusNoFH},
		{Err: core.ErrTooLong, Status: StatusTooLong},
		{Err: core.ErrExpired, Status: StatusExpired},
		{Err: core.ErrNotLast, Status: StatusNotLast},
		{Err: core.ErrTooManyPending, Status: StatusTooManyPending},
		delay,
		{Err: context.Canceled, Status: StatusCancelled},
		{Err: core.ErrDraining, NoReply: true},
		{Err: core.ErrMoved, NoReply: true},
	}
}

// ops is the rows both tables hold, each with the versions it is served in:
// Park's is version 2 alone, so version 1's table refuses it by its Versions
// column. Version 1's compound row is build's, over its members' table.
func (p *Protocol) ops() []*core.Op {
	both := core.Range{First: 1, Last: 2}

	return []*core.Op{
		{Code: OpSession, Name: "Session", Versions: both, Needs: core.NeedsNegotiated, Expired: true, Decode: core.Bind(
			decodeSession, noBound[sessionReq], nil, nil, nil, p.session, encodeNothing[empty])},
		{Code: OpLogoff, Name: "Logoff", Versions: both, Needs: core.NeedsSession, Role: core.RoleRelease, Expired: true, Decode: core.Bind(
			noArguments[empty], noBound[empty], nil, nil, nil, p.logoff, encodeNothing[empty])},
		{Code: OpTree, Name: "Tree", Versions: both, Needs: core.NeedsSession, Decode: core.Bind(
			noArguments[empty], noBound[empty], nil, nil, nil, p.tree, encodeNothing[empty])},
		{Code: OpNotify, Name: "Notify", Versions: both, Needs: core.NeedsNothing, Decode: core.Bind(
			decodeTarget, noBound[targetReq], nil, nil, nil, p.notify, encodeNothing[empty])},
		{Code: OpViolate, Name: "Violate", Versions: both, Needs: core.NeedsNothing, Decode: core.Bind(
			noArguments[empty], noBound[empty], nil, nil, nil, violate, encodeNothing[empty])},
		{Code: OpPark, Name: "Park", Versions: core.Range{First: 2, Last: 2}, Needs: core.NeedsTree | core.NeedsFH, Decode: core.Bind(
			decodeName, noBound[nameReq], nil, nil, nil, p.park, encodeNothing[empty])},
		{Code: OpNegotiate, Name: "Negotiate", Versions: both, Needs: core.NeedsNothing, Decode: core.Bind(
			noArguments[empty], noBound[empty], nil, nil, nil, negotiate, encodeVersion)},
		{Code: OpPutFH, Name: "PutFH", Versions: both, Needs: core.NeedsTree, Role: core.RoleContext, Decode: core.Bind(
			decodeHandle, noBound[handleReq], nil, nil, handleOf, putFH, encodeNothing[empty])},
		{Code: OpEcho, Name: "Echo", Versions: both, Needs: core.NeedsNothing, Decode: core.Bind(
			decodeEcho, echoBound, nil, echoRole, nil, echo, encodeEcho)},
		{Code: OpCreate, Name: "Create", Versions: both, Needs: core.NeedsTree | core.NeedsFH, Cacheable: true, Decode: core.Bind(
			decodeName, noBound[nameReq], nil, nil, nil, p.create, encodeNothing[empty])},
		{Code: OpPanic, Name: "Panic", Versions: both, Needs: core.NeedsNothing, Decode: core.Bind(
			noArguments[empty], noBound[empty], nil, nil, nil, panics, encodeNothing[empty])},
	}
}

// empty is the arguments, or the reply, of an operation that has none.
type empty struct{}

func noArguments[Req any](b []byte, _ core.Version, _ *Req) (int, error) { return len(b), nil }

func noBound[Req any](*Req) int { return 0 }

// encodeNothing encodes a reply with no body, a nil one included.
func encodeNothing[Rep any](*Rep, core.Version, *resources.Buf) error { return nil }

// stateOf is the request's state, which the Router set.
func stateOf(r *core.Request) (*state, error) {
	s, ok := r.State.(*state)
	if !ok {
		return nil, errors.New("testproto: a request the Router did not build")
	}

	return s, nil
}

// negotiate picks the lower of the version the request came in and the
// highest the settings allow, and answers it.
func negotiate(_ context.Context, r *core.Request, _ *empty) (*core.Version, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	v := min(r.Version, s.max)
	s.conn.version.Store(uint32(v))

	return &v, nil
}

// encodeVersion appends the version picked, one byte; a nil reply has no
// body.
func encodeVersion(v *core.Version, _ core.Version, out *resources.Buf) error {
	if v != nil {
		out.B = append(out.B, uint8(*v))
	}

	return nil
}

// handleReq is PutFH's arguments: the handle's bytes.
type handleReq struct{ handle vfs.Handle }

func decodeHandle(b []byte, _ core.Version, req *handleReq) (int, error) {
	req.handle = append(vfs.Handle(nil), b...)

	return len(b), nil
}

func handleOf(req *handleReq) (vfs.Handle, bool) { return req.handle, true }

// putFH makes the handle the current filehandle of the request and of the
// related members after it.
func putFH(_ context.Context, r *core.Request, req *handleReq) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	s.fh = req.handle
	r.Level |= core.NeedsFH // for the members after it in a compound, which is routed once

	return &empty{}, nil
}

// echoReq is Echo's arguments: its per-request role, then the payload.
type echoReq struct {
	role    core.Role
	payload []byte
}

func decodeEcho(b []byte, _ core.Version, req *echoReq) (int, error) {
	if len(b) < 1 {
		return 0, errors.New("testproto: an Echo with no role")
	}
	req.role, req.payload = core.Role(b[0]), b[1:]

	return len(b), nil
}

func echoBound(req *echoReq) int { return len(req.payload) }

func echoRole(_ *core.Request, req *echoReq) core.Role { return req.role }

// echo answers the payload.
func echo(_ context.Context, _ *core.Request, req *echoReq) (*[]byte, error) {
	return &req.payload, nil
}

func encodeEcho(payload *[]byte, _ core.Version, out *resources.Buf) error {
	if payload != nil {
		out.B = append(out.B, *payload...)
	}

	return nil
}

// nameReq is Create's and Park's arguments: a name.
type nameReq struct{ name []byte }

func decodeName(b []byte, _ core.Version, req *nameReq) (int, error) {
	req.name = b

	return len(b), nil
}

// create makes one service call: a Create of the name in the current
// filehandle's directory.
func (p *Protocol) create(ctx context.Context, r *core.Request, req *nameReq) (*empty, error) {
	s, err := stateOf(r)
	if err != nil {
		return nil, err
	}
	if _, err := p.d.VFS.Create(ctx, r.Call, s.fh, req.name, vfs.CreateArgs{}); err != nil {
		return nil, err
	}

	return &empty{}, nil
}

// panics is Panic's handler.
func panics(context.Context, *core.Request, *empty) (*empty, error) {
	panic("testproto: the Panic operation")
}
