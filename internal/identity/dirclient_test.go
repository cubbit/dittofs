package identity_test

import (
	"context"
	"errors"
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
