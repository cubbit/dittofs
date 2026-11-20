package client_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// Each principal accessor reaches its routes: a user created, read,
// patched, listed and deleted by an operation; a group created and a member
// added and removed; a grant and a quota put and read on a share; a
// netgroup created under *, refused a second create 412, replaced under its
// tag, listed and deleted.
func TestPrincipalAccessors(t *testing.T) {
	s := serve(t, admin())
	clock := newClock()
	c := dial(t, s.url, clock)
	ctx := t.Context()

	created, err := userResult(c.Users().Create(ctx, apiv1.UserSpec{Name: "alice"}))
	if err != nil || created.Resource == nil || created.Operation != nil {
		t.Fatalf("Users().Create: %+v err %v, want alice and no operation", created, err)
	}
	u := *created.Resource
	if u.ID == "" || u.Spec.UID == nil || u.ETag == "" {
		t.Fatalf("Users().Create: %+v, want alice with an allocated uid and an ETag", u)
	}
	if got, err := c.Users().Get(ctx, u.ID); err != nil || got.Spec.Name != "alice" || got.ETag != u.ETag {
		t.Errorf("Users().Get: %+v err %v, want alice under ETag %s", got, err, u.ETag)
	}
	pr, err := userResult(c.Users().Patch(ctx, u.ID, map[string]any{"spec": map[string]any{"disabled": true}}, u.ETag))
	if err != nil || pr.Resource == nil || !pr.Resource.Spec.Disabled {
		t.Fatalf("Users().Patch disabled: %+v err %v, want her disabled", pr, err)
	}
	patched := *pr.Resource
	var names []string
	for it, err := range c.Users().List(ctx, nil) {
		if err != nil {
			t.Fatalf("Users().List: %v", err)
		}
		names = append(names, it.Spec.Name)
	}
	if !slices.Equal(names, []string{"admin", "alice"}) {
		t.Errorf("Users().List: %v, want the installation's admin and alice alone", names)
	}

	g, err := c.Groups().Create(ctx, apiv1.GroupSpec{Name: "staff"})
	if err != nil || g.Spec.GID == nil {
		t.Fatalf("Groups().Create: %+v err %v, want staff with an allocated gid", g, err)
	}
	m, err := c.Members(g.ID).Put(ctx, u.ID, struct{}{}, "*")
	if err != nil || m.ID != u.ID || m.ETag == "" {
		t.Fatalf("Members().Put *: %+v err %v, want alice's membership with an ETag", m, err)
	}
	if op, err := c.Members(g.ID).Delete(ctx, u.ID, m.ETag); err != nil || op != nil {
		t.Errorf("Members().Delete: operation %+v err %v, want it removed in the call", op, err)
	}

	sh, err := c.CreateShare(ctx, newShare("builds", s.namespace(t)))
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	gr, err := c.Grants(sh.ID).Put(ctx, u.ID, apiv1.GrantSpec{Access: "none"}, "*")
	if err != nil || gr.Spec.Access != "none" {
		t.Fatalf("Grants().Put none: %+v err %v, want a none grant", gr, err)
	}
	if got, err := c.Grants(sh.ID).Get(ctx, u.ID); err != nil || got.Spec.Access != "none" || got.ETag != gr.ETag {
		t.Errorf("Grants().Get: %+v err %v, want the none grant under ETag %s", got, err, gr.ETag)
	}
	var listed []string
	for it, err := range c.Grants(sh.ID).List(ctx, nil) {
		if err != nil {
			t.Fatalf("Grants().List: %v", err)
		}
		listed = append(listed, it.ID)
	}
	if !slices.Equal(listed, []string{u.ID}) {
		t.Errorf("Grants().List: %v, want alice's alone", listed)
	}
	if op, err := c.Grants(sh.ID).Delete(ctx, u.ID, gr.ETag); err != nil || op != nil {
		t.Errorf("Grants().Delete: operation %+v err %v, want it removed in the call", op, err)
	}
	q, err := c.Quotas(sh.ID).Put(ctx, u.ID, apiv1.QuotaSpec{Hard: apiv1.Limit{Files: 10}}, "*")
	if err != nil || q.Spec.Hard.Files != 10 {
		t.Fatalf("Quotas().Put: %+v err %v, want a hard limit of 10 files", q, err)
	}
	if got, err := c.Quotas(sh.ID).Get(ctx, u.ID); err != nil || got.ETag != q.ETag {
		t.Errorf("Quotas().Get: %+v err %v, want it under ETag %s", got, err, q.ETag)
	}

	var quotas []string
	for it, err := range c.Quotas(sh.ID).List(ctx, nil) {
		if err != nil {
			t.Fatalf("Quotas().List: %v", err)
		}
		quotas = append(quotas, it.ID)
	}
	if !slices.Equal(quotas, []string{u.ID}) {
		t.Errorf("Quotas().List: %v, want alice's alone", quotas)
	}
	sq, err := c.ShareQuotas().Put(ctx, sh.ID, apiv1.QuotaSpec{Hard: apiv1.Limit{Bytes: 1 << 20}}, "*")
	if err != nil || sq.ID != sh.ID || sq.Spec.Hard.Bytes != 1<<20 {
		t.Fatalf("ShareQuotas().Put: %+v err %v, want the share's 1 MiB quota named by the share", sq, err)
	}
	if got, err := c.ShareQuotas().Get(ctx, sh.ID); err != nil || got.ETag != sq.ETag {
		t.Errorf("ShareQuotas().Get: %+v err %v, want it under ETag %s", got, err, sq.ETag)
	}
	var named []string
	for it, err := range c.Users().List(ctx, url.Values{"name": {"alice"}}) {
		if err != nil {
			t.Fatalf("Users().List name=alice: %v", err)
		}
		named = append(named, it.ID)
	}
	if !slices.Equal(named, []string{u.ID}) {
		t.Errorf("Users().List name=alice: %v, want alice alone", named)
	}

	n, err := c.Netgroups().Put(ctx, "labs", apiv1.NetgroupSpec{Members: []string{"a", "b"}}, "*")
	if err != nil || n.ID != "labs" {
		t.Fatalf("Netgroups().Put *: %+v err %v, want labs", n, err)
	}
	err = settled(t, "Netgroups().Put * on labs again", clock, func(ctx context.Context) error {
		_, err := c.Netgroups().Put(ctx, "labs", apiv1.NetgroupSpec{Members: []string{"c"}}, "*")
		return err
	})
	if p := problem(t, "Netgroups().Put * on labs again", err); p.Status != http.StatusPreconditionFailed {
		t.Errorf("Netgroups().Put * on labs again: %d %q, want 412", p.Status, p.Code)
	}
	n, err = c.Netgroups().Put(ctx, "labs", apiv1.NetgroupSpec{Members: []string{"c"}}, n.ETag)
	if err != nil || !slices.Equal(n.Spec.Members, []string{"c"}) {
		t.Fatalf("Netgroups().Put under its tag: %+v err %v, want c alone", n, err)
	}
	var groups []string
	for it, err := range c.Netgroups().List(ctx, nil) {
		if err != nil {
			t.Fatalf("Netgroups().List: %v", err)
		}
		groups = append(groups, it.ID)
	}
	if !slices.Equal(groups, []string{"labs"}) {
		t.Errorf("Netgroups().List: %v, want labs alone", groups)
	}
	if _, err := c.Netgroups().Delete(ctx, "labs", n.ETag); err != nil {
		t.Errorf("Netgroups().Delete: %v", err)
	}

	op, err := c.Users().Delete(ctx, u.ID, patched.ETag)
	if err != nil || op == nil || op.Kind != apiv1.OpUserDelete {
		t.Fatalf("Users().Delete: operation %+v err %v, want a user.delete", op, err)
	}
	checkKeys(t, s.front)
}

// A user write naming a Kerberos name a machine holds answers the
// machine's deletion: Create, Put and Patch each return that operation, not
// a user decoded from it.
func TestUserWriteMovingAMachineNameReturnsTheOperation(t *testing.T) {
	s := serve(t, admin())
	c := dial(t, s.url, newClock())
	ctx := t.Context()
	machine := func(name string) {
		t.Helper()
		m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()},
			IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: name}}}
		if _, err := s.store.PutUser(ctx, m, 0, entity.Alloc{}); err != nil {
			t.Fatal(err)
		}
	}
	moved := func(call string, res userWrite, err error) {
		t.Helper()
		if err != nil || res.Resource != nil || res.Operation == nil || res.Operation.Kind != apiv1.OpUserDelete {
			t.Fatalf("%s moving a machine's name: %+v err %v, want the machine's user.delete and no user", call, res, err)
		}
	}

	machine("host/a@AD.EXAMPLE")
	res, err := userResult(c.Users().Create(ctx, apiv1.UserSpec{Name: "alice", Kerberos: "host/a@AD.EXAMPLE"}))
	moved("Users().Create", res, err)

	bob, err := userResult(c.Users().Create(ctx, apiv1.UserSpec{Name: "bob"}))
	if err != nil || bob.Resource == nil {
		t.Fatalf("Users().Create bob: %+v err %v", bob, err)
	}
	machine("host/b@AD.EXAMPLE")
	sp := bob.Resource.Spec
	sp.Kerberos = "host/b@AD.EXAMPLE"
	res, err = userResult(c.Users().Put(ctx, bob.Resource.ID, sp, bob.Resource.ETag))
	moved("Users().Put", res, err)

	b, err := c.Users().Get(ctx, bob.Resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	machine("host/c@AD.EXAMPLE")
	res, err = userResult(c.Users().Patch(ctx, b.ID, map[string]any{"spec": map[string]any{"kerberos": "host/c@AD.EXAMPLE"}}, b.ETag))
	moved("Users().Patch", res, err)
}

// userWrite is what a user write returns.
type userWrite = client.Result[apiv1.UserSpec, struct{}]

// userResult is a user write's answer as a Result, the answer taken as any
// so the tests compile, and fail on their assertions, against a client
// whose user writes return the bare user instead.
func userResult(res any, err error) (userWrite, error) {
	switch r := res.(type) {
	case userWrite:
		return r, err
	case client.Tagged[apiv1.UserSpec, struct{}]:
		return userWrite{Resource: &r}, err
	}
	return userWrite{}, fmt.Errorf("a user write returned %T", res)
}
