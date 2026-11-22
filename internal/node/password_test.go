package node_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	apiclient "github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/cubbit/dittofs/pkg/secrets"
)

const passwordPath = "/api/v1/auth/password"

// changePassword sends POST /auth/password bearing token.
func (s *authNode) changePassword(t *testing.T, token, current, next string) authReply {
	t.Helper()
	return s.send(t, http.MethodPost, passwordPath, token, apiv1.PasswordChange{Current: current, New: next})
}

// setPassword sends p's :set-password bearing token.
func (s *authNode) setPassword(t *testing.T, token string, p entity.Principal, password string) authReply {
	t.Helper()
	return s.send(t, http.MethodPost, userPath(p)+":set-password", token, apiv1.SetPassword{Password: password})
}

// fileAccount stores an account named name holding role, a uid and so a
// SID, and password as both its password hash and its NT hash, sealed under
// the storage and the protocol roles as the API seals them, and returns it.
func fileAccount(t *testing.T, s *authNode, name, password string, role entity.APIRole) entity.User {
	t.Helper()
	p := sealer(t, s.h)
	u := entity.User{Principal: entity.NewUser(), Name: name, Role: role, Password: entity.SecretRef(uuid.New()),
		NTHash: entity.SecretRef(uuid.New()), IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "60010"}}}
	h, err := identity.NewHashes(u, []byte(password), identity.NTLMSetting{On: true})
	if err != nil {
		t.Fatalf("NewHashes: %v", err)
	}
	id := u.Principal.ID
	seal(t, s.h, p, secrets.Storage, entity.SecretPassword, entity.UserSecretOwner(id, entity.UserPasswordKey), u.Password, h.Password)
	seal(t, s.h, p, secrets.Protocol, entity.SecretNTHash, entity.UserSecretOwner(id, entity.UserNTHashKey), u.NTHash, h.NTHash)
	if _, err := s.h.Store().ControlPlane().PutUser(t.Context(), u, 0, entity.Alloc{RID: true}); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return u
}

// versionOf is ref's current version.
func versionOf(t *testing.T, s *authNode, ref entity.SecretRef) uint64 {
	t.Helper()
	v, _ := held(t, s.h, ref, 0)
	return v
}

// A password change answers a new session: from then on the session that
// made it, and the tokens a refresh of it issued before the change, are
// refused, a refresh of them and a call; the answered session's access token
// is accepted and its refresh token refreshes. The old password signs in no
// more and the new one does.
func TestPasswordChangeEndsEarlierSessions(t *testing.T) {
	s := newAuthNode(t)
	s.h.Account(t, "pat", "pat's first password", entity.RoleViewer)
	first := s.tokens(t, "pat", "pat's first password")
	refreshed := s.decodeTokens(t, "a refresh before the change", s.refresh(t, first.Refresh))

	r := s.changePassword(t, refreshed.Access, "pat's first password", "pat's second password")
	answered := s.decodeTokens(t, "the change", r)
	if answered.MustChange {
		t.Error("the change's tokens say the password must still change")
	}

	wantStatus(t, "a refresh of the session that made the change", s.refresh(t, refreshed.Refresh), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	wantStatus(t, "a call with an access token a refresh issued before the change", s.list(t, refreshed.Access), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	if r := s.list(t, answered.Access); r.status != http.StatusOK {
		t.Errorf("a call with the change's access token: answered %d %s, want 200", r.status, r.body)
	}
	next := s.decodeTokens(t, "a refresh of the change's session", s.refresh(t, answered.Refresh))
	if r := s.list(t, next.Access); r.status != http.StatusOK {
		t.Errorf("a call after refreshing the change's session: answered %d %s, want 200", r.status, r.body)
	}
	wantStatus(t, "a sign-in with the old password", s.signIn(t, "pat", "pat's first password"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	s.tokens(t, "pat", "pat's second password")
}

// A change presents the current password, must-change or not: one that
// presents none, or a wrong one, is refused 401 and changes nothing.
func TestPasswordChangePresentsTheCurrent(t *testing.T) {
	s := newAuthNode(t)
	for _, mustChange := range []bool{false, true} {
		name := "pat"
		if mustChange {
			name = "fresh"
		}
		p := s.h.Account(t, name, name+"'s password", entity.RoleViewer)
		if mustChange {
			s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error {
				u.MustChange = true
				_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
				return err
			})
		}
		tk := s.tokens(t, name, name+"'s password")
		wantStatus(t, name+": a change with no current password", s.send(t, http.MethodPost, passwordPath, tk.Access, json.RawMessage(`{"new_password":"next"}`)),
			http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		wantStatus(t, name+": a change with a wrong current password", s.changePassword(t, tk.Access, "a guess", "next"),
			http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		if u := s.stored(t, p); u.MustChange != mustChange {
			t.Errorf("%s: must-change after the refused changes: %v, want %v", name, u.MustChange, mustChange)
		}
		s.tokens(t, name, name+"'s password")
	}
}

// A must-change account changes its password, and its new session then
// reaches every other route.
func TestMustChangeAccountChangesItsPassword(t *testing.T) {
	s := newAuthNode(t)
	p := s.h.Account(t, "fresh", "first password", entity.RoleAdmin)
	s.change(t, p, func(tx entity.ControlPlaneTx, u *entity.User) error {
		u.MustChange = true
		_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
		return err
	})
	tk := s.tokens(t, "fresh", "first password")
	wantStatus(t, "a call before the change", s.list(t, tk.Access), http.StatusForbidden, apiv1.CodePasswordChangeRequired)
	answered := s.decodeTokens(t, "the change", s.changePassword(t, tk.Access, "first password", "chosen password"))
	if answered.MustChange {
		t.Error("the change's tokens say the password must still change")
	}
	if r := s.list(t, answered.Access); r.status != http.StatusOK {
		t.Errorf("a call after the change: answered %d %s, want 200", r.status, r.body)
	}
	if r := s.send(t, http.MethodGet, "/api/v1/audit", answered.Access, nil); r.status != http.StatusOK {
		t.Errorf("an admin's read of the audit after the change: answered %d %s, want 200", r.status, r.body)
	}
	if u := s.stored(t, p); u.MustChange {
		t.Error("the account is still must-change after the change")
	}
}

// The check of the current password is a sign-in's attempt: three failed
// changes and two failed sign-ins from one address make five, and the next
// of either is 429, the change's with the right password too.
func TestPasswordChangeCountsAsSignIn(t *testing.T) {
	s := newAuthNode(t)
	s.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	tk := s.tokens(t, "pat", "pat's password")
	for i := range 3 {
		wantStatus(t, fmt.Sprint("failed change ", i+1), s.changePassword(t, tk.Access, "a guess", "next"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	}
	for i := range 2 {
		wantStatus(t, fmt.Sprint("failed sign-in ", i+1), s.signIn(t, "pat", "a guess"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	}
	wantStatus(t, "the sixth attempt, a change", s.changePassword(t, tk.Access, "pat's password", "next"), http.StatusTooManyRequests, apiv1.CodeThrottled)
	wantStatus(t, "the sixth attempt, a sign-in", s.signIn(t, "pat", "pat's password"), http.StatusTooManyRequests, apiv1.CodeThrottled)
}

// :set-password resets the account's own sign-in count, which failed
// sign-ins raised, and sets the password the next sign-in checks.
func TestSetPasswordResetsTheCount(t *testing.T) {
	s := newAuthNode(t)
	admin := s.tokens(t, "admin", adminPassword).Access
	p := s.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	for range 3 {
		s.signIn(t, "pat", "a guess")
	}
	count := func() (entity.SignInFailures, bool) {
		f, found, err := s.h.Store().ControlPlane().SignInFailures(t.Context(), entity.SignInKey{Account: p})
		if err != nil {
			t.Fatal(err)
		}
		return f, found
	}
	if _, found := count(); !found {
		t.Fatal("three failed sign-ins raised no count on the account")
	}
	if r := s.setPassword(t, admin, p, "set by the admin"); r.status != http.StatusOK {
		t.Fatalf(":set-password: answered %d %s, want 200", r.status, r.body)
	}
	if f, found := count(); found {
		t.Errorf("the account's count after :set-password: %+v, want none", f)
	}
	wantStatus(t, "a sign-in with the old password", s.signIn(t, "pat", "pat's password"), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
	s.tokens(t, "pat", "set by the admin")
}

// An operator sets the password of a user holding no role, and setting an
// admin's is refused 404, as the admin is hidden from it, leaving the
// admin's password as it was.
func TestOperatorCannotSetAnAdminsPassword(t *testing.T) {
	s := newAuthNode(t)
	_, op := s.h.SignedIn(t, s.addr, "op", entity.RoleOperator)
	plain := s.h.Account(t, "plain", "plain's password", entity.RoleNone)
	root := s.principalNamed(t, "admin")
	before := s.stored(t, root)
	version := versionOf(t, s, before.Password)
	wantStatus(t, "the operator setting the admin's password", s.setPassword(t, op.Access, root, "taken over"), http.StatusNotFound, codeNotFound)
	if after := s.stored(t, root); after.Generation != before.Generation || versionOf(t, s, after.Password) != version {
		t.Errorf("the admin after the refused set: generation %d, password version %d; want %d and %d unchanged",
			after.Generation, versionOf(t, s, after.Password), before.Generation, version)
	}
	s.tokens(t, "admin", adminPassword)
	if r := s.setPassword(t, op.Access, plain, "plain's new password"); r.status != http.StatusOK {
		t.Errorf("the operator setting a plain user's password: answered %d %s, want 200", r.status, r.body)
	}
}

// With NTLM on, a change of the password of a user holding a uid rewrites
// both hashes: the previous versions of the password hash and the NT hash
// are pruned, and identity's NTLMv2 sign-in, the NT hash's consumer, then
// verifies a response for the new password and refuses one for the old.
func TestPasswordChangeRewritesBothHashes(t *testing.T) {
	s := newAuthNode(t)
	u := fileAccount(t, s, "pat", "pat's first password", entity.RoleViewer)
	signedIn(t, s.h, "before the change", "pat", "pat's first password", u.Principal)
	tk := s.tokens(t, "pat", "pat's first password")
	s.decodeTokens(t, "the change", s.changePassword(t, tk.Access, "pat's first password", "pat's second password"))

	after := s.stored(t, u.Principal)
	for _, sec := range []struct {
		what string
		old  entity.SecretRef
		now  entity.SecretRef
	}{{"password hash", u.Password, after.Password}, {"NT hash", u.NTHash, after.NTHash}} {
		if sec.now != sec.old {
			t.Errorf("the %s's reference after the change: %v, want %v kept", sec.what, uuid.UUID(sec.now), uuid.UUID(sec.old))
		}
		if v := versionOf(t, s, sec.now); v != 2 {
			t.Errorf("the %s's current version: %d, want 2", sec.what, v)
		}
		if _, stored := held(t, s.h, sec.old, 1); stored {
			t.Errorf("the %s's previous version is still stored", sec.what)
		}
	}
	caughtUp(t, s.h)
	signedIn(t, s.h, "with the new password", "pat", "pat's second password", u.Principal)
	refusedSignIn(t, s.h, "with the old password", "pat", "pat's first password")
}

// holdSeal holds the first read of one secret's current version once armed,
// as a seal reads it before its write, until release is closed, and signals
// reached.
type holdSeal struct {
	bound   string
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (f *holdSeal) At(op string, key []byte) (time.Duration, int, error) {
	if op == kvtest.OpBefore && string(key) == f.bound && f.armed.CompareAndSwap(true, false) {
		close(f.reached)
		<-f.release
	}
	return 0, 0, nil
}

// A change held after it read identity.ntlm, on, and before its write, by a
// fault at its seal of the NT hash, while identity.ntlm is turned off: its
// write refuses the moved setting, and it seals again with no NT hash, so it
// answers its tokens and stores no NT hash, the user naming none and the old
// one's reference holding no new version.
func TestNTLMOffDuringAChangeStoresNoNTHash(t *testing.T) {
	var fault *kvtest.FaultKV
	s := newAuthNode(t, withKV(func(inner kv.KV) kv.KV {
		fault = kvtest.NewFaultKV(inner)
		return fault
	}))
	u := fileAccount(t, s, "pat", "pat's first password", entity.RoleViewer)
	tk := s.tokens(t, "pat", "pat's first password")
	bound := append(keys.ID([]byte{keys.KindSEC}, u.NTHash[:]), bytes.Repeat([]byte{0xff}, 9)...)
	f := &holdSeal{bound: keys.Describe(bound), reached: make(chan struct{}), release: make(chan struct{})}
	f.armed.Store(true)
	fault.SetFault(f)
	var once sync.Once
	release := func() { once.Do(func() { close(f.release) }) }
	t.Cleanup(release)

	type reply struct {
		r   authReply
		err error
	}
	answered := make(chan reply, 1)
	go func() {
		r, err := s.try(t.Context(), http.MethodPost, passwordPath, tk.Access, apiv1.PasswordChange{Current: "pat's first password", New: "pat's second password"})
		answered <- reply{r, err}
	}()
	select {
	case <-f.reached:
	case a := <-answered:
		t.Fatalf("the change answered %d %s (%v) before it sealed the NT hash", a.r.status, a.r.body, a.err)
	case <-time.After(time.Minute):
		t.Fatal("the change did not reach its seal of the NT hash")
	}
	putSetting(t, s.h, "identity.ntlm", `false`)
	release()
	a := <-answered
	if a.err != nil {
		t.Fatal(a.err)
	}
	s.decodeTokens(t, "the change across the turn-off", a.r)
	after := s.stored(t, u.Principal)
	if after.NTHash != (entity.SecretRef{}) {
		t.Errorf("the user names NT hash %v after the change, want none", uuid.UUID(after.NTHash))
	}
	if v := versionOf(t, s, u.NTHash); v != 1 {
		t.Errorf("the old NT hash's current version: %d, want 1, no NT hash written", v)
	}
	if v := versionOf(t, s, after.Password); v != 2 {
		t.Errorf("the password hash's current version: %d, want 2", v)
	}
}

// Neither the old password nor the new one, of a change or of a
// :set-password, is held by any key or value of the store or by the log.
func TestPasswordsReachNeitherStoreNorLog(t *testing.T) {
	var log syncBuffer
	var raw kv.KV
	s := newAuthNode(t, nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		raw = cfg.KV
		cfg.Logger = slog.New(slog.NewMultiHandler(cfg.Logger.Handler(), slog.NewTextHandler(&log, nil)))
		return node.New(cfg)
	}))
	admin := s.tokens(t, "admin", adminPassword).Access
	p := s.h.Account(t, "pat", "pat-first-1d0c5", entity.RoleViewer)
	tk := s.tokens(t, "pat", "pat-first-1d0c5")
	s.decodeTokens(t, "the change", s.changePassword(t, tk.Access, "pat-first-1d0c5", "pat-second-8e2f1"))
	if r := s.setPassword(t, admin, p, "pat-third-4b7a9"); r.status != http.StatusOK {
		t.Fatalf(":set-password: answered %d %s, want 200", r.status, r.body)
	}
	var stored [][]byte
	err := raw.View(t.Context(), func(r kv.Reader) error {
		for kvp, err := range r.Scan(nil, nil) {
			if err != nil {
				return err
			}
			stored = append(stored, slices.Clone(kvp.Key), slices.Clone(kvp.Value))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range []string{"pat-first-1d0c5", "pat-second-8e2f1", "pat-third-4b7a9"} {
		if slices.ContainsFunc(stored, func(b []byte) bool { return bytes.Contains(b, []byte(pw)) }) {
			t.Errorf("the store holds the password %q", pw)
		}
		if strings.Contains(log.String(), pw) {
			t.Errorf("the log holds the password %q", pw)
		}
	}
}

// The client's ChangePassword sends the token its source holds and returns
// a refused current password's 401 as it is, refreshing nothing; with the
// right one it answers the new session's tokens. SetPassword sets a user's
// password, which the next sign-in checks.
func TestClientChangesAndSetsPasswords(t *testing.T) {
	s := newAuthNode(t)
	p := s.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	bare, err := apiclient.New("http://" + s.addr)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := bare.SignIn(t.Context(), "pat", []byte("pat's password"))
	if err != nil {
		t.Fatal(err)
	}
	src := &session{c: bare, tk: tk}
	c, err := apiclient.New("http://"+s.addr, apiclient.WithTokenSource(src))
	if err != nil {
		t.Fatal(err)
	}
	var pr *apiv1.Problem
	if _, err := c.ChangePassword(t.Context(), []byte("a guess"), []byte("next")); !errors.As(err, &pr) || pr.Code != apiv1.CodeUnauthenticated || src.refreshes != 0 {
		t.Errorf("ChangePassword with a wrong current password: %v after %d refreshes, want the 401 unauthenticated and none", err, src.refreshes)
	}
	answered, err := c.ChangePassword(t.Context(), []byte("pat's password"), []byte("pat's next password"))
	if err != nil || answered.Access == "" || answered.Refresh == "" {
		t.Fatalf("ChangePassword: %+v, %v; want both tokens", answered, err)
	}
	admin, err := bare.SignIn(t.Context(), "admin", []byte(adminPassword))
	if err != nil {
		t.Fatal(err)
	}
	ac, err := apiclient.New("http://"+s.addr, apiclient.WithTokenSource(&session{c: bare, tk: admin}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.Users().SetPassword(t.Context(), uuid.UUID(p.ID).String(), []byte("set by the admin")); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := bare.SignIn(t.Context(), "pat", []byte("set by the admin")); err != nil {
		t.Errorf("a sign-in with the password SetPassword set: %v", err)
	}
}
