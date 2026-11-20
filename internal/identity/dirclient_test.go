package identity_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/secrets"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// dirState is a default State naming an in-memory directory's record.
func dirState(d *identitytest.Directory) identity.State {
	st := state(1)
	name, _ := d.Account()
	st.Directory = &directory.Record{Address: "ldaps://dc.corp.example", Base: "DC=corp,DC=example", BindName: name,
		Realm: "CORP.EXAMPLE", NetBIOSDomain: "CORP"}
	return st
}

// passThrough wraps d's dials in a fault wrapper that injects nothing, so a
// test can read the dials it was sent.
func passThrough(t *testing.T, d *identitytest.Directory) *identitytest.FaultyDirectory {
	t.Helper()
	sched, err := sim.NewSchedule(1, sim.NewTestClock(time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return identitytest.NewFaultyDirectory(d.Dial, sched)
}

// With no Deps.Directory, or no record, New and Start succeed and every
// directory question answers ErrUnavailable.
func TestNoDirectoryAnswersUnavailable(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	d.AddUser(t, "alice", nil)
	f := passThrough(t, d)
	for name, tc := range map[string]struct {
		dial directory.Dialer
		st   identity.State
	}{
		"no dialer": {nil, dirState(d)},
		"no record": {f.Dial, state(1)},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := identity.New(identity.Deps{Store: newStore(t), Clock: sim.NewTestClock(time.Unix(0, 0)), Directory: tc.dial})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.Apply(tc.st); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, _, err := s.DirectoryLookup(context.Background(), directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("lookup = %v, want ErrUnavailable", err)
			}
		})
	}
	if got := f.Dials(); len(got) != 0 {
		t.Errorf("dials %v, want none", got)
	}
}

// The bind credential is resolved through Deps.Secrets before any dial: with
// no provider, or one that cannot resolve it, a question answers
// ErrUnavailable and nothing is dialled.
func TestDirectoryCredentialUnresolvedDialsNothing(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	d.AddUser(t, "alice", nil)
	for name, provider := range map[string]identity.SecretProvider{
		"no provider":      nil,
		"provider unbuilt": &secrets.Provider{},
	} {
		t.Run(name, func(t *testing.T) {
			f := passThrough(t, d)
			s, err := identity.New(identity.Deps{Store: newStore(t), Clock: sim.NewTestClock(time.Unix(0, 0)),
				Secrets: provider, Directory: f.Dial})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.Apply(dirState(d)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.DirectoryLookup(context.Background(), directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("lookup = %v, want ErrUnavailable", err)
			}
			if got := f.Dials(); len(got) != 0 {
				t.Errorf("dials %v, want none", got)
			}
		})
	}
}

// sealedServer is a server over a memory KV store whose secret provider
// seals the directory's bind password, through the seal it returns, dialling
// through dial and logging at debug into logs (nil: nowhere).
func sealedServer(t *testing.T, clock *sim.TestClock, dial directory.Dialer, logs io.Writer) (*identity.Server, func(n byte, pw []byte) entity.SecretRef) {
	t.Helper()
	st, _ := dirStore(t)
	p := protocolSecrets(t, st)
	if logs == nil {
		logs = io.Discard
	}
	s, err := identity.New(identity.Deps{Store: st, Clock: clock, Secrets: p, Directory: dial,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s, func(n byte, pw []byte) entity.SecretRef { return sealBindPassword(t, st, p, n, pw) }
}

// sealedState is dirState(d) with the record's address addr and its bind
// credential ref.
func sealedState(d *identitytest.Directory, addr string, ref entity.SecretRef) identity.State {
	st := dirState(d)
	st.Directory.Address, st.Directory.BindCredential = addr, ref
	return st
}

// An Apply of a second record while a question waits on the first
// directory's answer: the question ends on the first directory's answer and
// the next one on the second's, each bound with the password the secret
// provider resolves.
func TestApplySecondDirectoryWhileAnswerDelayed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		clock := sim.NewTestClock(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC))
		one, two := identitytest.NewDirectory("DC=corp,DC=example"), identitytest.NewDirectory("DC=corp,DC=example")
		one.AddUser(t, "alice", map[string][][]byte{"mail": attr("alice@one.example")})
		two.AddUser(t, "alice", map[string][][]byte{"mail": attr("alice@two.example")})
		slow := &switched{}
		f1 := identitytest.NewFaultyDirectory(one.Dial, slow)
		dial := func(ctx context.Context, rec directory.Record) (directory.Conn, error) {
			if rec.Address == "ldaps://dc1.corp.example" {
				return f1.Dial(ctx, rec)
			}
			return two.Dial(ctx, rec)
		}
		s, seal := sealedServer(t, clock, dial, nil)
		_, pw := one.Account()
		ref := seal(1, pw)
		if err := s.Apply(sealedState(one, "ldaps://dc1.corp.example", ref)); err != nil {
			t.Fatal(err)
		}
		sched, err := sim.NewSchedule(1, clock, sim.Rule{Op: "search", Key: userSearches, Count: 1,
			Action: sim.FaultAction{Delay: time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		slow.cur.Store(sched)
		type answer struct {
			mail string
			err  error
		}
		held := make(chan answer, 1)
		go func() {
			a, _, err := s.DirectoryLookup(ctx, directory.ByUserName, []byte("alice"))
			held <- answer{a.Mail, err}
		}()
		synctest.Wait()
		if err := s.Apply(sealedState(two, "ldaps://dc2.corp.example", ref)); err != nil {
			t.Fatal(err)
		}
		next := make(chan answer, 1)
		go func() {
			a, _, err := s.DirectoryLookup(ctx, directory.ByUserName, []byte("alice"))
			next <- answer{a.Mail, err}
		}()
		synctest.Wait()
		select {
		case got := <-next:
			if got.err != nil || got.mail != "alice@two.example" {
				t.Errorf("lookup after the second Apply = %q, %v; want alice@two.example", got.mail, got.err)
			}
		default:
			t.Error("lookup after the second Apply still waiting on the first directory, want the second's answer at once")
		}
		clock.Advance(time.Second)
		if got := <-held; got.err != nil || got.mail != "alice@one.example" {
			t.Errorf("lookup delayed across the Apply = %q, %v; want alice@one.example", got.mail, got.err)
		}
	})
}

// The bind password, sealed through the secret provider, is in no log line
// and no error, through an ambiguous answer and a bind the directory refuses.
func TestSealedBindPasswordNeverLogged(t *testing.T) {
	ctx := context.Background()
	d := identitytest.NewDirectory("DC=corp,DC=example")
	for _, cn := range []string{"dup1", "dup2"} {
		d.AddUser(t, cn, map[string][][]byte{"sAMAccountName": attr("dup")})
	}
	var logs bytes.Buffer
	s, seal := sealedServer(t, sim.NewTestClock(time.Unix(0, 0)), d.Dial, &logs)
	_, right := d.Account()
	wrong := []byte("a-wrong-password-7f3a")
	if err := s.Apply(sealedState(d, "ldaps://dc.corp.example", seal(1, right))); err != nil {
		t.Fatal(err)
	}
	_, _, err1 := s.DirectoryLookup(ctx, directory.ByUserName, []byte("dup"))
	if err := s.Apply(sealedState(d, "ldaps://dc.corp.example", seal(2, wrong))); err != nil {
		t.Fatal(err)
	}
	_, _, err2 := s.DirectoryLookup(ctx, directory.ByUserName, []byte("dup"))
	if err1 == nil || err2 == nil {
		t.Fatalf("lookups = %v, %v; want both refused", err1, err2)
	}
	for _, pw := range [][]byte{right, wrong} {
		if bytes.Contains(logs.Bytes(), pw) || strings.Contains(err1.Error()+err2.Error(), string(pw)) {
			t.Errorf("the password %q is in a log line or an error: %q, %v, %v", pw, logs.String(), err1, err2)
		}
	}
	if logs.Len() == 0 {
		t.Error("nothing logged, want the ambiguous answer and the refused bind")
	}
}

// Apply, Start and Stop return at once and dial nothing while every dial
// would hang until its context ends: the client dials at its first question.
func TestStartStopDialNothingWhileDialsHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		clock := sim.NewTestClock(time.Unix(0, 0))
		d := identitytest.NewDirectory("DC=corp,DC=example")
		d.AddUser(t, "alice", nil)
		sched, err := sim.NewSchedule(1, clock, sim.Rule{Op: "dial", Key: "*", Action: sim.FaultAction{Err: identitytest.ErrHold}})
		if err != nil {
			t.Fatal(err)
		}
		f := identitytest.NewFaultyDirectory(d.Dial, sched)
		s, seal := sealedServer(t, clock, f.Dial, nil)
		_, pw := d.Account()
		st := sealedState(d, "ldaps://dc.corp.example", seal(1, pw))
		done := make(chan error, 1)
		go func() {
			if err := s.Apply(st); err != nil {
				done <- err
				return
			}
			if err := s.Start(ctx); err != nil {
				done <- err
				return
			}
			done <- s.Stop(ctx)
		}()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Apply, Start, Stop = %v", err)
			}
		default:
			t.Error("Apply, Start and Stop still running with every dial held, want them returned at once")
		}
		if got := f.Dials(); len(got) != 0 {
			t.Errorf("dials %v, want none before a question", got)
		}
		// A dial a broken Apply sent ends at the client's timeout, so the
		// bubble can end.
		clock.Advance(time.Minute)
		synctest.Wait()
	})
}
