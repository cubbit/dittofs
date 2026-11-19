package identity_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/identity/directory"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
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

// countDials passes each dial through to next and counts it.
func countDials(n *atomic.Int32, next directory.Dialer) directory.Dialer {
	return func(ctx context.Context, rec directory.Record) (directory.Conn, error) {
		n.Add(1)
		return next(ctx, rec)
	}
}

// With no Deps.Directory, or no record, New and Start succeed and every
// directory question answers ErrUnavailable.
func TestNoDirectoryAnswersUnavailable(t *testing.T) {
	d := identitytest.NewDirectory("DC=corp,DC=example")
	d.AddUser(t, "alice", nil)
	var dials atomic.Int32
	for name, tc := range map[string]struct {
		dial directory.Dialer
		st   identity.State
	}{
		"no dialer": {nil, dirState(d)},
		"no record": {countDials(&dials, d.Dial), state(1)},
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
	if n := dials.Load(); n != 0 {
		t.Errorf("%d dials, want none", n)
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
			var dials atomic.Int32
			s, err := identity.New(identity.Deps{Store: newStore(t), Clock: sim.NewTestClock(time.Unix(0, 0)),
				Secrets: provider, Directory: countDials(&dials, d.Dial)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.Apply(dirState(d)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.DirectoryLookup(context.Background(), directory.ByUserName, []byte("alice")); !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("lookup = %v, want ErrUnavailable", err)
			}
			if n := dials.Load(); n != 0 {
				t.Errorf("%d dials, want none", n)
			}
		})
	}
}
