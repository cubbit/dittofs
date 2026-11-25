package adaptertest

import (
	"context"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// table is one row, code 1, whose arguments are nothing, whose reply is
// empty and whose handler is call.
func table(call func(ctx context.Context, r *core.Request) error) *core.OpTable {
	type none struct{}
	op := &core.Op{Code: 1, Name: "row", Decode: core.Bind(
		func([]byte, core.Version, *none) (int, error) { return 0, nil },
		func(*core.Request, *none, int) (int, bool) { return 0, true },
		nil, nil, nil,
		func(ctx context.Context, r *core.Request, _ *none) (*none, error) { return nil, call(ctx, r) },
		func(*none, core.Version, *resources.Buf) error { return nil },
	)}
	return core.NewOpTable(core.NewErrorTable(99, core.ErrorRow{Err: core.ErrDecode, Status: 1}), false, op)
}

// Each call RFC 20 section 4.3 places outside the one-call rule passes in
// the request it names and fails outside it, so an exemption dropped fails a
// passing case, and one widened to its operation alone a failing one. A row
// declared here makes the calls on the in-memory service; the verdict is
// read from the record over the faults.
func TestRunnerExemptsEachCallByItsRequest(t *testing.T) {
	rn := NewRunner(t)
	admin := rn.Mem.Admin()
	made, err := rn.Mem.Create(t.Context(), admin, rn.Mem.Store.Root, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := made.Handle
	revoked := &vfs.StateError{Kind: vfs.StateRevoked}
	held := vfs.StateRef{Type: vfs.StateOpen, Shard: 1, Instance: 2, Token: 7, Seq: 1}
	opened := vfs.OpenRef{Kind: vfs.RefOpened, State: held, Handle: h}
	other := held
	other.Token = 8
	// An SMB client reconnects its opens; the kinds are read by name, as an
	// adapter's test names no store type.
	var proto vfs.Protocol
	var recovery vfs.RecoveryKind
	if err := proto.UnmarshalText([]byte("smb")); err != nil {
		t.Fatal(err)
	}
	if err := recovery.UnmarshalText([]byte("reconnect")); err != nil {
		t.Fatal(err)
	}
	smb := func(id uint64) vfs.Call {
		c := admin
		c.Protocol, c.Session = proto, vfs.SessionRef{ID: id}
		return c
	}
	info := vfs.ClientInfo{Protocol: proto, Name: []byte("pc"), Boot: [8]byte{1}, Recovery: recovery}
	// disconnect connects a client on sessions 1 and 2, then answers a row
	// that disconnects session 1 as its last, at the Gen the first Connect
	// returned, and again at the Gen that Disconnect returned plus skew.
	disconnect := func(t *testing.T, skew uint64) func(context.Context, *core.Request) error {
		id, first, err := rn.Mem.Connect(t.Context(), smb(1), info, &vfstest.Callbacks{})
		if err != nil {
			t.Fatalf("Connect on session 1: %v", err)
		}
		if _, _, err := rn.Mem.Connect(t.Context(), smb(2), info, &vfstest.Callbacks{}); err != nil {
			t.Fatalf("Connect on session 2: %v", err)
		}
		return func(ctx context.Context, _ *core.Request) error {
			gen, err := rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: first})
			if err != nil || gen == 0 {
				t.Fatalf("the first Disconnect: Gen %d, %v; want a Gen", gen, err)
			}
			_, _ = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: gen + skew})
			return nil
		}
	}
	// lockThen locks, then answers what then answers: a waiting lock's
	// handler withdraws its waiter after a refused Park.
	lockThen := func(then func(*core.Request) error) func(context.Context, *core.Request) error {
		return func(ctx context.Context, r *core.Request) error {
			_, _ = rn.Rec.Lock(ctx, admin, vfs.Anonymous(h), vfs.LockReq{Ranges: []vfs.LockRange{{ByteRange: vfs.ByteRange{Length: 1}}}, Wait: vfs.WaitCallback})
			return then(r)
		}
	}
	// watchThen watches, then answers what then answers.
	watchThen := func(then func(*core.Request) error) func(context.Context, *core.Request) error {
		return func(ctx context.Context, r *core.Request) error {
			_, _ = rn.Rec.Watch(ctx, admin, vfs.WatchSpec{Dir: rn.Mem.Store.Root})
			return then(r)
		}
	}
	cancelLock := func(ctx context.Context) {
		_, _ = rn.Rec.CancelLock(ctx, admin, vfs.Anonymous(h), vfs.LockOwner{}, vfs.ByteRange{Length: 1})
	}
	// writeRevoked writes through opened, which a fault answers revoked, then
	// frees free.
	writeRevoked := func(free vfs.StateRef) func(context.Context, *core.Request) error {
		return func(ctx context.Context, _ *core.Request) error {
			rn.Faults.Fail(vfs.OpWrite, revoked)
			_, err := rn.Rec.Write(ctx, admin, opened, 0, []byte("x"), vfs.Unstable)
			_ = rn.Rec.FreeState(ctx, admin, 5, free)
			return err
		}
	}
	for _, c := range []struct {
		name string
		row  func(t *testing.T) func(context.Context, *core.Request) error
		e    Expect
		want string // a substring of the verdict; "": it passes
	}{
		{"a FreeState of the reference its call was answered revoked for", func(*testing.T) func(context.Context, *core.Request) error {
			return writeRevoked(held)
		}, Expect{Call: vfs.OpWrite}, ""},
		{"a FreeState of another reference than the revoked one", func(*testing.T) func(context.Context, *core.Request) error {
			return writeRevoked(other)
		}, Expect{Call: vfs.OpWrite}, "2 mutating calls, Write, FreeState"},
		{"a FreeState with no revoked answer before it", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, err := rn.Rec.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
				_ = rn.Rec.FreeState(ctx, admin, 5, held)
				return err
			}
		}, Expect{Call: vfs.OpWrite}, "2 mutating calls, Write, FreeState"},
		{"a CancelLock after a refused Park", func(*testing.T) func(context.Context, *core.Request) error {
			return lockThen(func(r *core.Request) error {
				_, err := r.Park(func() {})
				cancelLock(t.Context())
				return err
			})
		}, Expect{Call: vfs.OpLock}, ""},
		{"a CancelLock in a request whose Park was not refused", func(*testing.T) func(context.Context, *core.Request) error {
			return lockThen(func(*core.Request) error {
				cancelLock(t.Context())
				return vfs.ErrIO
			})
		}, Expect{Call: vfs.OpLock}, "2 mutating calls, Lock, CancelLock"},
		{"an Unwatch after a refused Park", func(*testing.T) func(context.Context, *core.Request) error {
			return watchThen(func(r *core.Request) error {
				_, err := r.Park(func() {})
				_ = rn.Rec.Unwatch(t.Context(), admin, vfs.WatchID{})
				return err
			})
		}, Expect{Call: vfs.OpWatch}, ""},
		{"an Unwatch in a request whose Park was not refused", func(*testing.T) func(context.Context, *core.Request) error {
			return watchThen(func(*core.Request) error {
				_ = rn.Rec.Unwatch(t.Context(), admin, vfs.WatchID{})
				return nil
			})
		}, Expect{Call: vfs.OpWatch}, "2 mutating calls, Watch, Unwatch"},
		{"a last Disconnect at the Gen the previous one returned", func(t *testing.T) func(context.Context, *core.Request) error {
			return disconnect(t, 0)
		}, Expect{Call: vfs.OpDisconnect}, ""},
		{"a last Disconnect at another Gen", func(t *testing.T) func(context.Context, *core.Request) error {
			return disconnect(t, 1)
		}, Expect{Call: vfs.OpDisconnect}, "2 mutating calls, Disconnect, Disconnect"},
		{"a Connect in the leg that completes a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _, err := rn.Rec.Connect(ctx, smb(3), info, &vfstest.Callbacks{})
				return err
			}
		}, Expect{Completes: true}, ""},
		{"a Connect in another leg of a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _, err := rn.Rec.Connect(ctx, smb(4), info, &vfstest.Callbacks{})
				return err
			}
		}, Expect{SessionSetup: true}, "a Connect call, where the request is due none"},
		{"a SignIn in a leg of a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _ = rn.Rec.SignIn(ctx, vfs.SignInStep{})
				return nil
			}
		}, Expect{SessionSetup: true}, ""},
		{"a SignIn outside a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _ = rn.Rec.SignIn(ctx, vfs.SignInStep{})
				return nil
			}
		}, Expect{}, "a SignIn call, where the request is due none"},
		{"a Find per SID in a request naming SIDs", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				for _, text := range []string{"S-1-5-21-1-2-3-1000", "S-1-5-21-1-2-3-1001"} {
					_, _ = rn.Rec.Find(ctx, admin, vfs.Spelling{Scheme: vfs.SchemeSID, Text: text})
				}
				return nil
			}
		}, Expect{NamesPrincipal: true}, ""},
		{"a Find in a READ", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _, err := rn.Rec.Read(ctx, admin, vfs.Anonymous(h), 0, make([]byte, 1))
				_, _ = rn.Rec.Find(ctx, admin, vfs.Spelling{Scheme: vfs.SchemeSID, Text: "S-1-5-21-1-2-3-1000"})
				return err
			}
		}, Expect{}, "a Find call, where the request is due none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, got := rn.run(t, table(c.row(t)), 1, &core.Request{Call: admin}, nil, c.e)
			switch {
			case c.want == "" && got != nil:
				t.Errorf("verdict %v, want it passed", got)
			case c.want != "" && (got == nil || !strings.Contains(got.Error(), c.want)):
				t.Errorf("verdict %v, want one naming %q", got, c.want)
			}
		})
	}
}

// A request's one call passes where it is the call due, and the rule fails a
// second call, naming both, a missing one, naming it, and one made where
// none or another is due.
func TestRunnerCountsTheOneCall(t *testing.T) {
	rn := NewRunner(t)
	admin := rn.Mem.Admin()
	made, err := rn.Mem.Create(t.Context(), admin, rn.Mem.Store.Root, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := made.Handle
	write := func(ctx context.Context) error {
		_, err := rn.Rec.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
		return err
	}
	for _, c := range []struct {
		name  string
		calls int // writes the row makes
		e     Expect
		want  string
	}{
		{"one call, due", 1, Expect{Call: vfs.OpWrite}, ""},
		{"two calls", 2, Expect{Call: vfs.OpWrite}, "2 mutating calls, Write, Write"},
		{"no call, where one is due", 0, Expect{Call: vfs.OpWrite}, "no Write call, where the request is due one"},
		{"a call, where none is due", 1, Expect{}, "a Write call, where the request is due none"},
		{"a call, where another is due", 1, Expect{Call: vfs.OpSetAttr}, "no SetAttr call"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tab := table(func(ctx context.Context, _ *core.Request) error {
				for range c.calls {
					if err := write(ctx); err != nil {
						return err
					}
				}
				return nil
			})
			_, got := rn.run(t, tab, 1, &core.Request{Call: admin}, nil, c.e)
			switch {
			case c.want == "" && got != nil:
				t.Errorf("verdict %v, want it passed", got)
			case c.want != "" && (got == nil || !strings.Contains(got.Error(), c.want)):
				t.Errorf("verdict %v, want one naming %q", got, c.want)
			}
		})
	}
}
