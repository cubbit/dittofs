package node_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// codeNotFound is the code a resource that does not exist, or that its
// caller may not know of, is answered with.
const codeNotFound apiv1.Code = "not_found"

const (
	signOut        = "POST /api/v1/auth/sign-out"
	changePassword = "POST /api/v1/auth/password"
)

// leastRole is the least role each route of the API admits, keyed by its
// method and its path as the OpenAPI document spells them, a verb after the
// path: RFC 23 §5.2's role table and §3.5's actions table, copied here
// rather than read from the route table, so a row that declares the wrong
// right fails. A route that is not here fails the test, so each new one
// takes a decision.
var leastRole = func() map[string]entity.APIRole {
	byRole := map[entity.APIRole][]string{
		// Public: a version, and how a caller gets a token.
		entity.RoleNone: {"GET /api/version", "POST /api/v1/auth/sign-in", "POST /api/v1/auth/refresh"},
		// Every resource but accounts, tokens and the audit; health detail,
		// operations, node status, a node's connections; signing out,
		// changing one's own password and one's own API tokens, another
		// account's being an admin's, which the handler checks. An
		// operation's :cancel asks the role that starts its kind, which its
		// handler checks, so the row itself admits a viewer.
		entity.RoleViewer: {
			"GET /api/v1/adapters/{sub}", "GET /api/v1/groups", "GET /api/v1/groups/{id}", "GET /api/v1/health",
			"GET /api/v1/installation", "GET /api/v1/namespaces", "GET /api/v1/namespaces/{id}",
			"GET /api/v1/namespaces/{id}/settings/{sub}", "GET /api/v1/netgroups", "GET /api/v1/netgroups/{sub}",
			"GET /api/v1/nodes", "GET /api/v1/nodes/{id}", "GET /api/v1/nodes/{id}/connections",
			"GET /api/v1/nodes/{id}/settings/{sub}", "GET /api/v1/operations", "GET /api/v1/operations/{id}",
			"POST /api/v1/operations/{id}:cancel", "GET /api/v1/settings/{sub}", "GET /api/v1/shares",
			"GET /api/v1/shares/{id}", "GET /api/v1/shares/{id}/export-policy", "GET /api/v1/shares/{id}/grants",
			"GET /api/v1/shares/{id}/grants/{sub}", "GET /api/v1/shares/{id}/quota", "GET /api/v1/shares/{id}/quotas",
			"GET /api/v1/shares/{id}/quotas/{sub}", "GET /api/v1/shares/{id}/settings/{sub}", "GET /api/v1/stores/{id}",
			"GET /api/v1/stores/{id}/settings/{sub}", "GET /api/v1/users", "GET /api/v1/users/{id}", signOut,
			changePassword, "GET /api/v1/users/{id}/api-tokens", "POST /api/v1/users/{id}/api-tokens",
			"DELETE /api/v1/users/{id}/api-tokens/{sub}",
		},
		// Shares and what hangs off them; stores, namespaces, adapters,
		// settings, netgroups and the secrets they name; users and groups
		// that hold no role, their passwords included; resolve-principal.
		entity.RoleOperator: {
			"PUT /api/v1/adapters/{sub}", "POST /api/v1/groups", "DELETE /api/v1/groups/{id}", "PATCH /api/v1/groups/{id}",
			"PUT /api/v1/groups/{id}", "DELETE /api/v1/groups/{id}/members/{sub}", "PUT /api/v1/groups/{id}/members/{sub}",
			"POST /api/v1/installation:resolve-principal", "POST /api/v1/namespaces", "DELETE /api/v1/namespaces/{id}",
			"DELETE /api/v1/namespaces/{id}/settings/{sub}", "PUT /api/v1/namespaces/{id}/settings/{sub}",
			"DELETE /api/v1/netgroups/{sub}", "PUT /api/v1/netgroups/{sub}", "DELETE /api/v1/nodes/{id}/settings/{sub}",
			"PUT /api/v1/nodes/{id}/settings/{sub}", "DELETE /api/v1/settings/{sub}", "PUT /api/v1/settings/{sub}",
			"POST /api/v1/shares", "DELETE /api/v1/shares/{id}", "PATCH /api/v1/shares/{id}",
			"PATCH /api/v1/shares/{id}/export-policy", "PUT /api/v1/shares/{id}/export-policy",
			"DELETE /api/v1/shares/{id}/grants/{sub}", "PUT /api/v1/shares/{id}/grants/{sub}", "PUT /api/v1/shares/{id}/quota",
			"PUT /api/v1/shares/{id}/quotas/{sub}", "DELETE /api/v1/shares/{id}/settings/{sub}",
			"PUT /api/v1/shares/{id}/settings/{sub}", "POST /api/v1/shares/{id}:quiesce", "POST /api/v1/shares/{id}:resume",
			"POST /api/v1/stores", "DELETE /api/v1/stores/{id}", "DELETE /api/v1/stores/{id}/credential",
			"PUT /api/v1/stores/{id}/credential", "DELETE /api/v1/stores/{id}/settings/{sub}",
			"PUT /api/v1/stores/{id}/settings/{sub}", "POST /api/v1/users", "DELETE /api/v1/users/{id}",
			"PATCH /api/v1/users/{id}", "PUT /api/v1/users/{id}", "POST /api/v1/users/{id}:set-password",
		},
		// The audit; the keytab, the installation's right (§3.5); and the
		// directory's actions but resolve-principal.
		entity.RoleAdmin: {
			"GET /api/v1/audit", "DELETE /api/v1/installation/keytab", "PUT /api/v1/installation/keytab",
			"POST /api/v1/installation:join-domain", "POST /api/v1/installation:rotate-machine-password",
			"POST /api/v1/installation:test-directory",
		},
	}
	out := map[string]entity.APIRole{}
	for role, routes := range byRole {
		for _, r := range routes {
			out[r] = role
		}
	}
	return out
}()

// unreadable is the least role that may know of what a route reaches, for
// the routes a role below it is answered 404 on: the audit, an admin's.
var unreadable = map[string]entity.APIRole{"GET /api/v1/audit": entity.RoleAdmin}

// routeOp is one operation of the API's OpenAPI document: its method and
// path, the least role the document says it admits, and whether the role it
// needs is instead the one that starts the kind of operation it names.
type routeOp struct {
	method, path string
	role         entity.APIRole
	byKind       bool
}

// actions is RFC 23 §4.1's table of actions: the resource each verb acts on,
// and whether its role is the one that starts its operation's kind rather
// than a role of its own.
var actions = map[string]struct {
	resource string
	byKind   bool
}{
	"quiesce": {"shares", false}, "resume": {"shares", false}, "rehome": {"shares", false},
	"accept-copy": {"namespaces", false}, "rebuild-index": {"namespaces", false},
	"rebuild-reverse-index": {"shares", false}, "raise-format": {"installation", false},
	"rotate-signing-key": {"installation", false}, "reload-provisioning": {"nodes", false},
	"set-password": {"users", false}, "drain": {"nodes", false}, "decommission": {"nodes", false},
	"acknowledge-unreadable": {"nodes", false}, "declare-lost": {"backup-locations", false},
	"resolve-principal": {"installation", false}, "test-directory": {"installation", false},
	"join-domain": {"installation", false}, "rotate-machine-password": {"installation", false},
	"cancel": {"operations", true},
}

func (o routeOp) key() string { return o.method + " " + o.path }

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
				Role   string `json:"x-dittofs-role"`
				ByKind bool   `json:"x-dittofs-role-by-kind"`
			}
			if json.Unmarshal(raw, &op) != nil || op.Role == "" {
				continue
			}
			o := routeOp{method: strings.ToUpper(method), path: path, byKind: op.ByKind}
			if err := o.role.UnmarshalText([]byte(op.Role)); err != nil {
				t.Fatalf("%s: the document names role %q: %v", o.key(), op.Role, err)
			}
			ops = append(ops, o)
		}
	}
	sortKey := func(o routeOp) string { return strconv.FormatBool(o.key() == signOut) + o.key() }
	slices.SortFunc(ops, func(a, b routeOp) int { return strings.Compare(sortKey(a), sortKey(b)) })
	return ops
}

// concrete is path with a made-up ID for {id} and a made-up {sub}.
func concrete(path string) string {
	return strings.NewReplacer("{id}", uuid.NewString(), "{sub}", "x").Replace(path)
}

// The document says each route admits the least role RFC 23 §5.2 gives it,
// every documented route has one, and every route there is documented; each
// action is a row of §4.1's table, on that row's resource, its role the
// operation kind's exactly where the row says so. TestWiredRoleSweep calls
// every route as each role.
func TestEveryRouteByRole(t *testing.T) {
	ops := documented(t)
	seen := map[string]bool{}
	for _, op := range ops {
		seen[op.key()] = true
		want, ok := leastRole[op.key()]
		switch {
		case !ok:
			t.Errorf("%s is documented and has no least role in this test: decide it from RFC 23 §5.2", op.key())
		case op.role != want:
			t.Errorf("%s: the document says %v may call it, RFC 23 §5.2 says %v", op.key(), op.role, want)
		}
		_, verb, isAction := strings.Cut(op.path, ":")
		coll, _, _ := strings.Cut(strings.TrimPrefix(op.path, "/api/v1/"), "/")
		coll, _, _ = strings.Cut(coll, ":")
		a, known := actions[verb]
		switch {
		case !isAction && op.byKind:
			t.Errorf("%s: its role is its operation kind's, and it is no action", op.key())
		case !isAction:
		case !known:
			t.Errorf("%s: action %s is not in RFC 23 §4.1's table", op.key(), verb)
		case a.resource != coll:
			t.Errorf("%s: action %s acts on %s, RFC 23 §4.1 puts it on %s", op.key(), verb, coll, a.resource)
		case a.byKind != op.byKind:
			t.Errorf("%s: its role by operation kind %v, RFC 23 §4.1 says %v", op.key(), op.byKind, a.byKind)
		}
	}
	for k := range leastRole {
		if !seen[k] {
			t.Errorf("%s has a least role in this test and is not documented", k)
		}
	}
}

// userPath is the path of the user p.
func userPath(p entity.Principal) string { return "/api/v1/users/" + uuid.UUID(p.ID).String() }

// etag reads the user p as token's caller, wanting 200, and returns its
// ETag.
func (s *authNode) etag(t *testing.T, token string, p entity.Principal) string {
	t.Helper()
	r := s.send(t, http.MethodGet, userPath(p), token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET %s: answered %d %s, want 200", userPath(p), r.status, r.body)
	}
	return r.header.Get("ETag")
}

// patchUser PATCHes the user p with patch under etag, as token's caller.
func (s *authNode) patchUser(t *testing.T, token string, p entity.Principal, etag, patch string) authReply {
	t.Helper()
	return s.send(t, http.MethodPatch, userPath(p), token, json.RawMessage(patch), "If-Match", etag)
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

// createUser POSTs body as token's caller under key, wanting 201, and
// returns the user's principal and its ETag.
func (s *authNode) createUser(t *testing.T, token, body, key string) (entity.Principal, string) {
	t.Helper()
	r := s.send(t, http.MethodPost, "/api/v1/users", token, json.RawMessage(body), "Idempotency-Key", key)
	var u apiv1.User
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &u) != nil {
		t.Fatalf("POST /users %s: answered %d %s, want 201", body, r.status, r.body)
	}
	id, err := entity.ParsePrincipalID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return entity.Principal{Kind: entity.PrincipalUser, ID: id}, r.header.Get("ETag")
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

// authFailures is the API's count of authentication refused for reason.
func (s *authNode) authFailures(t *testing.T, reason string) float64 {
	t.Helper()
	return seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{reason})
}

// seriesSum is the sum of the counter name over every label it carries.
func seriesSum(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				n += m.GetCounter().GetValue()
			}
		}
	}
	return n
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
	plain, plainTag := s.createUser(t, admin, `{"spec":{"name":"plain"}}`, "k-plain")
	root := s.principalNamed(t, "admin")

	wantStatus(t, "the operator giving itself admin", s.patchUser(t, tk.Access, op, s.etag(t, admin, op), `{"spec":{"role":"admin"}}`), http.StatusNotFound, codeNotFound)
	wantStatus(t, "the operator giving a user a role", s.patchUser(t, tk.Access, plain, plainTag, `{"spec":{"role":"viewer"}}`), http.StatusForbidden, apiv1.CodeForbidden)
	wantStatus(t, "the operator creating a user with a role",
		s.send(t, http.MethodPost, "/api/v1/users", tk.Access, json.RawMessage(`{"spec":{"name":"raised","role":"admin"}}`), "Idempotency-Key", "k-raised"),
		http.StatusForbidden, apiv1.CodeForbidden)
	wantStatus(t, "the operator editing an admin", s.patchUser(t, tk.Access, root, s.etag(t, admin, root), `{"spec":{"email":"x@example.com"}}`), http.StatusNotFound, codeNotFound)
	if u := s.stored(t, op); u.Role != entity.RoleOperator {
		t.Errorf("the operator's role after its refused raise: %v, want operator", u.Role)
	}
	if u := s.stored(t, plain); u.Role != entity.RoleNone {
		t.Errorf("plain's role after the refused grant: %v, want none", u.Role)
	}
	if u := s.stored(t, root); u.Email != "" {
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

// holdAdmins holds each of the first two transactions that count the
// enabled admins once it has read them, until release is closed, and
// signals reached once for each.
type holdAdmins struct {
	held    atomic.Int32
	reached chan struct{}
	release chan struct{}
}

func (f *holdAdmins) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpScan && string(key) == keys.Describe([]byte{keys.KindUA}) && f.held.Add(1) <= 2 {
		f.reached <- struct{}{}
		<-f.release
	}
	return 0, 0, nil
}

// demotion is one demotion's answer.
type demotion struct {
	i     int
	reply authReply
	err   error
}

// Two demotions of the last two enabled admins, each held once it has read
// the enabled admins until both have: one commits, and the other, counting
// again after its conflict, is refused 409 last_admin, so one enabled admin
// stays. A demotion that answers before both have counted fails the test at
// once.
func TestConcurrentDemotionsKeepOneAdmin(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	admin := s.tokens(t, "admin", adminPassword).Access
	second := s.h.Account(t, "second", "second's password", entity.RoleAdmin)
	targets := []entity.Principal{s.principalNamed(t, "admin"), second}
	tags := make([]string, len(targets))
	for i, p := range targets {
		tags[i] = s.etag(t, admin, p)
	}
	f := &holdAdmins{reached: make(chan struct{}), release: make(chan struct{})}
	fault.SetFault(f)
	answered := make(chan demotion, len(targets))
	var wg sync.WaitGroup
	var once sync.Once
	release := func() { once.Do(func() { close(f.release) }) }
	t.Cleanup(func() { release(); wg.Wait() })
	for i, p := range targets {
		wg.Go(func() {
			r, err := s.try(t.Context(), http.MethodPatch, userPath(p), admin, json.RawMessage(`{"spec":{"role":"operator"}}`), "If-Match", tags[i])
			answered <- demotion{i, r, err}
		})
	}
	for range targets {
		select {
		case <-f.reached:
		case d := <-answered:
			t.Fatalf("demotion %d answered %d %s (%v) before both demotions counted the enabled admins", d.i, d.reply.status, d.reply.body, d.err)
		case <-time.After(time.Minute):
			t.Fatal("the two demotions did not both count the enabled admins")
		}
	}
	release()
	var ok, refused int
	for range targets {
		d := <-answered
		switch {
		case d.err != nil:
			t.Errorf("demotion %d: %v", d.i, d.err)
		case d.reply.status == http.StatusOK:
			ok++
		case d.reply.status == http.StatusConflict && d.reply.problem(t).Code == apiv1.CodeLastAdmin:
			refused++
		default:
			t.Errorf("demotion %d: answered %d %s, want 200 or 409 last_admin", d.i, d.reply.status, d.reply.body)
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

// A machine principal holds no role: the store refuses a role written on it
// as an invalid argument, and the API's user routes do not reach a machine,
// so an admin's PATCH naming its ID is 404, audited as a missing user's, and
// changes it not.
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
	n := len(s.audits(t))
	wantStatus(t, "a PATCH giving a machine's ID a role", s.patchUser(t, admin, m.Principal, `"1"`, `{"spec":{"role":"operator"}}`), http.StatusNotFound, codeNotFound)
	var outcomes []string
	for _, a := range s.audits(t)[n:] {
		outcomes = append(outcomes, string(a.Outcome))
	}
	if !slices.Equal(outcomes, []string{"refused: not_found"}) {
		t.Errorf("the PATCH's audit outcomes: %q, want one refused: not_found", outcomes)
	}
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
	other, otherTk := s.h.SignedIn(t, s.addr, "other", entity.RoleViewer)
	for _, token := range []string{tk.Access, otherTk.Access} {
		if r := s.list(t, token); r.status != http.StatusOK {
			t.Fatalf("a call before the change: answered %d %s, want 200", r.status, r.body)
		}
	}
	if r := s.patchUser(t, admin, viewer, s.etag(t, admin, viewer), `{"spec":{"disabled":true}}`); r.status != http.StatusOK {
		t.Fatalf("disable the viewer: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "the disabled account's next call", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)

	for sess, err := range s.h.Store().ControlPlane().Sessions(t.Context(), other) {
		if err != nil {
			t.Fatal(err)
		}
		sess.Revoked = true
		s.write(t, func(tx entity.ControlPlaneTx) error { return tx.UpdateSession(sess) })
	}
	wantStatus(t, "the revoked session's next call", s.list(t, otherTk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	for reason, want := range map[string]float64{"account": 1, "session": 1} {
		if got := s.authFailures(t, reason); got != want {
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
	if r := s.patchUser(t, admin, op, s.etag(t, admin, op), `{"spec":{"role":"viewer"}}`); r.status != http.StatusOK {
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
// password_change_required on every documented route but the public ones,
// the password change and sign-out, which admit it.
func TestMustChangeRefusesOtherRoutes(t *testing.T) {
	s := newAuthNode(t)
	p := s.h.Account(t, "fresh", "first password", entity.RoleAdmin)
	s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.MustChange = true
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	tk := s.tokens(t, "fresh", "first password")
	if !tk.MustChange {
		t.Error("the tokens of a must-change account say nothing of it")
	}
	for _, op := range documented(t) {
		if leastRole[op.key()] == entity.RoleNone || op.key() == signOut || op.key() == changePassword {
			continue
		}
		wantStatus(t, op.key(), s.send(t, op.method, concrete(op.path), tk.Access, nil), http.StatusForbidden, apiv1.CodePasswordChangeRequired)
	}
	if r := s.send(t, http.MethodPost, "/api/v1/auth/sign-out", tk.Access, nil); r.status >= 300 {
		t.Errorf("sign-out: answered %d %s, want it admitted", r.status, r.body)
	}
}

// A call refused after authentication is audited, naming its caller and its
// refusal, and counted among the API's requests by its outcome, never as a
// refused authentication: a viewer's create refused 403, its read of the
// audit, which it may not know of, 404, and its read of an admin, hidden
// from it, 404. Its read of a user that does not exist is 404 too and
// audited by nothing, as no read of a missing record is.
func TestRefusedCallAuditedAndCounted(t *testing.T) {
	s := newAuthNode(t)
	viewer, tk := s.h.SignedIn(t, s.addr, "viewer", entity.RoleViewer)
	reg := s.h.Metrics().Registry()
	failures := seriesSum(t, reg, "dittofs_api_auth_failures_total")
	n := len(s.audits(t))
	wantStatus(t, "a viewer's share create", s.send(t, http.MethodPost, "/api/v1/shares", tk.Access, json.RawMessage(`{"spec":{"name":"s"}}`), "Idempotency-Key", "k"),
		http.StatusForbidden, apiv1.CodeForbidden)
	wantStatus(t, "a viewer's audit read", s.send(t, http.MethodGet, "/api/v1/audit", tk.Access, nil), http.StatusNotFound, codeNotFound)
	wantStatus(t, "a viewer's read of an admin", s.send(t, http.MethodGet, userPath(s.principalNamed(t, "admin")), tk.Access, nil), http.StatusNotFound, codeNotFound)
	wantStatus(t, "a viewer's read of no user", s.send(t, http.MethodGet, "/api/v1/users/"+uuid.NewString(), tk.Access, nil), http.StatusNotFound, codeNotFound)
	var got []string
	for _, a := range s.audits(t)[n:] {
		if a.Actor == viewer && a.ActorName == "viewer" {
			got = append(got, a.Target.Kind.String()+" "+string(a.Outcome))
		}
	}
	want := []string{"share refused: forbidden", "installation refused: not_found", "user refused: not_found"}
	if !slices.Equal(got, want) {
		t.Errorf("the viewer's audit records: %q, want %q", got, want)
	}
	if got := seriesValue(t, reg, "dittofs_api_requests_total", []string{"POST /api/v1/shares", "forbidden"}); got != 1 {
		t.Errorf("requests POST /api/v1/shares answered forbidden: %v, want 1", got)
	}
	if got := seriesSum(t, reg, "dittofs_api_auth_failures_total") - failures; got != 0 {
		t.Errorf("refused authentications counted for the refused calls: %v, want 0", got)
	}
}

// An account created with a role and no spelling is API-only: it holds no
// uid and no SID, and still none after an edit of its address; a user
// created with neither role nor spelling, and one created with a role and a
// uid, each hold a uid and a SID. A role that is none of the three is
// refused 400.
func TestAPIOnlyAccountHoldsNoSpelling(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	schemes := func(p entity.Principal) []entity.ProtocolScheme {
		var out []entity.ProtocolScheme
		for _, id := range s.stored(t, p).IDs {
			out = append(out, id.Scheme)
		}
		slices.Sort(out)
		return out
	}
	ci, tag := s.createUser(t, admin, `{"spec":{"name":"ci","role":"operator"}}`, "k-ci")
	r := s.send(t, http.MethodGet, userPath(ci), admin, nil)
	var u apiv1.User
	if json.Unmarshal(r.body, &u) != nil || u.Spec.Role != apiv1.RoleOperator || u.Spec.UID != nil {
		t.Errorf("the API-only account's read: %d %s, want role operator and no uid", r.status, r.body)
	}
	if got := schemes(ci); len(got) != 0 {
		t.Errorf("the API-only account's spellings: %v, want none", got)
	}
	if r := s.patchUser(t, admin, ci, tag, `{"spec":{"email":"ci@example.com"}}`); r.status != http.StatusOK {
		t.Fatalf("edit the API-only account's address: answered %d %s, want 200", r.status, r.body)
	}
	if got := schemes(ci); len(got) != 0 {
		t.Errorf("the API-only account's spellings after an edit: %v, want none", got)
	}
	plain, _ := s.createUser(t, admin, `{"spec":{"name":"plain"}}`, "k-plain")
	both, _ := s.createUser(t, admin, `{"spec":{"name":"both","uid":5000,"role":"viewer"}}`, "k-both")
	for name, p := range map[string]entity.Principal{"plain": plain, "both": both} {
		if got := schemes(p); !slices.Equal(got, []entity.ProtocolScheme{entity.SchemeUID, entity.SchemeSID}) {
			t.Errorf("%s's spellings: %v, want uid and sid", name, got)
		}
	}
	for _, role := range []string{"none", "root"} {
		r := s.send(t, http.MethodPost, "/api/v1/users", admin, json.RawMessage(`{"spec":{"name":"bad","role":"`+role+`"}}`), "Idempotency-Key", "k-"+role)
		if r.status != http.StatusBadRequest || r.problem(t).Field != "spec.role" {
			t.Errorf("a create with role %q: answered %d %s, want 400 naming spec.role", role, r.status, r.body)
		}
	}
}
