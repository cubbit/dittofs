package node_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// roleRefusals is how many calls the node has refused by their caller's
// role.
func (s *authNode) roleRefusals(t *testing.T, reason string) float64 {
	t.Helper()
	return seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{reason})
}

// user reads the user id names as token's caller, wanting 200, and returns
// it with its ETag.
func (s *authNode) user(t *testing.T, token string, id entity.Principal) (apiv1.User, string) {
	t.Helper()
	r := s.send(t, http.MethodGet, "/api/v1/users/"+uuid.UUID(id.ID).String(), token, nil)
	var u apiv1.User
	if r.status != http.StatusOK || json.Unmarshal(r.body, &u) != nil {
		t.Fatalf("GET user %s: answered %d %s, want 200", uuid.UUID(id.ID), r.status, r.body)
	}
	return u, r.header.Get("ETag")
}

// patchUser PATCHes the user id names with patch under etag, as token's
// caller.
func (s *authNode) patchUser(t *testing.T, token string, id entity.Principal, etag, patch string) authReply {
	t.Helper()
	return s.send(t, http.MethodPatch, "/api/v1/users/"+uuid.UUID(id.ID).String(), token, json.RawMessage(patch), "If-Match", etag)
}

// stored is p's record as the store holds it.
func (s *authNode) stored(t *testing.T, p entity.Principal) entity.User {
	t.Helper()
	u, ok, err := s.h.Store().ControlPlane().User(t.Context(), p)
	if err != nil || !ok {
		t.Fatalf("read %v: %v, %v", p, ok, err)
	}
	return u
}

// routeOp is one operation of the API's OpenAPI document: its method, its
// path and the least role that may make it.
type routeOp struct{ method, path, role string }

// documented is every operation the checked-in OpenAPI document describes,
// sign-out last, since it ends the session its caller presents.
func documented(t *testing.T) []routeOp {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "pkg", "node", "apiv1", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var ops []routeOp
	for path, methods := range doc.Paths {
		for method, raw := range methods {
			var op struct {
				Role string `json:"x-dittofs-role"`
			}
			if json.Unmarshal(raw, &op) == nil && op.Role != "" {
				ops = append(ops, routeOp{strings.ToUpper(method), path, op.Role})
			}
		}
	}
	slices.SortFunc(ops, func(a, b routeOp) int {
		out := func(o routeOp) bool { return o.path == "/api/v1/auth/sign-out" }
		switch {
		case out(a) != out(b):
			if out(a) {
				return 1
			}
			return -1
		case a.path != b.path:
			return strings.Compare(a.path, b.path)
		}
		return strings.Compare(a.method, b.method)
	})
	if len(ops) < 50 {
		t.Fatalf("the OpenAPI document describes %d operations; want the whole route table", len(ops))
	}
	return ops
}

// Every route is called as each role, a signed-in account of that role,
// and answered as RFC 23 §5.2's role table says: a caller below the least
// role a route needs is refused, 404 where its role may not read what the
// route reaches, as the audit, and 403 forbidden where it may read it, each
// counted as a refused authentication; a caller holding the role is not
// refused by it, whatever else the call's empty body and made-up IDs
// answer.
func TestEveryRouteByRole(t *testing.T) {
	s := newAuthNode(t)
	order := map[string]entity.APIRole{"none": entity.RoleNone, "viewer": entity.RoleViewer, "operator": entity.RoleOperator, "admin": entity.RoleAdmin}
	// The rights whose resources a role below them may not read, from the
	// role table: everything else every role reads.
	reads := map[string]entity.APIRole{"/api/v1/audit": entity.RoleAdmin}
	ops := documented(t)
	for _, role := range []entity.APIRole{entity.RoleViewer, entity.RoleOperator, entity.RoleAdmin} {
		_, tk := s.h.SignedIn(t, s.addr, "caller-"+role.String(), role)
		for _, op := range ops {
			need, ok := order[op.role]
			if !ok {
				t.Fatalf("%s %s: the document names role %q", op.method, op.path, op.role)
			}
			if need == entity.RoleNone {
				continue // public, so no role is refused it
			}
			path := strings.NewReplacer("{id}", uuid.NewString(), "{sub}", "x").Replace(op.path)
			before := s.roleRefusals(t, "role")
			r := s.send(t, op.method, path, tk.Access, nil)
			counted := s.roleRefusals(t, "role") - before
			what := role.String() + " " + op.method + " " + op.path
			switch {
			case role >= need:
				if counted != 0 || r.status == http.StatusForbidden && r.problem(t).Code == apiv1.CodeForbidden {
					t.Errorf("%s: answered %d %s, %v refusals by role counted; want no refusal by role", what, r.status, r.body, counted)
				}
			case role < reads[op.path]:
				wantStatus(t, what, r, http.StatusNotFound, codeNotFound)
			default:
				wantStatus(t, what, r, http.StatusForbidden, apiv1.CodeForbidden)
			}
			if role < need && counted != 1 {
				t.Errorf("%s: %v refusals by role counted, want 1", what, counted)
			}
		}
	}
}

// An operator cannot raise privilege: giving itself admin, giving a user it
// may edit a role, creating a user with one, and editing a user that holds
// one are each refused, 404 for a user it may not know of and 403 for a
// role it may not give, and the store holds no change. An admin gives a
// role.
func TestOperatorCannotRaisePrivilege(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	op, tk := s.h.SignedIn(t, s.addr, "op", entity.RoleOperator)
	_, plainTag := s.createUser(t, admin, `{"spec":{"name":"plain"}}`, "k-plain")
	plain := s.principalNamed(t, "plain")
	_, opTag := s.user(t, admin, op)
	before := s.roleRefusals(t, "role")

	wantStatus(t, "the operator giving itself admin", s.patchUser(t, tk.Access, op, opTag, `{"spec":{"role":"admin"}}`), http.StatusNotFound, codeNotFound)
	wantStatus(t, "the operator giving a user a role", s.patchUser(t, tk.Access, plain, plainTag, `{"spec":{"role":"viewer"}}`), http.StatusForbidden, apiv1.CodeForbidden)
	wantStatus(t, "the operator creating a user with a role",
		s.send(t, http.MethodPost, "/api/v1/users", tk.Access, json.RawMessage(`{"spec":{"name":"raised","role":"admin"}}`), "Idempotency-Key", "k-raised"),
		http.StatusForbidden, apiv1.CodeForbidden)
	_, adminTag := s.user(t, admin, s.adminPrincipal(t))
	wantStatus(t, "the operator editing an admin", s.patchUser(t, tk.Access, s.adminPrincipal(t), adminTag, `{"spec":{"email":"x@example.com"}}`), http.StatusNotFound, codeNotFound)
	if got := s.roleRefusals(t, "role") - before; got != 4 {
		t.Errorf("refusals by role counted: %v, want 4", got)
	}
	if u := s.stored(t, op); u.Role != entity.RoleOperator {
		t.Errorf("the operator's role after its refused raise: %v, want operator", u.Role)
	}
	if u := s.stored(t, plain); u.Role != entity.RoleNone {
		t.Errorf("plain's role after the refused grant: %v, want none", u.Role)
	}
	if u := s.stored(t, s.adminPrincipal(t)); u.Email != "" {
		t.Errorf("the admin's address after the refused edit: %q, want none", u.Email)
	}
	if _, found, err := s.h.Store().ControlPlane().UserNamed(t.Context(), "raised"); err != nil || found {
		t.Errorf("the refused create stored a user: %v, %v", found, err)
	}
	if r := s.patchUser(t, admin, plain, plainTag, `{"spec":{"role":"viewer"}}`); r.status != http.StatusOK {
		t.Fatalf("an admin giving plain a role: answered %d %s, want 200", r.status, r.body)
	}
	if u := s.stored(t, plain); u.Role != entity.RoleViewer {
		t.Errorf("plain's role after an admin gave it one: %v, want viewer", u.Role)
	}
}

// createUser POSTs body as token's caller under key, wanting 201, and
// returns the user and its ETag.
func (s *authNode) createUser(t *testing.T, token, body, key string) (apiv1.User, string) {
	t.Helper()
	r := s.send(t, http.MethodPost, "/api/v1/users", token, json.RawMessage(body), "Idempotency-Key", key)
	var u apiv1.User
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &u) != nil {
		t.Fatalf("POST /users %s: answered %d %s, want 201", body, r.status, r.body)
	}
	return u, r.header.Get("ETag")
}

// principalNamed is the user principal of name.
func (s *authNode) principalNamed(t *testing.T, name string) entity.Principal {
	t.Helper()
	u, found, err := s.h.Store().ControlPlane().UserNamed(t.Context(), name)
	if err != nil || !found {
		t.Fatalf("user %s: %v, %v", name, found, err)
	}
	return u.Principal
}

// holdAdmins holds each of the first two transactions that count the
// enabled admins once it has read them, until release is closed, and
// signals reached once for each.
type holdAdmins struct {
	mu      sync.Mutex
	held    int
	reached chan struct{}
	release chan struct{}
}

func (f *holdAdmins) At(op string, key []byte) (time.Duration, int, error) {
	if op != kvtest.OpScan || string(key) != keys.Describe([]byte{keys.KindUA}) {
		return 0, 0, nil
	}
	f.mu.Lock()
	hold := f.held < 2
	f.held++
	f.mu.Unlock()
	if hold {
		f.reached <- struct{}{}
		<-f.release
	}
	return 0, 0, nil
}

// Two demotions of the last two enabled admins, each held once it has read
// the enabled admins until both have: one commits, and the other, counting
// again after its conflict, is refused 409 last_admin, so one enabled admin
// stays.
func TestConcurrentDemotionsKeepOneAdmin(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	admin := s.tokens(t, "admin", adminPassword).Access
	second := s.account(t, "second", "second's password", entity.RoleAdmin)
	targets := []entity.Principal{s.adminPrincipal(t), second}
	tags := make([]string, len(targets))
	for i, p := range targets {
		_, tags[i] = s.user(t, admin, p)
	}
	f := &holdAdmins{reached: make(chan struct{}), release: make(chan struct{})}
	fault.SetFault(f)
	replies := make([]authReply, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		wg.Go(func() { replies[i] = s.patchUser(t, admin, p, tags[i], `{"spec":{"role":"operator"}}`) })
	}
	for range targets {
		select {
		case <-f.reached:
		case <-time.After(time.Minute):
			t.Fatal("the two demotions did not both count the enabled admins")
		}
	}
	close(f.release)
	wg.Wait()
	var ok, refused int
	for i, r := range replies {
		switch {
		case r.status == http.StatusOK:
			ok++
		case r.status == http.StatusConflict && r.problem(t).Code == apiv1.CodeLastAdmin:
			refused++
		default:
			t.Errorf("demotion %d: answered %d %s, want 200 or 409 last_admin", i, r.status, r.body)
		}
	}
	if ok != 1 || refused != 1 {
		t.Errorf("of the two demotions %d committed and %d were refused last_admin; want one each", ok, refused)
	}
	n := 0
	for _, p := range targets {
		if u := s.stored(t, p); u.Role == entity.RoleAdmin && !u.Disabled {
			n++
		}
	}
	if n != 1 {
		t.Errorf("enabled admins after the demotions: %d, want 1", n)
	}
}

// A machine principal holds no role: the store refuses one written on it as
// an invalid argument, which the API answers 400, and the API's user routes
// do not reach a machine, so a PATCH naming its ID is 404 and changes it not.
func TestMachineHoldsNoRole(t *testing.T) {
	s := newAuthNode(t)
	m := entity.User{Principal: entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()},
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host$@CORP.EXAMPLE"}}}
	s.write(t, func(tx entity.ControlPlaneTx) error {
		_, err := tx.PutUser(m, 0, entity.Alloc{})
		return err
	})
	err := s.h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		u, _, err := tx.User(m.Principal)
		if err != nil {
			return err
		}
		u.Role = entity.RoleOperator
		_, err = tx.PutUser(u, u.Generation, entity.Alloc{})
		return err
	})
	var invalid *merrs.StoreError
	if !errors.As(err, &invalid) || invalid.Code != merrs.ErrInvalidArgument {
		t.Errorf("a role written on a machine: %v, want the store's invalid argument", err)
	}
	admin := s.tokens(t, "admin", adminPassword).Access
	wantStatus(t, "a PATCH giving a machine's ID a role", s.patchUser(t, admin, m.Principal, `"1"`, `{"spec":{"role":"operator"}}`), http.StatusNotFound, codeNotFound)
	if u := s.stored(t, m.Principal); u.Role != entity.RoleNone {
		t.Errorf("the machine's role: %v, want none", u.Role)
	}
}

// Disabling an account refuses its next call with its access token
// unexpired, and so does revoking its session: the clock is not advanced,
// so neither waits for the token's expiry.
func TestDisabledAndRevokedRefusedAtOnce(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	viewer, tk := s.h.SignedIn(t, s.addr, "viewer", entity.RoleViewer)
	_, other := s.h.SignedIn(t, s.addr, "other", entity.RoleViewer)
	for _, token := range []string{tk.Access, other.Access} {
		if r := s.list(t, token); r.status != http.StatusOK {
			t.Fatalf("a call before the change: answered %d %s, want 200", r.status, r.body)
		}
	}
	_, tag := s.user(t, admin, viewer)
	if r := s.patchUser(t, admin, viewer, tag, `{"spec":{"disabled":true}}`); r.status != http.StatusOK {
		t.Fatalf("disable the viewer: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "the disabled account's next call", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)

	for sess, err := range s.h.Store().ControlPlane().Sessions(t.Context(), s.principalNamed(t, "other")) {
		if err != nil {
			t.Fatal(err)
		}
		sess.Revoked = true
		s.write(t, func(tx entity.ControlPlaneTx) error { return tx.UpdateSession(sess) })
	}
	wantStatus(t, "the revoked session's next call", s.list(t, other.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	for reason, want := range map[string]float64{"account": 1, "session": 1} {
		if got := s.roleRefusals(t, reason); got != want {
			t.Errorf("auth failures for %s: %v, want %v", reason, got, want)
		}
	}
}

// A demotion raises no not-before: the demoted account's access token and
// refresh token still serve it, now with the new role's rights.
func TestDemotionKeepsSessions(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	op, tk := s.h.SignedIn(t, s.addr, "op", entity.RoleOperator)
	share := `{"spec":{"name":"s1"}}`
	before := s.send(t, http.MethodPost, "/api/v1/shares", tk.Access, json.RawMessage(share), "Idempotency-Key", "k-before")
	if before.status == http.StatusForbidden {
		t.Fatalf("the operator's share create before the demotion: answered %d %s, want no refusal by role", before.status, before.body)
	}
	_, tag := s.user(t, admin, op)
	if r := s.patchUser(t, admin, op, tag, `{"spec":{"role":"viewer"}}`); r.status != http.StatusOK {
		t.Fatalf("demote the operator: answered %d %s, want 200", r.status, r.body)
	}
	if r := s.list(t, tk.Access); r.status != http.StatusOK {
		t.Errorf("a read with the demoted account's token: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "a share create with the demoted account's token",
		s.send(t, http.MethodPost, "/api/v1/shares", tk.Access, json.RawMessage(share), "Idempotency-Key", "k-after"), http.StatusForbidden, apiv1.CodeForbidden)
	refreshed := s.decodeTokens(t, "a refresh after the demotion", s.refresh(t, tk.Refresh))
	if r := s.list(t, refreshed.Access); r.status != http.StatusOK {
		t.Errorf("a read with the refreshed token: answered %d %s, want 200", r.status, r.body)
	}
}

// Until a must-change password is changed, its session is refused 403
// password_change_required on every route but the ones that admit it, sign-out
// among them, and each refusal is counted.
func TestMustChangeRefusesOtherRoutes(t *testing.T) {
	s := newAuthNode(t)
	p := s.account(t, "fresh", "first password", entity.RoleAdmin)
	s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.MustChange = true
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	tk := s.tokens(t, "fresh", "first password")
	if !tk.MustChange {
		t.Error("the tokens of a must-change account say nothing of it")
	}
	wantStatus(t, "a listing", s.list(t, tk.Access), http.StatusForbidden, apiv1.CodePasswordChangeRequired)
	wantStatus(t, "a create", s.send(t, http.MethodPost, "/api/v1/users", tk.Access, json.RawMessage(`{"spec":{"name":"x"}}`), "Idempotency-Key", "k1"),
		http.StatusForbidden, apiv1.CodePasswordChangeRequired)
	if got := s.roleRefusals(t, "must_change"); got != 2 {
		t.Errorf("auth failures for must_change: %v, want 2", got)
	}
	if r := s.send(t, http.MethodPost, "/api/v1/auth/sign-out", tk.Access, nil); r.status >= 300 {
		t.Errorf("sign-out: answered %d %s, want it admitted", r.status, r.body)
	}
}

// A call refused after authentication is audited, naming its caller and its
// refusal, and counted: a viewer's create refused 403, and its read of the
// audit, which it may not know of, 404.
func TestRefusedCallAuditedAndCounted(t *testing.T) {
	s := newAuthNode(t)
	viewer, tk := s.h.SignedIn(t, s.addr, "viewer", entity.RoleViewer)
	n := len(s.audits(t))
	wantStatus(t, "a viewer's share create", s.send(t, http.MethodPost, "/api/v1/shares", tk.Access, json.RawMessage(`{"spec":{"name":"s"}}`), "Idempotency-Key", "k"),
		http.StatusForbidden, apiv1.CodeForbidden)
	wantStatus(t, "a viewer's audit read", s.send(t, http.MethodGet, "/api/v1/audit", tk.Access, nil), http.StatusNotFound, codeNotFound)
	recs := s.audits(t)[n:]
	var got []string
	for _, a := range recs {
		if a.Actor == viewer && a.ActorName == "viewer" {
			got = append(got, a.Target.Kind.String()+" "+string(a.Outcome))
		}
	}
	want := []string{entity.ResourceShare.String() + " refused: forbidden", entity.ResourceInstallation.String() + " refused: not_found"}
	if !slices.Equal(got, want) {
		t.Errorf("the viewer's audit records: %q, want %q", got, want)
	}
	if got := s.roleRefusals(t, "role"); got != 2 {
		t.Errorf("auth failures for role: %v, want 2", got)
	}
}

// An account created with a role and no spelling is API-only: it holds no
// uid and no SID, where a user created with neither role nor spelling is
// allocated both, and one created with a role and a uid keeps the uid and is
// allocated a SID. A role that is none of the three is refused 400.
func TestAPIOnlyAccountHoldsNoSpelling(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	schemes := func(name string) []entity.ProtocolScheme {
		var out []entity.ProtocolScheme
		for _, id := range s.stored(t, s.principalNamed(t, name)).IDs {
			out = append(out, id.Scheme)
		}
		slices.Sort(out)
		return out
	}
	u, _ := s.createUser(t, admin, `{"spec":{"name":"ci","role":"operator"}}`, "k-ci")
	if u.Spec.Role != apiv1.RoleOperator || u.Spec.UID != nil {
		t.Errorf("the API-only account's answer: %+v, want role operator and no uid", u.Spec)
	}
	if got := schemes("ci"); len(got) != 0 {
		t.Errorf("the API-only account's spellings: %v, want none", got)
	}
	s.createUser(t, admin, `{"spec":{"name":"plain"}}`, "k-plain")
	s.createUser(t, admin, `{"spec":{"name":"both","uid":5000,"role":"viewer"}}`, "k-both")
	for _, role := range []string{"none", "root"} {
		r := s.send(t, http.MethodPost, "/api/v1/users", admin, json.RawMessage(`{"spec":{"name":"bad","role":"`+role+`"}}`), "Idempotency-Key", "k-"+role)
		if r.status != http.StatusBadRequest || r.problem(t).Field != "spec.role" {
			t.Errorf("a create with role %q: answered %d %s, want 400 naming spec.role", role, r.status, r.body)
		}
	}
	for name, want := range map[string][]entity.ProtocolScheme{
		"plain": {entity.SchemeUID, entity.SchemeSID},
		"both":  {entity.SchemeUID, entity.SchemeSID},
	} {
		w := slices.Clone(want)
		slices.Sort(w)
		if got := schemes(name); !slices.Equal(got, w) {
			t.Errorf("%s's spellings: %v, want %v", name, got, w)
		}
	}
}

// codeNotFound is the code a resource that does not exist, or that its
// caller may not know of, is answered with.
const codeNotFound apiv1.Code = "not_found"
