// Package clisuite is dfsctl's shared suite: rows that each run commands
// through a Runner against a Server and check what a user reads and what
// the server holds afterwards. One Runner runs dfsctl in process, over a
// node the harness starts; another runs it as a child process. Only tests
// import this package, so dfsctl's binary never links it or the harness.
package clisuite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/commands"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// Runner runs one dfsctl invocation with args under ctx, over a credentials
// file of its own, and returns its stdout, its stderr and its exit status.
type Runner func(ctx context.Context, args ...string) (stdout, stderr string, status int)

// InProcess runs each invocation through a root built in this process over
// env, its streams replaced by buffers of the invocation's own.
func InProcess(env cmdutil.Env) Runner {
	return func(ctx context.Context, args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		env.Out, env.Err = &out, &errOut
		status := commands.Run(ctx, env, args)
		return out.String(), errOut.String(), status
	}
}

// Server is the server the rows run against.
type Server struct {
	// Addr is the server a context names: its URL or its local socket's path.
	Addr string
	// User and Password sign in an account of the admin role.
	User, Password string
	// Harness is the node's harness when the server is one the test started,
	// and nil otherwise, when a harness-only row is skipped.
	Harness *nodetest.Harness
}

// Start starts a node over an in-memory store, its API on a local socket,
// with an admin account, and stops it when the test ends.
func Start(t *testing.T) Server {
	t.Helper()
	// Not t.TempDir(), whose path can pass the length a socket's may have.
	dir, err := os.MkdirTemp("", "clisuite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv := Server{Addr: filepath.Join(dir, "api.sock"), User: "suite", Password: "the suite's password"}
	srv.Harness = nodetest.New(t, nodetest.InMemory(), nodetest.APISocket(srv.Addr))
	if err := srv.Harness.Node().Start(t.Context()); err != nil {
		t.Fatalf("clisuite: start the node: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Harness.Node().Stop(context.Background()); err != nil {
			t.Errorf("clisuite: stop the node: %v", err)
		}
	})
	srv.Harness.Account(t, srv.User, srv.Password, entity.RoleAdmin)
	return srv
}

// Login signs run's context in to s as its admin account.
func (s Server) Login(t *testing.T, run Runner) {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pw, []byte(s.Password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, status := run(t.Context(), "login", "--server", s.Addr, "-u", s.User, "--password-file", pw); status != 0 {
		t.Fatalf("clisuite: dfsctl login: status %d, stderr %q, want 0", status, errOut)
	}
}

// Client returns a client of s signed in as its admin account in a session
// of its own, through which a row sets up and reads back.
func (s Server) Client(t *testing.T) *client.Client {
	t.Helper()
	c, err := cmdutil.Dial(s.Addr, "")
	if err != nil {
		t.Fatalf("clisuite: %v", err)
	}
	tk, err := c.SignIn(t.Context(), s.User, []byte(s.Password))
	if err != nil {
		t.Fatalf("clisuite: sign %s in: %v", s.User, err)
	}
	if c, err = cmdutil.Dial(s.Addr, "", client.WithTokenSource(session(tk.Access))); err != nil {
		t.Fatalf("clisuite: %v", err)
	}
	return c
}

// session is a session's access token, which it never refreshes: no row
// advances the clock past its lifetime.
type session string

func (k session) Token(context.Context) (string, error) { return string(k), nil }
func (session) Refresh(context.Context, string) (string, error) {
	return "", errors.New("clisuite: the suite's own session is not refreshed")
}

// Settle waits until op ends and returns it as it ended. On a harness's
// node it advances the node's clock a second at a time meanwhile, since
// nothing else moves that clock; on another server the operation ends on
// its own.
func (s Server) Settle(t *testing.T, c *client.Client, op apiv1.Operation) apiv1.Operation {
	t.Helper()
	type waited struct {
		op  apiv1.Operation
		err error
	}
	done := make(chan waited, 1)
	go func() {
		o, err := c.Wait(t.Context(), op)
		done <- waited{o, err}
	}()
	deadline := time.After(time.Minute)
	for {
		select {
		case w := <-done:
			if w.err != nil {
				t.Fatalf("clisuite: wait for operation %s: %v", op.ID, w.err)
			}
			return w.op
		case <-deadline:
			t.Fatalf("clisuite: operation %s still %s a minute on, want it ended", op.ID, op.State)
		case <-time.After(10 * time.Millisecond):
			// Polling the wait's end, a second of the node's clock a poll.
			if s.Harness != nil {
				s.Harness.Clock().Advance(time.Second)
			}
		}
	}
}

// Operation starts an operation on s, a group's deletion, and returns it as
// the deletion answered it.
func Operation(t *testing.T, c *client.Client) apiv1.Operation {
	t.Helper()
	g, err := c.Groups().Create(t.Context(), apiv1.GroupSpec{Name: "g-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("clisuite: create a group: %v", err)
	}
	op, err := c.Groups().Delete(t.Context(), g.ID, g.ETag)
	if err != nil || op == nil {
		t.Fatalf("clisuite: delete group %s: %+v, %v, want an operation", g.ID, op, err)
	}
	return *op
}

// row is one row of the suite. A harness-only row writes through the store
// or advances the node's clock itself, which only a harness's node allows.
type row struct {
	name        string
	harnessOnly bool
	run         func(t *testing.T, run Runner, s Server, c *client.Client)
}

// Run signs run in to s and runs every row as a subtest, a harness-only
// row only when s has a harness.
func Run(t *testing.T, run Runner, s Server) {
	s.Login(t, run)
	c := s.Client(t)
	for _, r := range rows() {
		t.Run(r.name, func(t *testing.T) {
			if r.harnessOnly && s.Harness == nil {
				t.Skip("harness-only: the server is not a node the test started")
			}
			r.run(t, run, s, c)
		})
	}
}

// rows is every row of the suite.
func rows() []row {
	return operationRows()
}
