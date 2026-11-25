package vfs

import (
	"context"
	"errors"
	"testing"
	"time"

	identitysrv "github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/settings"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// RFC 18 §10's "owner strings" at the service, under identity.nfs4_domain
// corp.example: a proven chown to the owner found from alice@corp.example
// makes alice the file's owner; alice@other.example names no one; and a file
// whose owner was deleted has no owner spelling.
func TestOwnerStrings(t *testing.T) {
	store := kvstoretest.NewFS(t, memKV)
	srv, err := identitysrv.New(identitysrv.Deps{Store: store.Store, Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	snap, _, err := settings.Read(t.Context(), store.Store.Nodes(), settings.Table{identitysrv.Section()}, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identitysrv.SettingsOf(snap, entity.NodeID{})
	if err != nil {
		t.Fatal(err)
	}
	ids.NFS4Domain = "corp.example"
	if err := srv.Apply(identitysrv.State{Settings: ids}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	s := startShares(t, newFixtureOver(t, Deps{Identity: srv, Names: srv}, store), nil)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	alice := s.user(t, "alice", "1500", s.share, entity.AccessReadWrite)
	bob := s.user(t, "bob", "1600", s.share, entity.AccessReadWrite)

	// chown sets the owner the string names, then reads it back as the file's.
	chown := func(name string, owner entity.Principal) vfs.Handle {
		t.Helper()
		r, err := s.Create(ctx, admin, s.fs.Root, []byte(name), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)})
		s.must(t, err)
		_, err = s.SetAttr(ctx, admin, vfs.Target{H: r.Handle}, vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(owner)})
		s.must(t, err)
		return r.Handle
	}
	p, err := s.Find(ctx, admin, vfs.Spelling{Scheme: vfs.SchemeOwner, Text: "alice@corp.example"})
	if err != nil {
		t.Fatalf("Find(owner alice@corp.example): %v", err)
	}
	h := chown("a", p)
	f, err := s.GetAttr(ctx, admin, h)
	s.must(t, err)
	if f.Owner != alice.Principal {
		t.Errorf("owner after the chown to alice@corp.example = %v, want alice %v", f.Owner, alice.Principal)
	}
	if sp, err := s.Spell(ctx, f.Owner, vfs.SchemeOwner); err != nil || sp.Text != "alice@corp.example" {
		t.Errorf("Spell of the file's owner = %q, %v; want alice@corp.example", sp.Text, err)
	}

	if p, err := s.Find(ctx, admin, vfs.Spelling{Scheme: vfs.SchemeOwner, Text: "alice@other.example"}); !errors.Is(err, vfs.ErrUnknownPrincipal) {
		t.Errorf("Find(owner alice@other.example) = %v, %v; want ErrUnknownPrincipal", p, err)
	}

	h = chown("b", bob.Principal)
	s.must(t, s.cp.DeleteUser(ctx, bob.Principal, userGeneration(t, s.fs.Store, bob.Principal)))
	f, err = s.GetAttr(ctx, admin, h)
	s.must(t, err)
	if f.Owner != bob.Principal {
		t.Fatalf("owner of bob's file after bob's deletion = %v, want bob's %v", f.Owner, bob.Principal)
	}
	if sp, err := s.Spell(ctx, f.Owner, vfs.SchemeOwner); !errors.Is(err, vfs.ErrUnknownPrincipal) {
		t.Errorf("Spell of a deleted owner = %+v, %v; want ErrUnknownPrincipal", sp, err)
	}
}
