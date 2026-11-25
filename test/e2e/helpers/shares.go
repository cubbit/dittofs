//go:build e2e

package helpers

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Share is a share a test created: its ID, which grants name, and its name,
// which a client mounts.
type Share struct {
	ID   string
	Name string
}

// NewShare creates a share named after prefix, unique on the server, in the
// fixture's namespace, holding no grant, whose export policy offers AUTH_SYS
// and then Kerberos to every client and squashes nobody, and waits until the
// node applied it. The test's end deletes it.
//
// decision: nobody is squashed, so a mount as uid 0 reaches admission as
// root rather than as nobody, and a root bypass, which squashing would hide,
// shows as an admitted mount.
func NewShare(t *testing.T, prefix string) Share {
	t.Helper()
	c := Start(t).API
	ctx, cancel := call()
	defer cancel()
	name := unique(prefix)
	sh, err := c.CreateShare(ctx, apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: name, Path: "/" + name, Namespace: served.Namespace()},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys", "kerberos"}, Squash: "none", Clients: []apiv1.ClientRule{}},
	})
	if err != nil {
		t.Fatalf("creating the share %s: %v", name, err)
	}
	t.Cleanup(func() { remove(t, "share "+name, Start(t).API.Shares(), sh.ID, "") })
	if err := served.WaitApplied(ctx, sh.ID); err != nil {
		t.Fatalf("waiting for the share %s: %v", name, err)
	}
	return Share{ID: sh.ID, Name: name}
}

// Grant gives the principal whose ID is principal access on share, through a put that creates the grant, refused
// when the principal already holds one. The test's end deletes it unless
// Ungrant did.
func Grant(t *testing.T, share Share, principal string, access Access) {
	t.Helper()
	c := Start(t).API
	ctx, cancel := call()
	defer cancel()
	if _, err := c.Grants(share.ID).Put(ctx, principal, apiv1.GrantSpec{Access: string(access)}, "*"); err != nil {
		t.Fatalf("granting %s %s on the share %s: %v", principal, access, share.Name, err)
	}
	t.Cleanup(func() {
		remove(t, "grant of "+principal+" on "+share.Name, Start(t).API.Grants(share.ID), principal, "")
	})
}

// Ungrant deletes the grant the principal whose ID is principal holds on
// share, failing t when it holds none.
func Ungrant(t *testing.T, share Share, principal string) {
	t.Helper()
	c := Start(t).API
	ctx, cancel := call()
	defer cancel()
	g, err := c.Grants(share.ID).Get(ctx, principal)
	if err == nil {
		_, err = c.Grants(share.ID).Delete(ctx, principal, g.ETag)
	}
	if err != nil {
		t.Fatalf("deleting the grant of %s on the share %s: %v", principal, share.Name, err)
	}
}
