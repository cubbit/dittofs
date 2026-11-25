package adaptertest

import (
	"context"
	"errors"
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
	// connect connects a client on sessions 1 and 2 and returns it and the
	// Gen the first Connect returned, which the second has made stale.
	connect := func(t *testing.T) (vfs.ClientID, uint64) {
		id, first, err := rn.Mem.Connect(t.Context(), smb(1), info, &vfstest.Callbacks{})
		if err != nil {
			t.Fatalf("Connect on session 1: %v", err)
		}
		if _, _, err := rn.Mem.Connect(t.Context(), smb(2), info, &vfstest.Callbacks{}); err != nil {
			t.Fatalf("Connect on session 2: %v", err)
		}
		return id, first
	}
	// disconnect connects, then answers a row that disconnects session 1, as
	// its last where last is set, at the Gen the first Connect returned, and
	// then once more as its last for each skew, at the Gen the Disconnect
	// before it returned plus the skew.
	disconnect := func(t *testing.T, last bool, skews ...uint64) func(context.Context, *core.Request) error {
		id, first := connect(t)
		return func(ctx context.Context, _ *core.Request) error {
			gen, err := rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: last, Gen: first})
			if err != nil || last && gen == 0 {
				t.Fatalf("the first Disconnect: Gen %d, %v; want a Gen", gen, err)
			}
			for _, skew := range skews {
				gen, _ = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: gen + skew})
			}
			return nil
		}
	}
	// repeat connects, then answers a row that disconnects session 1 as its
	// last at the stale Gen and repeats it twice, each at the Gen the one
	// before returned. Where relist is set session 2 is connected again
	// before the first repeat, so that repeat is answered a newer Gen, not
	// zero; the service's own Connect is not recorded.
	repeat := func(t *testing.T, relist bool) func(context.Context, *core.Request) error {
		id, gen := connect(t)
		return func(ctx context.Context, _ *core.Request) error {
			for i := range 3 {
				if i == 1 && relist {
					if _, _, err := rn.Mem.Connect(ctx, smb(2), info, &vfstest.Callbacks{}); err != nil {
						t.Fatalf("Connect on session 2 again: %v", err)
					}
				}
				// The last may find the record released, StaleClient.
				var err error
				if gen, err = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: gen}); err != nil && i < 2 {
					t.Fatalf("Disconnect %d: %v", i, err)
				}
				if want := i == 0 || i == 1 && relist; (gen != 0) != want {
					t.Fatalf("Disconnect %d answered Gen %d; want a non-zero one: %v", i, gen, want)
				}
			}
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
	// cancelLock withdraws the waiter of owner on h's anonymous open; the
	// Lock lockThen makes is the zero owner's.
	cancelLock := func(ctx context.Context, owner vfs.LockOwner) {
		_, _ = rn.Rec.CancelLock(ctx, admin, vfs.Anonymous(h), owner, vfs.ByteRange{Length: 1})
	}
	stranger := vfs.LockOwner{Client: 9, Name: []byte("stranger")}
	// writeRevoked writes through opened, which a fault answers revoked, then
	// frees each of free.
	writeRevoked := func(free ...vfs.StateRef) func(context.Context, *core.Request) error {
		return func(ctx context.Context, _ *core.Request) error {
			rn.Faults.Fail(vfs.OpWrite, revoked)
			_, err := rn.Rec.Write(ctx, admin, opened, 0, []byte("x"), vfs.Unstable)
			for _, ref := range free {
				_ = rn.Rec.FreeState(ctx, admin, 5, ref)
			}
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
		}, Expect{Call: vfs.OpWrite, FreesRevoked: true}, ""},
		{"a FreeState of another reference than the revoked one", func(*testing.T) func(context.Context, *core.Request) error {
			return writeRevoked(other)
		}, Expect{Call: vfs.OpWrite, FreesRevoked: true}, "2 mutating calls, Write, FreeState"},
		{"a FreeState of the revoked reference in a protocol whose own operation frees it", func(*testing.T) func(context.Context, *core.Request) error {
			return writeRevoked(held)
		}, Expect{Call: vfs.OpWrite}, "2 mutating calls, Write, FreeState"},
		{"a FreeState with no revoked answer before it", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, err := rn.Rec.Write(ctx, admin, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
				_ = rn.Rec.FreeState(ctx, admin, 5, held)
				return err
			}
		}, Expect{Call: vfs.OpWrite, FreesRevoked: true}, "2 mutating calls, Write, FreeState"},
		{"a CancelLock after a refused Park", func(*testing.T) func(context.Context, *core.Request) error {
			return lockThen(func(r *core.Request) error {
				_, err := r.Park(func() {})
				cancelLock(t.Context(), vfs.LockOwner{})
				return err
			})
		}, Expect{Call: vfs.OpLock}, ""},
		{"a CancelLock in a request whose Park was not refused", func(*testing.T) func(context.Context, *core.Request) error {
			return lockThen(func(*core.Request) error {
				cancelLock(t.Context(), vfs.LockOwner{})
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
			return disconnect(t, true, 0)
		}, Expect{Call: vfs.OpDisconnect}, ""},
		{"a last Disconnect at another Gen", func(t *testing.T) func(context.Context, *core.Request) error {
			return disconnect(t, true, 1)
		}, Expect{Call: vfs.OpDisconnect}, "2 mutating calls, Disconnect, Disconnect"},
		{"a FreeState of the revoked reference, twice", func(*testing.T) func(context.Context, *core.Request) error {
			return writeRevoked(held, held)
		}, Expect{Call: vfs.OpWrite, FreesRevoked: true}, "2 mutating calls, Write, FreeState"},
		{"a second CancelLock after a refused Park", func(*testing.T) func(context.Context, *core.Request) error {
			return lockThen(func(r *core.Request) error {
				_, err := r.Park(func() {})
				cancelLock(t.Context(), vfs.LockOwner{})
				cancelLock(t.Context(), vfs.LockOwner{})
				return err
			})
		}, Expect{Call: vfs.OpLock}, "2 mutating calls, Lock, CancelLock"},
		{"a CancelLock of another owner's waiter after a refused Park", func(*testing.T) func(context.Context, *core.Request) error {
			return lockThen(func(r *core.Request) error {
				_, err := r.Park(func() {})
				cancelLock(t.Context(), stranger)
				return err
			})
		}, Expect{Call: vfs.OpLock}, "2 mutating calls, Lock, CancelLock"},
		{"a CancelLock before its Lock, in a request whose Park was refused", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, r *core.Request) error {
				cancelLock(ctx, vfs.LockOwner{})
				return lockThen(func(r *core.Request) error {
					_, err := r.Park(func() {})
					return err
				})(ctx, r)
			}
		}, Expect{Call: vfs.OpLock}, "2 mutating calls, CancelLock, Lock"},
		{"a second Unwatch after a refused Park", func(*testing.T) func(context.Context, *core.Request) error {
			return watchThen(func(r *core.Request) error {
				_, err := r.Park(func() {})
				_ = rn.Rec.Unwatch(t.Context(), admin, vfs.WatchID{})
				_ = rn.Rec.Unwatch(t.Context(), admin, vfs.WatchID{})
				return err
			})
		}, Expect{Call: vfs.OpWatch}, "2 mutating calls, Watch, Unwatch"},
		{"a last Disconnect repeated at each newer Gen the one before returned", func(t *testing.T) func(context.Context, *core.Request) error {
			return repeat(t, true)
		}, Expect{Call: vfs.OpDisconnect}, ""},
		{"a last Disconnect repeated at the zero Gen the one before returned", func(t *testing.T) func(context.Context, *core.Request) error {
			return repeat(t, false)
		}, Expect{Call: vfs.OpDisconnect}, "2 mutating calls, Disconnect, Disconnect"},
		{"a last Disconnect at the Gen a Disconnect that was not the last returned", func(t *testing.T) func(context.Context, *core.Request) error {
			return disconnect(t, false, 0)
		}, Expect{Call: vfs.OpDisconnect}, "2 mutating calls, Disconnect, Disconnect"},
		{"a Disconnect in the leg that completes a session setup", func(t *testing.T) func(context.Context, *core.Request) error {
			return disconnect(t, true)
		}, Expect{Completes: true}, ""},
		{"a Disconnect that is not the last and a last one in the leg that completes a session setup", func(t *testing.T) func(context.Context, *core.Request) error {
			id, first := connect(t)
			return func(ctx context.Context, _ *core.Request) error {
				_, _ = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{})
				_, _ = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true, Gen: first})
				return nil
			}
		}, Expect{Completes: true}, ""},
		{"two Disconnects that are not the last in the leg that completes a session setup", func(t *testing.T) func(context.Context, *core.Request) error {
			id, _ := connect(t)
			return func(ctx context.Context, _ *core.Request) error {
				for range 2 {
					_, _ = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{})
				}
				return nil
			}
		}, Expect{Completes: true}, "a Disconnect call, where the request is due none"},
		{"three last Disconnects, none a repeat, in the leg that completes a session setup", func(t *testing.T) func(context.Context, *core.Request) error {
			id, _ := connect(t)
			return func(ctx context.Context, _ *core.Request) error {
				for range 3 {
					_, _ = rn.Rec.Disconnect(ctx, smb(1), id, vfs.SessionRef{ID: 1}, vfs.SessionEnd{Last: true})
				}
				return nil
			}
		}, Expect{Completes: true}, "a Disconnect call, where the request is due none"},
		{"a Disconnect in another leg of a session setup", func(t *testing.T) func(context.Context, *core.Request) error {
			return disconnect(t, true)
		}, Expect{SessionSetup: true}, "a Disconnect call, where the request is due none"},
		{"an AddSession in the leg that completes a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _ = rn.Rec.AddSession(ctx, smb(5), 5, nil, []byte("session"))
				return nil
			}
		}, Expect{Completes: true}, ""},
		{"two AddSessions in the leg that completes a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				for range 2 {
					_, _ = rn.Rec.AddSession(ctx, smb(5), 5, nil, []byte("session"))
				}
				return nil
			}
		}, Expect{Completes: true}, "AddSession call, where the request is due none"},
		{"an AddSession in another leg of a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _ = rn.Rec.AddSession(ctx, smb(5), 5, nil, []byte("session"))
				return nil
			}
		}, Expect{SessionSetup: true}, "AddSession call, where the request is due none"},
		{"a Connect in the leg that completes a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				_, _, err := rn.Rec.Connect(ctx, smb(3), info, &vfstest.Callbacks{})
				return err
			}
		}, Expect{Completes: true}, ""},
		{"two Connects in the leg that completes a session setup", func(*testing.T) func(context.Context, *core.Request) error {
			return func(ctx context.Context, _ *core.Request) error {
				for _, s := range []uint64{6, 7} {
					if _, _, err := rn.Rec.Connect(ctx, smb(s), info, &vfstest.Callbacks{}); err != nil {
						return err
					}
				}
				return nil
			}
		}, Expect{Completes: true}, "a Connect call, where the request is due none"},
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

// A request whose arguments do not decode makes no call, so it fails where a
// call is due and passes where none is.
func TestRunnerJudgesARequestThatDoesNotDecode(t *testing.T) {
	rn := NewRunner(t)
	type none struct{}
	tab := core.NewOpTable(core.NewErrorTable(99, core.ErrorRow{Err: core.ErrDecode, Status: 1}), false, &core.Op{Code: 1, Name: "row", Decode: core.Bind(
		func([]byte, core.Version, *none) (int, error) { return 0, errors.New("garbage") },
		func(*core.Request, *none, int) (int, bool) { return 0, true },
		nil, nil, nil,
		func(context.Context, *core.Request, *none) (*none, error) { return nil, nil },
		func(*none, core.Version, *resources.Buf) error { return nil },
	)})
	rep, got := rn.run(t, tab, 1, &core.Request{Call: rn.Mem.Admin()}, nil, Expect{Call: vfs.OpWrite})
	if rep.Status != 1 || rep.Body != nil {
		t.Errorf("reply %+v, want the decode status 1 alone", rep)
	}
	if want := "no Write call, where the request is due one"; got == nil || !strings.Contains(got.Error(), want) {
		t.Errorf("verdict %v, want one naming %q", got, want)
	}
	if _, got := rn.run(t, tab, 1, &core.Request{Call: rn.Mem.Admin()}, nil, Expect{}); got != nil {
		t.Errorf("verdict %v where no call is due, want it passed", got)
	}
}

// A fault a request never reached fails it, naming the method, and is
// disarmed, so the next request's call of that method reaches the service.
func TestRunnerFailsAndDisarmsAFaultTheRequestNeverReached(t *testing.T) {
	rn := NewRunner(t)
	admin := rn.Mem.Admin()
	made, err := rn.Mem.Create(t.Context(), admin, rn.Mem.Store.Root, []byte("f"), vfs.CreateArgs{Kind: vfs.CreateRegular})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var answered error
	write := table(func(ctx context.Context, _ *core.Request) error {
		_, answered = rn.Rec.Write(ctx, admin, vfs.Anonymous(made.Handle), 0, []byte("x"), vfs.Unstable)
		return answered
	})
	rn.Faults.Fail(vfs.OpSetAttr, vfs.ErrIO)
	_, got := rn.run(t, write, 1, &core.Request{Call: admin}, nil, Expect{Call: vfs.OpWrite})
	if want := "faults armed on [SetAttr] that the request never reached"; got == nil || !strings.Contains(got.Error(), want) {
		t.Errorf("verdict %v, want one naming %q", got, want)
	}
	setAttr := table(func(ctx context.Context, _ *core.Request) error {
		_, answered = rn.Rec.SetAttr(ctx, admin, vfs.Target{H: made.Handle}, vfs.AttrChange{})
		return answered
	})
	if _, got := rn.run(t, setAttr, 1, &core.Request{Call: admin}, nil, Expect{Call: vfs.OpSetAttr}); got != nil || answered != nil {
		t.Errorf("the next request: verdict %v, SetAttr answered %v; want both nil, the fault gone", got, answered)
	}
}
