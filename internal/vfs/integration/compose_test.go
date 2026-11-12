//go:build engine_conformance

// Package integration runs the filesystem service over the engine: the
// filesystem built over the engine fixture's real journal, metadata store,
// open-state table and test clock, with faults injected below them.
package integration

import (
	"net/netip"
	"testing"

	filesystem "github.com/cubbit/dittofs/internal/vfs"
	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// shareLimit is the share's journal limit: small enough that overwrites of
// one range reach it within a test.
const shareLimit = 8 << 20

// composition is the filesystem over the engine fixture, rebuilt with the
// open-state table and the engine at every process start.
type composition struct {
	*enginetest.Fixture
	fs         *filesystem.Filesystem // the filesystem running now
	admin      vfs.Call
	alice, bob entity.User
}

// client is where every call of a composition's test comes from.
var client = netip.MustParseAddr("10.0.0.5")

// compose builds the composition and signs up alice and bob, granted read
// and write on the share, beside the store's administrator. opts change what
// the engine fixture composes.
func compose(t *testing.T, opts ...enginetest.Option) *composition {
	t.Helper()
	c := &composition{}
	c.Fixture = enginetest.New(t, append(opts, enginetest.WithCompose(func(f *enginetest.Fixture, e *engine.Engine) enginetest.Service {
		// Read before the engine starts, which binds the share with it.
		f.Share = enginetest.ShareOf(t, f.FS.Store, f.Share.ID, shareLimit)
		fs, err := filesystem.New(filesystem.Deps{Meta: f.FS.Store, Open: f.Opens, Engine: e, Logger: f.Logger()})
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		c.fs = fs
		return fs
	}))...)
	// The administrator holds a uid, so it signs in as itself rather than as
	// the share's anonymous principal.
	c.user(t, c.FS.Admin.Principal, "admin", "100", entity.AccessAdmin)
	c.admin = signedIn(c.FS.Admin.Principal)
	c.alice = entity.User{Principal: entity.NewUser()}
	c.user(t, c.alice.Principal, "alice", "60001", entity.AccessReadWrite)
	c.bob = entity.User{Principal: entity.NewUser()}
	c.user(t, c.bob.Principal, "bob", "60002", entity.AccessReadWrite)
	return c
}

// user records p as a user named name holding uid, granted a on the share.
func (c *composition) user(t *testing.T, p entity.Principal, name, uid string, a entity.ShareAccess) {
	t.Helper()
	cp := c.FS.Store.ControlPlane()
	u := entity.User{Principal: p, Name: name, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: uid}}}
	if err := cp.PutUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if err := cp.SetGrant(t.Context(), entity.ShareGrant{Share: c.Share.ID, Principal: p, Access: a}); err != nil {
		t.Fatal(err)
	}
}

// signedIn is a call from p, proven by a ticket.
func signedIn(p entity.Principal) vfs.Call {
	return vfs.Call{Identity: vfs.Identity{
		Proof: vfs.ProofTicket, Protection: vfs.ProtectionPrivacy,
		Evidence: vfs.Evidence{Principal: p}, Client: client,
	}}
}

// owned creates name in the share's root, owned by owner with mode, as the
// administrator sets them, and answers its handle and ID.
func (c *composition) owned(t *testing.T, name string, owner entity.Principal, mode uint32) (vfs.Handle, entity.FileID) {
	t.Helper()
	h, id := c.File(name)
	files := c.FS.Store.Files()
	if _, err := files.SetAttrs(t.Context(), c.FS.Admin, h, entity.Attrs{}.WithOwner(owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := files.SetAttrs(t.Context(), c.FS.Admin, h, entity.Attrs{}.WithMode(mode)); err != nil {
		t.Fatal(err)
	}
	return h, id
}

// usage is p's usage on the share, as the store's quota record holds it.
func (c *composition) usage(t *testing.T, p entity.Principal) entity.Usage {
	t.Helper()
	u, err := c.FS.Store.Capacity().PrincipalUsage(t.Context(), c.Share.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
