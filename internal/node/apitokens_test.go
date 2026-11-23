package node_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// tokensPath is p's API tokens.
func tokensPath(p entity.Principal) string { return userPath(p) + "/api-tokens" }

// createToken asks, bearing bearer, for an API token of p expiring at
// expires.
func (s *authNode) createToken(t *testing.T, bearer string, p entity.Principal, expires time.Time, hdr ...string) authReply {
	t.Helper()
	return s.send(t, http.MethodPost, tokensPath(p), bearer, apiv1.APITokenSpec{Expires: expires}, hdr...)
}

// newToken is createToken wanting 200, and the token it answers.
func (s *authNode) newToken(t *testing.T, bearer string, p entity.Principal, expires time.Time) apiv1.APIToken {
	t.Helper()
	r := s.createToken(t, bearer, p, expires)
	if r.status != http.StatusOK {
		t.Fatalf("create an API token: answered %d %s, want 200", r.status, r.body)
	}
	var tok apiv1.APIToken
	if err := json.Unmarshal(r.body, &tok); err != nil || tok.Status.Token == "" || tok.ID == "" {
		t.Fatalf("create an API token: answer %s (%v), want its ID and the token", r.body, err)
	}
	return tok
}

// revokeToken deletes p's token id bearing bearer.
func (s *authNode) revokeToken(t *testing.T, bearer string, p entity.Principal, id string) authReply {
	t.Helper()
	return s.send(t, http.MethodDelete, tokensPath(p)+"/"+id, bearer, nil, "Idempotency-Key", uuid.NewString(), "If-Match", `"1"`)
}

// serviceAccount stores an account named name holding role and nothing
// else: no password and no protocol spelling.
func (s *authNode) serviceAccount(t *testing.T, name string, role entity.APIRole) entity.Principal {
	t.Helper()
	u := entity.User{Principal: entity.NewUser(), Name: name, Role: role}
	s.write(t, func(tx entity.ControlPlaneTx) error {
		_, err := tx.PutUser(u, 0, entity.Alloc{})
		return err
	})
	return u.Principal
}

// rawStore is every key and value db holds under prefix, nil for all.
func rawStore(t *testing.T, db kv.KV, prefix []byte) [][]byte {
	t.Helper()
	var out [][]byte
	err := db.View(t.Context(), func(r kv.Reader) error {
		for kvp, err := range r.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			out = append(out, slices.Clone(kvp.Key), slices.Clone(kvp.Value))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func holds(stored [][]byte, b []byte) bool {
	return slices.ContainsFunc(stored, func(v []byte) bool { return bytes.Contains(v, b) })
}

// A service account signs in with no password, and an API token an admin
// creates for it calls the API; it lists among the account's tokens without
// its value, and once revoked it is refused.
func TestAPITokenCreateCallRevoke(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleOperator)
	wantStatus(t, "a password sign-in of the service account", s.signIn(t, "backup", ""), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a password sign-in of the service account", s.signIn(t, "backup", "a guess"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)

	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(30*24*time.Hour))
	if r := s.list(t, tok.Status.Token); r.status != http.StatusOK {
		t.Fatalf("a call with the API token: answered %d %s, want 200", r.status, r.body)
	}
	r := s.send(t, http.MethodGet, tokensPath(svc), tok.Status.Token, nil)
	var page apiv1.Page[apiv1.APIToken]
	if err := json.Unmarshal(r.body, &page); r.status != http.StatusOK || err != nil {
		t.Fatalf("list the account's tokens: answered %d %s (%v), want 200", r.status, r.body, err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != tok.ID || page.Items[0].Status.Token != "" || !page.Items[0].Status.Created.Equal(tok.Status.Created) {
		t.Errorf("the account's tokens: %+v, want the one created, %s, with no token shown", page.Items, tok.ID)
	}
	if strings.Contains(string(r.body), tok.Status.Token) {
		t.Errorf("the listing %s shows the token", r.body)
	}
	if r := s.revokeToken(t, admin, svc, tok.ID); r.status != http.StatusOK {
		t.Fatalf("revoke the token: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "a call with the revoked token", s.list(t, tok.Status.Token), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if got := seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{"api_token"}); got != 1 {
		t.Errorf("auth failures for api_token: %v, want 1", got)
	}
}

// A token is refused once its expiry has passed on the node's clock, and a
// creation asking for an expiry past api.token_max_lifetime, or not in the
// future, is refused 400 and stores nothing.
func TestAPITokenExpiry(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	now := s.h.Clock().Now()
	wantStatus(t, "a token past the maximum lifetime", s.createToken(t, admin, svc, now.Add(366*24*time.Hour)), http.StatusBadRequest, "invalid")
	wantStatus(t, "a token already expired", s.createToken(t, admin, svc, now.Add(-time.Hour)), http.StatusBadRequest, "invalid")
	for tk, err := range s.h.Store().ControlPlane().APITokens(t.Context(), svc, [16]byte{}) {
		t.Errorf("a token stored after the refused creations: %+v, %v", tk, err)
	}
	tok := s.newToken(t, admin, svc, now.Add(time.Hour))
	if r := s.list(t, tok.Status.Token); r.status != http.StatusOK {
		t.Fatalf("a call before the token's expiry: answered %d %s, want 200", r.status, r.body)
	}
	s.h.Clock().Advance(time.Hour)
	wantStatus(t, "a call at the token's expiry", s.list(t, tok.Status.Token), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
}

// A token created before its account's not-before is refused: one raised
// directly, and one a password change raises. A token the account creates
// after the change, in the change's own second, is not refused by it.
func TestAPITokenBeforeNotBefore(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	s.h.Clock().Advance(2 * time.Second)
	storeReached(t, s.h, s.h.Clock().Now())
	s.change(t, svc, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.NotBefore = tx.Now().Truncate(time.Second).Add(time.Second)
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	wantStatus(t, "a call with a token created before the not-before", s.list(t, tok.Status.Token), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if got := seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{"not_before"}); got != 1 {
		t.Errorf("auth failures for not_before: %v, want 1", got)
	}

	pat := s.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	tk := s.tokens(t, "pat", "pat's password")
	own := s.newToken(t, tk.Access, pat, s.h.Clock().Now().Add(time.Hour))
	if r := s.list(t, own.Status.Token); r.status != http.StatusOK {
		t.Fatalf("a call with pat's own token: answered %d %s, want 200", r.status, r.body)
	}
	answered := s.decodeTokens(t, "the change", s.changePassword(t, tk.Access, "pat's password", "pat's next password"))
	wantStatus(t, "a call with a token created before the password change", s.list(t, own.Status.Token), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	after := s.newToken(t, answered.Access, pat, s.h.Clock().Now().Add(time.Hour))
	if r := s.list(t, after.Status.Token); r.status != http.StatusOK {
		t.Errorf("a call with a token created after the change: answered %d %s, want 200", r.status, r.body)
	}
}

// Demoting an account leaves its API token valid, and the token's next call
// is checked against the new role.
func TestAPITokenSurvivesDemotion(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "deployer", entity.RoleOperator)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour)).Status.Token
	operatorCall := func() authReply {
		return s.send(t, http.MethodPost, "/api/v1/groups", tok, map[string]apiv1.GroupSpec{"spec": {Name: "g-" + uuid.NewString()[:8]}}, "Idempotency-Key", uuid.NewString())
	}
	if r := operatorCall(); r.status != http.StatusCreated {
		t.Fatalf("an operator's call as an operator: answered %d %s, want 201", r.status, r.body)
	}
	s.change(t, svc, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.Role = entity.RoleViewer
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	if r := s.list(t, tok); r.status != http.StatusOK {
		t.Errorf("a viewer's call after the demotion: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "an operator's call after the demotion", operatorCall(), http.StatusForbidden, apiv1.CodeForbidden)
}

// An account's tokens are its own and an admin's: another account below
// admin is answered 404 for them, as though the account did not exist; an
// account creates its own; and a user holding no role, whom a token would
// admit to nothing, is given none.
func TestAPITokensOfAnotherAccount(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	_, op := s.h.SignedIn(t, s.addr, "olga", entity.RoleOperator)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	wantStatus(t, "an operator listing another's tokens", s.send(t, http.MethodGet, tokensPath(svc), op.Access, nil), http.StatusNotFound, codeNotFound)
	wantStatus(t, "an operator creating another's token", s.createToken(t, op.Access, svc, s.h.Clock().Now().Add(time.Hour)), http.StatusNotFound, codeNotFound)
	wantStatus(t, "an operator revoking another's token", s.revokeToken(t, op.Access, svc, tok.ID), http.StatusNotFound, codeNotFound)
	if r := s.list(t, tok.Status.Token); r.status != http.StatusOK {
		t.Errorf("a call with the token the operator could not revoke: answered %d %s, want 200", r.status, r.body)
	}
	nobody := s.serviceAccount(t, "files-only", entity.RoleNone)
	wantStatus(t, "a token for a user holding no role", s.createToken(t, admin, nobody, s.h.Clock().Now().Add(time.Hour)), http.StatusBadRequest, "invalid")
}

// An operator revoking another account's token through its own path, the
// token's ID under the operator's account, is refused as a token that
// account does not hold, and the token still answers.
func TestAPITokenRevokedOnlyUnderItsAccount(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	olga, op := s.h.SignedIn(t, s.addr, "olga", entity.RoleOperator)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	wantStatus(t, "an operator revoking another's token under its own account", s.revokeToken(t, op.Access, olga, tok.ID),
		http.StatusPreconditionFailed, apiv1.CodeGenerationMismatch)
	if r := s.list(t, tok.Status.Token); r.status != http.StatusOK {
		t.Errorf("a call with the token after the refused revocation: answered %d %s, want 200", r.status, r.body)
	}
}

// refusedAudits counts the store's audit records with outcome refused:
// invalid targeting kind.
func (s *authNode) refusedAudits(t *testing.T, kind entity.ResourceKind) int {
	t.Helper()
	n := 0
	for _, a := range s.audits(t) {
		if a.Target.Kind == kind && string(a.Outcome) == "refused: invalid" {
			n++
		}
	}
	return n
}

// A creation is refused 400 invalid, audited, and stores nothing: presented
// with an API token, so a token mints no successor; by an admin for another
// account that holds a password, or a protocol spelling, which is no
// service account; and for a disabled account. The account itself, signed in, creates its own.
func TestAPITokenCreationRefusals(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	pat := s.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	off := s.serviceAccount(t, "off", entity.RoleViewer)
	s.change(t, off, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.Disabled = true
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	// carol signs in through the directory: no password, but a Kerberos
	// spelling, so no service account.
	carol := s.serviceAccount(t, "carol", entity.RoleAdmin)
	s.change(t, carol, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.IDs = entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "carol@EXAMPLE.COM"}}
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	at := s.h.Clock().Now().Add(time.Hour)
	for _, tc := range []struct {
		what, bearer string
		p            entity.Principal
	}{
		{"a creation presented with an API token", tok.Status.Token, svc},
		{"an admin creating a token for an account holding a password", admin, pat},
		{"a token for a disabled account", admin, off},
		{"an admin creating a token for an account with a protocol spelling and no password", admin, carol},
	} {
		before := s.refusedAudits(t, entity.ResourceAPIToken)
		wantStatus(t, tc.what, s.createToken(t, tc.bearer, tc.p, at), http.StatusBadRequest, "invalid")
		if got := s.refusedAudits(t, entity.ResourceAPIToken) - before; got != 1 {
			t.Errorf("%s: %d refused audit records, want 1", tc.what, got)
		}
	}
	for _, p := range []entity.Principal{pat, off, carol} {
		for tk, err := range s.h.Store().ControlPlane().APITokens(t.Context(), p, [16]byte{}) {
			t.Errorf("a token stored after a refused creation: %+v, %v", tk, err)
		}
	}
	patTk := s.tokens(t, "pat", "pat's password")
	s.newToken(t, patTk.Access, pat, at)
}

// Signing out with an API token is refused 400 invalid and audited, and the
// token still answers: a token ends only by its revocation.
func TestSignOutWithAnAPITokenRefused(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour)).Status.Token
	before := s.refusedAudits(t, entity.ResourceSession)
	wantStatus(t, "a sign-out with an API token", s.send(t, http.MethodPost, "/api/v1/auth/sign-out", tok, nil), http.StatusBadRequest, "invalid")
	if got := s.refusedAudits(t, entity.ResourceSession) - before; got != 1 {
		t.Errorf("the refused sign-out: %d refused audit records, want 1", got)
	}
	if r := s.list(t, tok); r.status != http.StatusOK {
		t.Errorf("a call with the token after the refused sign-out: answered %d %s, want 200", r.status, r.body)
	}
}

// A token expiring at its creation time is refused; one a second later is
// not. The store's time is pinned to the clock's whole second first, so the
// creation's time is known.
func TestAPITokenExpiringAtCreation(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	first := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	storeReached(t, s.h, first.Status.Created)
	wantStatus(t, "a token expiring at its creation", s.createToken(t, admin, svc, first.Status.Created), http.StatusBadRequest, "invalid")
}

// A deleted account's tokens answer 404, to a list, a creation and a
// revocation alike.
func TestAPITokensOfADeletedAccount(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	got := s.send(t, http.MethodGet, userPath(svc), admin, nil)
	if r := s.send(t, http.MethodDelete, userPath(svc), admin, nil, "Idempotency-Key", uuid.NewString(), "If-Match", got.header.Get("ETag")); r.status != http.StatusAccepted {
		t.Fatalf("delete the account: answered %d %s, want 202", r.status, r.body)
	}
	wantStatus(t, "a deleted account's token list", s.send(t, http.MethodGet, tokensPath(svc), admin, nil), http.StatusNotFound, codeNotFound)
	wantStatus(t, "a deleted account's token creation", s.createToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour)), http.StatusNotFound, codeNotFound)
	wantStatus(t, "a deleted account's token revocation", s.revokeToken(t, admin, svc, tok.ID), http.StatusNotFound, codeNotFound)
}

// A creation's audit record targets the new token by its ID, applied, and
// no audit record holds the token.
func TestAPITokenCreationAudited(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	id := uuid.MustParse(tok.ID)
	found := false
	for _, a := range s.audits(t) {
		if a.Target.Kind == entity.ResourceAPIToken && bytes.Equal(a.Target.ID, id[:]) && string(a.Outcome) == "applied" {
			found = true
		}
		b, _ := json.Marshal(a)
		if bytes.Contains(b, []byte(tok.Status.Token)) {
			t.Errorf("audit record %s holds the token", b)
		}
	}
	if !found {
		t.Errorf("no applied audit record targets the token %s", tok.ID)
	}
}

// idempotencyRows is how many idempotency records db holds.
func idempotencyRows(t *testing.T, db kv.KV) int {
	t.Helper()
	return len(rawStore(t, db, []byte{keys.KindIK})) / 2
}

// Every call that carries or issues a credential refuses an Idempotency-Key
// 400, and the store holds no idempotency record. Its audit record of the
// refusal names the key, as every refused request's does.
func TestCredentialCallsTakeNoIdempotencyKey(t *testing.T) {
	var raw kv.KV
	s := newAuthNode(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		raw = cfg.KV
		return node.New(cfg)
	}))
	adminTk := s.tokens(t, "admin", adminPassword)
	pat := s.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	patTk := s.tokens(t, "pat", "pat's password")
	for i, tc := range []struct {
		what, method, path, bearer string
		body                       any
	}{
		{"sign-in", http.MethodPost, "/api/v1/auth/sign-in", "", apiv1.SignIn{Name: "admin", Password: adminPassword}},
		{"refresh", http.MethodPost, "/api/v1/auth/refresh", "", apiv1.Refresh{Token: adminTk.Refresh}},
		{"the password change", http.MethodPost, passwordPath, patTk.Access, apiv1.PasswordChange{Current: "pat's password", New: "pat's next password"}},
		{":set-password", http.MethodPost, userPath(pat) + ":set-password", adminTk.Access, apiv1.SetPassword{Password: "set by the admin"}},
		{"API-token creation", http.MethodPost, tokensPath(pat), patTk.Access, apiv1.APITokenSpec{Expires: s.h.Clock().Now().Add(time.Hour)}},
	} {
		key := "credential-key-" + string(rune('a'+i)) + "-5c1e9"
		audits := len(s.audits(t))
		wantStatus(t, tc.what+" with an Idempotency-Key", s.send(t, tc.method, tc.path, tc.bearer, tc.body, "Idempotency-Key", key), http.StatusBadRequest, "invalid")
		if n := idempotencyRows(t, raw); n != 0 {
			t.Errorf("%s with an Idempotency-Key: the store holds %d idempotency records, want none", tc.what, n)
		}
		if tc.path == passwordPath {
			recs := s.audits(t)[audits:]
			if len(recs) != 1 || string(recs[0].Outcome) != "refused: invalid" || recs[0].Target.Kind != entity.ResourceUser || !bytes.Equal(recs[0].Target.ID, pat.ID[:]) {
				t.Errorf("the password change with an Idempotency-Key: audit records %+v, want one refused: invalid targeting pat", recs)
			}
		}
	}
	if r := s.list(t, patTk.Access); r.status != http.StatusOK {
		t.Errorf("pat's session after the refused change: answered %d %s, want 200, the change not made", r.status, r.body)
	}
	// A revocation takes a key and records it, so the count above sees a
	// record when there is one.
	tok := s.newToken(t, patTk.Access, pat, s.h.Clock().Now().Add(time.Hour))
	if r := s.revokeToken(t, patTk.Access, pat, tok.ID); r.status != http.StatusOK {
		t.Fatalf("revoke a token: answered %d %s, want 200", r.status, r.body)
	}
	if n := idempotencyRows(t, raw); n != 1 {
		t.Errorf("idempotency records after a keyed revocation: %d, want 1", n)
	}
}

// An API token is held by no key or value of the store, nor by the log, nor
// is its random part; the store holds its hash.
func TestAPITokensReachNeitherStoreNorLog(t *testing.T) {
	var log syncBuffer
	var raw kv.KV
	s := newAuthNode(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		raw = cfg.KV
		cfg.Logger = slog.New(slog.NewMultiHandler(cfg.Logger.Handler(), slog.NewTextHandler(&log, nil)))
		return node.New(cfg)
	}))
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	tok := s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour))
	if r := s.list(t, tok.Status.Token); r.status != http.StatusOK {
		t.Fatalf("a call with the token: answered %d %s, want 200", r.status, r.body)
	}
	s.send(t, http.MethodGet, tokensPath(svc), tok.Status.Token, nil)
	secret, err := base64.RawURLEncoding.DecodeString(tok.Status.Token)
	if err != nil {
		t.Fatalf("the token %q is not base64url: %v", tok.Status.Token, err)
	}
	stored := rawStore(t, raw, nil)
	if holds(stored, []byte(tok.Status.Token)) || holds(stored, secret) {
		t.Error("the store holds the token")
	}
	if strings.Contains(log.String(), tok.Status.Token) {
		t.Error("the log holds the token")
	}
	hash := sha256.Sum256(secret)
	if !holds(stored, hash[:]) {
		t.Error("the store holds no hash of the token")
	}
}

// An account's tokens list a page at a time, each page resuming after the
// last, every token once.
func TestAPITokensListByPage(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	svc := s.serviceAccount(t, "backup", entity.RoleViewer)
	want := map[string]bool{}
	for range 3 {
		want[s.newToken(t, admin, svc, s.h.Clock().Now().Add(time.Hour)).ID] = true
	}
	got := map[string]int{}
	for path, pages := tokensPath(svc)+"?limit=1", 0; path != ""; pages++ {
		if pages > 3 {
			t.Fatalf("still paging after %d pages", pages)
		}
		r := s.send(t, http.MethodGet, path, admin, nil)
		var page apiv1.Page[apiv1.APIToken]
		if err := json.Unmarshal(r.body, &page); r.status != http.StatusOK || err != nil {
			t.Fatalf("a page of tokens: answered %d %s (%v), want 200", r.status, r.body, err)
		}
		for _, tk := range page.Items {
			got[tk.ID]++
		}
		path = ""
		if page.Next != "" {
			path = tokensPath(svc) + "?limit=1&after=" + page.Next
		}
	}
	if len(got) != len(want) {
		t.Errorf("tokens listed: %v, want each of %v once", got, want)
	}
	for id, n := range got {
		if !want[id] || n != 1 {
			t.Errorf("token %s listed %d times, want once and only if created", id, n)
		}
	}
}
