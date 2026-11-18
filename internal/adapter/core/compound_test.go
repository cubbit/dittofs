package core_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/metrics"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// compoundArgs is the arguments of a version 1 compound carrying members, as
// its row's Decode is handed them, and the compound row.
func compoundArgs(t *testing.T, members ...testproto.Msg) (*core.Op, []byte) {
	t.Helper()
	b := testproto.Append(nil, testproto.Msg{Op: testproto.OpCompound, Version: 1, Members: members})
	return routed(t)[0].Ops[testproto.OpCompound], b[4+40:]
}

// walk decodes the compound row's own arguments and walks its members.
func walk(t *testing.T, row *core.Op, args []byte) (n, at int, err error) {
	t.Helper()
	inv, used, err := row.Decode(args, 1)
	if err != nil {
		t.Fatal(err)
	}
	return core.Members(row, inv, args[used:], 1)
}

var (
	putFH = testproto.Msg{Op: testproto.OpPutFH, Handle: []byte("h")}
	echo  = testproto.Msg{Op: testproto.OpEcho, Payload: []byte("e")}
	panik = testproto.Msg{Op: testproto.OpPanic}
)

// The walk decodes exactly its count: three members of three; none, with
// the compound's decode error, for one byte more; the two a count of three
// has bytes for, the third's decode error at index 2; and a code the table
// lacks reported at its index, the members before it decoded.
func TestTheWalk(t *testing.T) {
	row, args := compoundArgs(t, putFH, echo, panik)
	if n, at, err := walk(t, row, args); n != 3 || at != 3 || err != nil {
		t.Errorf("three members: %d decoded, at %d, %v", n, at, err)
	}
	if n, at, err := walk(t, row, append(slices.Clone(args), 0)); n != 0 || at != -1 || !errors.Is(err, core.ErrDecode) {
		t.Errorf("a byte after the last member: %d decoded, at %d, %v; want none and the compound's decode error", n, at, err)
	}
	_, two := compoundArgs(t, putFH, echo)
	binary.BigEndian.PutUint32(two, 3)
	if n, at, err := walk(t, row, two); n != 2 || at != 2 || !errors.Is(err, core.ErrDecode) {
		t.Errorf("a count of three over two members: %d decoded, at %d, %v", n, at, err)
	}
	_, unknown := compoundArgs(t, putFH, testproto.Msg{Op: 99}, echo)
	if n, at, err := walk(t, row, unknown); n != 1 || at != 1 || !errors.Is(err, core.ErrNotSupported) {
		t.Errorf("an unknown code at index 1: %d decoded, at %d, %v", n, at, err)
	}
}

// The cap refuses a count past it before any member's code is read; a count
// at it is decoded.
func TestTheCapRefusesBeforeAnyMemberIsRead(t *testing.T) {
	row, _ := compoundArgs(t)
	var reads atomic.Int64
	members := *row.Members
	read := members.ReadOp
	members.ReadOp = func(b []byte) (uint32, int, error) {
		reads.Add(1)
		return read(b)
	}
	capped := *row
	capped.Members = &members
	for n, want := range map[int]error{9: core.ErrTooLong, 8: nil} {
		reads.Store(0)
		_, args := compoundArgs(t, slices.Repeat([]testproto.Msg{echo}, n)...)
		got, _, err := walk(t, &capped, args)
		if !errors.Is(err, want) || (want != nil && reads.Load() != 0) || (want == nil && got != n) {
			t.Errorf("%d members: %d decoded, %v, %d codes read; want %v", n, got, err, reads.Load(), want)
		}
	}
}

// judge serves a compound of a PutFH and an Echo that says release, refuses
// one whose Echo says none or whose last member does not decode, and returns
// the PutFH's handle in each.
func TestJudge(t *testing.T) {
	release := echo
	release.Role = core.RoleRelease
	none := echo
	none.Role = core.RoleNone
	for _, c := range []struct {
		name    string
		members []testproto.Msg
		whole   bool
	}{
		{"an Echo that says release", []testproto.Msg{putFH, release}, true},
		{"an Echo that says none", []testproto.Msg{putFH, none}, false},
		{"a last member that does not decode", []testproto.Msg{putFH, release, {Op: 99}}, false},
	} {
		row, args := compoundArgs(t, c.members...)
		inv, used, err := row.Decode(args, 1)
		if err != nil {
			t.Fatal(err)
		}
		whole, handles := core.Judge(row, inv, args[used:], 1, &core.Request{})
		if whole != c.whole || len(handles) != 1 || string(handles[0]) != "h" {
			t.Errorf("%s: judged whole %v with handles %q; want %v and the PutFH's", c.name, whole, handles, c.whole)
		}
	}
}

// A table with a compound row demands its cap, its members' encoders, and
// rows for ErrTooLong and ErrReplyTooBig naming neither operations nor
// versions; a table without one demands none of them.
func TestACompoundRowsTable(t *testing.T) {
	one := core.Range{First: 1, Last: 1}
	members := func() *core.OpTable {
		m := core.NewOpTable(table(), false, &core.Op{Code: 1, Name: "Echo", Versions: one})
		m.ReadOp = func([]byte) (uint32, int, error) { return 1, 1, nil }
		m.EncodeResult = func(*resources.Buf, uint32, core.Status, core.Version) error { return nil }
		return m
	}
	rows := func(extra ...core.ErrorRow) *core.ErrorTable {
		return table(append([]core.ErrorRow{{Err: core.ErrTooLong, Status: 1}, {Err: core.ErrReplyTooBig, Status: 2}}, extra...)...)
	}
	for _, c := range []struct {
		name  string
		errs  *core.ErrorTable
		row   core.Op
		names string // what the panic names; empty: none
	}{
		{"a compound row with its cap and rows", rows(), core.Op{Code: 9, Name: "Compound", Versions: one, Members: members(), MaxMembers: 8}, ""},
		{"no cap", rows(), core.Op{Code: 9, Name: "Compound", Versions: one, Members: members()}, "MaxMembers"},
		{"no ErrReplyTooBig row", table(core.ErrorRow{Err: core.ErrTooLong, Status: 1}), core.Op{Code: 9, Name: "Compound", Versions: one, Members: members(), MaxMembers: 8}, "reply"},
		{"an ErrReplyTooBig row naming an operation", table(core.ErrorRow{Err: core.ErrTooLong, Status: 1}, core.ErrorRow{Err: core.ErrReplyTooBig, Ops: []uint32{9}, Status: 2}), core.Op{Code: 9, Name: "Compound", Versions: one, Members: members(), MaxMembers: 8}, "reply"},
		{"no ErrTooLong row", table(core.ErrorRow{Err: core.ErrReplyTooBig, Status: 2}), core.Op{Code: 9, Name: "Compound", Versions: one, Members: members(), MaxMembers: 8}, "cap"},
		{"members with no encoders", rows(), core.Op{Code: 9, Name: "Compound", Versions: one, Members: core.NewOpTable(table(), false), MaxMembers: 8}, "EncodeResult"},
		{"no compound row, and neither row", table(), core.Op{Code: 9, Name: "Read", Versions: one}, ""},
	} {
		row := c.row
		p := opTablePanic(c.errs, false, &row)
		switch {
		case c.names == "" && p != nil:
			t.Errorf("%s: refused: %v", c.name, p)
		case c.names != "" && (p == nil || !containsFold(p, c.names)):
			t.Errorf("%s: panicked with %v, want a panic naming %s", c.name, p, c.names)
		}
	}
}

// served is a seam over a service of memory behind Faults, its requests
// signed in as the share's administrator, on a connection negotiated with a
// session and a tree: the root's handle, for a PutFH, is root.
type served struct {
	*seam
	svc    *memory.Service
	faults *vfstest.FaultService
	root   vfs.Handle
	proto  core.Protocol       // the server's protocol, for another connection
	tp     *testproto.Protocol // the testproto under proto, for SetHold; set by newChained
}

// newServed is a served seam in version 1, whose table answers every error
// at once, its stages changed by edit.
func newServed(t *testing.T, reply int64, d adapter.Deps, edit ...func(*core.Stages)) *served {
	t.Helper()
	return newServedOn(t, reply, d, func(p core.Protocol) core.Protocol {
		for _, e := range edit {
			p = edited{p, e}
		}
		return p
	})
}

// newServedOn is newServed on the protocol wrap makes of testproto's.
func newServedOn(t *testing.T, reply int64, d adapter.Deps, wrap func(core.Protocol) core.Protocol) *served {
	t.Helper()
	svc := memory.New(t)
	root, _, err := svc.Root(t.Context(), svc.Admin(), vfs.ShareRef{Name: svc.Store.Share.Name})
	if err != nil {
		t.Fatal(err)
	}
	faults := vfstest.Faults(svc)
	d.VFS = faults
	proto := wrap(testproto.New(d).WithCall(svc.Admin()))
	s := &served{seam: newSeamWith(t, proto, reply, d), svc: svc, faults: faults, root: root, proto: proto}
	for _, m := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1},
		{Op: testproto.OpSession, Version: 1, Session: 7},
		{Op: testproto.OpTree, Version: 1, Session: 7, Tree: 1},
	} {
		s.serve(m)
		if r := s.read(); r.Status != testproto.StatusOK {
			t.Fatalf("setting up the connection: %+v", r)
		}
	}
	return s
}

// compound serves a version 1 compound of members, under the session and
// tree, carrying the tag tagOf(id).
func (s *served) compound(id uint64, members ...testproto.Msg) {
	s.serve(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: id, Session: 7, Tree: 1, Members: members, Tag: tagOf(id)})
}

// tagOf is the tag the compound served with message ID id carries, which its
// reply's head echoes.
func tagOf(id uint64) []byte { return binary.BigEndian.AppendUint32(nil, 0x5eed0000|uint32(id)) }

// headIs says whether hd is a head of status s echoing tag.
func headIs(hd testproto.Head, s core.Status, tag []byte) bool {
	return hd.Status == s && bytes.Equal(hd.Tag, tag)
}

// resultsWithOps reads the next reply as a compound's: its status and its
// results, each naming the operation it answers.
func (s *served) resultsWithOps() (core.Status, []testproto.Result) {
	s.t.Helper()
	r := s.read()
	if r.Status != testproto.StatusOK {
		s.t.Fatalf("the compound is answered %d in its header", r.Status)
	}
	hd, rs, err := testproto.Results(r.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	if !bytes.Equal(hd.Tag, tagOf(r.ID)) {
		s.t.Errorf("the compound's head echoes tag %x, want its own %x", hd.Tag, tagOf(r.ID))
	}
	return hd.Status, rs
}

// results reads the next reply as a compound's: its status and its results'
// statuses.
func (s *served) results() (core.Status, []core.Status) {
	s.t.Helper()
	st, rs := s.resultsWithOps()
	var out []core.Status
	for _, res := range rs {
		out = append(out, res.Status)
	}
	return st, out
}

// creates is how many Creates reached the service.
func (s *served) creates() int {
	n := 0
	for _, op := range s.svc.Ops() {
		if op == vfs.OpCreate {
			n++
		}
	}
	return n
}

// A compound of a PutFH and two Creates runs them in turn: two files. With
// the first Create failed by the service, the second does not run and the
// reply ends at the failure; with a third member whose code the table
// lacks, the first two run and the reply ends at its error.
func TestACompoundRunsItsMembersInTurn(t *testing.T) {
	s := newServed(t, 1<<20, adapter.Deps{})
	s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "a"}, testproto.Msg{Op: testproto.OpCreate, Name: "b"})
	if st, rs := s.resultsWithOps(); st != testproto.StatusOK || len(rs) != 3 || s.creates() != 2 ||
		rs[0].Op != testproto.OpPutFH || rs[1].Op != testproto.OpCreate || rs[2].Op != testproto.OpCreate {
		t.Errorf("the compound is answered %d with results %v and made %d files; want 3 results, each naming its operation, and two files", st, rs, s.creates())
	}

	s.svc.Reset()
	s.faults.Fail(vfs.OpCreate, vfs.ErrExist)
	s.compound(2, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "c"}, testproto.Msg{Op: testproto.OpCreate, Name: "d"})
	if st, rs := s.results(); st != testproto.StatusFault || !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusFault}) || s.creates() != 0 {
		t.Errorf("a failed first Create: answered %d with results %v, %d files made; want the reply ending at the failure and no file", st, rs, s.creates())
	}

	s.svc.Reset()
	s.compound(4, testproto.Msg{Op: testproto.OpCreate, Name: "g"})
	if st, rs := s.results(); st != testproto.StatusNoFH || !slices.Equal(rs, []core.Status{testproto.StatusNoFH}) || s.creates() != 0 {
		t.Errorf("a Create with no filehandle before it: answered %d with %v, %d files made; want the row's check to refuse it", st, rs, s.creates())
	}

	s.svc.Reset()
	s.compound(3, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "e"}, testproto.Msg{Op: 99})
	if st, rs := s.results(); st != testproto.StatusNotSupported || !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusOK, testproto.StatusNotSupported}) || s.creates() != 1 {
		t.Errorf("a third member the table lacks: answered %d with results %v, %d files made; want the first two run and the third's error", st, rs, s.creates())
	}
}

// A compound runs on past a member answered a NotFailure row's status: a
// PutFH, an Echo answered it and a Create make the file. A member after it
// that does not decode is answered its error.
func TestACompoundRunsPastANotFailureStatus(t *testing.T) {
	s := newServed(t, 1<<20, adapter.Deps{})
	warn := testproto.Msg{Op: testproto.OpEcho, Payload: []byte(testproto.NotFailure)}
	s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, warn, testproto.Msg{Op: testproto.OpCreate, Name: "a"})
	if st, rs := s.results(); st != testproto.StatusOK || !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusNotFailure, testproto.StatusOK}) || s.creates() != 1 {
		t.Errorf("a compound past a NotFailure status is answered %d with results %v, %d files made; want the Create run and its file", st, rs, s.creates())
	}
	s.compound(2, warn, testproto.Msg{Op: 99})
	if st, rs := s.results(); st != testproto.StatusNotSupported || !slices.Equal(rs, []core.Status{testproto.StatusNotFailure, testproto.StatusNotSupported}) {
		t.Errorf("a member the table lacks after a NotFailure status: answered %d with results %v; want its error after the NotFailure status", st, rs)
	}
}

// A member whose error joins a NotFailure row's error to one a failure's row
// maps is answered that failure, and the members after it do not run: a
// PutFH, an Echo answered its NotFailure error joined to ErrIO, and a Create
// end at the Echo's StatusIO with no file made (RFC 20 §4.7's Map).
func TestAJoinedFailureEndsTheCompound(t *testing.T) {
	s := newServed(t, 1<<20, adapter.Deps{})
	joined := testproto.Msg{Op: testproto.OpEcho, Payload: []byte(testproto.JoinedIO)}
	s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, joined, testproto.Msg{Op: testproto.OpCreate, Name: "a"})
	if st, rs := s.results(); st != testproto.StatusIO || !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusIO}) || s.creates() != 0 {
		t.Errorf("a compound past a joined failure is answered %d with results %v, %d files made; want it ended at the Echo's StatusIO and no file", st, rs, s.creates())
	}
}

// A compound of 9 members is answered the too-long status with no member
// read or run; one of 8 runs.
func TestTheCapAtNineAndEight(t *testing.T) {
	m := metrics.New("v", "c")
	s := newServed(t, 1<<20, adapter.Deps{Metrics: m})
	s.compound(1, slices.Repeat([]testproto.Msg{{Op: testproto.OpCreate, Name: "x"}}, 9)...)
	r := s.read()
	if r.Status != testproto.StatusTooLong || s.creates() != 0 {
		t.Errorf("9 members: answered %d, %d files made; want the too-long status and none", r.Status, s.creates())
	}
	// A compound refused whole is still a compound's reply: its head, the
	// status and a count of no results, is there for the client to read.
	if hd, rs, err := testproto.Results(r.Body); err != nil || !headIs(hd, testproto.StatusTooLong, tagOf(1)) || len(rs) != 0 {
		t.Errorf("a compound refused past its cap is answered %+v with %v (%v), want its status, its tag and no results", hd, rs, err)
	}
	got, _ := scrape(t, m)
	if k := "adapter_refusals_total{protocol=" + testproto.Name + ",stage="; got[k+"decode_args}"] != 1 || got[k+"route}"] != 0 {
		t.Errorf("a compound past its cap counts %v under decode_args and %v under route, want 1 and 0", got[k+"decode_args}"], got[k+"route}"])
	}
	s.compound(2, slices.Repeat([]testproto.Msg{echo}, 8)...)
	if st, rs := s.results(); st != testproto.StatusOK || len(rs) != 8 {
		t.Errorf("8 members: answered %d with %d results", st, len(rs))
	}
}

// A compound whose charge passes a quarter of the reply pool's ceiling is
// answered the too-big status at once, nothing run and nothing reserved; one
// charged at a quarter runs. With the reply budget full, a compound makes no
// call until room is freed.
func TestTheQuarter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := resources.NewBufPool(resources.NewWaitBudget("probe", 1<<30))
		quarter := pool.Charge(4 << 10) // the small class's charge
		m := metrics.New("v", "c")
		s := newServed(t, 4*quarter, adapter.Deps{Metrics: m})
		// Each Echo's result is 12 bytes and its payload; the compound's
		// header, head with its 4-byte tag and one result head take 68.
		fits := testproto.Msg{Op: testproto.OpEcho, Payload: make([]byte, 4<<10-68-12-1)}
		over := testproto.Msg{Op: testproto.OpEcho, Payload: make([]byte, 4<<10-68-12+1)}
		s.compound(1, over)
		r := s.read()
		if r.Status != testproto.StatusReplyTooBig {
			t.Errorf("a compound charged past a quarter is answered %d, want the too-big status", r.Status)
		}
		if hd, rs, err := testproto.Results(r.Body); err != nil || !headIs(hd, testproto.StatusReplyTooBig, tagOf(r.ID)) || len(rs) != 0 {
			t.Errorf("a compound refused past a quarter is answered %+v with %v (%v), want its status, its tag and no results", hd, rs, err)
		}
		if got, _ := scrape(t, m); got["adapter_refusals_total{protocol="+testproto.Name+",stage=reply_too_big}"] != 1 {
			t.Errorf("the refusal past a quarter counted %v under reply_too_big, want 1", got["adapter_refusals_total{protocol="+testproto.Name+",stage=reply_too_big}"])
		}
		s.compound(2, fits)
		if st, _ := s.results(); st != testproto.StatusOK {
			t.Errorf("a compound charged at a quarter is answered %d", st)
		}
		// A quarter of 20 KiB: the over compound's bytes fit it, but past the
		// small class it is charged the next class whole, which the ceiling
		// holds and the quarter does not.
		wide := newServed(t, 4*(20<<10), adapter.Deps{})
		wide.compound(1, over)
		if r := wide.read(); r.Status != testproto.StatusReplyTooBig {
			t.Errorf("a compound under a quarter in bytes, past it in its charge, is answered %d, want the too-big status", r.Status)
		}
		// With the ceiling four bytes short of four quarters, a compound
		// charged a quarter is past a quarter of it.
		tight := newServed(t, 4*quarter-4, adapter.Deps{})
		tight.compound(1, fits)
		if r := tight.read(); r.Status != testproto.StatusReplyTooBig {
			t.Errorf("a compound charged past a quarter of a %d ceiling is answered %d, want the too-big status", 4*quarter-4, r.Status)
		}

		synctest.Wait()
		free := s.full()
		free2 := s.full()
		free3 := s.full()
		free4 := s.full()
		s.compound(3, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "q"})
		synctest.Wait()
		if s.creates() != 0 {
			t.Error("a compound ran a member before its reply was reserved")
		}
		free()
		free2()
		free3()
		free4()
		if st, _ := s.results(); st != testproto.StatusOK || s.creates() != 1 {
			t.Errorf("once room was freed the compound is answered %d with %d files made", st, s.creates())
		}
	})
}

// file writes n bytes, each its offset's low byte, to a new file under the
// root through the service, and returns its handle and its bytes.
func (s *served) file(name string, n int) (vfs.Handle, []byte) {
	s.t.Helper()
	ctx, c := s.t.Context(), s.svc.Admin()
	f, err := s.svc.Create(ctx, c, s.root, []byte(name), vfs.CreateArgs{Kind: vfs.CreateRegular})
	if err != nil {
		s.t.Fatal(err)
	}
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i)
	}
	if _, err := s.svc.Write(ctx, c, vfs.Anonymous(f.Handle), 0, data, vfs.FileSync); err != nil {
		s.t.Fatal(err)
	}
	return f.Handle, data
}

// readOf is a Read result's eof flag and data.
func readOf(res testproto.Result) (eof bool, data []byte) {
	if len(res.Body) == 0 {
		return false, nil
	}
	return res.Body[0] == 1, res.Body[1:]
}

// The record cap a compound's members pass, the Echo's hook ignoring its
// room, refuses the compound before any member runs, counted reply_too_big:
// the Create after the Echo makes no file. At the cap it runs, its payload
// intact and the file made. The reply pool's quarter holds both, so the cap
// alone refuses.
func TestTheRecordCapRefusesACompoundPastIt(t *testing.T) {
	m := metrics.New("v", "c")
	s := newServed(t, 64<<20, adapter.Deps{Metrics: m})
	record, _ := testproto.New(adapter.Deps{}).Framer().MaxRecord()
	// Each empty Echo adds a 12-byte result head to the reply and fewer bytes
	// to the request, which the record cap also bounds, so the reply passes
	// the cap while the request, the root's handle in it, does not.
	of := func(payload []byte, name string) []testproto.Msg {
		empty := testproto.Msg{Op: testproto.OpEcho}
		return []testproto.Msg{{Op: testproto.OpPutFH, Handle: s.root}, {Op: testproto.OpEcho, Payload: payload},
			empty, empty, empty, empty, {Op: testproto.OpCreate, Name: name}}
	}
	// The compound's ReplyLen, its header, its head with a 4-byte tag and
	// one result's head, takes 68, and the seven results' heads 12 each.
	at := record - 68 - 7*12
	s.compound(1, of(make([]byte, at+1), "a")...)
	r := s.read()
	if hd, rs, err := testproto.Results(r.Body); r.Status != testproto.StatusReplyTooBig || err != nil || !headIs(hd, testproto.StatusReplyTooBig, tagOf(1)) || len(rs) != 0 || s.creates() != 0 {
		t.Errorf("a compound one byte past the record cap is answered %d, head %d with %d results (%v), %d files made; want the too-big status, no results and no file", r.Status, hd.Status, len(rs), err, s.creates())
	}
	if got, _ := scrape(t, m); got["adapter_refusals_total{protocol="+testproto.Name+",stage=reply_too_big}"] != 1 {
		t.Errorf("the refusal past the record cap counted %v under reply_too_big, want 1", got["adapter_refusals_total{protocol="+testproto.Name+",stage=reply_too_big}"])
	}
	payload := bytes.Repeat([]byte("ab"), at/2)
	s.compound(2, of(payload, "b")...)
	if st, rs := s.resultsWithOps(); st != testproto.StatusOK || len(rs) != 7 || !bytes.Equal(rs[1].Body, payload) || s.creates() != 1 {
		t.Errorf("a compound at the record cap is answered %d with %d results, %d files made; want its payload of %d bytes back intact and one file", st, len(rs), s.creates(), len(payload))
	}
}

// Before any member runs, each Read is handed its room within the record
// cap: of two Reads of 768 KiB from a file of 1.5 MiB, the first is answered
// short, at least one byte and eof false, and the second whole, both of the
// file's own bytes, the reply within the cap, and one read counted
// shortened.
func TestACompoundsReadIsShortenedToItsRoom(t *testing.T) {
	m := metrics.New("v", "c")
	s := newServed(t, 64<<20, adapter.Deps{Metrics: m})
	fh, data := s.file("f", 1536<<10)
	s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: fh},
		testproto.Msg{Op: testproto.OpRead, Offset: 0, Count: 768 << 10},
		testproto.Msg{Op: testproto.OpRead, Offset: 768 << 10, Count: 768 << 10})
	st, rs := s.resultsWithOps()
	if st != testproto.StatusOK || len(rs) != 3 {
		t.Fatalf("the compound is answered %d with %d results, want 3", st, len(rs))
	}
	eof1, first := readOf(rs[1])
	eof2, second := readOf(rs[2])
	if eof1 || len(first) < 1 || len(first) >= 768<<10 || !bytes.Equal(first, data[:len(first)]) {
		t.Errorf("the first Read is answered %d bytes, eof %v; want a short read of the file's own bytes, eof false", len(first), eof1)
	}
	if !eof2 || !bytes.Equal(second, data[768<<10:]) {
		t.Errorf("the second Read is answered %d bytes, eof %v; want the file's last 768 KiB whole, eof true", len(second), eof2)
	}
	if got, _ := scrape(t, m); got["adapter_reads_shortened_total{protocol="+testproto.Name+"}"] != 1 {
		t.Errorf("adapter_reads_shortened_total is %v, want the first Read counted", got["adapter_reads_shortened_total{protocol="+testproto.Name+"}"])
	}
}

// A Read whose room, the members after it counted at their bounds, holds no
// byte refuses the compound before any member runs, though the Read after it
// could be shortened to make room: the Create before it makes no file (RFC 20
// §4.3).
func TestAReadLeftNoByteRefusesTheCompound(t *testing.T) {
	s := newServed(t, 64<<20, adapter.Deps{})
	s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "f"},
		testproto.Msg{Op: testproto.OpRead, Offset: 0, Count: 10},
		testproto.Msg{Op: testproto.OpRead, Offset: 10, Count: 2 << 20})
	r := s.read()
	if hd, rs, err := testproto.Results(r.Body); r.Status != testproto.StatusReplyTooBig || err != nil || !headIs(hd, testproto.StatusReplyTooBig, tagOf(1)) || len(rs) != 0 || s.creates() != 0 {
		t.Errorf("a compound whose first Read has no room is answered %d, head %d with %d results (%v), %d files made; want the too-big status, no results and no file", r.Status, hd.Status, len(rs), err, s.creates())
	}
}

// While draining, a compound with one refused member gets no reply and runs
// nothing, refused at decode_args, judged whole before any member resolves
// its IDs (RFC 20 §10.1); a compound begun before the drain runs whole.
func TestADrainedCompound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		s := newServed(t, 1<<20, adapter.Deps{Metrics: m})
		none := echo
		none.Role = core.RoleNone
		hold := s.faults.Hold(vfs.OpCreate)
		s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "before"}, none)
		<-hold.Reached()
		s.p.Drain()
		hold.Release()
		if st, rs := s.results(); st != testproto.StatusOK || len(rs) != 3 {
			t.Errorf("a compound begun before the drain is answered %d with %v, want all three run", st, rs)
		}

		s.svc.Reset()
		s.compound(2, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "drained"}, none)
		if !s.sentNothing() || s.creates() != 0 {
			t.Errorf("a drained compound with a refused member was answered, or made %d files", s.creates())
		}
		got, _ := scrape(t, m)
		if n, wrong := got["adapter_refusals_total{protocol="+testproto.Name+",stage=decode_args}"], got["adapter_refusals_total{protocol="+testproto.Name+",stage=resolve_ids}"]; n != 1 || wrong != 0 {
			t.Errorf("the drained compound counted %v under decode_args and %v under resolve_ids, want 1 and 0", n, wrong)
		}
	})
}

// A compound still waiting for its reply when the drain begins has not been
// handed to the service, so it is judged once its reply is reserved, as a
// request alone is: one the drain does not serve whole is answered nothing
// and runs nothing (§3.1, §4.7).
func TestACompoundWaitingForItsReplyAtTheDrainIsJudged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, 4*smallCharge, adapter.Deps{})
		none := echo
		none.Role = core.RoleNone
		synctest.Wait() // the set-up's replies are released
		frees := []func(){s.full(), s.full(), s.full(), s.full()}
		s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "late"}, none)
		synctest.Wait() // the compound waits for its reply
		s.p.Drain()
		for _, free := range frees {
			free()
		}
		if !s.sentNothing() || s.creates() != 0 {
			t.Errorf("a compound that waited for its reply through the drain was answered, or made %d files", s.creates())
		}
	})
}

// A member whose call the connection's close ended ends the compound with
// nothing sent and no member after it run: the close's cancel is no error, so
// nothing counts as an error no row maps and nothing logs at error. The second
// Create's call is ended as a call under the closed connection's context
// ends, with its cancel.
func TestAMemberCallTheCloseEndsIsNoError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, logs := metrics.New("v", "c"), &logSink{}
		s := newServed(t, 1<<20, adapter.Deps{Metrics: m, Logger: logs.logger()})
		hold := s.faults.Hold(vfs.OpCreate)
		s.faults.Fail(vfs.OpCreate, context.Canceled)
		s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "a"}, testproto.Msg{Op: testproto.OpCreate, Name: "b"}, testproto.Msg{Op: testproto.OpCreate, Name: "c"})
		<-hold.Reached()
		s.p.Close()
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, _ := scrape(t, m)
		if k := "adapter_unmapped_errors_total{protocol=" + testproto.Name + "}"; got[k] != 0 {
			t.Errorf("%s = %v after the close ended a member's call, want 0", k, got[k])
		}
		if e := logs.at(t, "ERROR"); len(e) != 0 {
			t.Errorf("a member's call the close ended logged at error: %v", e)
		}
		if n := s.creates(); n != 1 {
			t.Errorf("the compound made %d files, want only the first: no member runs after the one the close ended", n)
		}
	})
}

// A member's error that a NoReply row maps leaves the whole compound
// unanswered, the members before it applied, since a results array cut short
// under a success is malformed (RFC 20 §4.3).
func TestANoReplyMemberLeavesTheCompoundUnanswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, 1<<20, adapter.Deps{})
		hold := s.faults.Hold(vfs.OpCreate)
		s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "a"}, testproto.Msg{Op: testproto.OpCreate, Name: "b"}, echo)
		<-hold.Reached()
		s.faults.Fail(vfs.OpCreate, core.ErrDraining)
		hold.Release()
		if !s.sentNothing() || s.creates() != 1 {
			t.Errorf("a compound whose second Create meets a NoReply row was answered, or made %d files; want nothing sent and the first file made", s.creates())
		}
	})
}

// A compound's members each run in a span of their own, children of the
// request's.
func TestEachMemberHasASpan(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	s := newServed(t, 1<<20, adapter.Deps{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test"), SampleRatio: 1})
	s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, echo)
	s.results()
	s.finish()
	var request string
	children := map[string]string{}
	for _, sp := range spans.Ended() {
		switch sp.Name() {
		case "adapter." + testproto.Name + ".Compound":
			request = sp.SpanContext().SpanID().String()
		case "adapter." + testproto.Name + ".PutFH", "adapter." + testproto.Name + ".Echo":
			children[sp.Name()] = sp.Parent().SpanID().String()
		}
	}
	if len(children) != 2 || request == "" {
		t.Fatalf("spans: request %q, members %v; want the compound's and one per member", request, children)
	}
	for name, parent := range children {
		if parent != request {
			t.Errorf("%s's parent is %s, want the request span %s", name, parent, request)
		}
	}
}

// A compound's request span carries the compound's own status and the
// service's result: the member's refusal and no result for a compound whose
// member never ran, and zero with the service's ok for one that ran.
func TestACompoundsSpanCarriesItsStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		member testproto.Msg
		want   core.Status
		result string
	}{
		{"run whole", echo, 0, "ok"},
		{"refused at its row check", testproto.Msg{Op: testproto.OpCreate, Name: "b"}, testproto.StatusNoFH, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			spans := tracetest.NewSpanRecorder()
			s := newServed(t, 1<<20, adapter.Deps{Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("test"), SampleRatio: 1})
			s.compound(1, c.member)
			s.results()
			s.finish()
			var got map[string]string
			for _, sp := range spans.Ended() {
				if sp.Name() != "adapter."+testproto.Name+".Compound" {
					continue
				}
				got = map[string]string{}
				for _, kv := range sp.Attributes() {
					got[string(kv.Key)] = kv.Value.String()
				}
			}
			if got == nil {
				t.Fatal("the compound's request span did not end")
			}
			if want := strconv.FormatInt(int64(c.want), 10); got["status"] != want {
				t.Errorf("the compound's span carries status %q, want the compound's %q", got["status"], want)
			}
			if got["result"] != c.result {
				t.Errorf("the compound's span carries result %q, want %q", got["result"], c.result)
			}
		})
	}
}

// A compound's reply reservation is timed under reply_wait, as a single
// request's is: the time it waited.
func TestACompoundTimesItsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := metrics.New("v", "c")
		// A quarter of 64 KiB clears a small class's charge, so the compound
		// is not refused past the ceiling; five holds then fill the rest.
		s := newServed(t, 64<<10, adapter.Deps{Metrics: m})
		before, _ := scrape(t, m)
		var free []func()
		for range 5 {
			held, ok := s.reply.TryGet(peer, 0)
			if !ok {
				t.Fatalf("the reply budget holds %d small classes, want five", len(free))
			}
			free = append(free, held.Release)
		}
		s.compound(1, echo)
		synctest.Wait()
		time.Sleep(50 * time.Millisecond)
		free[0]()
		s.results()
		for _, f := range free[1:] {
			f()
		}
		after, _ := scrape(t, m)
		k := "adapter_wire_seconds_sum{protocol=" + testproto.Name + ",stage=reply_wait}"
		if d := after[k] - before[k]; d < 0.049 || d > 0.051 {
			t.Errorf("a compound that waited 50ms observed %v reply_wait seconds, want 0.05", d)
		}
	})
}

// containsFold reports whether p, printed, names s, in any case.
func containsFold(p any, s string) bool {
	return strings.Contains(strings.ToLower(fmt.Sprint(p)), strings.ToLower(s))
}

// laxRow is a compound row over a members table declared in the test, which
// takes each code's length and each member's length as the bytes give them,
// negative or past the end included: a lax protocol, an input to the walk and
// not a double. steps records how many bytes each ReadOp was handed.
func laxRow(steps *[]int) *core.Op {
	one := core.Range{First: 1, Last: 1}
	decode := func(b []byte, _ core.Version) (core.Invocation, int, error) {
		n := 0
		if len(b) > 0 {
			n = int(int8(b[0]))
		}
		return core.Invocation{}, n, nil
	}
	members := core.NewOpTable(table(), false,
		&core.Op{Code: 0, Name: "Zero", Versions: one, Decode: decode},
		&core.Op{Code: 1, Name: "One", Versions: one, Decode: decode})
	members.ReadOp = func(b []byte) (uint32, int, error) {
		*steps = append(*steps, len(b))
		if len(b) < 2 {
			return 0, 0, errors.New("short")
		}
		return uint32(b[0] % 3), int(int8(b[1])), nil
	}
	return &core.Op{Code: 9, Name: "Compound", Versions: one, Members: members, MaxMembers: 8}
}

// The walks over any bytes end: a compound's reads each member's code
// strictly forward and inside them, and refuses a step that is not with the
// walk's own errors; a chain's, served through a lax codec, reads each header
// strictly forward and inside the record, and closes on a bad step with
// ErrDecode.
func FuzzRecordWalk(f *testing.F) {
	for _, seed := range [][]byte{
		{2, 0, 2, 1, 0, 1, 2, 1, 0},
		{1, 0, 0, 0},
		{3, 1, 255, 0},
		{2, 0, 2, 100},
		{9},
		{255, 0, 2},
		{2, 0, 0, 0},   // a code that takes no byte
		{1, 0, 2, 255}, // arguments of -1 bytes
	} {
		f.Add(seed)
	}
	fr := testproto.New(adapter.Deps{}).Framer()
	e := func(id uint64) testproto.Msg { return testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: id} }
	three, offs := chainOf(fr, e(1), e(2), e(3))
	for _, next := range []int{0, offs[1], offs[1] - 1, len(three), 1 << 31} {
		b := slices.Clone(three)
		binary.BigEndian.PutUint32(b[offs[1]:], uint32(next))
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		walkChain(t, b)
		if len(b) == 0 {
			return
		}
		var steps []int
		count, rest := int(int8(b[0])), b[1:]
		n, at, err := core.Members(laxRow(&steps), core.Invocation{Count: count}, rest, 1)
		for i, s := range steps {
			if s > len(rest) || (i > 0 && s >= steps[i-1]) {
				t.Fatalf("the walk read a code at %v of %d bytes: not strictly forward inside them", steps, len(rest))
			}
		}
		switch {
		case err == nil && n != count:
			t.Fatalf("the walk decoded %d of %d members with no error", n, count)
		case err != nil && !errors.Is(err, core.ErrDecode) && !errors.Is(err, core.ErrTooLong) && !errors.Is(err, core.ErrNotSupported):
			t.Fatalf("the walk refused with %v, not one of its own errors", err)
		case err != nil && at >= 0 && n != at:
			t.Fatalf("the walk stopped at %d with %d decoded", at, n)
		}
	})
}

// A compound row whose Decode sets no head is a fault of the protocol's
// wiring: the connection closes naming the row, and nothing is answered.
func TestACompoundRowThatSetsNoHeadClosesTheConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		noHead := func(inv core.Invocation) core.Invocation {
			inv.EncodeHead = nil
			return inv
		}
		s := newSeam(t, wrapRows(testproto.New(adapter.Deps{}), noHead), 1<<20)
		s.serve(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 1, Members: []testproto.Msg{echo}})
		if err := <-s.closed; err == nil || !strings.Contains(err.Error(), "the Compound row's Decode sets no EncodeHead") {
			t.Errorf("a compound row with no head closed the connection with %v, want the fault naming the row", err)
		}
		if !s.sentNothing() {
			t.Error("a compound row with no head was answered")
		}
	})
}

// A compound refused at route, before its row decodes it, is answered its
// status alone with its head: the tag its header's codec read, and no
// results. So it is on Bare too, whose stages have no Security to read the
// tag after the header (RFC 20 §4.1).
func TestACompoundRefusedAtRouteEchoesItsTag(t *testing.T) {
	off := testproto.Settings{Adapter: settings.Adapter{DisabledOperations: []string{"Compound"}}}
	for name, proto := range map[string]core.Protocol{"full": testproto.New(adapter.Deps{}), "bare": testproto.New(adapter.Deps{}).Bare()} {
		t.Run(name, func(t *testing.T) {
			s := newSeamSet(t, proto, 1<<20, adapter.Deps{}, off)
			s.serve(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 3, Tag: tagOf(3), Members: []testproto.Msg{echo}})
			r := s.read()
			hd, rs, err := testproto.Results(r.Body)
			if r.Status != testproto.StatusNotSupported || err != nil || !headIs(hd, testproto.StatusNotSupported, tagOf(3)) || len(rs) != 0 {
				t.Errorf("a compound switched off is answered %d with head %+v and %v (%v), want not supported, its tag and no results", r.Status, hd, rs, err)
			}
		})
	}
}

// A compound answered its status alone carries a head as long as its tag,
// which its header's codec writes: the reply is reserved at its header's
// ReplyLen, so its bytes lie within its own charge however long the tag. So
// it is refused at route, after Run's own Decode; at Admit on the reader,
// whose status's buffer is tried at once; and at Admit while the pool has no
// room at once for that buffer, so the refusal waits for it.
func TestAStatusAloneReplyIsReservedForItsHead(t *testing.T) {
	tag := bytes.Repeat([]byte{'t'}, 5<<10)
	small := resources.NewBufPool(resources.NewWaitBudget("probe", 1<<30)).Charge(0)
	directory := resources.NewBufPool(resources.NewWaitBudget("probe", 1<<30)).Charge(len(tag) + 64)
	for _, c := range []struct {
		name    string
		off     []string
		credits uint16
		reply   int64
		want    core.Status
		held    bool // a small buffer held, so the status's buffer does not fit at once
	}{
		{"at route", []string{"Compound"}, 0, 1 << 20, testproto.StatusNotSupported, false},
		{"at admit", nil, 9, 1 << 20, testproto.StatusOverdraw, false},
		{"at admit, waiting for room", nil, 9, directory + 4<<10, testproto.StatusOverdraw, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				off := testproto.Settings{Adapter: settings.Adapter{DisabledOperations: c.off}}
				s := newSeamSet(t, testproto.New(adapter.Deps{}), c.reply, adapter.Deps{}, off)
				var free func()
				if c.held {
					held, ok := s.reply.TryGet(peer, 0)
					if !ok || s.budgets.Reply.Reserved() != small {
						t.Fatal("the small buffer does not fit")
					}
					free = held.Release
				}
				var charge int64
				var n int
				s.sent = func(msg *resources.Buf) { charge, n = msg.Charge(), len(msg.B) }
				s.serve(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 5, Credits: c.credits, Tag: tag, Members: []testproto.Msg{echo}})
				if free != nil {
					synctest.Wait()
					if n != 0 {
						t.Fatal("the refusal went out before its buffer had room")
					}
					free()
				}
				r := s.read()
				if hd, _, err := testproto.Results(r.Body); err != nil || !headIs(hd, c.want, tag) {
					t.Fatalf("a compound with a %d-byte tag is answered %d, %v: want %d and its tag", len(tag), r.Status, err, c.want)
				}
				if want := s.reply.Charge(n); charge < want {
					t.Errorf("a reply of %d bytes went out charged %d, under the %d its class costs", n, charge, want)
				}
			})
		})
	}
}

// A member whose code or arguments do not decode is ErrDecode wrapping its
// codec's own error once: the cause's text kept, and the cause not matched.
func TestAMembersDecodeErrorWrapsItsCauseOnce(t *testing.T) {
	row, args := compoundArgs(t, echo)
	inv, used, err := row.Decode(args, 1)
	if err != nil {
		t.Fatal(err)
	}
	rest := args[used:]
	_, _, short := row.Members.ReadOp(rest[:3])
	code, n, _ := row.Members.ReadOp(rest)
	_, _, cut := row.Members.Ops[code].Decode(rest[n:len(rest)-1], 1)
	for _, c := range []struct {
		name  string
		rest  []byte
		cause error
	}{
		{"its code", rest[:3], short},
		{"its arguments", rest[:len(rest)-1], cut},
	} {
		if c.cause == nil || errors.Is(c.cause, core.ErrDecode) {
			t.Fatalf("%s: the codec answered %v, want its own error", c.name, c.cause)
		}
		_, _, err := core.Members(row, inv, c.rest, 1)
		if errors.Unwrap(err) != core.ErrDecode || errors.Is(err, c.cause) || !strings.Contains(err.Error(), c.cause.Error()) || strings.Count(err.Error(), core.ErrDecode.Error()) != 1 {
			t.Errorf("%s: the walk refused with %v, want ErrDecode wrapping %q once", c.name, err, c.cause)
		}
	}
}

// On an expired session, a compound's member that says release runs and
// one that says none is refused the expired status, ending the reply.
func TestAnExpiredCredentialInACompound(t *testing.T) {
	s := newServed(t, 1<<20, adapter.Deps{})
	s.serve(testproto.Msg{Op: testproto.OpSession, Version: 1, Session: 7, Expires: time.Now().Add(-time.Hour)})
	s.read()
	release, none := echo, echo
	release.Role, none.Role = core.RoleRelease, core.RoleNone
	s.compound(1, release, none)
	if st, rs := s.results(); st != testproto.StatusExpired || !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusExpired}) {
		t.Errorf("a compound on an expired session is answered %d with %v, want the release run and the none refused expired", st, rs)
	}
}

// A compound's members refused before their calls count under the stage
// that refused them, in the members' table's series: a member before its
// version at route, an unknown code at route and not at decode_args, and a
// member on an expired credential at resolve_ids; a member whose row runs on
// an expired credential, Session, is answered.
func TestACompoundsRefusedMembersAreCounted(t *testing.T) {
	m := metrics.New("v", "c")
	s := newServed(t, 1<<20, adapter.Deps{Metrics: m})
	key := "adapter_refusals_total{protocol=" + testproto.Name + ",stage="
	s.compound(1, echo, testproto.Msg{Op: 99})
	if st, _ := s.results(); st != testproto.StatusNotSupported {
		t.Errorf("a compound with an unknown member is answered %d", st)
	}
	if got, _ := scrape(t, m); got[key+"route}"] != 1 || got[key+"decode_args}"] != 0 {
		t.Errorf("an unknown member counted %v at route and %v at decode_args, want 1 and 0", got[key+"route}"], got[key+"decode_args}"])
	}
	s.compound(3, echo, testproto.Msg{Op: testproto.OpPark})
	s.results()
	if got, _ := scrape(t, m); got[key+"route}"] != 2 {
		t.Errorf("a member its version does not serve counted %v at route in all, want 2", got[key+"route}"])
	}
	s.serve(testproto.Msg{Op: testproto.OpSession, Version: 1, Session: 7, Expires: time.Now().Add(-time.Hour)})
	s.read()
	release, none := echo, echo
	release.Role, none.Role = core.RoleRelease, core.RoleNone
	s.compound(2, release, testproto.Msg{Op: testproto.OpSession, Session: 7, Expires: time.Now().Add(-time.Hour)}, none)
	if st, rs := s.results(); st != testproto.StatusExpired || !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusOK, testproto.StatusExpired}) {
		t.Errorf("a compound on an expired session is answered %d with %v, want the release and Session run, the none refused expired", st, rs)
	}
	if got, _ := scrape(t, m); got[key+"resolve_ids}"] != 1 {
		t.Errorf("the expired member counted %v at resolve_ids, want 1", got[key+"resolve_ids}"])
	}
}

// A compound member whose arguments do not decode is answered, once the
// member before it ran, and counted at decode_args, as a request alone's
// would be.
func TestAMemberWhoseArgumentsDoNotDecodeIsCounted(t *testing.T) {
	m := metrics.New("v", "c")
	s := newServed(t, 1<<20, adapter.Deps{Metrics: m})
	b := body(s.f, testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 1, Members: []testproto.Msg{echo, echo}, Tag: tagOf(1)})
	// The second Echo's arguments, its role and payload "e", cut to none.
	b = b[:len(b)-2]
	binary.BigEndian.PutUint32(b[len(b)-4:], 0)
	s.serveBody(b)
	if _, rs := s.results(); !slices.Equal(rs, []core.Status{testproto.StatusOK, testproto.StatusDecode}) {
		t.Errorf("the compound is answered %v: want the first Echo, then the decode status", rs)
	}
	if got, _ := scrape(t, m); got["adapter_refusals_total{protocol="+testproto.Name+",stage=decode_args}"] != 1 {
		t.Errorf("a member whose arguments do not decode counted %v at decode_args, want 1", got["adapter_refusals_total{protocol="+testproto.Name+",stage=decode_args}"])
	}
}

// A compound member whose Resolve fails is counted at resolve_ids.
func TestAMemberWhoseResolveFailsIsCounted(t *testing.T) {
	m := metrics.New("v", "c")
	d := adapter.Deps{Metrics: m}
	// The members' Echo has its Invocation.Resolve fail, a fault injected in a
	// real row.
	stale := retable(testproto.New(d), func(op *core.Op, member bool) {
		if !member || op.Code != testproto.OpEcho {
			return
		}
		dec := op.Decode
		op.Decode = func(b []byte, v core.Version) (core.Invocation, int, error) {
			inv, n, err := dec(b, v)
			if err == nil {
				inv.Resolve = func(*core.Request) error { return vfs.ErrStale }
			}
			return inv, n, err
		}
	})
	s := newSeamWith(t, stale, 1<<20, d)
	for _, msg := range []testproto.Msg{
		{Op: testproto.OpNegotiate, Version: 1},
		{Op: testproto.OpSession, Version: 1, Session: 7},
		{Op: testproto.OpTree, Version: 1, Session: 7, Tree: 1},
	} {
		s.serve(msg)
		s.read()
	}
	s.serve(testproto.Msg{Op: testproto.OpCompound, Version: 1, ID: 1, Session: 7, Tree: 1, Members: []testproto.Msg{{Op: testproto.OpEcho, Payload: []byte("e")}}, Tag: tagOf(1)})
	s.read()
	if got, _ := scrape(t, m); got["adapter_refusals_total{protocol="+testproto.Name+",stage=resolve_ids}"] != 1 {
		t.Errorf("a member whose Resolve failed counted %v at resolve_ids, want 1", got["adapter_refusals_total{protocol="+testproto.Name+",stage=resolve_ids}"])
	}
}

// echoMember is p with each Echo its compounds carry handed to edit as it
// decodes: a protocol the test declares, the member's own hooks held or
// failed on cue, not a double.
func echoMember(edit func(core.Invocation) core.Invocation) func(core.Protocol) core.Protocol {
	return func(p core.Protocol) core.Protocol {
		return retable(p, func(op *core.Op, member bool) {
			if !member || op.Code != testproto.OpEcho {
				return
			}
			dec := op.Decode
			op.Decode = func(b []byte, v core.Version) (core.Invocation, int, error) {
				inv, n, err := dec(b, v)
				if err == nil {
					inv = edit(inv)
				}
				return inv, n, err
			}
		})
	}
}

// heldWrap is the stages' Security whose Wrap, once armed, is held until
// open is closed: a delay injected on cue.
type heldWrap struct {
	core.Security
	armed atomic.Bool
	open  chan struct{}
}

func (w *heldWrap) Wrap(h core.Header, reply *resources.Buf) error {
	if w.armed.Load() {
		<-w.open
	}
	return w.Security.Wrap(h, reply)
}

// wireSince is how many times m observed each stage of adapter_wire_seconds
// since before, a scrape of it, and the seconds it observed.
func wireSince(t *testing.T, m *metrics.Registry, before map[string]float64) (counts, sums map[string]float64) {
	t.Helper()
	after, _ := scrape(t, m)
	was, wasSum := wireSeconds(before)
	counts, sums = wireSeconds(after)
	for st := range counts {
		counts[st] -= was[st]
		sums[st] -= wasSum[st]
	}
	return counts, sums
}

// A compound observes each stage it runs once, its members' runs summed, and
// a chain each stage once per member but wrap and send, which its members
// share, once per record it sends: a compound of a PutFH and an Echo, every
// stage; one whose only member names a code its table lacks, every stage but
// resolve_ids, since that member is answered its error with no call; one
// past its cap, every stage but reply_wait and resolve_ids, since
// decode_args refuses it before either; one the drain refuses whole, the
// stages to reply_wait, the drain judging it once its reply is reserved, and
// map_error, since it gets no reply; one whose only member's Resolve refuses,
// every stage; a chain of two Echo sent as one record; a chain of an Echo and
// a second whose version the router refuses, the status finding no room at
// once, split in two records; a chain whose second header the codec refuses,
// its refusal in a record of its own, which Wrap never sees; and a chain whose
// first member the drain refuses, its interim alone in its record, which
// answers no request and so times no wrap or send, as an interim alone does
// (RFC 20 §10.1).
func TestEachStageOfACompoundOrAChainIsObservedOnce(t *testing.T) {
	_, wire := core.StageNames()
	order := orderedValues(wire)
	each := func(n float64, but map[string]float64) map[string]float64 {
		want := map[string]float64{}
		for _, st := range order {
			want[st] = n
		}
		maps.Copy(want, but)
		return want
	}
	for _, c := range []struct {
		name  string
		serve func(t *testing.T, d adapter.Deps) (s *served, send func())
		want  map[string]float64
	}{
		{"a compound of a PutFH and an Echo", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newServed(t, 1<<20, d)
			return s, func() {
				s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, echo)
				s.results()
			}
		}, each(1, nil)},
		{"a compound whose only member names a code its table lacks", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newServed(t, 1<<20, d)
			return s, func() {
				s.compound(1, testproto.Msg{Op: 99})
				s.results()
			}
		}, each(1, map[string]float64{"resolve_ids": 0})},
		{"a compound past its cap", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newServed(t, 1<<20, d)
			return s, func() {
				s.compound(1, slices.Repeat([]testproto.Msg{echo}, 9)...)
				s.read()
			}
		}, each(1, map[string]float64{"reply_wait": 0, "resolve_ids": 0})},
		{"a compound the drain refuses whole", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newServed(t, 1<<20, d)
			return s, func() {
				s.p.Drain()
				none := echo
				none.Role = core.RoleNone
				s.compound(1, none)
			}
		}, each(1, map[string]float64{"resolve_ids": 0, "encode": 0, "wrap": 0, "send": 0})},
		{"a compound whose only member's Resolve refuses", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newServedOn(t, 1<<20, d, echoMember(func(inv core.Invocation) core.Invocation {
				inv.Resolve = func(*core.Request) error { return vfs.ErrStale }
				return inv
			}))
			return s, func() {
				s.compound(1, echo)
				s.results()
			}
		}, each(1, nil)},
		{"a chain of two Echo in one record", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newChained(t, 1<<20, d)
			return s, func() {
				s.serve(echoIn(1, 7), echoIn(2, 7))
				if rs, err := s.client.Recv(); err != nil || len(rs) != 2 {
					t.Fatalf("the chain is answered %+v, %v: want one record of both", rs, err)
				}
			}
		}, each(2, map[string]float64{"wrap": 1, "send": 1})},
		{"a chain split in two records", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newChained(t, 1<<20, d)
			return s, func() {
				free := fillBut(t, s, true)
				refused := echoIn(2, 7)
				refused.Version = 3
				s.serve(echoIn(1, 7), refused)
				synctest.Wait()
				free()
				if r := s.recvOne(); r.ID != 1 {
					t.Errorf("the first record is %+v, want the first Echo", r)
				}
				if r := s.recvOne(); r.ID != 2 || r.Status != testproto.StatusVersion {
					t.Errorf("the second record is %+v, want the version refused", r)
				}
			}
		}, each(2, map[string]float64{"decode_args": 1, "reply_wait": 1, "resolve_ids": 1})},
		{"a chain whose second header the codec refuses", func(t *testing.T, d adapter.Deps) (*served, func()) {
			codec := &refusesAt{off: -1}
			s := newChained(t, 1<<20, d, func(st *core.Stages) {
				codec.HeaderCodec = st.Header
				st.Header = codec
			})
			return s, func() {
				b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
				codec.off = offs[1]
				s.serveBody(b)
				s.recvOne()
				s.recvOne()
			}
		}, each(1, map[string]float64{"decode_header": 2, "map_error": 2, "encode": 2, "send": 2})},
		{"a chain whose first member the drain refuses", func(t *testing.T, d adapter.Deps) (*served, func()) {
			s := newChained(t, 1<<20, d)
			return s, func() {
				s.p.Drain()
				refused := echoIn(1, 7)
				refused.Role = core.RoleNone
				s.serve(refused, echoIn(2, 7))
				if r := s.recvOne(); !r.Interim {
					t.Errorf("the chain is answered %+v, want the first member's interim alone", r)
				}
			}
		}, each(1, map[string]float64{"encode": 0, "wrap": 0, "send": 0})},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := metrics.New("v", "c")
				s, send := c.serve(t, adapter.Deps{Metrics: m})
				synctest.Wait() // the setup's requests ended
				before, _ := scrape(t, m)
				send()
				if err := s.p.Wait(context.Background()); err != nil {
					t.Fatal(err)
				}
				counts, _ := wireSince(t, m, before)
				for _, st := range order {
					if counts[st] != c.want[st] {
						t.Errorf("%s observed %v times, want %v", st, counts[st], c.want[st])
					}
				}
			})
		})
	}
}

// On a clock that moves 1 ms at each read, a compound of a PutFH and an Echo
// sums each stage's runs, each timed from the end of the step before it, so
// each run reads 1 ms: route 3 ms, the compound's own row checks and each
// member's; resolve_ids, map_error and encode 2 ms, one run per member; and
// every other stage 1 ms (RFC 20 §10.1).
func TestACompoundSumsItsMembersRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, clk := metrics.New("v", "c"), newManualClock()
		clk.step = time.Millisecond
		s := newServed(t, 1<<20, adapter.Deps{Metrics: m})
		synctest.Wait() // the setup's requests ended
		core.SetClock(s.sh, clk)
		before, _ := scrape(t, m)
		s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, echo)
		s.results()
		if err := s.p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, sums := wireSince(t, m, before)
		_, wire := core.StageNames()
		for _, st := range orderedValues(wire) {
			runs := 1
			switch st {
			case "route":
				runs = 3
			case "resolve_ids", "map_error", "encode":
				runs = 2
			}
			if want := (time.Duration(runs) * time.Millisecond).Seconds(); math.Abs(sums[st]-want) > 1e-9 {
				t.Errorf("%s sums %v s, want %v", st, sums[st], want)
			}
		}
	})
}

// The test moves the server's clock while a compound or a chain is held in
// one step, and the time lands under that step's stage alone, every other
// stage observing 0: held in a member's Decode, under decode_args, which the
// compound's walk ends; a member in its call, under none, so neither
// map_error, timed from the call's end, nor wrap, timed from the last
// member's encode for a compound and from its record's start for a chain,
// holds it; in a member's Bound, then refused past the record cap, under
// none, since reply_too_big is no timed stage and map_error is timed from the
// refusal; in a member's Role, then refused by the drain, under map_error,
// the drain judging it once decode_args and reply_wait are observed; 30 s in
// its reply reservation, the reply budget full, under reply_wait, where its
// deadline refuses it; a chain's first or second member in Admit, under
// admit; the record a chain's second member splits off, or the refusal's
// record the next unrelated member sends, in its Wrap, under wrap, the
// member's own stage timed from the record's end; and a chain's first refused
// member in its status wait, under encode, as every status's wait is (RFC 20
// §10.1).
func TestACompoundOrAChainsStageIsTimedFromTheStepBeforeIt(t *testing.T) {
	for _, c := range []struct {
		name  string
		serve func(t *testing.T, d adapter.Deps, open chan struct{}) (s *served, send func() (release func()))
		moved time.Duration // how far the clock moves while it is held
		stage string        // where that time lands; none in the call or before a refusal no stage times
	}{
		{"held in a member's Decode", func(t *testing.T, d adapter.Deps, open chan struct{}) (*served, func() func()) {
			s := newServedOn(t, 1<<20, d, echoMember(func(inv core.Invocation) core.Invocation {
				<-open
				return inv
			}))
			return s, func() func() {
				s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, echo)
				return func() { close(open) }
			}
		}, time.Second, "decode_args"},
		{"a compound's member held in its call", func(t *testing.T, d adapter.Deps, _ chan struct{}) (*served, func() func()) {
			s := newServed(t, 1<<20, d)
			return s, func() func() {
				hold := s.faults.Hold(vfs.OpCreate)
				s.compound(1, testproto.Msg{Op: testproto.OpPutFH, Handle: s.root}, testproto.Msg{Op: testproto.OpCreate, Name: "a"})
				return hold.Release
			}
		}, time.Second, ""},
		{"held in a member's Bound, then refused past the record cap", func(t *testing.T, d adapter.Deps, open chan struct{}) (*served, func() func()) {
			s := newServedOn(t, 1<<20, d, echoMember(func(inv core.Invocation) core.Invocation {
				inv.Bound = func(*core.Request, int) (int, bool) {
					<-open
					return 0, false
				}
				return inv
			}))
			return s, func() func() {
				s.compound(1, echo)
				return func() { close(open) }
			}
		}, time.Second, ""},
		{"held in a member's Role, then refused by the drain", func(t *testing.T, d adapter.Deps, open chan struct{}) (*served, func() func()) {
			s := newServedOn(t, 1<<20, d, echoMember(func(inv core.Invocation) core.Invocation {
				inv.Role = func(*core.Request) core.Role {
					<-open
					return core.RoleNone
				}
				return inv
			}))
			return s, func() func() {
				s.p.Drain()
				s.compound(1, echo)
				return func() { close(open) }
			}
		}, time.Second, "map_error"},
		{"held in its reply reservation past its deadline", func(t *testing.T, d adapter.Deps, _ chan struct{}) (*served, func() func()) {
			// A quarter of 64 KiB clears the compound's charge, so it is not
			// refused past the ceiling; five small classes fill the rest.
			s := newServed(t, 64<<10, d)
			return s, func() func() {
				var free []func()
				for range 5 {
					held, ok := s.reply.TryGet(peer, 0)
					if !ok {
						t.Fatalf("the reply budget holds %d small classes, want five", len(free))
					}
					free = append(free, held.Release)
				}
				s.compound(1, echo)
				return func() {
					for _, f := range free {
						f()
					}
				}
			}
		}, 30 * time.Second, "reply_wait"},
		{"a chain's first member held in Admit", func(t *testing.T, d adapter.Deps, open chan struct{}) (*served, func() func()) {
			s := &served{seam: newSeamWith(t, edited{testproto.New(d), func(st *core.Stages) { st.Flow = heldFlow{st.Flow, open} }}, 1<<20, d)}
			return s, func() func() {
				served := start(func() {
					s.serve(testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 1}, testproto.Msg{Op: testproto.OpEcho, Version: 1, ID: 2})
				})
				return func() {
					close(open)
					<-served.done
				}
			}
		}, time.Second, "admit"},
		{"a chain's second member held in Admit", func(t *testing.T, d adapter.Deps, _ chan struct{}) (*served, func() func()) {
			flow := &gatedFlow{gate: make(chan struct{})}
			s := newChained(t, 1<<20, d, func(st *core.Stages) {
				flow.FlowControl = st.Flow
				st.Flow = flow
			})
			return s, func() func() {
				flow.armed.Store(true)
				s.serve(echoIn(1, 7), echoIn(2, 7))
				return func() { close(flow.gate) }
			}
		}, time.Second, "admit"},
		{"a chain's second member held in its call", func(t *testing.T, d adapter.Deps, _ chan struct{}) (*served, func() func()) {
			s := newChained(t, 1<<20, d)
			return s, func() func() {
				hold := s.faults.Hold(vfs.OpCreate)
				s.serve(
					testproto.Msg{Op: testproto.OpPutFH, Version: 2, ID: 1, Session: 7, Tree: 1, Handle: s.root},
					testproto.Msg{Op: testproto.OpCreate, Version: 2, ID: 2, Session: inherited, Tree: 1, Name: "one", Related: true},
				)
				return hold.Release
			}
		}, time.Second, ""},
		{"the record a chain's second member splits off held in its Wrap", func(t *testing.T, d adapter.Deps, open chan struct{}) (*served, func() func()) {
			w := &heldWrap{open: open}
			s := newChained(t, 1<<20, d, func(st *core.Stages) {
				w.Security = st.Security
				st.Security = w
			})
			return s, func() func() {
				b, offs := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
				b[offs[1]+headerLen+1] ^= 1 // the second member fails Unwrap
				w.armed.Store(true)
				s.serveBody(b)
				return func() { close(open) }
			}
		}, time.Second, "wrap"},
		{"a refusal's record the next unrelated member sends held in its Wrap", func(t *testing.T, d adapter.Deps, open chan struct{}) (*served, func() func()) {
			w := &heldWrap{open: open}
			s := newChained(t, 1<<20, d, func(st *core.Stages) {
				w.Security = st.Security
				st.Security = w
			})
			return s, func() func() {
				b, _ := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
				b[headerLen+1] ^= 1 // the first member fails Unwrap
				w.armed.Store(true)
				s.serveBody(b)
				return func() { close(open) }
			}
		}, time.Second, "wrap"},
		{"a chain's first refused member held in its status wait", func(t *testing.T, d adapter.Deps, _ chan struct{}) (*served, func() func()) {
			s := newChained(t, 1<<20, d)
			return s, func() func() {
				give := s.p.HoldStatusWait()
				b, _ := chainOf(s.f, echoIn(1, 7), echoIn(2, 7))
				b[headerLen+1] ^= 1 // the first member fails Unwrap
				served := start(func() { s.serveBody(b) })
				return func() {
					give()
					<-served.done
				}
			}
		}, time.Second, "encode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				open, clk, m := make(chan struct{}), newManualClock(), metrics.New("v", "c")
				s, send := c.serve(t, adapter.Deps{Metrics: m}, open)
				synctest.Wait() // the setup's requests ended
				core.SetClock(s.sh, clk)
				before, _ := scrape(t, m)
				release := send()
				synctest.Wait()
				clk.Advance(c.moved)
				synctest.Wait()
				release()
				if err := s.p.Wait(context.Background()); err != nil {
					t.Fatal(err)
				}
				counts, sums := wireSince(t, m, before)
				if c.stage != "" && counts[c.stage] == 0 {
					t.Errorf("%s observed nothing, want the %v held there", c.stage, c.moved)
				}
				for st, n := range counts {
					want := 0.0
					if st == c.stage {
						want = c.moved.Seconds()
					}
					if n > 0 && math.Abs(sums[st]-want) > 1e-9 {
						t.Errorf("%s observed %v s, want %v", st, sums[st], want)
					}
				}
			})
		})
	}
}
