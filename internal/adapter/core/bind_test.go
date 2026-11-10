package core_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// args is a decoded request: n is its one wire byte, v the version it was
// decoded at.
type args struct {
	n byte
	v core.Version
}

// reply carries what the handler saw of its request, and val, the count of
// calls that produced it.
type reply struct{ n, v, val byte }

// bind builds a row whose encoder writes four bytes: the request byte the
// handler saw, the version it was decoded at, the reply's call count, and
// the version Encode was given. The resolve hook sets the request byte to 99,
// so a closure handed a copy, or a second decode, leaves 99 out of the reply.
func bind(withHooks bool, herr error) func([]byte, core.Version) (core.Invocation, int, error) {
	// dec consumes one byte: the bytes after it are the next member's.
	dec := func(b []byte, v core.Version, a *args) (int, error) {
		if len(b) == 0 {
			return 0, errBadArgs
		}
		a.n, a.v = b[0], v
		return 1, nil
	}
	bound := func(a *args) int { return int(a.n) }
	var calls byte
	h := func(_ context.Context, _ *core.Request, a *args) (*reply, error) {
		calls++
		return &reply{n: a.n, v: byte(a.v), val: calls}, herr
	}
	enc := func(r *reply, v core.Version, out *resources.Buf) error {
		out.B = append(out.B, r.n, r.v, r.val, byte(v))
		return nil
	}
	if !withHooks {
		return core.Bind(dec, bound, nil, nil, nil, h, enc)
	}
	resolve := func(_ *core.Request, a *args) error { a.n = 99; return nil }
	role := func(_ *core.Request, a *args) core.Role {
		if a.n == 99 {
			return core.RoleFlush
		}
		return core.RoleNone
	}
	handle := func(a *args) (vfs.Handle, bool) { return vfs.Handle{a.n}, true }
	return core.Bind(dec, bound, resolve, role, handle, h, enc)
}

var errBadArgs = errors.New("bad arguments")

// Every closure of the Invocation works on the one request the decoder
// filled, and Encode on the reply Call produced: what Resolve writes into the
// request reaches Bound, Role, Handle and the encoded reply.
func TestBindHandsEveryClosureTheDecodedRequest(t *testing.T) {
	ctx, r := context.Background(), &core.Request{}
	inv, n, err := bind(true, nil)([]byte{42, 7}, 3)
	if err != nil || n != 1 {
		t.Fatalf("decode: n %d, err %v; want the decoder's 1, nil", n, err)
	}
	if inv.Count != 0 {
		t.Errorf("Count %d on a row that is no compound, want 0", inv.Count)
	}
	if got := inv.Bound(); got != 42 {
		t.Errorf("Bound before Resolve %d, want the decoded 42", got)
	}
	if err := inv.Resolve(r); err != nil {
		t.Errorf("Resolve: %v", err)
	}
	if got := inv.Bound(); got != 99 {
		t.Errorf("Bound after Resolve %d, want Resolve's 99", got)
	}
	if inv.Role == nil || inv.Role(r) != core.RoleFlush {
		t.Errorf("Role: want RoleFlush, which the role hook answers only for Resolve's 99")
	}
	if inv.Handle == nil {
		t.Fatal("Handle nil though a handle hook was given")
	}
	if h, ok := inv.Handle(); !ok || !bytes.Equal(h, vfs.Handle{99}) {
		t.Errorf("Handle %v, %v; want [99], true", h, ok)
	}
	if err := inv.Call(ctx, r); err != nil {
		t.Errorf("Call: %v", err)
	}
	out := &resources.Buf{}
	if err := inv.Encode(out, 4); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// request byte 99, decode version 3, the first call, Encode's version 4
	if want := []byte{99, 3, 1, 4}; !bytes.Equal(out.B, want) {
		t.Errorf("encoded reply %v, want %v", out.B, want)
	}
}

// A handler may return a reply with its error (a parked reply, an NFSv3
// failure's WCC data), and Encode is handed that reply; a re-driven Call
// replaces it.
func TestBindKeepsTheLastReplyWhateverTheError(t *testing.T) {
	for _, herr := range []error{core.ErrParked, errBadArgs} {
		inv, _, err := bind(false, herr)([]byte{1}, 0)
		if err != nil {
			t.Fatal(err)
		}
		r := &core.Request{}
		for i := range 2 {
			if err := inv.Call(context.Background(), r); !errors.Is(err, herr) {
				t.Fatalf("Call %d: %v, want %v", i+1, err, herr)
			}
		}
		out := &resources.Buf{}
		if err := inv.Encode(out, 0); err != nil {
			t.Fatalf("with %v: Encode: %v", herr, err)
		}
		// request byte 1, decode version 0, the second call, Encode's version 0
		if want := []byte{1, 0, 2, 0}; !bytes.Equal(out.B, want) {
			t.Errorf("with %v: encoded reply %v, want the second call's %v", herr, out.B, want)
		}
	}
}

// A nil resolve resolves nothing and succeeds; a nil role and a nil handle
// leave Invocation.Role and Invocation.Handle nil, so Op.Role decides alone
// rather than a defaulted role overriding it while draining.
func TestBindNilHooks(t *testing.T) {
	inv, _, err := bind(false, nil)([]byte{1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Resolve == nil {
		t.Fatal("Resolve nil; want one that succeeds")
	}
	if err := inv.Resolve(&core.Request{}); err != nil {
		t.Errorf("Resolve with no resolve hook: %v, want nil", err)
	}
	if inv.Role != nil {
		t.Error("Role set with no role hook; want nil, so Op.Role decides alone")
	}
	if inv.Handle != nil {
		t.Error("Handle set with no handle hook; want nil")
	}
}

// A decode error is returned and builds no Invocation, so a garbage request
// never reaches its handler.
func TestBindDecodeError(t *testing.T) {
	inv, _, err := bind(true, nil)(nil, 0)
	if !errors.Is(err, errBadArgs) {
		t.Fatalf("decode of a bad body: %v, want its decoder's error", err)
	}
	if inv.Call != nil || inv.Resolve != nil || inv.Encode != nil || inv.Bound != nil {
		t.Error("a decode error built an Invocation")
	}
}
