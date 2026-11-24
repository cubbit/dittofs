package clisuite

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// principalRows are the rows of the user, group, netgroup, token and
// installation commands.
func principalRows() []row {
	return []row{
		{name: "user create, update and update --email \"\" set, keep and clear the address", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			name := unique("u")
			uid := uniqueID()
			mustRun(t, run, "user", "create", name, "--uid", fmt.Sprint(uid), "--email", name+"@example.com", "--role", "viewer")
			u := userNamed(t, c, name)
			if u.Spec.Email != name+"@example.com" || u.Spec.UID == nil || *u.Spec.UID != uid || u.Spec.Role != apiv1.RoleViewer {
				t.Fatalf("user create %s: read back email %q uid %s role %q, want %s@example.com, %d and viewer", name, u.Spec.Email, id32(u.Spec.UID), u.Spec.Role, name, uid)
			}
			mustRun(t, run, "user", "update", name, "--disabled", "--uid", fmt.Sprint(uid+1), "--role", "")
			if u = userNamed(t, c, name); !u.Spec.Disabled || u.Spec.UID == nil || *u.Spec.UID != uid+1 || u.Spec.Role != "" || u.Spec.Email != name+"@example.com" {
				t.Errorf("user update --disabled --uid %d --role \"\": read back disabled %v uid %s role %q email %q, want true, %d, none and the address kept",
					uid+1, u.Spec.Disabled, id32(u.Spec.UID), u.Spec.Role, u.Spec.Email, uid+1)
			}
			mustRun(t, run, "user", "update", name, "--email", "")
			if u = userNamed(t, c, name); u.Spec.Email != "" || !u.Spec.Disabled {
				t.Errorf("user update --email \"\": read back email %q disabled %v, want no address and the rest kept", u.Spec.Email, u.Spec.Disabled)
			}
		}},
		{name: "user get and list print the user", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			name := unique("u")
			mustRun(t, run, "user", "create", name)
			want := userNamed(t, c, name)
			got := decodeAs[apiv1.User](t, mustRun(t, run, "user", "get", "--id", want.ID, "-o", "json"))
			if got.ID != want.ID || got.Spec.Name != name {
				t.Errorf("user get --id %s: got %+v, want user %s", want.ID, got, name)
			}
			if out := mustRun(t, run, "user", "list"); !strings.Contains(out, want.ID) {
				t.Errorf("user list: got %q, want a row for %s", out, want.ID)
			}
		}},
		{name: "user set-password sets the password the user signs in with", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			name := unique("u")
			mustRun(t, run, "user", "create", name, "--role", "viewer")
			pw := secretFile(t, "a password set by dfsctl")
			// A key sent on :set-password is refused, so its success is its
			// absence.
			mustRun(t, run, "user", "set-password", name, "--password-file", pw)
			sc, err := cmdutil.Dial(s.Addr, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sc.SignIn(t.Context(), name, []byte("a password set by dfsctl")); err != nil {
				t.Errorf("sign %s in with the password set-password set: %v", name, err)
			}
		}},
		{name: "user delete --wait ends the deletion and the user is gone", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			name := unique("u")
			mustRun(t, run, "user", "create", name)
			out, errOut, status := s.awaitRun(t, run, "user", "delete", name, "--force", "--wait", "-o", "json")
			if status != 0 {
				t.Fatalf("user delete %s --wait: status %d, stderr %q, want 0", name, status, errOut)
			}
			if op := decodeAs[apiv1.Operation](t, out); op.State != apiv1.OpSucceeded {
				t.Errorf("user delete %s --wait: printed %+v, want its deletion succeeded", name, op)
			}
			if _, errOut, status := run(t.Context(), "user", "get", name); status == 0 || !strings.Contains(errOut, fmt.Sprintf("no user named %q", name)) {
				t.Errorf("user get %s after its deletion: status %d, stderr %q, want no user of that name", name, status, errOut)
			}
		}},
		{name: "group create, update and delete --wait", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			name := unique("g")
			gid := uniqueID()
			mustRun(t, run, "group", "create", name, "--gid", fmt.Sprint(gid))
			mustRun(t, run, "group", "update", name, "--gid", fmt.Sprint(gid+1))
			g := decodeAs[apiv1.Group](t, mustRun(t, run, "group", "get", name, "-o", "json"))
			if g.Spec.GID == nil || *g.Spec.GID != gid+1 {
				t.Errorf("group get %s after update --gid %d: gid %s, want %d", name, gid+1, id32(g.Spec.GID), gid+1)
			}
			if out := mustRun(t, run, "group", "list"); !strings.Contains(out, g.ID) {
				t.Errorf("group list: got %q, want a row for %s", out, g.ID)
			}
			out, errOut, status := s.awaitRun(t, run, "group", "delete", name, "--force", "--wait", "-o", "json")
			if status != 0 {
				t.Fatalf("group delete %s --wait: status %d, stderr %q, want 0", name, status, errOut)
			}
			if op := decodeAs[apiv1.Operation](t, out); op.State != apiv1.OpSucceeded {
				t.Errorf("group delete %s --wait: printed %+v, want its deletion succeeded", name, op)
			}
			if _, errOut, status := run(t.Context(), "group", "get", name); status == 0 || !strings.Contains(errOut, fmt.Sprintf("no group named %q", name)) {
				t.Errorf("group get %s after its deletion: status %d, stderr %q, want no group of that name", name, status, errOut)
			}
		}},
		{name: "group add-member adds the user once and remove-member removes it once", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			g, u := unique("g"), unique("u")
			mustRun(t, run, "group", "create", g)
			mustRun(t, run, "user", "create", u)
			mustRun(t, run, "group", "add-member", g, u)
			// A membership has no read route, so it is read back as the API
			// answers a second add or removal: refused 412 while it exists, and
			// once it is gone.
			already := fmt.Sprintf("user %q is already a member of group %q", u, g)
			if _, errOut, status := run(t.Context(), "group", "add-member", g, u); status == 0 || !strings.Contains(errOut, already) {
				t.Errorf("group add-member %s %s a second time: status %d, stderr %q, want %q", g, u, status, errOut, already)
			}
			mustRun(t, run, "group", "remove-member", g, u)
			notMember := fmt.Sprintf("user %q is not a member of group %q", u, g)
			if _, errOut, status := run(t.Context(), "group", "remove-member", g, u); status == 0 || !strings.Contains(errOut, notMember) {
				t.Errorf("group remove-member %s %s a second time: status %d, stderr %q, want %q", g, u, status, errOut, notMember)
			}
			mustRun(t, run, "group", "add-member", g, u)
		}},
		{name: "netgroup create, update and delete", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			name := unique("n")
			mustRun(t, run, "netgroup", "create", name, "--member", "10.0.0.7", "--member", "10.1.0.0/16")
			got := decodeAs[apiv1.Netgroup](t, mustRun(t, run, "netgroup", "get", name, "-o", "json"))
			if want := []string{"10.0.0.7", "10.1.0.0/16"}; got.ID != name || !slices.Equal(got.Spec.Members, want) {
				t.Errorf("netgroup get %s: got %+v, want members %q", name, got, want)
			}
			if _, errOut, status := run(t.Context(), "netgroup", "update", name); status == 0 || !strings.Contains(errOut, "member") {
				t.Errorf("netgroup update %s with no --member: status %d, stderr %q, want it refused naming --member", name, status, errOut)
			}
			mustRun(t, run, "netgroup", "update", name, "--member", "build1.example.com")
			if n, err := c.Netgroups().Get(t.Context(), name); err != nil || !slices.Equal(n.Spec.Members, []string{"build1.example.com"}) {
				t.Errorf("netgroup %s after update: %+v, %v, want only build1.example.com", name, n.Spec.Members, err)
			}
			if out := mustRun(t, run, "netgroup", "list"); !strings.Contains(out, name) {
				t.Errorf("netgroup list: got %q, want a row for %s", out, name)
			}
			mustRun(t, run, "netgroup", "delete", name, "--force")
			if _, err := c.Netgroups().Get(t.Context(), name); !isStatus(err, http.StatusNotFound) {
				t.Errorf("netgroup %s after delete: read answered %v, want 404", name, err)
			}
		}},
		{name: "token create signs a context in through login --token-file until token revoke", run: func(t *testing.T, _ Runner, s Server, c *client.Client) {
			acct := s.account(t, c, apiv1.RoleViewer)
			run := s.newRunner(t)
			acct.Login(t, run)
			// API-token creation refuses an idempotency key, so its success
			// is its absence.
			tok := strings.TrimSpace(mustRun(t, run, "token", "create", "--expires-in", "1h"))
			if tok == "" {
				t.Fatal("token create: printed no token")
			}
			withToken := s.newRunner(t)
			mustRun(t, withToken, "login", "--server", s.Addr, "--token-file", secretFile(t, tok+"\n"))
			mustRun(t, withToken, "group", "list")
			if _, errOut, status := withToken(t.Context(), "token", "list"); status == 0 || !strings.Contains(errOut, "--user") {
				t.Errorf("token list in a context signed in with an API token: status %d, stderr %q, want it refused naming --user", status, errOut)
			}
			ts := decodeAs[[]apiv1.APIToken](t, mustRun(t, run, "token", "list", "-o", "json"))
			if len(ts) != 1 || ts[0].Status.Token != "" {
				t.Fatalf("token list: got %+v, want the one token, not showing it", ts)
			}
			if got, want := ts[0].Spec.Expires, ts[0].Status.Created.Add(time.Hour); got.Before(want.Add(-time.Minute)) || got.After(want.Add(time.Minute)) {
				t.Errorf("token create --expires-in 1h: expires %v, want an hour after its creation, %v", got, want)
			}
			mustRun(t, run, "token", "revoke", ts[0].ID)
			none := "no API token " + ts[0].ID + " for this account"
			if _, errOut, status := run(t.Context(), "token", "revoke", ts[0].ID); status == 0 || !strings.Contains(errOut, none) {
				t.Errorf("token revoke %s a second time: status %d, stderr %q, want %q", ts[0].ID, status, errOut, none)
			}
			if _, errOut, status := withToken(t.Context(), "group", "list"); status == 0 || !strings.Contains(errOut, string(apiv1.CodeUnauthenticated)) {
				t.Errorf("group list with the revoked token: status %d, stderr %q, want it refused %s", status, errOut, apiv1.CodeUnauthenticated)
			}
		}},
		{name: "token create refuses a lifetime not positive, and one past the maximum naming the clock", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			for in, shown := range map[string]string{"0s": "0s", "-1h": "-1h0m0s"} {
				want := "--expires-in " + shown + ": a token's lifetime must be positive"
				if _, errOut, status := run(t.Context(), "token", "create", "--expires-in", in); status == 0 || !strings.Contains(errOut, want) {
					t.Errorf("token create --expires-in %s: status %d, stderr %q, want %q", in, status, errOut, want)
				}
			}
			// A day past the default api.token_max_lifetime of 365 days.
			want := "--expires-in 8784h0m0s: the server refused the expiry it gives, which must fall after the server's time and within its api.token_max_lifetime; this machine's clock"
			if _, errOut, status := run(t.Context(), "token", "create", "--expires-in", "8784h"); status == 0 || !strings.Contains(errOut, want) {
				t.Errorf("token create --expires-in 8784h: status %d, stderr %q, want %q", status, errOut, want)
			}
		}},
		{name: "token create --user makes an admin a service account's token", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			svc := unique("svc")
			if _, err := c.Users().Create(t.Context(), apiv1.UserSpec{Name: svc, Role: apiv1.RoleViewer}); err != nil {
				t.Fatalf("create the service account %s: %v", svc, err)
			}
			created := decodeAs[apiv1.APIToken](t, mustRun(t, run, "token", "create", "--user", svc, "-o", "json"))
			if created.Status.Token == "" {
				t.Fatalf("token create --user %s -o json: got %+v, want the token in its status", svc, created)
			}
			ts := decodeAs[[]apiv1.APIToken](t, mustRun(t, run, "token", "list", "--user", svc, "-o", "json"))
			if len(ts) != 1 || ts[0].ID != created.ID {
				t.Errorf("token list --user %s: got %+v, want token %s", svc, ts, created.ID)
			}
		}},
		{name: "installation rotate-signing-key --wait ends succeeded", harnessOnly: true, run: func(t *testing.T, run Runner, s Server, _ *client.Client) {
			// The rotation ends once the tokens the old key signed have
			// expired, 15 minutes of the node's clock after its switch, which
			// Await reaches a second a poll. A token the command refreshes is
			// issued at the store's time, which trails the clock while its
			// ceiling waits on a real one-second ticker; a raise stalled for
			// about nine real seconds, a hundred clock seconds each, would leave
			// it expired on arrival, so that stall bounds this row.
			a := Await(t, s, "the signing key's rotation", start(t, run, "installation", "rotate-signing-key", "--wait", "-o", "json"))
			if a.status != 0 {
				t.Fatalf("installation rotate-signing-key --wait: status %d, stderr %q, want 0", a.status, a.errOut)
			}
			if op := decodeAs[apiv1.Operation](t, a.out); op.Kind != apiv1.OpRotateSigningKey || op.State != apiv1.OpSucceeded {
				t.Errorf("installation rotate-signing-key --wait: printed %+v, want a %s operation succeeded", op, apiv1.OpRotateSigningKey)
			}
		}},
	}
}

// unique is a name starting with prefix that no other row's names.
func unique(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// uniqueID is a uid or gid, an even one in [100000, 2^31), that no other
// row names but by a collision of 30 random bits, so a rerun against a
// server that keeps its users finds it free.
func uniqueID() uint32 {
	u := uuid.New()
	return 100000 + (binary.BigEndian.Uint32(u[:4])%(1<<30))*2
}

// id32 is an optional uid or gid as text, "none" when it is absent.
func id32(p *uint32) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// isStatus reports whether err is a problem the server answered with status.
func isStatus(err error, status int) bool {
	var p *apiv1.Problem
	return errors.As(err, &p) && p.Status == status
}

// userNamed reads the user named name through c.
func userNamed(t *testing.T, c *client.Client, name string) apiv1.User {
	t.Helper()
	for u, err := range c.Users().List(t.Context(), map[string][]string{"name": {name}}) {
		if err != nil {
			t.Fatalf("list the user named %s: %v", name, err)
		}
		return u
	}
	t.Fatalf("no user named %s", name)
	return apiv1.User{}
}

// secretFile is a file under the test's directory holding secret.
func secretFile(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// account creates through c an account of role with a password of its own
// and returns s signed in as it.
func (s Server) account(t *testing.T, c *client.Client, role apiv1.Role) Server {
	t.Helper()
	a := s
	a.User, a.Password = unique("a"), "the account's password"
	res, err := c.Users().Create(t.Context(), apiv1.UserSpec{Name: a.User, Role: role})
	if err != nil || res.Resource == nil {
		t.Fatalf("create account %s: %+v, %v", a.User, res, err)
	}
	if err := c.Users().SetPassword(t.Context(), res.Resource.ID, []byte(a.Password)); err != nil {
		t.Fatalf("set %s's password: %v", a.User, err)
	}
	return a
}

// answered is what one dfsctl invocation answered.
type answered struct {
	out, errOut string
	status      int
}

// start runs args through run in the background and answers on the
// channel it returns.
func start(t *testing.T, run Runner, args ...string) <-chan answered {
	done := make(chan answered, 1)
	go func() {
		out, errOut, status := run(t.Context(), args...)
		done <- answered{out, errOut, status}
	}()
	return done
}

// awaitRun runs args through run, which waits on an operation, while Await
// moves the node's clock, and returns what it answered.
func (s Server) awaitRun(t *testing.T, run Runner, args ...string) (out, errOut string, status int) {
	t.Helper()
	a := Await(t, s, "dfsctl "+strings.Join(args, " "), start(t, run, args...))
	return a.out, a.errOut, a.status
}

// decodeAs reads out, printed as -o json, as a T.
func decodeAs[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return v
}
