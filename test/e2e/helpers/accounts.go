//go:build e2e

package helpers

import (
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// Account is a user as a test presents it: its ID, its name, the password
// set on it, and the uid and the gids of its groups it presents over
// AUTH_SYS, where the gids sent are the caller's groups.
type Account struct {
	ID       string
	Name     string
	Password string
	UID      uint32
	GIDs     []uint32
}

// Group is a group as a test names it: its ID, which grants and memberships
// name, and its gid, which a member presents over AUTH_SYS.
type Group struct {
	ID  string
	GID uint32
}

// Admin is the bootstrap administrator, which holds no uid: it presents uid
// 0 over AUTH_SYS, the uid a root bypass would key on, with no group.
func Admin() Account { return Account{Name: "admin", UID: 0} }

// NewAccount creates a user named after prefix, unique on the server, with
// uid, 0 for one the server allocates, a password set through
// :set-password, and no grant, and then gives it role, "" for none. A uid
// given must be unique in the package, since a deleted user may hold its
// uid until its deletion's operation ends. The test's end deletes it.
//
// decision: the role is patched on after the create, since a create with a
// role and no uid makes an API-only account, which holds no uid to present;
// the account must hold every spelling, so that a refusal can come only from
// admission.
func NewAccount(t *testing.T, prefix string, role apiv1.Role, uid uint32) Account {
	t.Helper()
	c := Start(t).API
	ctx, cancel := call()
	defer cancel()
	spec := apiv1.UserSpec{Name: unique(prefix)}
	if uid != 0 {
		spec.UID = &uid
	}
	res, err := c.Users().Create(ctx, spec)
	if err != nil {
		t.Fatalf("creating the user %s: %v", spec.Name, err)
	}
	if res.Resource == nil || res.Resource.Spec.UID == nil {
		t.Fatalf("creating the user %s answered %+v, want the user with a uid", spec.Name, res)
	}
	u := res.Resource
	t.Cleanup(func() { remove(t, "user "+spec.Name, Start(t).API.Users().Resource, u.ID, "") })
	if role != "" {
		patch := map[string]any{"spec": map[string]any{"role": role}}
		res, err := c.Users().Patch(ctx, u.ID, patch, u.ETag)
		if err != nil || res.Resource == nil || res.Resource.Spec.UID == nil || *res.Resource.Spec.UID != *u.Spec.UID {
			t.Fatalf("giving the user %s the role %s: answered %+v, %v; want the user keeping uid %d", spec.Name, role, res, err, *u.Spec.UID)
		}
	}
	a := Account{ID: u.ID, Name: spec.Name, Password: rand.Text(), UID: *u.Spec.UID}
	if err := c.Users().SetPassword(ctx, u.ID, []byte(a.Password)); err != nil {
		t.Fatalf("setting the password of the user %s: %v", spec.Name, err)
	}
	return a
}

// NewGroup creates a group named after prefix, unique on the server, with a
// gid the server allocates. The test's end deletes it.
func NewGroup(t *testing.T, prefix string) Group {
	t.Helper()
	c := Start(t).API
	ctx, cancel := call()
	defer cancel()
	name := unique(prefix)
	g, err := c.Groups().Create(ctx, apiv1.GroupSpec{Name: name})
	if err != nil || g.Spec.GID == nil {
		t.Fatalf("creating the group %s: answered %+v, %v; want the group with a gid", name, g.Spec, err)
	}
	t.Cleanup(func() { remove(t, "group "+name, Start(t).API.Groups(), g.ID, "") })
	return Group{ID: g.ID, GID: *g.Spec.GID}
}

// Member adds the account a to the group g, and g's gid to the gids a
// presents. The test's end removes the membership.
func Member(t *testing.T, g Group, a *Account) {
	t.Helper()
	c := Start(t).API
	ctx, cancel := call()
	defer cancel()
	m, err := c.Members(g.ID).Put(ctx, a.ID, struct{}{}, "*")
	if err != nil {
		t.Fatalf("adding %s to the group %s: %v", a.Name, g.ID, err)
	}
	a.GIDs = append(a.GIDs, g.GID)
	// A membership has no Get, so its removal is tagged with the put's ETag.
	t.Cleanup(func() { remove(t, "member "+a.ID+" of "+g.ID, Start(t).API.Members(g.ID), a.ID, m.ETag) })
}

// remove deletes the resource id names, whose tag is etag or, when etag is
// empty, the one a read answers, unless it is already gone, without waiting
// for an operation the deletion starts, failing t when the server refuses
// it.
func remove[S, T any](t *testing.T, what string, r client.Resource[S, T], id, etag string) {
	t.Helper()
	ctx, cancel := call()
	defer cancel()
	var err error
	if etag == "" {
		var cur client.Tagged[S, T]
		cur, err = r.Get(ctx, id)
		etag = cur.ETag
	}
	if err == nil {
		_, err = r.Delete(ctx, id, etag)
	}
	if !gone(err) {
		t.Errorf("deleting the %s: %v", what, err)
	}
}

// gone reports whether err is nil or the server's 404.
func gone(err error) bool {
	var p *apiv1.Problem
	return err == nil || errors.As(err, &p) && p.Status == http.StatusNotFound
}

// unique is prefix with a random suffix, as names are unique on the server
// that every test of the package shares.
func unique(prefix string) string { return prefix + "-" + strings.ToLower(rand.Text()[:8]) }
