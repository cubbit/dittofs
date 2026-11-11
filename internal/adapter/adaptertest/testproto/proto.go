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

// holdFor is how long version 2 holds a request the service answered
// ErrDelay before it answers StatusDelay.
const holdFor = 10 * time.Second

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
	tables func() [2]*core.OpTable
}

// New returns the protocol over d: its handlers call d.VFS.
//
// decision: the tables are built at the first Route, not here, so a test that
// starts a server on testproto and routes nothing needs no table
// construction. A table that fails its checks therefore shows at the first
// request, which a test protocol can afford. Withdraw it if a test needs the
// failure at New.
func New(d adapter.Deps) *Protocol {
	p := &Protocol{d: d}
	p.tables = sync.OnceValue(p.build)

	return p
}

// Bare is the same frames with the Header and Router stages only, on version
// 1: what core's defaults for every other stage are tested on.
func (p *Protocol) Bare() core.Protocol {
	return &Protocol{d: p.d, bare: true, tables: p.tables}
}

// Framer finds a record's length.
func (p *Protocol) Framer() core.Framer { return framer{} }

// NewConn returns the connection's stages.
func (p *Protocol) NewConn(c *core.Conn) core.Stages {
	cs := &connState{p: p, c: c}

	return core.Stages{Header: headerCodec{}, Router: &router{cs}}
}

// connState is one connection's state.
type connState struct {
	p       *Protocol
	c       *core.Conn
	version atomic.Uint32 // the version Negotiate picked; 0 before one
}

// state is one request's, as Request.State: a related member shares the one
// of the member before it, so the filehandle a PutFH set is the next's.
type state struct {
	conn *connState
	fh   vfs.Handle   // the current filehandle; nil: none
	max  core.Version // on a Negotiate: the highest version it may pick
}

// router chooses the version's table and builds the Request.
type router struct{ cs *connState }

// Route answers the table of the header's version: 1 or 2, and 1 alone on
// Bare.
func (rt *router) Route(h core.Header, prev *core.Request) (*core.OpTable, *core.Request, error) {
	v := h.Version()
	if v < 1 || v > 2 || (rt.cs.p.bare && v != 1) {
		return nil, nil, fmt.Errorf("testproto: version %d: %w", v, core.ErrVersion)
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
		if set, ok := rt.cs.c.Settings().(Settings); ok && set.MaxVersion != 0 {
			s.max = set.MaxVersion
		}
	}
	level := core.NeedsNothing
	if rt.cs.version.Load() != 0 {
		level = core.NeedsNegotiated
	}
	if s.fh != nil {
		level |= core.NeedsFH
	}

	return rt.cs.p.tables()[v-1], &core.Request{Version: v, Level: level, Conn: rt.cs.c, State: s}, nil
}

// build is both versions' tables: version 1 answers every error at once,
// version 2 holds.
func (p *Protocol) build() [2]*core.OpTable {
	return [2]*core.OpTable{
		core.NewOpTable(core.NewErrorTable(StatusFault, errorRows(false)...), false, p.ops(1)...),
		core.NewOpTable(core.NewErrorTable(StatusFault, errorRows(true)...), true, p.ops(2)...),
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
		delay.Hold = func(_ error, start time.Time) time.Time { return start.Add(holdFor) }
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

// ops is version v's rows.
func (p *Protocol) ops(core.Version) []*core.Op {
	both := core.Range{First: 1, Last: 2}

	return []*core.Op{
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
