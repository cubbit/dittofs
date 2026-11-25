package adaptertest

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/memory"
)

// Runner runs one operation table's rows against the in-memory service, the
// way core runs them but with no socket and no pipeline: decode, bound,
// resolve, call, map the error through the table and encode. It names no
// protocol: a protocol's harness builds the *core.Request, State included,
// and lays the reply out as its wire does.
//
// Its service is three layers in one order: the filesystem over memory, the
// fault wrapper over it, and the record of mutating calls over both. A handler
// calls Rec, so a call a fault answers is recorded with the fault as its
// answer, and the one-call rule reads Rec, never Mem's own record, which sits
// under the faults and never sees a call they answer.
type Runner struct {
	Mem    *memory.Service       // the filesystem, its store and its content, for setup and for the state a test asserts
	Faults *vfstest.FaultService // arms the error or hold the next call of a method meets
	Rec    *vfstest.Mutations    // the service the handlers under test call
}

// NewRunner checks t for leaks first, as CheckLeaks asks, then builds the
// service's three layers. t is a top-level test, and no test runs beside it:
// the leak check is process-wide. Its subtests share the runner.
func NewRunner(t *testing.T) *Runner {
	t.Helper()
	CheckLeaks(t, nil)
	mem := memory.New(t)
	faults := vfstest.Faults(mem)
	return &Runner{Mem: mem, Faults: faults, Rec: vfstest.RecordMutations(faults)}
}

// Expect is what the caller says the request is, which the record alone
// cannot tell: the one mutating call it is due to make, and whether it is a
// request RFC 20 section 4.3 lets make further calls of the client record or
// the principals.
type Expect struct {
	// Call is the mutating call the request is due to make; zero: none.
	Call vfs.Op
	// SessionSetup is a leg of a request that sets up a session: its SignIn
	// calls are outside the rule.
	SessionSetup bool
	// Completes is the leg that completes a session setup with a verified
	// identity: its client-record calls are outside the rule, one Connect,
	// one AddSession and at most two Disconnects, one of them the last, and
	// so is SignIn.
	Completes bool
	// FreesRevoked is a request in a protocol with no operation that frees a
	// reference (NFSv4.0, SMB): its FreeState of a reference its call was
	// answered revoked for is outside the rule.
	FreesRevoked bool
	// NamesPrincipal is a request that carries a SID, a principal's name, or a
	// security descriptor or ACL naming one: its Find calls are outside the
	// rule.
	NamesPrincipal bool
}

// Reply is what a row answered: its status, mapped through the table, and
// what its Encode wrote after the status, which the protocol's header writes.
type Reply struct {
	Status core.Status
	Body   []byte // nil where the arguments did not decode: a status alone
}

// room is the reply room a row's Bound is handed: wide enough that no READ
// count is clamped, so a handler test sees the count its request asked for.
const room = math.MaxInt32

// Run runs the row of tab whose code is code over args, with r, which the
// caller builds as its protocol's router would, and fails t where the
// request broke the one-call rule as e reads it, encoded past its Bound, or
// left a fault armed unfired, which it disarms. The record is reset first,
// so it holds this request's calls alone, a request whose arguments did not
// decode included: it made no call, so a call it was due fails it.
func (rn *Runner) Run(t testing.TB, tab *core.OpTable, code uint32, r *core.Request, args []byte, e Expect) Reply {
	t.Helper()
	rep, err := rn.run(t, tab, code, r, args, e)
	if err != nil {
		t.Errorf("adaptertest: row %d: %v", code, err)
	}
	return rep
}

// run is Run, the verdict returned rather than reported.
func (rn *Runner) run(t testing.TB, tab *core.OpTable, code uint32, r *core.Request, args []byte, e Expect) (Reply, error) {
	t.Helper()
	op := tab.Ops[code]
	if op == nil {
		t.Fatalf("adaptertest: the table has no row %d", code)
	}
	rn.Rec.Reset()
	inv, _, err := op.Decode(args, r.Version)
	if err != nil {
		s, _, _ := tab.Errors.Map(fmt.Errorf("%w: %v", core.ErrDecode, err), op, r.Version)
		return Reply{Status: s}, rn.judge(false, e)
	}
	bound, ok := inv.Bound(r, room)
	if !ok {
		t.Fatalf("adaptertest: row %s has no byte of room in %d", op.Name, room)
	}
	err = inv.Resolve(r)
	if err == nil {
		err = inv.Call(t.Context(), r)
	}
	if errors.Is(err, core.ErrParked) {
		t.Fatalf("adaptertest: row %s parked, which a request with no pipeline cannot: its Park is always refused", op.Name)
	}
	var s core.Status
	if err != nil {
		s, _, _ = tab.Errors.Map(err, op, r.Version)
	}
	var out resources.Buf
	if err := inv.Encode(&out, r.Version); err != nil {
		t.Fatalf("adaptertest: row %s: encode: %v", op.Name, err)
	}
	if len(out.B) > bound {
		t.Errorf("adaptertest: row %s encoded %d bytes past the %d its Bound reserves", op.Name, len(out.B), bound)
	}
	body := out.B
	if body == nil {
		body = []byte{}
	}
	return Reply{Status: s, Body: body}, rn.judge(errors.Is(err, parkRefused), e)
}

// judge is the request's verdict: the one-call rule over the record, refused
// telling whether its Park was refused, and each fault armed that the request
// never reached, which is disarmed so the next request meets none of it.
func (rn *Runner) judge(refused bool, e Expect) error {
	var unused error
	if ops := rn.Faults.Disarm(); len(ops) > 0 {
		unused = fmt.Errorf("faults armed on %v that the request never reached", ops)
	}
	return errors.Join(verdict(rn.Rec.Calls(), refused, e), unused)
}

// parkRefused is the error Park answers on a request no pipeline runs, whose
// park it always refuses: every request the runner runs is one.
//
// decision: a refused Park is read from the handler's answer, which a handler
// returns as Park gave it (RFC 20 section 4.7), matched against this one
// value, so a CancelLock or an Unwatch is exempted only in a request whose
// own Park was refused, never after another error. A granted Park cannot
// happen here: a parked request is tested over a loopback server. Withdraw it
// if a handler is ever allowed to answer a refused Park with another error.
var parkRefused = func() error {
	_, err := (&core.Request{}).Park(func() {})
	return err
}()

// verdict judges a request's mutating calls, in the order they started,
// against the one-call rule: the calls left once each exemption e and the
// record allow is taken out are at most one, and that one is e.Call, which
// some call of the request made.
func verdict(calls []vfstest.Mutation, refused bool, e Expect) error {
	var counted []string
	made := false
	ex := exemptions{refused: refused, e: e, used: make([]bool, len(calls))}
	for i, m := range calls {
		made = made || m.Op == e.Call
		if !ex.exempt(calls[:i], m) {
			counted = append(counted, m.Op.String())
		}
	}
	switch {
	case len(counted) > 1:
		return fmt.Errorf("%d mutating calls, %s, where one is allowed", len(counted), strings.Join(counted, ", "))
	case e.Call != 0 && !made:
		return fmt.Errorf("no %s call, where the request is due one", e.Call)
	case e.Call == 0 && len(counted) == 1:
		return fmt.Errorf("a %s call, where the request is due none", counted[0])
	case len(counted) == 1 && counted[0] != e.Call.String():
		return fmt.Errorf("a %s call, where the request is due a %s", counted[0], e.Call)
	}
	return nil
}

// exemptions is one request's exemptions as its calls are judged in order:
// what the request is, and which earlier calls have already exempted a later
// one, each of which exempts one call at most.
type exemptions struct {
	refused bool   // the request's Park was refused
	e       Expect // what the caller says the request is
	used    []bool // by index into the calls, an earlier call that has exempted one

	// A completing leg's client-record calls exempted so far.
	connected, added bool
	disconnects      int  // Disconnects exempted by the leg, not as a repeat
	plain            bool // one of them was not the last
}

// exempt reports whether m, made after before in one request, is a call RFC 20
// section 4.3 places outside the one-call rule in that request: each by what
// the request is or by the call before it that it answers, never by its
// operation alone, and the earlier call it answers, once.
//
// decision: a FreeState is exempted by a recorded call's revoked answer, and
// read-only calls are not recorded, so one after a READ's revoked Read counts
// as the request's one call: a test of it names FreeState as the call due.
// Withdraw it if the record comes to keep a read-only call's answer.
//
// decision: a completing leg's Disconnects are bounded by count, not by
// origin: one may be other than the last, SMB's PreviousSessionId cleanup,
// and one the last, the Connect decision's, but the record keeps no Gen
// Connect or AddSession answered, so the last one is not matched to it, and
// an NFSv4.1 CREATE_SESSION, which has no cleanup, may still send one that is
// not the last. Match them if the record comes to keep those Gens and Expect
// to name the protocol.
//
// decision: a CancelLock withdraws the waiter of an earlier Lock of the
// request on the same open and owner, its range unmatched since the record
// keeps a Lock's owner and open but not its ranges; an Unwatch withdraws an
// earlier Watch of the request, unmatched by ID since the record keeps no
// Watch's answer. Each Lock or Watch exempts one withdrawal. Match the range
// and the watch's ID if the record comes to keep them.
func (x *exemptions) exempt(before []vfstest.Mutation, m vfstest.Mutation) bool {
	switch m.Op {
	case vfs.OpCancelLock:
		return x.refused && x.take(before, func(p vfstest.Mutation) bool {
			return p.Op == vfs.OpLock && reflect.DeepEqual(p.Open, m.Open) && reflect.DeepEqual(p.Owner, m.Owner)
		})
	case vfs.OpUnwatch:
		return x.refused && x.take(before, func(p vfstest.Mutation) bool { return p.Op == vfs.OpWatch })
	case vfs.OpFreeState:
		return x.e.FreesRevoked && x.take(before, func(p vfstest.Mutation) bool {
			var se *vfs.StateError
			return errors.As(p.Err, &se) && se.Kind == vfs.StateRevoked && p.Open != nil && sameState(p.Open.State, m.State)
		})
	case vfs.OpDisconnect:
		// A repeat at the non-zero Gen the previous one returned, as often as
		// each answers one: zero says the entry is gone.
		if m.End.Last && m.End.Gen != 0 && x.take(before, func(p vfstest.Mutation) bool {
			return p.Op == vfs.OpDisconnect && p.Err == nil && p.End.Last && p.Gen == m.End.Gen
		}) {
			return true
		}
		if !x.e.Completes || x.disconnects == 2 || !m.End.Last && x.plain {
			return false
		}
		x.disconnects++
		x.plain = x.plain || !m.End.Last
		return true
	case vfs.OpConnect:
		return x.e.Completes && once(&x.connected)
	case vfs.OpAddSession:
		return x.e.Completes && once(&x.added)
	case vfs.OpSignIn:
		return x.e.SessionSetup || x.e.Completes
	case vfs.OpFind:
		return x.e.NamesPrincipal
	}
	return false
}

// once reports whether done was unset, and sets it.
func once(done *bool) bool {
	was := *done
	*done = true
	return !was
}

// take marks the first earlier call that answers holds for and that has
// exempted none yet, and reports whether there was one.
func (x *exemptions) take(before []vfstest.Mutation, answers func(vfstest.Mutation) bool) bool {
	for i, p := range before {
		if !x.used[i] && answers(p) {
			x.used[i] = true
			return true
		}
	}
	return false
}

// sameState reports whether a and b name one piece of state: its type, shard,
// instance and token, whatever sequence each presented.
func sameState(a, b vfs.StateRef) bool {
	a.Seq, b.Seq = 0, 0
	return a == b
}
