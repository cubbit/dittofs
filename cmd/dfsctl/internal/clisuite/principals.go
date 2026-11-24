package clisuite

import (
	"encoding/json"
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
			mustRun(t, run, "user", "create", name, "--uid", "41000", "--email", name+"@example.com")
			u := userNamed(t, c, name)
			if u.Spec.Email != name+"@example.com" || u.Spec.UID == nil || *u.Spec.UID != 41000 {
				t.Fatalf("user create %s: read back email %q uid %v, want %s@example.com and 41000", name, u.Spec.Email, u.Spec.UID, name)
			}
			mustRun(t, run, "user", "update", name, "--disabled")
			if u = userNamed(t, c, name); !u.Spec.Disabled || u.Spec.Email != name+"@example.com" {
				t.Errorf("user update --disabled: read back disabled %v email %q, want true and the address kept", u.Spec.Disabled, u.Spec.Email)
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
			if _, errOut, status := s.awaitRun(t, run, "user", "delete", name, "--force", "--wait"); status != 0 {
				t.Fatalf("user delete %s --wait: status %d, stderr %q, want 0", name, status, errOut)
			}
			if _, errOut, status := run(t.Context(), "user", "get", name); status == 0 || !strings.Contains(errOut, name) {
				t.Errorf("user get %s after its deletion: status %d, stderr %q, want it refused naming it", name, status, errOut)
			}
		}},
		{name: "group create, update and delete --wait", run: func(t *testing.T, run Runner, s Server, c *client.Client) {
			name := unique("g")
			mustRun(t, run, "group", "create", name, "--gid", "42000")
			mustRun(t, run, "group", "update", name, "--gid", "42001")
			g := decodeAs[apiv1.Group](t, mustRun(t, run, "group", "get", name, "-o", "json"))
			if g.Spec.GID == nil || *g.Spec.GID != 42001 {
				t.Errorf("group get %s after update --gid 42001: gid %v, want 42001", name, g.Spec.GID)
			}
			if _, errOut, status := s.awaitRun(t, run, "group", "delete", name, "--force", "--wait"); status != 0 {
				t.Fatalf("group delete %s --wait: status %d, stderr %q, want 0", name, status, errOut)
			}
			if _, _, status := run(t.Context(), "group", "get", name); status == 0 {
				t.Errorf("group get %s after its deletion: status 0, want it refused", name)
			}
		}},
		{name: "group add-member adds the user once and remove-member removes it once", run: func(t *testing.T, run Runner, _ Server, c *client.Client) {
			g, u := unique("g"), unique("u")
			mustRun(t, run, "group", "create", g)
			mustRun(t, run, "user", "create", u)
			mustRun(t, run, "group", "add-member", g, u)
			// The membership is read back as the API answers a second add of
			// it, refused while it exists.
			if _, _, status := run(t.Context(), "group", "add-member", g, u); status == 0 {
				t.Errorf("group add-member %s %s a second time: status 0, want it refused as a member already", g, u)
			}
			mustRun(t, run, "group", "remove-member", g, u)
			if _, _, status := run(t.Context(), "group", "remove-member", g, u); status == 0 {
				t.Errorf("group remove-member %s %s a second time: status 0, want it refused as no member", g, u)
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
			mustRun(t, run, "netgroup", "update", name, "--member", "build1.example.com")
			if n, err := c.Netgroups().Get(t.Context(), name); err != nil || !slices.Equal(n.Spec.Members, []string{"build1.example.com"}) {
				t.Errorf("netgroup %s after update: %+v, %v, want only build1.example.com", name, n.Spec.Members, err)
			}
			mustRun(t, run, "netgroup", "delete", name, "--force")
			if _, err := c.Netgroups().Get(t.Context(), name); err == nil {
				t.Errorf("netgroup %s after delete: still read", name)
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
			mustRun(t, run, "token", "revoke", ts[0].ID)
			if _, errOut, status := withToken(t.Context(), "group", "list"); status == 0 {
				t.Errorf("group list with the revoked token: status 0, stderr %q, want it refused", errOut)
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
			done := start(t, run, "installation", "rotate-signing-key", "--wait", "-o", "json")
			// The rotation ends once the tokens the old key signed have
			// expired, 15 minutes of the node's clock after its switch, so
			// the clock moves five seconds a poll. No more: the store's time,
			// which a refreshed token is issued at, follows the clock up to a
			// real second late, and a hundred polls of a larger step would
			// leave a token the command refreshes expired on arrival.
			var a answered
			for deadline, ended := time.After(time.Minute), false; !ended; {
				select {
				case a = <-done:
					ended = true
				case <-deadline:
					t.Fatal("installation rotate-signing-key --wait: not ended a minute on")
				case <-time.After(10 * time.Millisecond):
					s.Harness.Clock().Advance(5 * time.Second)
				}
			}
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
