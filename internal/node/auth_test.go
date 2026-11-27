package node_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/api/auth"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	apiclient "github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/cubbit/dittofs/pkg/secrets"
)

// adminPassword is the administrator's first password in these tests, given
// as a reference so it need not be changed before a call.
const adminPassword = "correct horse battery staple"

// authNode is a started node whose administrator's password is known, and
// the address of its API.
type authNode struct {
	h    *nodetest.Harness
	addr string
}

// withKV builds each node over wrap's KV around the one the harness opened,
// so the test counts or faults the node's transactions while the harness's
// own store, which the test writes through, stays unwrapped.
func withKV(wrap func(kv.KV) kv.KV) nodetest.Option {
	return nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.KV = wrap(cfg.KV)
		return node.New(cfg)
	})
}

func newAuthNode(t *testing.T, opts ...nodetest.Option) *authNode {
	t.Helper()
	h := nodetest.New(t, append([]nodetest.Option{nodetest.Watch(), nodetest.AdminPassword(adminPassword)}, opts...)...)
	started(t, h)
	addr, _ := h.Node().Addrs()
	return &authNode{h: h, addr: addr}
}

// authReply is one answer of the API.
type authReply struct {
	status int
	body   []byte
	header http.Header
}

// problem decodes r as a problem.
func (r authReply) problem(t *testing.T) apiv1.Problem {
	t.Helper()
	var p apiv1.Problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("answer %d %s is no problem: %v", r.status, r.body, err)
	}
	return p
}

// stripped is r's body with its request ID removed, the one field two
// refusals of different requests differ by.
func (r authReply) stripped(t *testing.T) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("answer %d %s: %v", r.status, r.body, err)
	}
	delete(m, "request_id")
	b, _ := json.Marshal(m)
	return string(b)
}

// send sends method path with body, nil for none, bearing token when set,
// and the header pairs in hdr.
func (s *authNode) send(t *testing.T, method, path, token string, body any, hdr ...string) authReply {
	t.Helper()
	r, err := s.try(t.Context(), method, path, token, body, hdr...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// try is send answering its error, for a goroutine of the test's, which may
// not fail the test fatally.
func (s *authNode) try(ctx context.Context, method, path, token string, body any, hdr ...string) (authReply, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return authReply{}, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+s.addr+path, rd)
	if err != nil {
		return authReply{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Do(req)
	if err != nil {
		return authReply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return authReply{resp.StatusCode, b, resp.Header}, err
}

// signIn signs name in with password.
func (s *authNode) signIn(t *testing.T, name, password string) authReply {
	t.Helper()
	return s.send(t, http.MethodPost, "/api/v1/auth/sign-in", "", apiv1.SignIn{Name: name, Password: password})
}

// tokens signs name in with password, wanting 200, and returns its tokens.
func (s *authNode) tokens(t *testing.T, name, password string) apiv1.Tokens {
	t.Helper()
	return s.decodeTokens(t, "sign-in as "+name, s.signIn(t, name, password))
}

func (s *authNode) decodeTokens(t *testing.T, what string, r authReply) apiv1.Tokens {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("%s: answered %d %s, want 200", what, r.status, r.body)
	}
	var tk apiv1.Tokens
	if err := json.Unmarshal(r.body, &tk); err != nil || tk.Access == "" || tk.Refresh == "" {
		t.Fatalf("%s: tokens %s (%v), want both", what, r.body, err)
	}
	return tk
}

func (s *authNode) refresh(t *testing.T, refresh string) authReply {
	t.Helper()
	return s.send(t, http.MethodPost, "/api/v1/auth/refresh", "", apiv1.Refresh{Token: refresh})
}

// list GETs the shares with token.
func (s *authNode) list(t *testing.T, token string) authReply {
	t.Helper()
	return s.send(t, http.MethodGet, "/api/v1/shares", token, nil)
}

// wantStatus fails unless r answered status with code.
func wantStatus(t *testing.T, what string, r authReply, status int, code apiv1.Code) {
	t.Helper()
	if r.status != status {
		t.Errorf("%s: answered %d %s, want %d %s", what, r.status, r.body, status, code)
		return
	}
	if p := r.problem(t); p.Code != code {
		t.Errorf("%s: code %q, want %q", what, p.Code, code)
	}
}

// write commits fn through the store's control plane, as another node would.
func (s *authNode) write(t *testing.T, fn func(entity.ControlPlaneTx) error) {
	t.Helper()
	if err := s.h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, fn); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// change rewrites p's record through fn.
func (s *authNode) change(t *testing.T, p entity.Principal, fn func(tx entity.ControlPlaneTx, u *entity.User) error) {
	t.Helper()
	s.write(t, func(tx entity.ControlPlaneTx) error {
		u, ok, err := tx.User(p)
		if err != nil || !ok {
			return errors.Join(err, errors.New("no such user"))
		}
		return fn(tx, &u)
	})
}

// audits is every audit record the store holds.
func (s *authNode) audits(t *testing.T) []entity.AuditRecord {
	t.Helper()
	var out []entity.AuditRecord
	for a, err := range s.h.Store().ControlPlane().AuditRecords(t.Context(), entity.AuditFilter{}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// sourceCount is the sign-in count of the loopback source the tests call
// from.
func (s *authNode) sourceCount(t *testing.T) entity.SignInFailures {
	t.Helper()
	k := entity.SignInKey{Source: entity.SourceKey(netip.MustParseAddr("127.0.0.1"))}
	f, _, err := s.h.Store().ControlPlane().SignInFailures(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A sign-in checks the name and password through identity and answers an
// access token, which the next call is authenticated by, and a refresh
// token; a wrong password is refused 401 unauthenticated.
func TestSignInAnswersBothTokens(t *testing.T) {
	s := newAuthNode(t)
	tk := s.tokens(t, "admin", adminPassword)
	if r := s.list(t, tk.Access); r.status != http.StatusOK {
		t.Errorf("GET /shares with the access token: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "a call with no token", s.list(t, ""), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a wrong password", s.signIn(t, "admin", "wrong"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
}

// A refresh answers new tokens and the refresh token it was given refreshes
// no more: presented again, it is refused and revokes the whole session, so
// the token that replaced it and the session's access tokens are refused too.
// Sign-out ends a session the same way.
func TestRefreshRotatesAndReuseRevokes(t *testing.T) {
	s := newAuthNode(t)
	first := s.tokens(t, "admin", adminPassword)
	second := s.decodeTokens(t, "refresh", s.refresh(t, first.Refresh))
	if second.Refresh == first.Refresh {
		t.Fatal("refresh answered the refresh token it was given")
	}
	if r := s.list(t, second.Access); r.status != http.StatusOK {
		t.Fatalf("a call with the refreshed access token: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "the replaced refresh token", s.refresh(t, first.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "the current refresh token, after the session was revoked", s.refresh(t, second.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a call on the revoked session", s.list(t, second.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	for _, err := range s.h.Store().ControlPlane().Sessions(t.Context(), s.principalNamed(t, "admin")) {
		if err != nil {
			t.Fatal(err)
		}
		t.Error("the revoked session is still stored")
	}
	wantStatus(t, "a token no session holds", s.refresh(t, "not a token"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	// A refused refresh is a refused authentication: the reused token under
	// its own reason, the revoked session's and the malformed token's under
	// refresh.
	reg := s.h.Metrics().Registry()
	for reason, want := range map[string]float64{"refresh_reused": 1, "refresh": 2} {
		if got := seriesValue(t, reg, "dittofs_api_auth_failures_total", []string{reason}); got != want {
			t.Errorf("auth failures for %s: %v, want %v", reason, got, want)
		}
	}
}

// signingKeys is the secret provider as access tokens open the signing key.
type signingKeys struct{ p *secrets.Provider }

func (k signingKeys) Versions(ctx context.Context, kind entity.SecretKind, owner entity.SecretOwner, ref entity.SecretRef) ([]auth.KeyVersion, error) {
	vs, err := k.p.Versions(ctx, secrets.Storage, kind, owner, ref)
	out := make([]auth.KeyVersion, len(vs))
	for i, v := range vs {
		out[i] = auth.KeyVersion{Version: v.Number, Value: v.Value}
	}
	return out, err
}

// An access token naming one account and another account's session, signed
// with the installation's own key, is refused at its session.
func TestTokenNamingAnotherAccountsSession(t *testing.T) {
	s := newAuthNode(t)
	alice := s.h.Account(t, "alice", "alice's password", entity.RoleViewer)
	s.tokens(t, "admin", adminPassword)
	var session [16]byte
	for sess, err := range s.h.Store().ControlPlane().Sessions(t.Context(), s.principalNamed(t, "admin")) {
		if err != nil {
			t.Fatal(err)
		}
		session = sess.ID
	}
	in, err := s.h.Store().Installation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tk := auth.NewTokens(signingKeys{s.h.Secrets(t)}, in.ID, in.SigningKey, s.h.Clock())
	defer tk.Close()
	forged, err := tk.Issue(t.Context(), alice, session, s.h.Clock().Now().Truncate(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "alice's token naming admin's session", s.list(t, forged), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if got := seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{"session"}); got != 1 {
		t.Errorf("auth failures for session: %v, want 1", got)
	}
}

// The bearer scheme's name is matched in any case.
func TestBearerSchemeAnyCase(t *testing.T) {
	s := newAuthNode(t)
	tk := s.tokens(t, "admin", adminPassword)
	for _, scheme := range []string{"bearer ", "BEARER "} {
		if r := s.send(t, http.MethodGet, "/api/v1/shares", "", nil, "Authorization", scheme+tk.Access); r.status != http.StatusOK {
			t.Errorf("a call under %q: answered %d %s, want 200", scheme, r.status, r.body)
		}
	}
}

// onRead runs fn once, the first time a key whose description starts with
// prefix is read while it is armed.
type onRead struct {
	prefix string
	armed  atomic.Bool
	fn     func()
}

func (f *onRead) At(op string, key []byte) (time.Duration, int, error) {
	read := op == kvtest.OpGet || op == kvtest.OpBefore || op == kvtest.OpScan
	if read && strings.HasPrefix(string(key), f.prefix) && f.armed.CompareAndSwap(true, false) {
		f.fn()
	}
	return 0, 0, nil
}

// A failed sign-in settles its source's count at the settle's store time,
// not the attempt's: the clock is moved while the password is read, between
// the count and the settle, and the count's last settled failure follows.
func TestFailureSettlesAtItsSettle(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	u, ok, err := s.h.Store().ControlPlane().User(t.Context(), s.principalNamed(t, "admin"))
	if err != nil || !ok {
		t.Fatalf("read admin: %v, %v", ok, err)
	}
	moved := s.h.Clock().Now().Add(time.Minute)
	f := &onRead{prefix: keys.Describe(keys.ID([]byte{keys.KindSEC}, u.Password[:])), fn: func() {
		s.h.Clock().Advance(time.Minute)
		storeReached(t, s.h, moved)
	}}
	fault.SetFault(f)
	f.armed.Store(true)
	wantStatus(t, "a wrong password", s.signIn(t, "admin", "wrong"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if f.armed.Load() {
		t.Fatal("the sign-in read no password, so the clock never moved during its check")
	}
	if got := s.sourceCount(t); got.Failed.Before(moved) {
		t.Errorf("the source's last settled failure: %v, want at or after %v, the settle's time", got.Failed, moved)
	}
}

// Sign-out takes no idempotency key, so one session's key cannot answer
// another's sign-out from its record: each of two sessions, signed out under
// one key, is refused 400 and stays live; signed out without one, each ends,
// its access and refresh tokens refused.
func TestSignOutEndsEachSession(t *testing.T) {
	s := newAuthNode(t)
	a, b := s.tokens(t, "admin", adminPassword), s.tokens(t, "admin", adminPassword)
	for _, tk := range []apiv1.Tokens{a, b} {
		r := s.send(t, http.MethodPost, "/api/v1/auth/sign-out", tk.Access, nil, "Idempotency-Key", "bye")
		if r.status != http.StatusBadRequest {
			t.Errorf("sign-out under a key: answered %d %s, want 400", r.status, r.body)
		}
		if r := s.list(t, tk.Access); r.status != http.StatusOK {
			t.Errorf("a call after a refused sign-out: answered %d %s, want 200", r.status, r.body)
		}
	}
	for _, tk := range []apiv1.Tokens{a, b} {
		if r := s.send(t, http.MethodPost, "/api/v1/auth/sign-out", tk.Access, nil); r.status != http.StatusOK {
			t.Fatalf("sign-out: answered %d %s, want 200", r.status, r.body)
		}
		wantStatus(t, "a call after sign-out", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		wantStatus(t, "a refresh after sign-out", s.refresh(t, tk.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	}
}

// An account marked deleted, before its batches run, is refused a sign-in, a
// refresh and a call with its access token; its sign-in's refusal says no
// more than a wrong password's, and a disabled account's says the same.
func TestDisabledAndDeletedAccountsRefused(t *testing.T) {
	s := newAuthNode(t)
	alice := s.h.Account(t, "alice", "alice's password", entity.RoleViewer)
	bob := s.h.Account(t, "bob", "bob's password", entity.RoleViewer)
	tk := s.tokens(t, "alice", "alice's password")
	bobs := s.tokens(t, "bob", "bob's password")
	wrong := s.signIn(t, "alice", "not it")
	wantStatus(t, "a wrong password", wrong, http.StatusUnauthorized, apiv1.CodeUnauthenticated)

	s.change(t, alice, func(tx entity.ControlPlaneTx, u *entity.User) error { return tx.DeleteUser(u.Principal, u.Generation) })
	s.change(t, bob, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.Disabled = true
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	deleted := s.signIn(t, "alice", "alice's password")
	disabled := s.signIn(t, "bob", "bob's password")
	for _, r := range []struct {
		what string
		r    authReply
	}{{"the deleted account's sign-in", deleted}, {"the disabled account's sign-in", disabled}} {
		if got, want := r.r.stripped(t), wrong.stripped(t); r.r.status != wrong.status || got != want {
			t.Errorf("%s: answered %d %s, want %d %s, a wrong password's", r.what, r.r.status, got, wrong.status, want)
		}
	}
	wantStatus(t, "the deleted account's refresh", s.refresh(t, tk.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a call with the deleted account's access token", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a call with the disabled account's access token", s.list(t, bobs.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "the disabled account's refresh", s.refresh(t, bobs.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	// A marked account's sessions are found by no read, so its call is
	// refused at its session; a disabled one's at its account.
	reg := s.h.Metrics().Registry()
	for reason, want := range map[string]float64{"session": 1, "account": 1} {
		if got := seriesValue(t, reg, "dittofs_api_auth_failures_total", []string{reason}); got != want {
			t.Errorf("auth failures for %s: %v, want %v", reason, got, want)
		}
	}
}

// inWindow runs write once, as another node's, the first time the node's
// write sets a session key while it is armed: after the write read the
// account and before it commits, so the commit conflicts and runs again over
// what write changed.
type inWindow struct {
	armed atomic.Bool
	write func()
}

func (f *inWindow) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpSet && strings.HasPrefix(string(key), "SS") && f.armed.CompareAndSwap(true, false) {
		f.write()
	}
	return 0, 0, nil
}

// An account disabled or deleted between its password's check and the
// sign-in's write, or between a refresh's read and its write, is refused 401
// there: the write reads the account again. The sign-in's refusal is
// audited as a failure, and no session is stored.
func TestAccountChangedInTheWindow(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	disable := func(p entity.Principal) func() {
		return func() {
			s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error {
				u.Disabled = true
				_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
				return err
			})
		}
	}
	edit := func(p entity.Principal) func() {
		return func() {
			s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error {
				u.Email = "edited@example.com"
				_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
				return err
			})
		}
	}
	remove := func(p entity.Principal) func() {
		return func() {
			s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error { return tx.DeleteUser(u.Principal, u.Generation) })
		}
	}
	// An edit is a change of the record the password was checked against,
	// so the sign-in is refused; a refresh checks no password, so it
	// refreshes over an edited account.
	for _, tc := range []struct {
		what    string
		change  func(entity.Principal) func()
		refresh int
	}{{"disabled", disable, http.StatusUnauthorized}, {"deleted", remove, http.StatusUnauthorized}, {"edited", edit, http.StatusOK}} {
		t.Run(tc.what, func(t *testing.T) {
			w := &inWindow{}
			fault.SetFault(w)
			p := s.h.Account(t, "signin-"+tc.what, "its password", entity.RoleViewer)
			audits := len(s.audits(t))
			w.write = tc.change(p)
			w.armed.Store(true)
			wantStatus(t, "a sign-in", s.signIn(t, "signin-"+tc.what, "its password"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
			if w.armed.Load() {
				t.Fatal("the sign-in's write set no session key, so nothing changed in its window")
			}
			recs := s.audits(t)
			if len(recs) != audits+1 || string(recs[len(recs)-1].Outcome) != "refused: unauthenticated" {
				t.Errorf("audit records: %d new, the last %q; want 1, refused: unauthenticated", len(recs)-audits, recs[len(recs)-1].Outcome)
			}
			if tc.what == "disabled" {
				for _, err := range s.h.Store().ControlPlane().Sessions(t.Context(), p) {
					t.Errorf("a session is stored for the disabled account (%v)", err)
				}
			}

			q := s.h.Account(t, "refresh-"+tc.what, "its password", entity.RoleViewer)
			tk := s.tokens(t, "refresh-"+tc.what, "its password")
			w.write = tc.change(q)
			w.armed.Store(true)
			if r := s.refresh(t, tk.Refresh); r.status != tc.refresh {
				t.Errorf("a refresh: answered %d %s, want %d", r.status, r.body, tc.refresh)
			}
			if w.armed.Load() {
				t.Fatal("the refresh's write set no session key, so nothing changed in its window")
			}
		})
	}
}

// A success resets its account's count and its pair's: after 4 failures
// and a success, neither is stored.
func TestSuccessResetsCounts(t *testing.T) {
	s := newAuthNode(t)
	alice := s.h.Account(t, "alice", "alice's password", entity.RoleViewer)
	for range 4 {
		s.signIn(t, "alice", "wrong")
	}
	s.tokens(t, "alice", "alice's password")
	src := entity.SourceKey(netip.MustParseAddr("127.0.0.1"))
	for _, k := range []entity.SignInKey{{Account: alice}, {Account: alice, Source: src}} {
		if f, found, err := s.h.Store().ControlPlane().SignInFailures(t.Context(), k); err != nil || found {
			t.Errorf("count %+v after a success: %+v, found %v (%v); want none", k, f, found, err)
		}
	}
}

// A session past api.session_lifetime, 12 hours by default, refreshes no
// more: the clock is moved a second past it, since the sign-in's store time
// is a few nanoseconds past the clock's.
func TestSessionLifetimeEndsRefresh(t *testing.T) {
	s := newAuthNode(t)
	tk := s.tokens(t, "admin", adminPassword)
	s.h.Clock().Advance(12*time.Hour + time.Second)
	storeReached(t, s.h, s.h.Clock().Now())
	wantStatus(t, "a refresh 12 h after sign-in", s.refresh(t, tk.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
}

// A session past its lifetime refuses a call with an access token that has
// not yet expired: refreshed 5 min before the 12 h, its new token holds 15
// min, and 10 min on its session's end refuses it.
func TestSessionLifetimeEndsCalls(t *testing.T) {
	s := newAuthNode(t)
	tk := s.tokens(t, "admin", adminPassword)
	s.h.Clock().Advance(12*time.Hour - 5*time.Minute)
	storeReached(t, s.h, s.h.Clock().Now())
	tk = s.decodeTokens(t, "a refresh 5 min before the session ends", s.refresh(t, tk.Refresh))
	if r := s.list(t, tk.Access); r.status != http.StatusOK {
		t.Fatalf("a call with the refreshed token: answered %d %s, want 200", r.status, r.body)
	}
	s.h.Clock().Advance(10 * time.Minute)
	wantStatus(t, "a call 5 min after the session ended", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if got := seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{"session"}); got != 1 {
		t.Errorf("auth failures for session: %v, want 1", got)
	}
}

// A not-before raised to the whole second after now, then a sign-in before
// the clock reaches it: the session is created at the not-before, its
// creation time raised to it, so its refresh and its calls are not refused
// by the raise it follows.
func TestSessionAfterNotBeforeRaise(t *testing.T) {
	s := newAuthNode(t)
	s.change(t, s.principalNamed(t, "admin"), func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.NotBefore = tx.Now().Truncate(time.Second).Add(time.Second)
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	tk := s.tokens(t, "admin", adminPassword)
	if r := s.list(t, tk.Access); r.status != http.StatusOK {
		t.Errorf("a call right after the raise: answered %d %s, want 200", r.status, r.body)
	}
	s.decodeTokens(t, "a refresh right after the raise", s.refresh(t, tk.Refresh))
}

// A not-before raised past a session's sign-in refuses its access token and
// its refresh, as signing out everywhere does.
func TestNotBeforeRefusesEarlierTokens(t *testing.T) {
	s := newAuthNode(t)
	tk := s.tokens(t, "admin", adminPassword)
	s.h.Clock().Advance(2 * time.Second)
	storeReached(t, s.h, s.h.Clock().Now())
	s.change(t, s.principalNamed(t, "admin"), func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.NotBefore = tx.Now().Truncate(time.Second).Add(time.Second)
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	wantStatus(t, "a call with a token issued before the not-before", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a refresh of a session created before the not-before", s.refresh(t, tk.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if got := seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_auth_failures_total", []string{"not_before"}); got != 1 {
		t.Errorf("auth failures for not_before: %v, want 1", got)
	}
}

// A session's creation time is the store time of its sign-in's write, never
// the node's clock. With the clock moved an hour on, store time stays behind
// it until the store raises its ceiling, about once a second of real time;
// a raise of the not-before inside that lag is past the sign-in in store
// time, yet before the node's clock, so a session created at the node's
// clock would survive it. The sign-in and the raise are tried again until
// the raise lands inside the lag, as its own store time shows.
func TestSessionCreatedAtStoreTime(t *testing.T) {
	s := newAuthNode(t)
	admin := s.principalNamed(t, "admin")
	for range 20 {
		s.h.Clock().Advance(time.Hour)
		tk := s.tokens(t, "admin", adminPassword)
		var notBefore time.Time
		s.change(t, admin, func(tx entity.ControlPlaneTx, u *entity.User) error {
			u.NotBefore = tx.Now().Truncate(time.Second).Add(time.Second)
			notBefore = u.NotBefore
			_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
			return err
		})
		if notBefore.After(s.h.Clock().Now()) {
			continue // the store's ceiling was raised during the attempt
		}
		wantStatus(t, "a call with a token issued before the raise", s.list(t, tk.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		wantStatus(t, "a refresh of a session created before the raise", s.refresh(t, tk.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		return
	}
	t.Fatal("in 20 tries the store's time never stayed behind the clock across a sign-in and a raise")
}

// A checked sign-in makes exactly two Updates, the count and its
// settlement, whether it succeeds or fails. Only the Updates that write a
// sign-in count or a session are counted, so a turn of the node's own loops,
// or of anything else on the store, committing during the sign-in is not.
func TestCheckedSignInCommitsTwice(t *testing.T) {
	writes := &signInWrites{}
	h := nodetest.New(t, nodetest.Watch(), nodetest.AdminPassword(adminPassword), withKV(func(inner kv.KV) kv.KV {
		writes.KV = inner
		return writes
	}))
	started(t, h)
	addr, _ := h.Node().Addrs()
	s := &authNode{h: h, addr: addr}
	for _, tc := range []struct {
		what, password string
		status         int
	}{{"a success", adminPassword, http.StatusOK}, {"a failure", "wrong", http.StatusUnauthorized}} {
		writes.n.Store(0)
		if r := s.signIn(t, "admin", tc.password); r.status != tc.status {
			t.Errorf("%s: answered %d %s, want %d", tc.what, r.status, r.body, tc.status)
		}
		if got := writes.n.Load(); got != 2 {
			t.Errorf("%s: %d Updates writing a sign-in count or a session, want 2", tc.what, got)
		}
	}
}

// signInWrites passes every Update through and counts those whose final
// attempt set or deleted a sign-in count or a session key, committed or not:
// each write of a sign-in writes one of them, and no loop of the node does
// while the clock stands still, as none of them is due for the sweep.
type signInWrites struct {
	kv.KV
	n atomic.Int32
}

func (w *signInWrites) Update(ctx context.Context, fn func(kv.Txn) error) error {
	var wrote bool
	err := w.KV.Update(ctx, func(tx kv.Txn) error {
		wrote = false
		return fn(signInTxn{tx, &wrote})
	})
	if wrote {
		w.n.Add(1)
	}
	return err
}

// signInTxn is a Txn that notes a Set or Delete of a sign-in count or a
// session key.
type signInTxn struct {
	kv.Txn
	wrote *bool
}

func (t signInTxn) Set(key, value []byte) error { t.note(key); return t.Txn.Set(key, value) }
func (t signInTxn) Delete(key []byte) error     { t.note(key); return t.Txn.Delete(key) }

func (t signInTxn) note(key []byte) {
	if len(key) > 0 && (key[0] == keys.KindSF || key[0] == keys.KindSS) {
		*t.wrote = true
	}
}

// Every spelling of a name shares one count: 4 failures as alice and 1 as
// ALICE hold CORP\alice and alice@CORP.EXAMPLE at 429, the right password
// unchecked.
func TestSpellingsShareOneCount(t *testing.T) {
	s := newAuthNode(t)
	alice := s.h.Account(t, "alice", "alice's password", entity.RoleViewer)
	for _, name := range []string{"alice", "alice", "alice", "alice", "ALICE"} {
		wantStatus(t, "failure as "+name, s.signIn(t, name, "wrong"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	}
	// The source's own count reaches 5 too, so the 429 alone does not show
	// that both spellings counted on alice.
	if f, _, err := s.h.Store().ControlPlane().SignInFailures(t.Context(), entity.SignInKey{Account: alice}); err != nil || f.Count != 5 {
		t.Errorf("alice's account count after 5 failures in two spellings: %d (%v), want 5", f.Count, err)
	}
	for _, name := range []string{`CORP\alice`, "alice@CORP.EXAMPLE"} {
		r := s.signIn(t, name, "alice's password")
		wantStatus(t, "the right password as "+name, r, http.StatusTooManyRequests, apiv1.CodeThrottled)
		if r.header.Get("Retry-After") == "" {
			t.Errorf("the right password as %s: no Retry-After", name)
		}
	}
}

// A failed sign-in is audited under the account its name resolves to, a
// 429 is not, and the sign-in series counts each by its result, the audit
// series each record by its outcome; a call
// refused authentication is counted by its reason. Neither the password nor
// a token reaches the log, the audit or a series' labels.
func TestSignInAuditSeriesAndSecrets(t *testing.T) {
	var log syncBuffer
	s := newAuthNode(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Logger = slog.New(slog.NewMultiHandler(cfg.Logger.Handler(), slog.NewTextHandler(&log, nil)))
		return node.New(cfg)
	}))
	admin := s.principalNamed(t, "admin")
	tk := s.tokens(t, "admin", adminPassword)
	before := len(s.audits(t))
	for range 5 {
		s.signIn(t, "admin", "a wrong guess")
	}
	recs := s.audits(t)
	if got := len(recs) - before; got != 5 {
		t.Fatalf("5 failed sign-ins: %d audit records, want 5", got)
	}
	last := recs[len(recs)-1]
	if last.Actor != admin || string(last.Outcome) != "refused: unauthenticated" || last.Target.Kind != entity.ResourceSession {
		t.Errorf("a failed sign-in's record: actor %v, outcome %q, target %v; want admin, refused: unauthenticated, a session", last.Actor, last.Outcome, last.Target)
	}
	wantStatus(t, "the sixth attempt", s.signIn(t, "admin", adminPassword), http.StatusTooManyRequests, apiv1.CodeThrottled)
	if got := len(s.audits(t)); got != len(recs) {
		t.Errorf("a 429: %d audit records, want %d, none new", got, len(recs))
	}
	s.list(t, "")
	s.list(t, "not a token")

	reg := s.h.Metrics().Registry()
	for _, tc := range []struct {
		series string
		labels []string
		want   float64
	}{
		{"dittofs_api_sign_ins_total", []string{"password", "ok"}, 1},
		{"dittofs_api_sign_ins_total", []string{"password", "unauthenticated"}, 5},
		{"dittofs_api_sign_ins_total", []string{"password", "throttled"}, 1},
		{"dittofs_api_auth_failures_total", []string{"missing"}, 1},
		{"dittofs_api_auth_failures_total", []string{"token"}, 1},
		{"dittofs_audit_records_total", []string{"applied"}, 1},
		{"dittofs_audit_records_total", []string{"refused"}, 5},
	} {
		if got := seriesValue(t, reg, tc.series, tc.labels); got != tc.want {
			t.Errorf("%s%v: %v, want %v", tc.series, tc.labels, got, tc.want)
		}
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var exposed strings.Builder
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "dittofs_api_") {
			exposed.WriteString(mf.String())
		}
	}
	var audit bytes.Buffer
	for _, a := range s.audits(t) {
		b, _ := json.Marshal(a)
		audit.Write(b)
	}
	for _, secret := range []struct{ role, value string }{
		{"the password", adminPassword}, {"a wrong password", "a wrong guess"}, {"the access token", tk.Access}, {"the refresh token", tk.Refresh},
	} {
		for where, text := range map[string]string{"the log": log.String(), "the audit": audit.String(), "the series": exposed.String()} {
			if strings.Contains(text, secret.value) {
				t.Errorf("%s holds %s", where, secret.role)
			}
		}
	}
}

// seriesValue is the value of the counter name with labels, in their
// order, from reg; zero when it holds none.
func seriesValue(t *testing.T, reg *prometheus.Registry, name string, labels []string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var vs []string
			for _, l := range m.GetLabel() {
				vs = append(vs, l.GetValue())
			}
			if slices.Equal(vs, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// unavailable fails each read of a key whose description starts with
// prefix while it is on, as a store that cannot answer.
type unavailable struct {
	prefix string
	on     atomic.Bool
}

var errStoreDown = errors.New("the store is down")

func (f *unavailable) At(op string, key []byte) (time.Duration, int, error) {
	read := op == kvtest.OpGet || op == kvtest.OpBefore || op == kvtest.OpScan
	if f.on.Load() && read && strings.HasPrefix(string(key), f.prefix) {
		return 0, 0, errStoreDown
	}
	return 0, 0, nil
}

// A store that cannot answer is no failed credential. With the sealed
// password unreadable, a sign-in answers 503, settling no failure, auditing
// nothing and taking back its raise of the source's count; with the
// account's record unreadable, a refresh and a call with a valid access
// token answer 503, not 401.
func TestUnavailableStoreIsNotARefusal(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	alice := s.h.Account(t, "alice", "alice's password", entity.RoleViewer)
	tk := s.tokens(t, "alice", "alice's password")
	count, audits := s.sourceCount(t), len(s.audits(t))

	secrets := &unavailable{prefix: "SEC"}
	fault.SetFault(secrets)
	secrets.on.Store(true)
	wantStatus(t, "a sign-in with the password unreadable", s.signIn(t, "alice", "alice's password"), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	secrets.on.Store(false)
	if got := s.sourceCount(t); got.Count != count.Count || !got.Failed.Equal(count.Failed) {
		t.Errorf("the source's count: %d, last failure %v; want %d, %v, as before", got.Count, got.Failed, count.Count, count.Failed)
	}
	if got := len(s.audits(t)); got != audits {
		t.Errorf("%d audit records, want %d, none new", got, audits)
	}

	// A refresh reads the account's record twice too, the session's
	// liveness and then Account; failing only the second, Account's own
	// read is the store that cannot answer.
	userKey := keys.Describe(keys.ID([]byte{keys.KindU}, alice.ID[:]))
	account := &nthRead{prefix: userKey, n: 2}
	fault.SetFault(account)
	account.on.Store(true)
	wantStatus(t, "a refresh with Account's read failing", s.refresh(t, tk.Refresh), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	account.on.Store(false)
	if account.seen.Load() < 2 {
		t.Errorf("the refresh read the account %d times, want 2", account.seen.Load())
	}
	user := &unavailable{prefix: userKey}
	fault.SetFault(user)
	user.on.Store(true)
	wantStatus(t, "a call with the account unreadable", s.list(t, tk.Access), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	user.on.Store(false)
	if r := s.list(t, tk.Access); r.status != http.StatusOK {
		t.Errorf("a call once the store answers: %d %s, want 200, the session intact", r.status, r.body)
	}

	// A call reads the account's record twice, the session's liveness and
	// then the account itself; failing only the second, Account's own read
	// is the store that cannot answer.
	second := &nthRead{prefix: user.prefix, n: 2}
	fault.SetFault(second)
	second.on.Store(true)
	wantStatus(t, "a call with the account's own read failing", s.list(t, tk.Access), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	second.on.Store(false)
	if second.seen.Load() < 2 {
		t.Errorf("the call read the account %d times, want 2", second.seen.Load())
	}
}

// A signing key that cannot be read is found before anything commits: a
// refresh and a sign-in each answer 503 with nothing written, the refresh
// token still current and no session or audit record left behind, and each
// succeeds once the key reads again. The clock is moved past the access
// tokens' once-a-second re-read of the key before each faulted call.
func TestSigningKeyReadBeforeCommit(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	in, err := s.h.Store().Installation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	key := &unavailable{prefix: keys.Describe(keys.ID([]byte{keys.KindSEC}, in.SigningKey[:]))}
	fault.SetFault(key)
	tk := s.tokens(t, "admin", adminPassword)
	admin := s.principalNamed(t, "admin")
	sessions := func() int {
		n := 0
		for _, err := range s.h.Store().ControlPlane().Sessions(t.Context(), admin) {
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		return n
	}
	before, audits := sessions(), len(s.audits(t))

	s.h.Clock().Advance(2 * time.Second)
	key.on.Store(true)
	wantStatus(t, "a refresh with the signing key unreadable", s.refresh(t, tk.Refresh), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	key.on.Store(false)
	s.h.Clock().Advance(2 * time.Second)
	key.on.Store(true)
	wantStatus(t, "a sign-in with the signing key unreadable", s.signIn(t, "admin", adminPassword), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	key.on.Store(false)
	if got := sessions(); got != before {
		t.Errorf("sessions: %d, want %d, none opened", got, before)
	}
	if got := len(s.audits(t)); got != audits {
		t.Errorf("audit records: %d, want %d, none written", got, audits)
	}
	s.decodeTokens(t, "the same refresh once the key reads", s.refresh(t, tk.Refresh))
	s.tokens(t, "admin", adminPassword)
}

// nthRead fails only the nth Get of a key whose description starts with
// prefix once it is on.
type nthRead struct {
	prefix string
	n      int64
	on     atomic.Bool
	seen   atomic.Int64
}

func (f *nthRead) At(op string, key []byte) (time.Duration, int, error) {
	if f.on.Load() && op == kvtest.OpGet && strings.HasPrefix(string(key), f.prefix) && f.seen.Add(1) == f.n {
		return 0, 0, errStoreDown
	}
	return 0, 0, nil
}

// session is a token source of the test's own over the client's SignIn and
// Refresh, counting its refreshes.
type session struct {
	c         *apiclient.Client
	mu        sync.Mutex
	tk        apiv1.Tokens
	refreshes int
}

func (s *session) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tk.Access, nil
}

func (s *session) Refresh(ctx context.Context, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tk, err := s.c.Refresh(ctx, s.tk.Refresh)
	if err != nil {
		return "", err
	}
	s.tk = tk
	s.refreshes++
	return tk.Access, nil
}

// A client whose access token has expired is refused 401 on a create,
// refreshes once through the real Refresh and replays the create with its
// body and key: one group and one idempotency record. A Wait poll is
// refreshed the same way. A sign-in's 401 is returned as it is.
func TestClientRefreshesAndReplays(t *testing.T) {
	s := newAuthNode(t)
	bare, err := apiclient.New("http://" + s.addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.SignIn(t.Context(), "admin", []byte("wrong")); err == nil || !strings.Contains(err.Error(), string(apiv1.CodeUnauthenticated)) {
		t.Errorf("SignIn with a wrong password: %v, want the 401 unauthenticated", err)
	}
	tk, err := bare.SignIn(t.Context(), "admin", []byte(adminPassword))
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	src := &session{c: bare, tk: tk}
	c, err := apiclient.New("http://"+s.addr, apiclient.WithTokenSource(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(t.Context(), "admin", []byte("wrong")); err == nil || src.refreshes != 0 {
		t.Errorf("SignIn with a wrong password through a token source: %v after %d refreshes, want the 401 and none", err, src.refreshes)
	}
	s.h.Clock().Advance(16 * time.Minute)
	storeReached(t, s.h, s.h.Clock().Now())
	if _, err := c.Groups().Create(t.Context(), apiv1.GroupSpec{Name: "staff"}); err != nil {
		t.Fatalf("create a group after the access token expired: %v", err)
	}
	if src.refreshes != 1 {
		t.Errorf("the create: %d refreshes, want 1", src.refreshes)
	}
	n := 0
	for g, err := range c.Groups().List(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if g.Spec.Name == "staff" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("groups named staff: %d, want 1", n)
	}
	if got := seriesValue(t, s.h.Metrics().Registry(), "dittofs_api_idempotency_records_written_total", nil); got != 1 {
		t.Errorf("idempotency records written: %v, want 1", got)
	}

	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
	cp := s.h.Store().ControlPlane()
	if _, err := cp.PutOperation(t.Context(), op, 0); err != nil {
		t.Fatal(err)
	}
	s.h.Clock().Advance(16 * time.Minute)
	storeReached(t, s.h, s.h.Clock().Now())
	type waited struct {
		op  apiv1.Operation
		err error
	}
	done := make(chan waited, 1)
	go func() {
		got, err := c.Wait(t.Context(), apiv1.Operation{ID: uuid.UUID(op.ID).String(), State: apiv1.OpPending})
		done <- waited{got, err}
	}()
	// The runner ends the deletion, of a user that does not exist, on its
	// turns, and the parked poll re-reads once a second, both on the node's
	// clock, so the clock is moved a second at a time until the wait answers.
	var w waited
	deadline := time.After(30 * time.Second)
	for answered := false; !answered; {
		select {
		case w = <-done:
			answered = true
		case <-deadline:
			t.Fatal("Wait did not answer within 30 s")
		case <-time.After(50 * time.Millisecond):
			s.h.Clock().Advance(time.Second)
		}
	}
	if w.err != nil || (w.op.State != apiv1.OpSucceeded && w.op.State != apiv1.OpFailed) {
		t.Errorf("Wait after the access token expired: %v, %v; want the operation ended", w.op.State, w.err)
	}
	if src.refreshes != 2 {
		t.Errorf("the Wait poll: %d refreshes in all, want 2", src.refreshes)
	}
}
