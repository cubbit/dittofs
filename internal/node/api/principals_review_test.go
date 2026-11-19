package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// switchable authenticates every request as the caller last set.
type switchable struct{ c atomic.Pointer[auth.Caller] }

func (w *switchable) set(p entity.Principal, role entity.APIRole) {
	w.c.Store(&auth.Caller{Principal: p, Name: "caller", Credential: "session-1", Role: role})
}

func (w *switchable) authenticate(*http.Request) (auth.Caller, error) { return *w.c.Load(), nil }

// etagOf is a generation as an ETag.
func etagOf(gen uint64) string { return `"` + strconv.FormatUint(gen, 10) + `"` }

// An operator may not disable, rename or delete an account holding an API
// role: each is refused 403 and the account is as it was. An admin may.
func TestOperatorCannotChangeARoleHolder(t *testing.T) {
	var w switchable
	w.set(entity.NewUser(), entity.RoleOperator)
	s := serve(t, w.authenticate)
	root := entity.NewUser()
	gen, err := s.store.PutUser(t.Context(), entity.User{Principal: root, Name: "root", Role: entity.RoleAdmin}, 0, entity.Alloc{})
	if err != nil {
		t.Fatalf("put the admin: %v", err)
	}
	path := "/api/v1/users/" + uuid.UUID(root.ID).String()
	for _, tc := range []struct{ method, body string }{
		{http.MethodPatch, `{"spec":{"disabled":true}}`},
		{http.MethodPut, `{"spec":{"name":"renamed","disabled":false}}`},
		{http.MethodDelete, ""},
	} {
		r := s.do(t, tc.method, path, tc.body, "If-Match", etagOf(gen), "Idempotency-Key", "k-"+tc.method)
		if p := r.problem(t); r.status != http.StatusForbidden || p.Code != apiv1.CodeForbidden {
			t.Errorf("%s the admin as an operator: answered %d %q, want 403 forbidden", tc.method, r.status, p.Code)
		}
	}
	if u, ok := s.userRecord(t, uuid.UUID(root.ID).String()); !ok || u.Disabled || u.Name != "root" || u.Generation != gen {
		t.Fatalf("store's admin after the operator's writes: %+v, %v; want it unchanged", u, ok)
	}
	w.set(entity.NewUser(), entity.RoleAdmin)
	if r := s.do(t, http.MethodPatch, path, `{"spec":{"disabled":true}}`, "If-Match", etagOf(gen)); r.status != http.StatusOK {
		t.Errorf("PATCH the admin as an admin: answered %d %s, want 200", r.status, r.body)
	}
}

// A viewer reading another user gets the record without its address, by
// its GET and in a listing; the user itself and an operator read it.
func TestEmailVisibility(t *testing.T) {
	var w switchable
	w.set(entity.NewUser(), entity.RoleOperator)
	s := serve(t, w.authenticate)
	u, _ := s.createUser(t, apiv1.UserSpec{Name: "alice", Email: "alice@example.com"}, "c1")
	alice, _ := entity.ParsePrincipalID(u.ID)
	read := func() (got, listed string) {
		t.Helper()
		var one apiv1.User
		if r := s.do(t, http.MethodGet, "/api/v1/users/"+u.ID, ""); r.status != http.StatusOK || json.Unmarshal(r.body, &one) != nil {
			t.Fatalf("GET alice: answered %d %s", r.status, r.body)
		}
		var page apiv1.Page[apiv1.User]
		if r := s.do(t, http.MethodGet, "/api/v1/users", ""); r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil || len(page.Items) != 1 {
			t.Fatalf("GET /users: answered %d %s", r.status, r.body)
		}
		return one.Spec.Email, page.Items[0].Spec.Email
	}
	for _, tc := range []struct {
		who  string
		p    entity.Principal
		role entity.APIRole
		want string
	}{
		{"another viewer", entity.NewUser(), entity.RoleViewer, ""},
		{"alice, a viewer", entity.Principal{Kind: entity.PrincipalUser, ID: alice}, entity.RoleViewer, "alice@example.com"},
		{"an operator", entity.NewUser(), entity.RoleOperator, "alice@example.com"},
	} {
		w.set(tc.p, tc.role)
		if got, listed := read(); got != tc.want || listed != tc.want {
			t.Errorf("%s reads alice's address %q, and %q listed; want %q", tc.who, got, listed, tc.want)
		}
	}
}

// A PUT under If-None-Match: * of a user or group that does not exist is
// 404, creating nothing: they are created by POST alone.
func TestPutCreatesNoPrincipal(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	for _, path := range []string{"/api/v1/users/" + uuid.NewString(), "/api/v1/groups/" + uuid.NewString()} {
		if r := s.do(t, http.MethodPut, path, `{"spec":{"name":"x"}}`, "If-None-Match", "*"); r.status != http.StatusNotFound {
			t.Errorf("PUT * %s: answered %d %s, want 404", path, r.status, r.body)
		}
	}
	if n := s.userCount(t); n != 0 {
		t.Errorf("users after the PUTs: %d, want none", n)
	}
}

// A user minted with no name can be disabled, keeping no name; a named user
// cannot lose its name.
func TestUnnamedUserEdit(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	p := entity.NewUser()
	if err := s.db.Minter().Create(t.Context(), entity.MintBatch{Users: []entity.MintUser{{User: entity.User{Principal: p, Name: "taken"}, Unnamed: true}}}); err != nil {
		t.Fatalf("mint the unnamed user: %v", err)
	}
	id := uuid.UUID(p.ID).String()
	rec, _ := s.userRecord(t, id)
	if r := s.do(t, http.MethodPatch, "/api/v1/users/"+id, `{"spec":{"disabled":true}}`, "If-Match", etagOf(rec.Generation)); r.status != http.StatusOK {
		t.Errorf("PATCH the unnamed user disabled: answered %d %s, want 200", r.status, r.body)
	}
	if rec, _ := s.userRecord(t, id); !rec.Disabled || rec.Name != "" {
		t.Errorf("store's unnamed user: %+v, want it disabled with no name", rec)
	}
	u, tag := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	if r := s.do(t, http.MethodPatch, "/api/v1/users/"+u.ID, `{"spec":{"name":""}}`, "If-Match", tag); r.status != http.StatusBadRequest {
		t.Errorf("PATCH alice's name away: answered %d %s, want 400", r.status, r.body)
	}
}

// A netgroup's name must be valid UTF-8 of at most 255 bytes, and each
// member an address, a CIDR or a host name; a refused one stores nothing,
// and its refusal is audited.
func TestNetgroupValidation(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	long := strings.Repeat("a", 70_000)
	for _, tc := range []struct{ what, path, body string }{
		{"a name that is not UTF-8", "/api/v1/netgroups/%FF", `{"spec":{"members":[]}}`},
		{"a 70k name", "/api/v1/netgroups/" + long, `{"spec":{"members":[]}}`},
		{"a member that is no host", "/api/v1/netgroups/labs", `{"spec":{"members":["10.0.0.1","bad host!"]}}`},
	} {
		r := s.do(t, http.MethodPut, tc.path, tc.body, "If-None-Match", "*")
		if r.status != http.StatusBadRequest {
			t.Errorf("PUT %s: answered %d %.200s, want 400", tc.what, r.status, r.body)
		}
	}
	for g, err := range s.store.Netgroups(t.Context(), "") {
		t.Errorf("netgroup %+v (%v) stored, want none", g, err)
	}
	refused := 0
	for _, a := range s.audits(t) {
		if a.Target.Kind == entity.ResourceNetgroup && string(a.Outcome) != "applied" {
			refused++
		}
	}
	if refused != 3 {
		t.Errorf("netgroup refusals audited: %d, want 3", refused)
	}
	ok := s.do(t, http.MethodPut, "/api/v1/netgroups/labs", `{"spec":{"members":["10.0.0.1","10.0.0.0/8","build-1.example.com"]}}`, "If-None-Match", "*")
	if ok.status != http.StatusOK {
		t.Errorf("PUT an address, a CIDR and a host name: answered %d %s, want 200", ok.status, ok.body)
	}
}

// A quota's grace is refused below zero and beyond what a duration holds.
func TestQuotaGraceBounds(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	_, loc := s.createShare(t, "builds", s.namespace(t))
	u, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	for _, g := range []string{"-1", "9223372037"} {
		r := s.do(t, http.MethodPut, loc+"/quotas/"+u.ID, `{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":`+g+`,"advisory":{"bytes":0,"files":0}}}`, "If-None-Match", "*")
		if p := r.problem(t); r.status != http.StatusBadRequest || p.Field != "spec.grace_seconds" {
			t.Errorf("PUT a grace of %s s: answered %d %+v, want 400 naming spec.grace_seconds", g, r.status, p)
		}
	}
}

// ?name= on users and groups answers the one of that name, its case
// folded, or none.
func TestPrincipalNameFilter(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	s.createUser(t, apiv1.UserSpec{Name: "bob"}, "c2")
	g := s.createGroup(t, apiv1.GroupSpec{Name: "staff"}, "g1")
	s.createGroup(t, apiv1.GroupSpec{Name: "other"}, "g2")
	names := func(path string) []string {
		t.Helper()
		r := s.do(t, http.MethodGet, path, "")
		var page apiv1.Page[apiv1.Resource[map[string]any, struct{}]]
		if r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil {
			t.Fatalf("GET %s: answered %d %s", path, r.status, r.body)
		}
		var out []string
		for _, it := range page.Items {
			out = append(out, it.ID)
		}
		return out
	}
	if got := names("/api/v1/users?name=ALICE"); len(got) != 1 {
		t.Errorf("GET /users?name=ALICE: %v, want alice alone", got)
	}
	if got := names("/api/v1/groups?name=staff"); !slices.Equal(got, []string{g.ID}) {
		t.Errorf("GET /groups?name=staff: %v, want %s alone", got, g.ID)
	}
	for _, path := range []string{"/api/v1/users?name=nobody", "/api/v1/groups?name=nobody"} {
		if got := names(path); len(got) != 0 {
			t.Errorf("GET %s: %v, want none", path, got)
		}
	}
}

// A share's grants and its quotas on users and groups are listed page by
// page in principal ID order; its own quota is put and read at /quota.
func TestGrantAndQuotaListings(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	_, loc := s.createShare(t, "builds", s.namespace(t))
	a, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	g := s.createGroup(t, apiv1.GroupSpec{Name: "staff"}, "g1")
	want := []string{a.ID, g.ID}
	slices.Sort(want)
	for _, id := range want {
		if r := s.do(t, http.MethodPut, loc+"/grants/"+id, `{"spec":{"access":"none"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
			t.Fatalf("PUT a grant: answered %d %s", r.status, r.body)
		}
		if r := s.do(t, http.MethodPut, loc+"/quotas/"+id, `{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`, "If-None-Match", "*"); r.status != http.StatusOK {
			t.Fatalf("PUT a quota: answered %d %s", r.status, r.body)
		}
	}
	sq := s.do(t, http.MethodPut, loc+"/quota", `{"spec":{"hard":{"bytes":0,"files":9},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`, "If-None-Match", "*")
	if sq.status != http.StatusOK {
		t.Fatalf("PUT the share's quota: answered %d %s", sq.status, sq.body)
	}
	paged := func(path string) []string {
		t.Helper()
		var out []string
		for q, n := "?limit=1", 0; ; n++ {
			if n > 10 {
				t.Fatalf("GET %s: more than 10 pages of one, want 2", path)
			}
			r := s.do(t, http.MethodGet, path+q, "")
			var page apiv1.Page[apiv1.Resource[map[string]any, struct{}]]
			if r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil {
				t.Fatalf("GET %s%s: answered %d %s", path, q, r.status, r.body)
			}
			for _, it := range page.Items {
				out = append(out, it.ID)
			}
			if page.Next == "" {
				return out
			}
			q = "?limit=1&after=" + page.Next
		}
	}
	if got := paged(loc + "/grants"); !slices.Equal(got, want) {
		t.Errorf("GET grants page by page: %v, want %v", got, want)
	}
	if got := paged(loc + "/quotas"); !slices.Equal(got, want) {
		t.Errorf("GET quotas page by page: %v, want %v, the share's own apart", got, want)
	}
	got := s.do(t, http.MethodGet, loc+"/quota", "")
	var q apiv1.Quota
	if err := json.Unmarshal(got.body, &q); err != nil || got.status != http.StatusOK || q.Spec.Hard.Files != 9 || got.header.Get("ETag") != sq.header.Get("ETag") {
		t.Errorf("GET the share's quota: answered %d %s, want 9 files under %s", got.status, got.body, sq.header.Get("ETag"))
	}
}

// A grant's and a netgroup's write audit their record under one target
// whether the role table refuses it before the adapter or the adapter
// applies it: the share's ID then the principal's, and the netgroup's name.
func TestPairAndNameTargets(t *testing.T) {
	var w switchable
	w.set(entity.NewUser(), entity.RoleOperator)
	s := serve(t, w.authenticate)
	share, loc := s.createShare(t, "builds", s.namespace(t))
	u, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	pid, _ := entity.ParsePrincipalID(u.ID)
	for _, role := range []entity.APIRole{entity.RoleViewer, entity.RoleOperator} {
		w.set(entity.NewUser(), role)
		s.do(t, http.MethodPut, loc+"/grants/"+u.ID, `{"spec":{"access":"read"}}`, "If-None-Match", "*")
		s.do(t, http.MethodPut, "/api/v1/netgroups/labs", `{"spec":{"members":[]}}`, "If-None-Match", "*")
	}
	targets := map[entity.ResourceKind][][]byte{}
	for _, a := range s.audits(t) {
		if a.Target.Kind == entity.ResourceGrant || a.Target.Kind == entity.ResourceNetgroup {
			targets[a.Target.Kind] = append(targets[a.Target.Kind], a.Target.ID)
		}
	}
	for kind, want := range map[entity.ResourceKind][]byte{entity.ResourceGrant: append(share[:], pid[:]...), entity.ResourceNetgroup: []byte("labs")} {
		got := targets[kind]
		if len(got) != 2 || !bytes.Equal(got[0], want) || !bytes.Equal(got[1], want) {
			t.Errorf("%s audit targets %x, want the refused and the applied write both at %x", kind, got, want)
		}
	}
}
