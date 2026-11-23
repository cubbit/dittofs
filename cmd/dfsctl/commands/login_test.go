package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// childConfig, set in a process's environment, makes the test binary a
// dfsctl process instead: it reads the node's health through the current
// context of the credentials file it names, as every command reaches the
// API, and exits 0 if it was answered.
const childConfig = "DFSCTL_TEST_CHILD_CONFIG"

func TestMain(m *testing.M) {
	if path := os.Getenv(childConfig); path != "" {
		err := health(context.Background(), cmdutil.Env{Err: os.Stderr, ConfigPath: path})
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// health reads the node's health through env's current context.
func health(ctx context.Context, env cmdutil.Env) error {
	c, _, err := cmdutil.NewClient(env)
	if err != nil {
		return err
	}
	_, err = c.Health(ctx)
	return err
}

// accessLife is how long the node's access tokens last; a test advancing its
// clock past it makes the next call refresh.
const accessLife = 15 * time.Minute

// testNode is a started node serving its API on a local socket.
type testNode struct {
	h      *nodetest.Harness
	socket string
}

// startNode starts a node over an in-memory store, its API on a socket of
// its own, stopped when the test ends.
func startNode(t *testing.T) *testNode {
	t.Helper()
	// Not t.TempDir(), whose path can pass the length a socket's may have.
	dir, err := os.MkdirTemp("", "dfsctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")
	h := nodetest.New(t, nodetest.InMemory(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.API.Socket = path
		return node.New(cfg)
	}))
	if err := h.Node().Start(t.Context()); err != nil {
		t.Fatalf("start the node: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("stop the node: %v", err)
		}
	})
	return &testNode{h: h, socket: path}
}

// expire advances the node's clock past the access token's lifetime and
// waits until the store's time has reached it, which the store raises on
// real time about once a second: a session refreshed before then would be
// issued tokens already expired.
func (n *testNode) expire(t *testing.T) {
	t.Helper()
	n.h.Clock().Advance(accessLife + time.Minute)
	at := n.h.Clock().Now()
	// Polling the store's time is the wait: no clock the test holds moves it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		a := &entity.AuditRecord{Target: entity.ResourceRef{Kind: entity.ResourceShare, ID: []byte("store time")}}
		if err := n.h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{Audit: a}, func(entity.ControlPlaneTx) error { return nil }); err != nil {
			t.Fatalf("read the store's time: %v", err)
		}
		if !a.Time.Before(at) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store's time did not reach %v within 10 s", at)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newEnv is an invocation's environment over a credentials file of the
// test's own, with stdin as its input.
func newEnv(t *testing.T, stdin string, interactive bool) cmdutil.Env {
	t.Helper()
	return cmdutil.Env{
		In: strings.NewReader(stdin), Out: io.Discard, Err: io.Discard,
		ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json"), Interactive: interactive,
	}
}

// dfsctl runs one invocation in env and returns its stdout, its stderr and
// its error.
func dfsctl(env cmdutil.Env, args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	env.Out, env.Err = &out, &errOut
	root := NewRoot(env)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

// mustRun is dfsctl failing the test on an error.
func mustRun(t *testing.T, env cmdutil.Env, args ...string) string {
	t.Helper()
	out, errOut, err := dfsctl(env, args...)
	if err != nil {
		t.Fatalf("dfsctl %s: %v (stderr %q)", strings.Join(args, " "), err, errOut)
	}
	return out
}

// file writes content to a file under the test's temporary directory and
// returns its path.
func file(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stored is the context name as env's credentials file holds it.
func stored(t *testing.T, env cmdutil.Env, name string) credentials.Context {
	t.Helper()
	s, err := credentials.NewStoreAt(env.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.GetContext(name)
	if err != nil {
		t.Fatalf("context %q: %v", name, err)
	}
	return *c
}

// login signs name in with password to n's socket, under the context
// "default".
func (n *testNode) login(t *testing.T, env cmdutil.Env, name, password string) credentials.Context {
	t.Helper()
	mustRun(t, env, "login", "--server", n.socket, "-u", name, "--password-file", file(t, "password", password+"\n"))
	c := stored(t, env, "default")
	if c.ServerURL != n.socket || c.Username != name || c.AccessToken == "" || c.RefreshToken == "" {
		t.Fatalf("the context after login: %+v, want the socket %s, %s and both tokens", c, n.socket, name)
	}
	return c
}

// After the access token's lifetime a command still succeeds, refreshing the
// session, and the file holds the new pair; the context names the node's
// local socket throughout.
func TestCommandAfterExpiryRefreshes(t *testing.T) {
	n := startNode(t)
	n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	env := newEnv(t, "", false)
	before := n.login(t, env, "pat", "pat's password")

	n.expire(t)
	if err := health(t.Context(), env); err != nil {
		t.Fatalf("a command after the access token expired: %v, want it refreshed and answered", err)
	}
	after := stored(t, env, "default")
	if after.RefreshToken == before.RefreshToken || after.AccessToken == before.AccessToken {
		t.Errorf("the tokens after the refresh: unchanged, want the new pair saved")
	}
}

// Two dfsctl processes refreshing one context at once both succeed, one
// refreshing and the other using its tokens, and the session survives: a
// third command, after the access token expires again, refreshes with the
// refresh token the file holds. The test holds the lock until both
// processes are waiting on it, so both read the expired token first.
func TestConcurrentRefreshKeepsTheSession(t *testing.T) {
	n := startNode(t)
	n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	env := newEnv(t, "", false)
	n.login(t, env, "pat", "pat's password")
	n.expire(t)

	lock := env.ConfigPath + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	waiting := make(chan struct{}, 2)
	type child struct {
		cmd    *exec.Cmd
		stderr bytes.Buffer
		done   chan struct{}
	}
	children := make([]*child, 2)
	for i := range children {
		c := &child{cmd: exec.CommandContext(ctx, os.Args[0]), done: make(chan struct{})}
		c.cmd.Env = append(os.Environ(), childConfig+"="+env.ConfigPath)
		pipe, err := c.cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.cmd.Start(); err != nil {
			t.Fatalf("start dfsctl %d: %v", i, err)
		}
		go func() {
			defer close(c.done)
			sc := bufio.NewScanner(pipe)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "waiting for "+lock) {
					waiting <- struct{}{}
				}
				c.stderr.WriteString(sc.Text() + "\n")
			}
		}()
		children[i] = c
	}
	for range children {
		select {
		case <-waiting:
		case <-ctx.Done():
			t.Fatalf("waiting for both processes to wait on %s: %v", lock, ctx.Err())
		}
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	for i, c := range children {
		<-c.done
		if err := c.cmd.Wait(); err != nil {
			t.Errorf("dfsctl %d: %v, want it answered (stderr %q)", i, err, c.stderr.String())
		}
	}

	n.expire(t)
	if err := health(t.Context(), env); err != nil {
		t.Errorf("a third command after both refreshed: %v, want the session alive", err)
	}
}

// A context over TLS keeps the CA bundle --cacert names and its commands
// trust it; without the bundle the server's certificate is refused.
func TestLoginWithCACert(t *testing.T) {
	n := startNode(t)
	n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", n.socket)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	front := httptest.NewTLSServer(&httputil.ReverseProxy{Transport: tr, Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme, r.Out.URL.Host = "http", "node"
	}})
	t.Cleanup(front.Close)
	ca := file(t, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw})))
	pw := file(t, "password", "pat's password")

	env := newEnv(t, "", false)
	if _, _, err := dfsctl(env, "login", "--server", front.URL, "-u", "pat", "--password-file", pw); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Errorf("login over TLS without --cacert: %v, want the certificate refused", err)
	}
	mustRun(t, env, "login", "--server", front.URL, "-u", "pat", "--password-file", pw, "--cacert", ca)
	if c := stored(t, env, "default"); c.CACert != ca || c.ServerURL != front.URL {
		t.Errorf("the context: server %q, CA %q, want %q and %q", c.ServerURL, c.CACert, front.URL, ca)
	}
	if err := health(t.Context(), env); err != nil {
		t.Errorf("a command in the TLS context: %v, want it answered under the stored CA", err)
	}
}

// A must-change account's commands are refused password_change_required
// until password change, which the account reads from prompts on one
// reader; then they succeed, and the new password signs in.
func TestMustChangeThenPasswordChange(t *testing.T) {
	n := startNode(t)
	p := n.h.Account(t, "fresh", "first password", entity.RoleViewer)
	err := n.h.Store().ControlPlane().Write(t.Context(), entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
		u, ok, err := tx.User(p)
		if err != nil || !ok {
			return errors.Join(err, errors.New("no such user"))
		}
		u.MustChange = true
		_, err = tx.PutUser(u, u.Generation, entity.Alloc{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	env := newEnv(t, "first password\nsecond password\nsecond password\n", true)
	_, errOut, err := dfsctl(env, "login", "--server", n.socket, "-u", "fresh", "--password-file", file(t, "pw", "first password"))
	if err != nil || !strings.Contains(errOut, "dfsctl password change") {
		t.Fatalf("login of a must-change account: %v, stderr %q, want it signed in and told to change", err, errOut)
	}
	if err := health(t.Context(), env); err == nil || !strings.Contains(err.Error(), string(apiv1.CodePasswordChangeRequired)) {
		t.Fatalf("a command before the change: %v, want %s", err, apiv1.CodePasswordChangeRequired)
	}
	before := stored(t, env, "default")
	mustRun(t, env, "password", "change")
	if after := stored(t, env, "default"); after.RefreshToken == before.RefreshToken {
		t.Error("the refresh token after the change: unchanged, want the change's session saved")
	}
	if err := health(t.Context(), env); err != nil {
		t.Errorf("a command after the change: %v, want it answered", err)
	}
	n.login(t, newEnv(t, "", false), "fresh", "second password")
}

// password change refuses two new passwords that differ, changing nothing.
func TestPasswordChangeConfirms(t *testing.T) {
	n := startNode(t)
	n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	env := newEnv(t, "pat's password\nnext one\nnext two\n", true)
	n.login(t, env, "pat", "pat's password")
	if _, _, err := dfsctl(env, "password", "change"); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("password change with differing new passwords: %v, want it refused", err)
	}
	n.login(t, newEnv(t, "", false), "pat", "pat's password")
}

// Without a person at stdin, login and password change refuse to prompt,
// naming the flag that answers the prompt.
func TestNoPromptWithoutATerminal(t *testing.T) {
	n := startNode(t)
	n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	env := newEnv(t, "pat's password\n", false)
	if _, _, err := dfsctl(env, "login", "--server", n.socket, "-u", "pat"); err == nil || !strings.Contains(err.Error(), "--password-file") {
		t.Errorf("login with no terminal and no --password-file: %v, want a refusal naming --password-file", err)
	}
	n.login(t, env, "pat", "pat's password")
	if _, _, err := dfsctl(env, "password", "change"); err == nil || !strings.Contains(err.Error(), "--current-password-file") {
		t.Errorf("password change with no terminal: %v, want a refusal naming --current-password-file", err)
	}
}

// logout ends the session on the server, so its refresh token refreshes no
// more, and clears the context's tokens, keeping its server and account.
func TestLogoutEndsTheSession(t *testing.T) {
	n := startNode(t)
	n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	env := newEnv(t, "", false)
	before := n.login(t, env, "pat", "pat's password")
	if _, errOut, err := dfsctl(env, "logout"); err != nil || errOut != "" {
		t.Fatalf("logout: %v, stderr %q, want it signed out", err, errOut)
	}
	after := stored(t, env, "default")
	if after.AccessToken != "" || after.RefreshToken != "" || after.ServerURL != n.socket || after.Username != "pat" {
		t.Errorf("the context after logout: %+v, want no tokens and the server and account kept", after)
	}
	c, err := cmdutil.Dial(n.socket, "")
	if err != nil {
		t.Fatal(err)
	}
	var p *apiv1.Problem
	if _, err := c.Refresh(t.Context(), before.RefreshToken); !errors.As(err, &p) || p.Status != http.StatusUnauthorized {
		t.Errorf("a refresh of the signed-out session: %v, want 401", err)
	}
	if err := health(t.Context(), env); !errors.Is(err, credentials.ErrNotLoggedIn) {
		t.Errorf("a command after logout: %v, want ErrNotLoggedIn", err)
	}
}

// login --token-file keeps an API token, with no refresh token, in a context
// of its own, and its commands carry it.
func TestLoginWithAPIToken(t *testing.T) {
	n := startNode(t)
	p := n.h.Account(t, "pat", "pat's password", entity.RoleViewer)
	env := newEnv(t, "", false)
	session := n.login(t, env, "pat", "pat's password")

	body, _ := json.Marshal(apiv1.APITokenSpec{Expires: n.h.Clock().Now().Add(24 * time.Hour)})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://node/api/v1/users/"+uuid.UUID(p.ID).String()+"/api-tokens", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", n.socket)
	}}}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tok apiv1.APIToken
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK || tok.Status.Token == "" {
		t.Fatalf("create an API token: status %d, %v, want 200 and a token", resp.StatusCode, err)
	}

	mustRun(t, env, "login", "-c", "ci", "--server", n.socket, "--token-file", file(t, "token", tok.Status.Token+"\n"))
	if c := stored(t, env, "ci"); c.AccessToken != tok.Status.Token || c.RefreshToken != "" {
		t.Errorf("the API token's context: %+v, want the token and no refresh token", c)
	}
	if err := health(t.Context(), env); err != nil {
		t.Errorf("a command with the API token: %v, want it answered", err)
	}
	if _, _, err := dfsctl(env, "login", "-c", "bad", "--server", n.socket, "--token-file", file(t, "bad", "not a token")); err == nil {
		t.Error("login with a token the server refuses: nil, want an error")
	}
	if _, err := credentials.NewStoreAt(env.ConfigPath); err != nil {
		t.Fatal(err)
	}
	s, _ := credentials.NewStoreAt(env.ConfigPath)
	if _, err := s.GetContext("bad"); !errors.Is(err, credentials.ErrContextNotFound) {
		t.Errorf("the context of a refused token: %v, want it never saved", err)
	}
}
