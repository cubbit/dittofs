package cmdutil_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

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
		res, err := cmdutil.Update(cmd, env, c.Shares(), cmdutil.ByPatch(c.Shares()), "share", id, func(s *apiv1.ShareSpec) error { s.Path = path; return nil })
		if err != nil {
			return err
		}
		return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", nil)
	}}
	cmdutil.NameOrID(update, "share")
	update.Flags().String("path", "", "")
	// A netgroup's route takes only PUT, and a netgroup is named by its ID.
	members := &cobra.Command{Use: "members", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := cmdutil.NewClient(env)
		if err != nil {
			return err
		}
		m, _ := cmd.Flags().GetStringSlice("member")
		_, err = cmdutil.Update(cmd, env, c.Netgroups(), cmdutil.ByPut(c.Netgroups()), "netgroup", args[0], func(s *apiv1.NetgroupSpec) error { s.Members = m; return nil })
		return err
	}}
	members.Flags().StringSlice("member", nil, "")
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
		return cmdutil.Follow(cmd, c, op)
	}}
	cmdutil.NameOrID(del, "share")
	cmdutil.AddWait(del)
	cmd := &cobra.Command{Use: "sh"}
	cmd.AddCommand(update, del, members)
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
// is one named by --id, and the server holds each change; -o json and -o
// yaml each print the share's ID, generation and spec as the client reads
// them back.
func TestUpdateResolvesTheName(t *testing.T) {
	f := newFixture(t)
	for i, tc := range []struct{ format, path string }{{"json", "/moved"}, {"yaml", "/again"}} {
		args := []string{"sh", "update", "builds", "--path", tc.path, "-o", tc.format}
		if i == 1 {
			args = []string{"sh", "update", "--id", f.share.ID, "--path", tc.path, "-o", tc.format}
		}
		out, err := run(t.Context(), f.env, args...)
		if err != nil {
			t.Fatalf("dfsctl %s: %v", strings.Join(args, " "), err)
		}
		got, err := decodeShare(tc.format, out)
		if err != nil {
			t.Fatalf("dfsctl %s: decoding %q: %v", strings.Join(args, " "), out, err)
		}
		want, err := f.c.Shares().Get(t.Context(), f.share.ID)
		if err != nil {
			t.Fatalf("read the share back: %v", err)
		}
		// A write's answer carries only the state of the status, so the
		// status is left out.
		got.Status, want.Status = apiv1.ShareStatus{}, apiv1.ShareStatus{}
		if !reflect.DeepEqual(got, want.Resource) || want.Spec.Path != tc.path {
			t.Errorf("dfsctl %s: printed %+v, server holds %+v, want both at %s", strings.Join(args, " "), got, want.Resource, tc.path)
		}
	}
}

// decodeShare reads out, a share printed as format, json or yaml, the YAML
// through JSON as dfsctl prints it.
func decodeShare(format, out string) (apiv1.Share, error) {
	var sh apiv1.Share
	b := []byte(out)
	if format == "yaml" {
		var tree any
		if err := yaml.Unmarshal(b, &tree); err != nil {
			return sh, err
		}
		var err error
		if b, err = json.Marshal(tree); err != nil {
			return sh, err
		}
	}
	return sh, json.Unmarshal(b, &sh)
}

// A positional argument is a name even when it spells another share's ID:
// the share so named is updated, and the share of that ID is not.
func TestAPositionalArgumentIsAlwaysAName(t *testing.T) {
	f := newFixture(t)
	other, err := f.c.CreateShare(t.Context(), apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: f.share.ID, Path: "/other", Namespace: f.share.Spec.Namespace},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}}},
	})
	if err != nil {
		t.Fatalf("create a share named %s: %v", f.share.ID, err)
	}
	if _, err := run(t.Context(), f.env, "sh", "update", f.share.ID, "--path", "/named", "-o", "json"); err != nil {
		t.Fatalf("sh update %s: %v", f.share.ID, err)
	}
	if got := f.path(t); got != "/builds" {
		t.Errorf("the share whose ID was given as a name: path %q, want /builds unchanged", got)
	}
	if sh, err := f.c.Shares().Get(t.Context(), other.ID); err != nil || sh.Spec.Path != "/named" {
		t.Errorf("the share named %s: path %q (%v), want /named", f.share.ID, sh.Spec.Path, err)
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

// A resource changed between the update's read and its write is refused
// 412, through PATCH as through PUT: the command prints the difference
// between the spec it read and the one the server holds, naming the field,
// and writes nothing, so the resource keeps the other writer's value.
func TestUpdateConflictPrintsTheDiff(t *testing.T) {
	f := newFixture(t)
	ng, err := f.c.Netgroups().Put(t.Context(), "hosts", apiv1.NetgroupSpec{Members: []string{"a.example"}}, "*")
	if err != nil {
		t.Fatalf("create the netgroup: %v", err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		other func() error
		diff  []string
		held  func() string
		want  string
	}{
		{"share through PATCH", []string{"sh", "update", "builds", "--path", "/mine"},
			func() error {
				sh, err := f.c.Shares().Get(t.Context(), f.share.ID)
				if err == nil {
					_, err = f.c.Shares().Patch(t.Context(), f.share.ID, map[string]any{"spec": map[string]any{"path": "/theirs"}}, sh.ETag)
				}
				return err
			},
			[]string{"-path: /builds", "+path: /theirs"}, func() string { return f.path(t) }, "/theirs"},
		{"netgroup through PUT", []string{"sh", "members", "hosts", "--member", "mine.example"},
			func() error {
				_, err := f.c.Netgroups().Put(t.Context(), "hosts", apiv1.NetgroupSpec{Members: []string{"theirs.example"}}, ng.ETag)
				return err
			},
			[]string{"-  - a.example", "+  - theirs.example"},
			func() string {
				got, err := f.c.Netgroups().Get(t.Context(), "hosts")
				if err != nil {
					t.Fatalf("read the netgroup back: %v", err)
				}
				return strings.Join(got.Spec.Members, ",")
			}, "theirs.example"},
	} {
		paused := 0
		env := cmdutil.PauseAfterRead(f.env, func() {
			paused++
			if err := tc.other(); err != nil {
				t.Fatalf("%s: change it in the pause: %v", tc.name, err)
			}
		})
		_, err := run(t.Context(), env, tc.args...)
		if paused != 1 {
			t.Fatalf("%s: the pause after the read ran %d times, want once", tc.name, paused)
		}
		if err == nil {
			t.Fatalf("%s: got no error, want the 412 refused", tc.name)
		}
		for _, want := range append([]string{"changed since it was read, so nothing was written"}, tc.diff...) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: got %q, want it to contain %q", tc.name, err, want)
			}
		}
		if got := tc.held(); got != tc.want {
			t.Errorf("%s: the server holds %q after the refused update, want %q, the other writer's", tc.name, got, tc.want)
		}
	}
}

// An update that changes nothing writes nothing, so the share's generation
// stays as it was.
func TestUpdateOfNothingWritesNothing(t *testing.T) {
	f := newFixture(t)
	if _, err := run(t.Context(), f.env, "sh", "update", "builds", "--path", "/builds", "-o", "json"); err != nil {
		t.Fatalf("sh update builds to its own path: %v", err)
	}
	if sh, err := f.c.Shares().Get(t.Context(), f.share.ID); err != nil || sh.ETag != f.share.ETag {
		t.Errorf("the share after an update that changed nothing: tag %q (%v), want %q, unwritten", sh.ETag, err, f.share.ETag)
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

// A delete without --wait prints its operation's ID and returns while it
// runs; with --wait it follows the operation to its end as the node's clock
// advances, and prints it succeeded.
func TestDeleteWaitsOnlyWithWait(t *testing.T) {
	f := newFixture(t)
	// Nothing advances the node's clock here, so a delete that waited would
	// end only with this bound, failing on its error.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := run(ctx, f.env, "sh", "delete", "builds", "-o", "json")
	if err != nil {
		t.Fatalf("share delete builds: %v", err)
	}
	var op apiv1.Operation
	if err := json.Unmarshal([]byte(out), &op); err != nil || op.ID == "" || op.Kind != apiv1.OpShareDelete || op.State != apiv1.OpPending {
		t.Fatalf("share delete builds -o json: got %q (%v), want a pending share.delete with its ID", out, err)
	}

	g := newFixture(t)
	w := clisuite.Await(t, g.srv, "share delete --wait", background(t.Context(), g.env, "sh", "delete", "builds", "--wait", "-o", "json"))
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
	// Nothing advances the node's clock here, so a delete that waited would
	// end only with this bound, failing on its error.
	bounded, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	out, err := run(bounded, f.env, "sh", "delete", "builds", "-o", "json")
	var op apiv1.Operation
	if err != nil || json.Unmarshal([]byte(out), &op) != nil || op.ID == "" {
		t.Fatalf("share delete builds -o json: got %q, %v, want its operation", out, err)
	}
	base := f.srv.Harness.Clock().Pending()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := background(ctx, f.env, "operation", "wait", op.ID)
	// Polling until the wait has armed a timer on the node's clock, which
	// nothing advances here: past its read of the operation, so the
	// interrupt lands in the wait. Whether the timer is the poll's own or
	// another the node arms meanwhile, what the command prints is the same.
	for end := time.Now().Add(time.Minute); f.srv.Harness.Clock().Pending() <= base; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the wait's poll did not park within a minute")
		}
	}
	cancel()
	var w waited
	select {
	case w = <-done:
	case <-time.After(time.Minute):
		t.Fatal("operation wait: still waiting a minute after its context ended, want it ended")
	}
	want := "'dfsctl operation wait " + op.ID + "' resumes the wait"
	if w.err == nil || !strings.Contains(w.err.Error(), want) {
		t.Fatalf("operation wait interrupted: got %v, want an error naming %q", w.err, want)
	}
	if now, err := f.c.Operation(t.Context(), op.ID); err != nil || (now.State != apiv1.OpPending && now.State != apiv1.OpRunning) {
		t.Fatalf("the operation once its wait was interrupted: %s (%v), want it pending or running", now.State, err)
	}

	env := f.login(t)
	w = clisuite.Await(t, f.srv, "operation wait", background(t.Context(), env, "operation", "wait", op.ID, "-o", "json"))
	var ended apiv1.Operation
	if w.err != nil || json.Unmarshal([]byte(w.out), &ended) != nil || ended.ID != op.ID || ended.State != apiv1.OpSucceeded {
		t.Errorf("operation wait %s from a fresh context: got %q, %v, want it succeeded", op.ID, w.out, w.err)
	}
}
