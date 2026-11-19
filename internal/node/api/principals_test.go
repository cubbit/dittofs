package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/google/uuid"
)

// createUser POSTs a user of spec under key, wanting 201, and returns it as
// answered with its ETag.
func (s *served) createUser(t *testing.T, spec apiv1.UserSpec, key string) (apiv1.User, string) {
	t.Helper()
	r := s.do(t, http.MethodPost, "/api/v1/users", jsonOf(t, map[string]any{"spec": spec}), "Idempotency-Key", key)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /users %+v: answered %d %s, want 201", spec, r.status, r.body)
	}
	var u apiv1.User
	if err := json.Unmarshal(r.body, &u); err != nil {
		t.Fatalf("POST /users: body %s: %v", r.body, err)
	}
	if loc := r.header.Get("Location"); loc != "/api/v1/users/"+u.ID {
		t.Errorf("POST /users: Location %q, want /api/v1/users/%s", loc, u.ID)
	}
	return u, r.header.Get("ETag")
}

// createGroup POSTs a group of spec under key, wanting 201, and returns it.
func (s *served) createGroup(t *testing.T, spec apiv1.GroupSpec, key string) apiv1.Group {
	t.Helper()
	r := s.do(t, http.MethodPost, "/api/v1/groups", jsonOf(t, map[string]any{"spec": spec}), "Idempotency-Key", key)
	if r.status != http.StatusCreated {
		t.Fatalf("POST /groups %+v: answered %d %s, want 201", spec, r.status, r.body)
	}
	var g apiv1.Group
	if err := json.Unmarshal(r.body, &g); err != nil {
		t.Fatalf("POST /groups: body %s: %v", r.body, err)
	}
	return g
}

// userRecord is the store's record of the user id names, false when there
// is none or it is marked deleted.
func (s *served) userRecord(t *testing.T, id string) (entity.User, bool) {
	t.Helper()
	pid, err := entity.ParsePrincipalID(id)
	if err != nil {
		t.Fatalf("user ID %q: %v", id, err)
	}
	u, ok, err := s.store.User(t.Context(), entity.Principal{Kind: entity.PrincipalUser, ID: pid})
	if err != nil {
		t.Fatalf("read user %s: %v", id, err)
	}
	return u, ok
}

func (s *served) userCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, err := range s.store.Users(t.Context(), "") {
		if err != nil {
			t.Fatalf("list users: %v", err)
		}
		n++
	}
	return n
}

// setIDRange writes identity.id_range at installation scope through the
// store, as the settings route will.
func (s *served) setIDRange(t *testing.T, r entity.IDRange) {
	t.Helper()
	v, _ := json.Marshal(r)
	if _, err := s.store.PutSetting(t.Context(), entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: "identity.id_range", Value: v}, 0); err != nil {
		t.Fatalf("put identity.id_range: %v", err)
	}
}

// A user created with no uid gets one allocated in identity.id_range, and a
// second create another; the create's answer names it, its Location the
// user. A create naming a uid another user holds is refused by the store and
// writes nothing, as is one naming an address that is not one; and a
// replayed create is the first answer, writing nothing more.
func TestUserCreate(t *testing.T) {
	caller, _ := one(entity.RoleOperator) // one account, whose keys a replay looks up
	s := serve(t, caller)
	s.setIDRange(t, entity.IDRange{Lo: 5000, Hi: 5999})
	a, tag := s.createUser(t, apiv1.UserSpec{Name: "alice", Email: "alice@example.com"}, "c-alice")
	b, _ := s.createUser(t, apiv1.UserSpec{Name: "bob"}, "c-bob")
	for _, u := range []apiv1.User{a, b} {
		if u.Spec.UID < 5000 || u.Spec.UID > 5999 {
			t.Errorf("created %s: uid %d, want one in identity.id_range [5000, 5999]", u.Spec.Name, u.Spec.UID)
		}
		rec, ok := s.userRecord(t, u.ID)
		if !ok || rec.Name != u.Spec.Name || strconv.FormatUint(uint64(u.Spec.UID), 10) != uidOf(rec) {
			t.Errorf("store's %s: %+v, %v; want it holding uid %d", u.Spec.Name, rec, ok, u.Spec.UID)
		}
	}
	if a.Spec.UID == b.Spec.UID {
		t.Errorf("alice and bob both got uid %d, want two", a.Spec.UID)
	}
	if rec, _ := s.userRecord(t, a.ID); rec.Email != "alice@example.com" || tag != `"`+strconv.FormatUint(rec.Generation, 10)+`"` {
		t.Errorf("alice: stored %+v, answered ETag %s; want the address as given and the record's generation", rec, tag)
	}

	again := s.do(t, http.MethodPost, "/api/v1/users", jsonOf(t, map[string]any{"spec": apiv1.UserSpec{Name: "alice", Email: "alice@example.com"}}), "Idempotency-Key", "c-alice")
	if again.status != http.StatusCreated || !strings.Contains(string(again.body), a.ID) {
		t.Errorf("replayed POST /users: answered %d %s, want the first 201 naming %s", again.status, again.body, a.ID)
	}
	for _, tc := range []struct {
		name string
		spec apiv1.UserSpec
		code int
	}{
		{"a uid alice holds", apiv1.UserSpec{Name: "carol", UID: a.Spec.UID}, http.StatusConflict},
		{"an invalid address", apiv1.UserSpec{Name: "dave", Email: "not an address"}, http.StatusBadRequest},
		{"an address with a display name", apiv1.UserSpec{Name: "erin", Email: "Erin <erin@example.com>"}, http.StatusBadRequest},
	} {
		r := s.do(t, http.MethodPost, "/api/v1/users", jsonOf(t, map[string]any{"spec": tc.spec}), "Idempotency-Key", "c-"+tc.spec.Name)
		if r.status != tc.code {
			t.Errorf("POST /users naming %s: answered %d %s, want %d", tc.name, r.status, r.body, tc.code)
		}
	}
	if n := s.userCount(t); n != 2 {
		t.Errorf("users stored after the refusals: %d, want alice and bob alone", n)
	}
}

func uidOf(u entity.User) string {
	for _, id := range u.IDs {
		if id.Scheme == entity.SchemeUID {
			return id.Value
		}
	}
	return ""
}

// A user's PATCH sets disabled and clears its address, read back by its GET
// and from the store; an invalid address in a PATCH is refused and changes
// nothing.
func TestUserEdit(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	u, tag := s.createUser(t, apiv1.UserSpec{Name: "alice", Email: "alice@example.com"}, "c1")
	path := "/api/v1/users/" + u.ID
	if r := s.do(t, http.MethodPatch, path, `{"spec":{"email":"bad"}}`, "If-Match", tag); r.status != http.StatusBadRequest {
		t.Errorf("PATCH an invalid address: answered %d %s, want 400", r.status, r.body)
	}
	r := s.do(t, http.MethodPatch, path, `{"spec":{"disabled":true,"email":""}}`, "If-Match", tag)
	if r.status != http.StatusOK {
		t.Fatalf("PATCH disabled: answered %d %s, want 200", r.status, r.body)
	}
	got := s.do(t, http.MethodGet, path, "")
	var read apiv1.User
	if err := json.Unmarshal(got.body, &read); err != nil || got.status != http.StatusOK {
		t.Fatalf("GET the user: answered %d %s (%v)", got.status, got.body, err)
	}
	if !read.Spec.Disabled || read.Spec.Email != "" || read.Spec.UID != u.Spec.UID || got.header.Get("ETag") != r.header.Get("ETag") {
		t.Errorf("GET the user: %+v ETag %s, want it disabled, with no address, its uid %d kept, ETag %s", read, got.header.Get("ETag"), u.Spec.UID, r.header.Get("ETag"))
	}
	if rec, _ := s.userRecord(t, u.ID); !rec.Disabled || rec.Email != "" {
		t.Errorf("store's user: %+v, want it disabled with no address", rec)
	}
}

// A user's delete is an operation, answered 202 once the user is marked: its
// GET is 404 from then on and the store finds it nowhere. A group's is too.
func TestPrincipalDeleteIsAnOperation(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	u, tag := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	g := s.createGroup(t, apiv1.GroupSpec{Name: "staff"}, "g1")
	for _, tc := range []struct {
		path, etag string
		kind       apiv1.OpKind
	}{
		{"/api/v1/users/" + u.ID, tag, apiv1.OpUserDelete},
		{"/api/v1/groups/" + g.ID, `"` + strconv.FormatUint(g.Generation, 10) + `"`, apiv1.OpGroupDelete},
	} {
		r := s.do(t, http.MethodDelete, tc.path, "", "If-Match", tc.etag, "Idempotency-Key", "d-"+tc.path)
		if r.status != http.StatusAccepted {
			t.Fatalf("DELETE %s: answered %d %s, want 202", tc.path, r.status, r.body)
		}
		var op apiv1.Operation
		if err := json.Unmarshal(r.body, &op); err != nil || op.Kind != tc.kind || r.header.Get("Location") != "/api/v1/operations/"+op.ID {
			t.Errorf("DELETE %s: operation %+v Location %q (%v), want a %s naming its own path", tc.path, op, r.header.Get("Location"), err, tc.kind)
		}
		if got := s.do(t, http.MethodGet, tc.path, ""); got.status != http.StatusNotFound {
			t.Errorf("GET %s after its delete: answered %d %s, want 404", tc.path, got.status, got.body)
		}
	}
	if _, ok := s.userRecord(t, u.ID); ok {
		t.Error("the store still finds alice after her delete's 202")
	}
}

// A group created with no gid gets one in identity.id_range; a member added
// by PUT under If-None-Match: * is a member to the store, a second add is
// refused 412, and a DELETE under the membership's tag removes it.
func TestGroupMembers(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	s.setIDRange(t, entity.IDRange{Lo: 7000, Hi: 7999})
	u, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	g := s.createGroup(t, apiv1.GroupSpec{Name: "staff"}, "g1")
	if g.Spec.GID < 7000 || g.Spec.GID > 7999 {
		t.Errorf("created staff: gid %d, want one in [7000, 7999]", g.Spec.GID)
	}
	path := "/api/v1/groups/" + g.ID + "/members/" + u.ID
	r := s.do(t, http.MethodPut, path, `{"spec":{}}`, "If-None-Match", "*")
	if r.status != http.StatusOK {
		t.Fatalf("PUT the member: answered %d %s, want 200", r.status, r.body)
	}
	if !slices.Contains(s.groupsOf(t, u.ID), g.ID) {
		t.Errorf("alice's groups after the add: %v, want staff %s", s.groupsOf(t, u.ID), g.ID)
	}
	if again := s.do(t, http.MethodPut, path, `{"spec":{}}`, "If-None-Match", "*"); again.status != http.StatusPreconditionFailed {
		t.Errorf("PUT the member again: answered %d %s, want 412", again.status, again.body)
	}
	if d := s.do(t, http.MethodDelete, path, "", "If-Match", r.header.Get("ETag"), "Idempotency-Key", "rm"); d.status != http.StatusOK {
		t.Fatalf("DELETE the member under %s: answered %d %s, want 200", r.header.Get("ETag"), d.status, d.body)
	}
	if got := s.groupsOf(t, u.ID); len(got) != 0 {
		t.Errorf("alice's groups after the removal: %v, want none", got)
	}
}

// groupsOf is the groups the user id names is a member of, as the store's
// resolution reads them.
func (s *served) groupsOf(t *testing.T, id string) []string {
	t.Helper()
	pid, _ := entity.ParsePrincipalID(id)
	var out []string
	for g, err := range s.db.Principals().Groups(t.Context(), entity.Principal{Kind: entity.PrincipalUser, ID: pid}) {
		if err != nil {
			t.Fatalf("groups of %s: %v", id, err)
		}
		out = append(out, uuid.UUID(g.ID).String())
	}
	return out
}

// A none grant is stored and read back as none, apart from no grant, which
// reads 404. A user's grant deleted is gone while its group's stays, so its
// group decides again. A grant naming a machine principal is refused 400
// and writes nothing.
func TestGrants(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	share, loc := s.createShare(t, "builds", s.namespace(t))
	u, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	g := s.createGroup(t, apiv1.GroupSpec{Name: "staff"}, "g1")
	at := func(id string) string { return loc + "/grants/" + id }

	none := s.do(t, http.MethodPut, at(u.ID), `{"spec":{"access":"none"}}`, "If-None-Match", "*")
	if none.status != http.StatusOK {
		t.Fatalf("PUT alice's none grant: answered %d %s, want 200", none.status, none.body)
	}
	if r := s.do(t, http.MethodPut, at(g.ID), `{"spec":{"access":"read"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT staff's read grant: answered %d %s, want 200", r.status, r.body)
	}
	got := s.do(t, http.MethodGet, at(u.ID), "")
	var gr apiv1.Grant
	if err := json.Unmarshal(got.body, &gr); err != nil || got.status != http.StatusOK || gr.Spec.Access != "none" || got.header.Get("ETag") != none.header.Get("ETag") {
		t.Errorf("GET alice's grant: answered %d %s ETag %s, want her none grant under ETag %s", got.status, got.body, got.header.Get("ETag"), none.header.Get("ETag"))
	}
	other, _ := s.createUser(t, apiv1.UserSpec{Name: "bob"}, "c2")
	if r := s.do(t, http.MethodGet, at(other.ID), ""); r.status != http.StatusNotFound {
		t.Errorf("GET bob's grant, never set: answered %d %s, want 404, apart from a none grant", r.status, r.body)
	}

	if r := s.do(t, http.MethodDelete, at(u.ID), "", "If-Match", none.header.Get("ETag"), "Idempotency-Key", "dg"); r.status != http.StatusOK {
		t.Fatalf("DELETE alice's grant: answered %d %s, want 200", r.status, r.body)
	}
	if _, ok := s.grantOf(t, share, u.ID); ok {
		t.Error("the store still holds alice's grant after its DELETE")
	}
	if a, ok := s.grantOf(t, share, g.ID); !ok || a != entity.AccessRead {
		t.Errorf("staff's grant after alice's DELETE: %v, %v; want read, so alice falls through to it", a, ok)
	}

	m := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
	if _, err := s.store.PutUser(t.Context(), entity.User{Principal: m, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/a@EXAMPLE.COM"}}}, 0, entity.Alloc{}); err != nil {
		t.Fatalf("put the machine: %v", err)
	}
	r := s.do(t, http.MethodPut, at(uuid.UUID(m.ID).String()), `{"spec":{"access":"read"}}`, "If-None-Match", "*")
	if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "invalid" {
		t.Errorf("PUT a machine's grant: answered %d %+v, want 400 invalid", r.status, p)
	}
	if _, ok := s.grantOf(t, share, uuid.UUID(m.ID).String()); ok {
		t.Error("the store holds the machine's refused grant")
	}
}

func (s *served) grantOf(t *testing.T, share entity.ShareID, id string) (entity.ShareAccess, bool) {
	t.Helper()
	pid, _ := entity.ParsePrincipalID(id)
	g, ok, err := s.store.ShareGrant(t.Context(), share, entity.Principal{ID: pid})
	if err != nil {
		t.Fatalf("read the grant of %s: %v", id, err)
	}
	return g.Access, ok
}

// A netgroup is created, then replaced whole: its members are the second
// write's alone; its DELETE removes it. A quota set on a share's user is
// what the store holds, and read back by its GET.
func TestNetgroupsAndQuotas(t *testing.T) {
	s := serve(t, as(entity.RoleOperator, false))
	path := "/api/v1/netgroups/labs"
	c := s.do(t, http.MethodPut, path, `{"spec":{"members":["10.0.0.0/8","build-1"]}}`, "If-None-Match", "*")
	if c.status != http.StatusOK {
		t.Fatalf("PUT the netgroup: answered %d %s, want 200", c.status, c.body)
	}
	r := s.do(t, http.MethodPut, path, `{"spec":{"members":["build-2"]}}`, "If-Match", c.header.Get("ETag"))
	if r.status != http.StatusOK {
		t.Fatalf("PUT the netgroup again: answered %d %s, want 200", r.status, r.body)
	}
	if g, ok, err := s.store.Netgroup(t.Context(), "labs"); err != nil || !ok || !slices.Equal(g.Members, []string{"build-2"}) {
		t.Errorf("store's labs: %+v, %v, %v; want build-2 alone", g, ok, err)
	}
	list := s.do(t, http.MethodGet, "/api/v1/netgroups", "")
	var page apiv1.Page[apiv1.Netgroup]
	if err := json.Unmarshal(list.body, &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != "labs" {
		t.Errorf("GET /netgroups: answered %d %s, want labs alone", list.status, list.body)
	}
	if d := s.do(t, http.MethodDelete, path, "", "If-Match", r.header.Get("ETag"), "Idempotency-Key", "dn"); d.status != http.StatusOK {
		t.Fatalf("DELETE the netgroup: answered %d %s, want 200", d.status, d.body)
	}
	if _, ok, _ := s.store.Netgroup(t.Context(), "labs"); ok {
		t.Error("the store still holds labs after its DELETE")
	}

	share, loc := s.createShare(t, "builds", s.namespace(t))
	u, _ := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	spec := apiv1.QuotaSpec{Hard: apiv1.Limit{Bytes: 1 << 30}, Soft: apiv1.Limit{Files: 100}, GraceSeconds: 3600}
	q := s.do(t, http.MethodPut, loc+"/quotas/"+u.ID, jsonOf(t, map[string]any{"spec": spec}), "If-None-Match", "*")
	if q.status != http.StatusOK {
		t.Fatalf("PUT alice's quota: answered %d %s, want 200", q.status, q.body)
	}
	var held []entity.Quota
	for x, err := range s.store.Quotas(t.Context(), share) {
		if err != nil {
			t.Fatalf("list quotas: %v", err)
		}
		held = append(held, x)
	}
	if len(held) != 1 || held[0].Hard.Bytes != 1<<30 || held[0].Soft.Files != 100 || held[0].Grace != time.Hour || uuid.UUID(held[0].Principal.ID).String() != u.ID {
		t.Errorf("store's quotas: %+v, want alice's alone, hard 1 GiB, soft 100 files, grace 1 h", held)
	}
	got := s.do(t, http.MethodGet, loc+"/quotas/"+u.ID, "")
	var read apiv1.Quota
	if err := json.Unmarshal(got.body, &read); err != nil || read.Spec != spec || got.header.Get("ETag") != q.header.Get("ETag") {
		t.Errorf("GET alice's quota: answered %d %s ETag %s, want %+v under ETag %s", got.status, got.body, got.header.Get("ETag"), spec, q.header.Get("ETag"))
	}
}

// Each write of a user, a group or a membership raises the principals
// generation, so its answer waits for the node to apply it; a grant, a
// quota and a netgroup raise neither generation and answer at commit.
func TestPrincipalWritesWaitForTheNode(t *testing.T) {
	hold := api.NewHold(nil)
	close(hold.Release) // every wait returns at once; the test reads which writes waited
	s := serve(t, as(entity.RoleOperator, false), func(d *api.Deps) { d.Applied = hold.Applied })
	waited := func(what string, want bool) {
		t.Helper()
		select {
		case <-hold.Reached:
			if !want {
				t.Errorf("%s waited for the node, want it answered at its commit", what)
			}
		default:
			if want {
				t.Errorf("%s answered without waiting for the node, want a wait after its commit", what)
			}
		}
	}
	u, tag := s.createUser(t, apiv1.UserSpec{Name: "alice"}, "c1")
	waited("POST /users", true)
	if r := s.do(t, http.MethodPatch, "/api/v1/users/"+u.ID, `{"spec":{"disabled":true}}`, "If-Match", tag); r.status != http.StatusOK {
		t.Fatalf("PATCH the user: answered %d %s", r.status, r.body)
	}
	waited("PATCH /users/{id}", true)
	g := s.createGroup(t, apiv1.GroupSpec{Name: "staff"}, "g1")
	waited("POST /groups", true)
	if r := s.do(t, http.MethodPut, "/api/v1/groups/"+g.ID+"/members/"+u.ID, `{"spec":{}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT the member: answered %d %s", r.status, r.body)
	}
	waited("PUT /groups/{id}/members/{sub}", true)
	_, loc := s.createShare(t, "builds", s.namespace(t))
	if r := s.do(t, http.MethodPut, loc+"/grants/"+u.ID, `{"spec":{"access":"read"}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT the grant: answered %d %s", r.status, r.body)
	}
	waited("PUT /shares/{id}/grants/{sub}", false)
	if r := s.do(t, http.MethodPut, loc+"/quotas/"+u.ID, `{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT the quota: answered %d %s", r.status, r.body)
	}
	waited("PUT /shares/{id}/quotas/{sub}", false)
	if r := s.do(t, http.MethodPut, "/api/v1/netgroups/labs", `{"spec":{"members":["a"]}}`, "If-None-Match", "*"); r.status != http.StatusOK {
		t.Fatalf("PUT the netgroup: answered %d %s", r.status, r.body)
	}
	waited("PUT /netgroups/{sub}", false)
}
