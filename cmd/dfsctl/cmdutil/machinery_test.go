package cmdutil_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/commands"
	"github.com/cubbit/dittofs/cmd/dfsctl/internal/clisuite"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
)

// shareCmd is a share command, sh beside the root's own share, built on the
// machinery the way a resource's noun is: update resolves its share, by name or --id, and writes its path
// by read-modify-write; delete starts the share's deletion and follows it.
func shareCmd(env cmdutil.Env) *cobra.Command {
	update := &cobra.Command{Use: "update", RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := cmdutil.NewClient(env)
		if err != nil {
			return err
		}
		id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
		if err != nil {
			return err
		}
		path, _ := cmd.Flags().GetString("path")
		sh, err := cmdutil.Update(cmd, env, c.Shares(), "share", id, func(s *apiv1.ShareSpec) error { s.Path = path; return nil })
		if err != nil {
			return err
		}
		return cmdutil.PrintOutput(cmd, sh.Resource, false, "", nil)
	}}
	cmdutil.NameOrID(update, "share")
	update.Flags().String("path", "", "")
	del := &cobra.Command{Use: "delete", RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := cmdutil.NewClient(env)
		if err != nil {
			return err
		}
		id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
		if err != nil {
			return err
		}
		sh, err := c.Shares().Get(cmd.Context(), id)
		if err != nil {
			return err
		}
		op, err := c.Shares().Delete(cmd.Context(), id, sh.ETag)
		if err != nil {
			return err
		}
		return cmdutil.Follow(cmd, c, *op)
	}}
	cmdutil.NameOrID(del, "share")
	cmdutil.AddWait(del)
	cmd := &cobra.Command{Use: "sh"}
	cmd.AddCommand(update, del)
	return cmd
}

// fixture is a node, a share on it named "builds" at /builds, and an
// invocation's environment signed in to the node as an admin.
type fixture struct {
	srv   clisuite.Server
	c     *client.Client
	env   cmdutil.Env
	share client.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{srv: clisuite.Start(t)}
	f.c = f.srv.Client(t)
	f.env = f.login(t)
	// The namespace is written through the store: the API refuses a
	// namespace's create until the settings are served.
	ns := entity.NamespaceID(uuid.New())
	if _, err := f.srv.Harness.Store().ControlPlane().PutNamespace(t.Context(), entity.Namespace{ID: ns, Store: uuid.New()}, 0); err != nil {
		t.Fatalf("put a namespace: %v", err)
	}
	var err error
	f.share, err = f.c.CreateShare(t.Context(), apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: "builds", Path: "/builds", Namespace: uuid.UUID(ns).String()},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}}},
	})
	if err != nil {
		t.Fatalf("create the share: %v", err)
	}
	return f
}

// login is a fresh credentials file, signed in to the node.
func (f *fixture) login(t *testing.T) cmdutil.Env {
	t.Helper()
	env := cmdutil.Env{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "dfsctl", "config.json")}
	f.srv.Login(t, clisuite.InProcess(env))
	return env
}

// run runs one invocation in env, the share command added to its root, under
// ctx, and returns its stdout and its error.
func run(ctx context.Context, env cmdutil.Env, args ...string) (string, error) {
	var out bytes.Buffer
	env.Out = &out
	root := commands.NewRoot(env)
	root.AddCommand(shareCmd(env))
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// path is the share's path as the server holds it.
func (f *fixture) path(t *testing.T) string {
	t.Helper()
	sh, err := f.c.Shares().Get(t.Context(), f.share.ID)
	if err != nil {
		t.Fatalf("read the share back: %v", err)
	}
	return sh.Spec.Path
}

// A share named on the command line is resolved to its ID and updated, as
// is one named by --id, and the server holds each change.
func TestUpdateResolvesTheName(t *testing.T) {
	f := newFixture(t)
	out, err := run(t.Context(), f.env, "sh", "update", "builds", "--path", "/moved", "-o", "json")
	if err != nil {
		t.Fatalf("share update builds: %v", err)
	}
	var printed apiv1.Share
	if err := json.Unmarshal([]byte(out), &printed); err != nil || printed.ID != f.share.ID || printed.Spec.Path != "/moved" {
		t.Errorf("share update builds -o json: got %q (%v), want share %s at /moved", out, err, f.share.ID)
	}
	if got := f.path(t); got != "/moved" {
		t.Errorf("the share's path after update by name: got %q, want /moved", got)
	}
	if _, err := run(t.Context(), f.env, "sh", "update", "--id", f.share.ID, "--path", "/again", "-o", "json"); err != nil {
		t.Fatalf("share update --id: %v", err)
	}
	if got := f.path(t); got != "/again" {
		t.Errorf("the share's path after update by --id: got %q, want /again", got)
	}
}

// A name that matches no share, and an --id that names none, are each
// refused naming it, and so is naming the share both ways or neither.
func TestUpdateRefusesWhatNamesNothing(t *testing.T) {
	f := newFixture(t)
	missing := uuid.NewString()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"nosuch"}, `dfsctl sh update: no share named "nosuch"`},
		{[]string{"--id", missing}, "dfsctl sh update: reading share " + missing + ": 404"},
		{[]string{"builds", "--id", f.share.ID}, "dfsctl sh update: name the share by its name or by --id, exactly one"},
		{nil, "dfsctl sh update: name the share by its name or by --id, exactly one"},
	} {
		args := append([]string{"sh", "update", "--path", "/x"}, tc.args...)
		if _, err := run(t.Context(), f.env, args...); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("dfsctl %s: got %v, want an error starting %q", strings.Join(args, " "), err, tc.want)
		}
	}
	if got := f.path(t); got != "/builds" {
		t.Errorf("the share's path after the refusals: got %q, want /builds", got)
	}
}

// A share changed between the update's read and its write is refused 412:
// the command prints the difference between the spec it read and the one
// the server holds, naming the field, and writes nothing, so the share
// keeps the other writer's value.
func TestUpdateConflictPrintsTheDiff(t *testing.T) {
	f := newFixture(t)
	paused := 0
	env := cmdutil.PauseAfterRead(f.env, func() {
		paused++
		sh, err := f.c.Shares().Get(t.Context(), f.share.ID)
		if err != nil {
			t.Fatalf("read the share in the pause: %v", err)
		}
		if _, err := f.c.Shares().Patch(t.Context(), f.share.ID, map[string]any{"spec": map[string]any{"path": "/theirs"}}, sh.ETag); err != nil {
			t.Fatalf("change the share in the pause: %v", err)
		}
	})
	_, err := run(t.Context(), env, "sh", "update", "builds", "--path", "/mine")
	if paused != 1 {
		t.Fatalf("the pause after the read: ran %d times, want once", paused)
	}
	if err == nil {
		t.Fatal("share update after a concurrent change: got no error, want the 412 refused")
	}
	for _, want := range []string{"changed since it was read, so nothing was written", "-path: /builds", "+path: /theirs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("share update after a concurrent change: got %q, want it to contain %q", err, want)
		}
	}
	if got := f.path(t); got != "/theirs" {
		t.Errorf("the share's path after the refused update: got %q, want /theirs, the other writer's", got)
	}
}

// waited is what an invocation run in the background returned.
type waited struct {
	out string
	err error
}

// background runs one invocation under ctx in the background.
func background(ctx context.Context, env cmdutil.Env, args ...string) <-chan waited {
	done := make(chan waited, 1)
	go func() {
		out, err := run(ctx, env, args...)
		done <- waited{out, err}
	}()
	return done
}

// settle advances the node's clock a second at a time until done answers,
// and returns the answer.
func (f *fixture) settle(t *testing.T, what string, done <-chan waited) waited {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		select {
		case w := <-done:
			return w
		case <-deadline:
			t.Fatalf("%s: not ended a minute on", what)
		case <-time.After(10 * time.Millisecond):
			// Polling the command's end, a second of the node's clock a poll.
			f.srv.Harness.Clock().Advance(time.Second)
		}
	}
}

// A delete without --wait prints its operation's ID and returns while it
// runs; with --wait it follows the operation to its end as the node's clock
// advances, and prints it succeeded.
func TestDeleteWaitsOnlyWithWait(t *testing.T) {
	f := newFixture(t)
	out, err := run(t.Context(), f.env, "sh", "delete", "builds", "-o", "json")
	if err != nil {
		t.Fatalf("share delete builds: %v", err)
	}
	var op apiv1.Operation
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.ID == "" || op.Kind != apiv1.OpShareDelete || op.State != apiv1.OpPending {
		t.Fatalf("share delete builds -o json: got %q (%v), want a pending share.delete with its ID", out, err)
	}

	g := newFixture(t)
	w := g.settle(t, "share delete --wait", background(t.Context(), g.env, "sh", "delete", "builds", "--wait", "-o", "json"))
	if w.err != nil {
		t.Fatalf("share delete builds --wait: %v", w.err)
	}
	if err := json.Unmarshal([]byte(w.out), &op); err != nil || op.State != apiv1.OpSucceeded || op.Target.ID != g.share.ID {
		t.Errorf("share delete builds --wait -o json: got %q (%v), want the deletion of %s succeeded", w.out, err, g.share.ID)
	}
	if _, err := g.c.Shares().Get(t.Context(), g.share.ID); err == nil {
		t.Errorf("the share after its deletion ended: read answered it, want it gone")
	}
}

// A wait interrupted through its context leaves the operation running and
// prints the command that resumes it; that command, from a fresh root and
// credentials file, follows the operation to its end.
func TestInterruptedWaitResumes(t *testing.T) {
	f := newFixture(t)
	out, err := run(t.Context(), f.env, "sh", "delete", "builds", "-o", "json")
	var op apiv1.Operation
	if err != nil || json.Unmarshal([]byte(out), &op) != nil || op.ID == "" {
		t.Fatalf("share delete builds -o json: got %q, %v, want its operation", out, err)
	}
	base := f.srv.Harness.Clock().Pending()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := background(ctx, f.env, "operation", "wait", op.ID)
	// Polling until the wait's poll parks on the node's clock, which
	// nothing advances here.
	for end := time.Now().Add(time.Minute); f.srv.Harness.Clock().Pending() <= base; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the wait's poll did not park within a minute")
		}
	}
	cancel()
	w := <-done
	want := "'dfsctl operation wait " + op.ID + "' resumes the wait"
	if w.err == nil || !strings.Contains(w.err.Error(), want) {
		t.Fatalf("operation wait interrupted: got %v, want an error naming %q", w.err, want)
	}
	if now, err := f.c.Operation(t.Context(), op.ID); err != nil || now.State == apiv1.OpSucceeded {
		t.Fatalf("the operation once its wait was interrupted: %s (%v), want it still running", now.State, err)
	}

	env := f.login(t)
	w = f.settle(t, "operation wait", background(t.Context(), env, "operation", "wait", op.ID, "-o", "json"))
	var ended apiv1.Operation
	if w.err != nil || json.Unmarshal([]byte(w.out), &ended) != nil || ended.ID != op.ID || ended.State != apiv1.OpSucceeded {
		t.Errorf("operation wait %s from a fresh context: got %q, %v, want it succeeded", op.ID, w.out, w.err)
	}
}
