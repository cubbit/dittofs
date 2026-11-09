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

type args struct{ n byte }
type reply struct{ n byte }

// row records what each of Bind's arguments was handed.
type row struct {
	decodes  int
	decoded  *args
	version  core.Version
	seen     map[string]*args
	replied  *reply
	encoded  *reply
	handlerE error
}

func (w *row) bind(withHooks bool) func([]byte, core.Version) (core.Invocation, int, error) {
	w.seen = map[string]*args{}
	// dec consumes one byte: the bytes after it are the next member's.
	dec := func(b []byte, v core.Version, a *args) (int, error) {
		w.decodes++
		if len(b) == 0 {
			return 0, errBadArgs
		}
		w.decoded, w.version, a.n = a, v, b[0]
		return 1, nil
	}
	bound := func(a *args) int { w.seen["Bound"] = a; return int(a.n) }
	h := func(_ context.Context, _ *core.Request, a *args) (*reply, error) {
		w.seen["Call"] = a
		w.replied = &reply{n: a.n}
		return w.replied, w.handlerE
	}
	enc := func(r *reply, _ core.Version, _ *resources.Buf) error { w.encoded = r; return nil }
	if !withHooks {
		return core.Bind(dec, bound, nil, nil, nil, h, enc)
	}
	resolve := func(_ *core.Request, a *args) error { w.seen["Resolve"] = a; return nil }
	role := func(_ *core.Request, a *args) core.Role { w.seen["Role"] = a; return core.RoleFlush }
	handle := func(a *args) (vfs.Handle, bool) { w.seen["Handle"] = a; return vfs.Handle{a.n}, true }
	return core.Bind(dec, bound, resolve, role, handle, h, enc)
}

var errBadArgs = errors.New("bad arguments")

// Every closure of the Invocation sees the one request the decoder filled,
// and Encode the reply Call produced: a Bind that decoded twice, or handed a
// closure a copy, would show a second pointer.
func TestBindHandsEveryClosureTheDecodedRequest(t *testing.T) {
	ctx, r := context.Background(), &core.Request{}
	var w row
	inv, n, err := w.bind(true)([]byte{42, 7}, 3)
	if err != nil || n != 1 {
		t.Fatalf("decode: n %d, err %v; want the decoder's 1, nil", n, err)
	}
	if inv.Count != 0 {
		t.Errorf("Count %d on a row that is no compound, want 0", inv.Count)
	}
	if w.version != 3 {
		t.Errorf("dec saw version %d, want 3", w.version)
	}
	if got := inv.Bound(); got != 42 {
		t.Errorf("Bound %d, want 42", got)
	}
	if err := inv.Resolve(r); err != nil {
		t.Errorf("Resolve: %v", err)
	}
	if inv.Role == nil || inv.Role(r) != core.RoleFlush {
		t.Errorf("Role: want the role hook's RoleFlush")
	}
	if inv.Handle == nil {
		t.Fatal("Handle nil though a handle hook was given")
	}
	if h, ok := inv.Handle(); !ok || !bytes.Equal(h, vfs.Handle{42}) {
		t.Errorf("Handle %v, %v; want [42], true", h, ok)
	}
	if err := inv.Call(ctx, r); err != nil {
		t.Errorf("Call: %v", err)
	}
	if err := inv.Encode(nil, 3); err != nil {
		t.Errorf("Encode: %v", err)
	}
	if w.decodes != 1 {
		t.Errorf("decoded %d times, want once", w.decodes)
	}
	for _, name := range []string{"Bound", "Resolve", "Role", "Handle", "Call"} {
		if w.seen[name] != w.decoded {
			t.Errorf("%s saw %p, not the decoded request %p", name, w.seen[name], w.decoded)
		}
	}
	if w.encoded == nil || w.encoded != w.replied {
		t.Errorf("Encode saw %p, not the reply Call produced %p", w.encoded, w.replied)
	}
}

// A parked handler returns its reply value with ErrParked and fills it later,
// so Encode is handed that reply although Call returned an error.
func TestBindKeepsAParkedReply(t *testing.T) {
	w := row{handlerE: core.ErrParked}
	inv, _, err := w.bind(false)([]byte{1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Call(context.Background(), &core.Request{}); !errors.Is(err, core.ErrParked) {
		t.Fatalf("Call: %v, want ErrParked", err)
	}
	_ = inv.Encode(nil, 0)
	if w.encoded == nil || w.encoded != w.replied {
		t.Errorf("Encode saw %p, not the parked reply %p", w.encoded, w.replied)
	}
}

// A nil resolve resolves nothing and succeeds; a nil role and a nil handle
// leave Invocation.Role and Invocation.Handle nil, so Op.Role decides alone
// rather than a defaulted role overriding it while draining.
func TestBindNilHooks(t *testing.T) {
	var w row
	inv, _, err := w.bind(false)([]byte{1}, 0)
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
	var w row
	inv, _, err := w.bind(true)(nil, 0)
	if !errors.Is(err, errBadArgs) {
		t.Fatalf("decode of a bad body: %v, want its decoder's error", err)
	}
	if inv.Call != nil || inv.Resolve != nil || inv.Encode != nil || inv.Bound != nil {
		t.Error("a decode error built an Invocation")
	}
}
