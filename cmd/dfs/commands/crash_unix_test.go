//go:build unix

package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/google/uuid"
)

// adminPassword is the administrator's first password dfs init sets for
// the crash rows, read from adminPasswordEnv.
const (
	adminPassword    = "dfs crash test admin password"
	adminPasswordEnv = "DFS_CRASH_TEST_ADMIN_PASSWORD"
)

// crashInstall initialises an installation under a directory of its own
// with dfs init, then, before any node opens it, writes through the
// metadata store a remote store with one setting, which nothing dials, and
// a namespace on it, and runs seed over the store. It returns the
// bootstrap's path and the namespace.
func crashInstall(t *testing.T, seed func(*kvstore.Store, entity.NamespaceID)) (config string, ns entity.NamespaceID) {
	t.Helper()
	dir := t.TempDir()
	skipUnsuitableMount(t, dir)
	config = initConfig(t, dir)
	t.Setenv(adminPasswordEnv, adminPassword)
	if _, err := runDfs("init", "--config", config, "--admin-password-env", adminPasswordEnv); err != nil {
		t.Fatalf("dfs init: %v", err)
	}
	ctx := t.Context()
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(dir, "store"), NodeID: testNode})
	if err != nil {
		t.Fatalf("open the metadata store to seed it: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the seeded store: %v", err)
		}
	}()
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatalf("open the metadata store to seed it: %v", err)
	}
	cp := st.ControlPlane()
	store := uuid.New()
	endpoint := entity.Setting{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: store}, Key: "endpoint", Value: []byte("https://store.invalid")}
	if _, err := cp.PutSetting(ctx, endpoint, 0); err != nil {
		t.Fatalf("seed the remote store: %v", err)
	}
	ns = entity.NewNamespaceID()
	if _, err := cp.PutNamespace(ctx, entity.Namespace{ID: ns, Store: store}, 0); err != nil {
		t.Fatalf("seed the namespace: %v", err)
	}
	if seed != nil {
		seed(st, ns)
	}
	return config, ns
}

// child is a dfs start child as a crash row drives it: its API, signed in
// as the administrator, and its output, read to its end as it comes.
type child struct {
	cmd   *exec.Cmd
	api   *client.Client
	held  chan struct{} // closed once the child printed heldLine
	ended chan struct{} // closed once its output ended, the child exited

	mu  sync.Mutex
	log strings.Builder
}

// output is the child's output so far.
func (c *child) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.log.String()
}

// serve starts dfs start over config with env, waits until its API answers
// ready, and signs in as the administrator. Every wait on it ends with its
// output, once startChild's bound kills it.
func serve(t *testing.T, config string, env ...string) *child {
	t.Helper()
	cmd, out := startChild(t, config, env...)
	c := &child{cmd: cmd, held: make(chan struct{}), ended: make(chan struct{})}
	addr := make(chan string, 1)
	go func() {
		defer close(c.ended)
		lines := bufio.NewScanner(out)
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			line := lines.Text()
			c.mu.Lock()
			c.log.WriteString(line + "\n")
			c.mu.Unlock()
			if m := apiAddr.FindStringSubmatch(line); m != nil && len(addr) == 0 {
				addr <- m[1]
			}
			if line == heldLine {
				close(c.held)
			}
		}
		// A line past the scanner's buffer ends the scan, not the child, so
		// the rest is drained and the child never blocks on its output.
		_, _ = io.Copy(io.Discard, out)
	}()
	var a string
	select {
	case a = <-addr:
	case <-c.ended:
		t.Fatalf("dfs start ended before it logged its API's address:\n%s", c.output())
	}
	bare, err := client.New("http://" + a)
	if err != nil {
		t.Fatal(err)
	}
	poll(t, c, "its API answered ready", func(ctx context.Context) (string, error) { return ready(ctx, a) },
		func(word string) bool { return word == "ready" })
	tk, err := bare.SignIn(t.Context(), "admin", []byte(adminPassword))
	if err != nil {
		t.Fatalf("sign in as admin: %v\n%s", err, c.output())
	}
	if c.api, err = client.New("http://"+a, client.WithTokenSource(accessToken(tk.Access))); err != nil {
		t.Fatal(err)
	}
	return c
}

// ready asks the API's readiness probe at addr once and returns its word.
func ready(ctx context.Context, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health/ready", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), err
}

// accessToken is an access token a client carries as it is; a crash row's child
// lives shorter than the token.
type accessToken string

func (k accessToken) Token(context.Context) (string, error) { return string(k), nil }

func (k accessToken) Refresh(context.Context, string) (string, error) {
	return "", errors.New("the crash rows' token is not refreshed")
}

// heldOn waits until the child holds the call its holdEnv names.
func (c *child) heldOn(t *testing.T, what string) {
	t.Helper()
	select {
	case <-c.held:
	case <-c.ended:
		t.Fatalf("dfs start ended before it held %s:\n%s", what, c.output())
	}
}

// kill ends the child with SIGKILL, never a clean stop, and checks the
// signal ended it.
func (c *child) kill(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	<-c.ended
	if ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("dfs start after SIGKILL: %v, want killed by the signal:\n%s", c.cmd.ProcessState, c.output())
	}
}

// stop ends the child with SIGTERM and checks it exited zero.
func (c *child) stop(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	<-c.ended
	if code := c.cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("dfs start on SIGTERM: exit %d, want 0:\n%s", code, c.output())
	}
}

// hold is holdEnv's value: hold the n-th op on key made once the store
// holds a row under arm, nil for any.
func hold(op string, key []byte, n int, arm []byte) string {
	return fmt.Sprintf("%s=%s\n%s\n%d\n%x", holdEnv, op, keys.Describe(key), n, arm)
}

// poll reads what read answers until done holds of it, pausing between
// reads; the child's bound, which ends its API, ends the poll.
func poll[T any](t *testing.T, c *child, what string, read func(context.Context) (T, error), done func(T) bool) T {
	t.Helper()
	for {
		v, err := read(t.Context())
		if err == nil && done(v) {
			return v
		}
		select {
		case <-c.ended:
			t.Fatalf("dfs start ended before %s (last read %+v, %v):\n%s", what, v, err, c.output())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// applied reports whether a share's status has it applied on every node
// reporting it, at least one, and refused by none.
func applied(st apiv1.ShareStatus) bool {
	return st.Applied != nil && st.Applied.Of > 0 && st.Applied.Nodes == st.Applied.Of && st.Refused == 0
}

// §2.1's crash half: a share whose create was answered 201, the node killed
// before its reconcile read the share list, serves once the node restarts.
// Until that read the share reads applied on no node, so the row catches a
// status the API made up rather than read from the node, and a create whose
// record the crash lost.
func TestCrashBeforeTheShareListReadServesTheCreatedShare(t *testing.T) {
	config, ns := crashInstall(t, nil)
	// No share's row exists until the create commits, so the first scan of
	// the shares' rows after it is the reconcile's read of the share list.
	c := serve(t, config, hold(kvtest.OpScan, []byte{keys.KindS}, 1, []byte{keys.KindS}))
	sh, err := c.api.CreateShare(t.Context(), apiv1.NewShare{
		Spec:         apiv1.ShareSpec{Name: "crash", Path: "/crash", Namespace: uuid.UUID(ns).String()},
		ExportPolicy: apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{}},
	})
	if err != nil {
		t.Fatalf("POST /shares: %v, want 201\n%s", err, c.output())
	}
	c.heldOn(t, "the reconcile's read of the share list")
	got, err := c.api.Shares().Get(t.Context(), sh.ID)
	if err != nil {
		t.Fatalf("GET /shares/%s while the reconcile is held: %v", sh.ID, err)
	}
	if a := got.Status.Applied; a == nil || a.Nodes != 0 {
		t.Fatalf("share %s's status before the reconcile read the share list: applied %+v, want on no node", sh.ID, a)
	}
	c.kill(t)

	c = serve(t, config)
	poll(t, c, "the share served after the restart", func(ctx context.Context) (client.Tagged[apiv1.ShareSpec, apiv1.ShareStatus], error) {
		return c.api.Shares().Get(ctx, sh.ID)
	}, func(s client.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]) bool { return applied(s.Status) })
	c.stop(t)
}

// deletedFiles is how many files the share the deletion row removes holds.
//
// ponytail: RFC 23's row seeds 10⁵ files. Each batch of step 4 scans from the
// share's first file key, over the rows earlier batches deleted, so the drop
// grows with the square of the files: under -race 4000 files do not end
// inside the child's bound. 50 files are 151 rows under the share's file
// prefix, 8 batches of step 4, so the files phase spans several batches on
// each side of each kill. Seed 10⁵ once the shard's drop keeps its last key,
// as the ponytail on ContinueShardRemoval says.
const deletedFiles = 50

// §3.2 delete: a share's deletion, the node killed with its files phase half
// done, some of the share's file rows dropped and some left, reads back after
// the restart as the same operation, running, Done never below what it read
// before the kill; the restarted node, held and killed at its second batch,
// has dropped more rows, so it resumed the drop; a third start ends it
// succeeded with no file row left. A second delete, under a key of its own,
// is refused 409 stale.
func TestCrashDuringAShareDeletionEndsSucceeded(t *testing.T) {
	if testing.Short() {
		t.Skip("the row starts dfs three times")
	}
	share := entity.Share{ID: entity.NewShareID(), Name: "doomed", Path: "/doomed", Root: entity.NewFileID()}
	config, _ := crashInstall(t, func(st *kvstore.Store, ns entity.NamespaceID) {
		ctx := t.Context()
		share.Namespaces = []entity.NamespaceGen{{Generation: 1, Namespace: ns}}
		if err := st.ControlPlane().Write(ctx, entity.WriteMeta{}, func(tx entity.ControlPlaneTx) error {
			_, err := tx.PutShare(share, 0)
			return err
		}); err != nil {
			t.Fatalf("seed the share: %v", err)
		}
		var err error
		if share, _, err = st.ShareIndex().Share(ctx, share.ID); err != nil {
			t.Fatalf("read the seeded share back: %v", err)
		}
		owner := entity.Identity{Principal: entity.NewUser(), Authenticated: true, Admin: true, Privileged: true}
		if _, err := st.ControlPlane().PutUser(ctx, entity.User{Principal: owner.Principal, Name: "files-owner"}, 0, entity.Alloc{}); err != nil {
			t.Fatalf("seed the files' owner: %v", err)
		}
		if _, err := st.ControlPlane().SetGrant(ctx, entity.ShareGrant{Share: share.ID, Principal: owner.Principal, Access: entity.AccessAdmin}, 0); err != nil {
			t.Fatalf("grant the files' owner the share: %v", err)
		}
		root := entity.NewHandle(share.ID, share.Root)
		for i := range deletedFiles {
			if _, _, err := st.Namespace().Create(ctx, owner, root, fmt.Appendf(nil, "f%d", i), kvstore.Regular(0o644)); err != nil {
				t.Fatalf("seed file %d: %v", i, err)
			}
		}
	})

	// Every scan of the share's file rows is a batch of step 4, the files
	// phase: the second scan is held, after the first batch dropped its rows.
	// Both holds below count on that: nothing else scans the share's file
	// prefix, at a start or otherwise. A start that did would have the hold
	// land on the first batch instead, and the row then fails on the rows
	// left at a kill, not at random.
	files := keys.ID([]byte{keys.KindF}, share.ID[:])
	seeded := fileRows(t, config, files)
	c := serve(t, config, hold(kvtest.OpScan, files, 2, nil))
	id := share.ID.String()
	poll(t, c, "the share served", func(ctx context.Context) (client.Tagged[apiv1.ShareSpec, apiv1.ShareStatus], error) {
		return c.api.Shares().Get(ctx, id)
	}, func(s client.Tagged[apiv1.ShareSpec, apiv1.ShareStatus]) bool { return applied(s.Status) })
	tagged, err := c.api.Shares().Get(t.Context(), id)
	if err != nil {
		t.Fatalf("GET /shares/%s: %v", id, err)
	}
	op, err := c.api.Shares().Delete(t.Context(), id, tagged.ETag)
	if err != nil || op == nil {
		t.Fatalf("DELETE /shares/%s: %+v, %v; want 202 with an operation", id, op, err)
	}
	c.heldOn(t, "the removal's next batch of files")
	before, err := c.api.Operation(t.Context(), op.ID)
	if err != nil || before.State != apiv1.OpRunning || before.Progress.Phase != "files" || before.Progress.Done <= 0 {
		t.Fatalf("operation %s held at a batch of files: %+v (%v), want running in the files phase with Done above 0", op.ID, before, err)
	}
	var p *apiv1.Problem
	if _, err := c.api.Shares().Delete(t.Context(), id, tagged.ETag); !errors.As(err, &p) || p.Status != http.StatusConflict || p.Code != "stale" {
		t.Fatalf("a second DELETE /shares/%s under a new key: %v, want 409 stale", id, err)
	}
	c.kill(t)
	left := fileRows(t, config, files)
	if left == 0 || left >= seeded {
		t.Fatalf("share %s's file rows at the kill: %d left of %d, want some dropped and some left", id, left, seeded)
	}

	c = serve(t, config, hold(kvtest.OpScan, files, 2, nil))
	c.heldOn(t, "the resumed removal's second batch of files")
	resumed, err := c.api.Operation(t.Context(), op.ID)
	if err != nil || resumed.ID != op.ID || resumed.State != apiv1.OpRunning || resumed.Progress.Done < before.Progress.Done {
		t.Fatalf("operation %s after the restart, held at its second batch of files: %+v (%v), want the same operation running, Done at least %d", op.ID, resumed, err, before.Progress.Done)
	}
	c.kill(t)
	if now := fileRows(t, config, files); now >= left {
		t.Fatalf("share %s's file rows at the second kill: %d left, want fewer than the %d at the first, the drop resumed", id, now, left)
	}

	c = serve(t, config)
	last := resumed
	end := poll(t, c, "the deletion succeeded after the restart", func(ctx context.Context) (apiv1.Operation, error) {
		now, err := c.api.Operation(ctx, op.ID)
		if err != nil {
			return now, err
		}
		if now.ID != op.ID || now.Progress.Done < last.Progress.Done ||
			now.State != apiv1.OpRunning && now.State != apiv1.OpSucceeded {
			t.Fatalf("operation %s after the restart: %+v, after %+v; want it running or succeeded, Done never falling", op.ID, now, last)
		}
		last = now
		return now, nil
	}, func(o apiv1.Operation) bool { return o.State == apiv1.OpSucceeded })
	if end.Progress.Done <= before.Progress.Done {
		t.Errorf("operation %s succeeded with Done %d, want above the %d it read before the kill", op.ID, end.Progress.Done, before.Progress.Done)
	}
	var gone *apiv1.Problem
	if _, err := c.api.Shares().Get(t.Context(), id); !errors.As(err, &gone) || gone.Status != http.StatusNotFound {
		t.Errorf("GET /shares/%s once its deletion succeeded: %v, want 404", id, err)
	}
	c.stop(t)
	if left := fileRows(t, config, files); left != 0 {
		t.Errorf("share %s's file rows once its deletion succeeded: %d left, want none", id, left)
	}
}

// fileRows counts the records under prefix in the metadata store of the
// installation whose bootstrap is config, with no node holding it open.
func fileRows(t *testing.T, config string, prefix []byte) int {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{Dir: filepath.Join(filepath.Dir(config), "store"), NodeID: testNode})
	if err != nil {
		t.Fatalf("open the metadata store to count its file rows: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close the counted store: %v", err)
		}
	}()
	n := 0
	if err := db.View(t.Context(), func(r kv.Reader) error {
		for _, err := range r.Scan(prefix, nil) {
			if err != nil {
				return err
			}
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("count the file rows under %x: %v", prefix, err)
	}
	return n
}
